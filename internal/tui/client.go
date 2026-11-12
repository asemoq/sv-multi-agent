// Package tui implements the interactive terminal client for the
// sv-multi-agent HTTP/JSON control plane. It is modelled on the OpenCode TUI:
// a live task list, a streaming event log, and an inline prompt to submit work.
package tui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/asemoq/sv-multi-agent/internal/events"
	"github.com/asemoq/sv-multi-agent/internal/store"
)

// Client is a small HTTP/SSE client for the control plane.
type Client struct {
	base string
	http *http.Client
}

// NewClient normalises addr (host:port or URL) into a base URL.
func NewClient(addr string) *Client {
	base := strings.TrimSpace(addr)
	if base == "" {
		base = "127.0.0.1:50050"
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	return &Client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// Base returns the normalised base URL.
func (c *Client) Base() string { return c.base }

// Health reports whether the service answers /healthz.
func (c *Client) Health(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	return resp.StatusCode == http.StatusOK
}

// ListTasks returns the most recent tasks.
func (c *Client) ListTasks(ctx context.Context, limit int) ([]store.Task, error) {
	endpoint := fmt.Sprintf("%s/v1/tasks?limit=%d", c.base, limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, httpError(resp.StatusCode, data)
	}
	var payload struct {
		Tasks []store.Task `json:"tasks"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode tasks: %w", err)
	}
	return payload.Tasks, nil
}

// Submit creates a generate-mode task and returns its ID.
func (c *Client) Submit(ctx context.Context, prompt string, maxIterations int) (string, error) {
	body, err := json.Marshal(map[string]any{
		"prompt":         prompt,
		"max_iterations": maxIterations,
	})
	if err != nil {
		return "", err
	}
	return c.postTask(ctx, body)
}

// SubmitAnalyze creates an analyze-mode task from an existing RTL bundle
// (files keyed as rtl/... or tb/...).
func (c *Client) SubmitAnalyze(ctx context.Context, prompt string, maxIterations int, files map[string]string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"prompt":         prompt,
		"max_iterations": maxIterations,
		"source_mode":    "analyze",
		"files":          files,
	})
	if err != nil {
		return "", err
	}
	return c.postTask(ctx, body)
}

func (c *Client) postTask(ctx context.Context, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/tasks", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", httpError(resp.StatusCode, data)
	}
	var out struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode submit: %w", err)
	}
	if out.TaskID == "" {
		return "", fmt.Errorf("server did not return task_id")
	}
	return out.TaskID, nil
}

// Report fetches the markdown report for a task.
func (c *Client) Report(ctx context.Context, id string) (string, error) {
	endpoint := c.base + "/v1/tasks/" + url.PathEscape(id) + "/report"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", httpError(resp.StatusCode, data)
	}
	return string(data), nil
}

// StreamEvents connects to the SSE endpoint for id and pushes events into out.
// It closes out when the stream ends or ctx is cancelled.
func (c *Client) StreamEvents(ctx context.Context, id string, out chan<- events.Event) {
	defer close(out)
	endpoint := c.base + "/v1/tasks/" + url.PathEscape(id) + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	// A dedicated client without a global timeout: the context governs the
	// lifetime of the long-lived stream.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var ev events.Event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			return
		}
	}
}

func httpError(status int, data []byte) error {
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &payload) == nil && payload.Error != "" {
		return fmt.Errorf("%s", payload.Error)
	}
	return fmt.Errorf("http %d", status)
}
