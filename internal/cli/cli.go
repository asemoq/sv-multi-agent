// Package cli implements the client commands shared by svctl and the unified
// svma binary: submit, get, list and wait. It talks to the sv-multi-agent
// service over the HTTP/JSON control plane and renders the live event stream.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/asemoq/sv-multi-agent/internal/events"
)

// ErrTaskFailed is returned by Wait when the task ends in failed/interrupted.
var ErrTaskFailed = errors.New("task did not complete successfully")

// Run executes a client command. args is os.Args[1:].
func Run(args []string) error {
	if len(args) == 0 {
		return usageError()
	}

	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addr := fs.String("addr", env("SVMA_ADDR", "127.0.0.1:50050"), "sv-multi-agent HTTP address")
	prompt := fs.String("prompt", "", "task description")
	taskID := fs.String("task-id", "", "task ID")
	maxIterations := fs.Int("max-iterations", 2, "maximum debug/simulation iterations (1..5)")
	limit := fs.Int("limit", 20, "maximum tasks to list")
	interval := fs.Duration("interval", 2*time.Second, "poll interval for wait -json")
	timeout := fs.Duration("timeout", 30*time.Minute, "overall wait timeout")
	jsonOut := fs.Bool("json", false, "print raw JSON instead of the live view")
	detach := fs.Bool("detach", false, "submit and return immediately (no live view)")
	rtlPath := fs.String("rtl", "", "existing RTL file or directory (analyze mode)")
	tbPath := fs.String("tb", "", "existing testbench file or directory (analyze mode)")
	dutTop := fs.String("top", "", "DUT top module for linting (analyze mode)")
	specPath := fs.String("spec", "", "specification file (analyze mode)")

	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	base := normalizeBaseURL(*addr)
	client := &http.Client{Timeout: 30 * time.Second}

	switch command {
	case "submit":
		if *prompt == "" {
			return fmt.Errorf("-prompt is required")
		}
		return submitTask(base, client, map[string]any{
			"prompt":         *prompt,
			"max_iterations": *maxIterations,
		}, *jsonOut, *detach, *timeout)
	case "analyze":
		text := strings.TrimSpace(*prompt)
		if *specPath != "" {
			data, err := os.ReadFile(*specPath)
			if err != nil {
				return fmt.Errorf("read spec: %w", err)
			}
			spec := strings.TrimSpace(string(data))
			if spec != "" {
				if text != "" {
					text = spec + "\n\n" + text
				} else {
					text = spec
				}
			}
		}
		if text == "" {
			return fmt.Errorf("-prompt or -spec is required")
		}
		if *rtlPath == "" {
			return fmt.Errorf("-rtl is required")
		}
		files := map[string]string{}
		if err := collectSourceDir(*rtlPath, "rtl", files); err != nil {
			return err
		}
		if *tbPath != "" {
			if err := collectSourceDir(*tbPath, "tb", files); err != nil {
				return err
			}
		}
		if err := checkBundleSize(files); err != nil {
			return err
		}
		return submitTask(base, client, map[string]any{
			"prompt":         text,
			"max_iterations": *maxIterations,
			"source_mode":    "analyze",
			"dut_top":        *dutTop,
			"files":          files,
		}, *jsonOut, *detach, *timeout)
	case "get":
		if *taskID == "" {
			return fmt.Errorf("-task-id is required")
		}
		result, err := doJSON(context.Background(), client, http.MethodGet, base+"/v1/tasks/"+url.PathEscape(*taskID), nil)
		if err != nil {
			return err
		}
		printJSON(result)
		return nil
	case "list":
		result, err := doJSON(context.Background(), client, http.MethodGet, base+"/v1/tasks?limit="+strconv.Itoa(*limit), nil)
		if err != nil {
			return err
		}
		printJSON(result)
		return nil
	case "wait":
		if *taskID == "" {
			return fmt.Errorf("-task-id is required")
		}
		if *jsonOut {
			return waitForTask(client, base, *taskID, *interval, *timeout)
		}
		return streamTaskEvents(base, *taskID, *timeout)
	case "report":
		if *taskID == "" {
			return fmt.Errorf("-task-id is required")
		}
		data, err := fetchText(client, base+"/v1/tasks/"+url.PathEscape(*taskID)+"/report")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	default:
		return usageError()
	}
}

const maxSourceBundleBytes = 24 << 20

func submitTask(base string, client *http.Client, payload map[string]any, jsonOut, detach bool, timeout time.Duration) error {
	result, err := doJSON(context.Background(), client, http.MethodPost, base+"/v1/tasks", payload)
	if err != nil {
		return err
	}
	if jsonOut || detach {
		printJSON(result)
		return nil
	}
	id, _ := result["task_id"].(string)
	if id == "" {
		return fmt.Errorf("server did not return task_id")
	}
	return streamTaskEvents(base, id, timeout)
}

// collectSourceDir adds an RTL/TB file or directory tree to the upload bundle.
// Keys are always prefix/<relative-path> with POSIX separators.
func collectSourceDir(root, prefix string, files map[string]string) error {
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("read %s: %w", root, err)
	}
	if !info.IsDir() {
		if !isCLIHDL(root) {
			return fmt.Errorf("%s is not a .sv or .svh file", root)
		}
		data, err := os.ReadFile(root)
		if err != nil {
			return fmt.Errorf("read %s: %w", root, err)
		}
		files[prefix+"/"+filepath.Base(root)] = string(data)
		return nil
	}

	count := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !isCLIHDL(path) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		files[prefix+"/"+filepath.ToSlash(rel)] = string(data)
		count++
		return nil
	})
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("no SystemVerilog sources found in %s", root)
	}
	return nil
}

func checkBundleSize(files map[string]string) error {
	total := 0
	for _, content := range files {
		total += len(content)
	}
	if total > maxSourceBundleBytes {
		return fmt.Errorf("source bundle is %d bytes; limit is %d bytes", total, maxSourceBundleBytes)
	}
	return nil
}

func isCLIHDL(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".sv", ".svh", ".sva":
		return true
	default:
		return false
	}
}

// streamTaskEvents subscribes to the task's SSE stream and renders it.
func streamTaskEvents(base, id string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	endpoint := base + "/v1/tasks/" + url.PathEscape(id) + "/events"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "text/event-stream")
	// No client timeout: the context governs the stream lifetime.
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("events: http %d", response.StatusCode)
	}

	renderer := newRenderer(os.Stdout)
	renderer.Header(id)
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var event events.Event
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		renderer.Render(event)
		if event.Kind == events.KindDone {
			if event.Status != "completed" {
				return fmt.Errorf("%w: %s", ErrTaskFailed, event.Status)
			}
			return nil
		}
	}
	return scanner.Err()
}

func waitForTask(client *http.Client, base, id string, interval, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		result, err := doJSON(ctx, client, http.MethodGet, base+"/v1/tasks/"+url.PathEscape(id), nil)
		if err != nil {
			return err
		}
		printJSON(result)

		status, _ := result["status"].(string)
		switch status {
		case "completed":
			return nil
		case "failed", "interrupted":
			return fmt.Errorf("%w: %s", ErrTaskFailed, status)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// fetchText performs a GET and returns the raw response body for non-2xx checks.
func fetchText(client *http.Client, endpoint string) ([]byte, error) {
	response, err := client.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var result map[string]any
		if json.Unmarshal(data, &result) == nil {
			if message, ok := result["error"].(string); ok && message != "" {
				return nil, errors.New(message)
			}
		}
		return nil, fmt.Errorf("http %d", response.StatusCode)
	}
	return data, nil
}

// doJSON performs one HTTP request and decodes a JSON object response.
func doJSON(ctx context.Context, client *http.Client, method, endpoint string, payload any) (map[string]any, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if message, ok := result["error"].(string); ok && message != "" {
			return nil, errors.New(message)
		}
		return nil, fmt.Errorf("http %d", response.StatusCode)
	}
	return result, nil
}

func normalizeBaseURL(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = "127.0.0.1:50050"
	}
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	return strings.TrimRight(addr, "/")
}

func usageError() error {
	return fmt.Errorf("usage: svma <serve|submit|analyze|get|list|wait|report|version> [flags]")
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// renderer prints the live event stream with a compact, opencode-like style.
type renderer struct {
	out   io.Writer
	color bool
}

func newRenderer(out io.Writer) *renderer {
	color := os.Getenv("NO_COLOR") == "" && term.IsTerminal(int(os.Stdout.Fd()))
	return &renderer{out: out, color: color}
}

func (r *renderer) Header(id string) {
	fmt.Fprintf(r.out, "%s %s\n", r.dim("task"), id)
}

func (r *renderer) Render(event events.Event) {
	switch event.Kind {
	case events.KindTaskStart:
		fmt.Fprintf(r.out, "\n%s %s\n", r.bold("▶ task"), event.Text)
	case events.KindStepStart:
		fmt.Fprintf(r.out, "\n%s %s\n", r.dim("⏳"), r.bold(event.Step))
	case events.KindStepEnd:
		icon := r.green("✓")
		if event.Status != "completed" {
			icon = r.red("✗")
		}
		fmt.Fprintf(r.out, "%s %s %s\n", icon, r.bold(event.Step), r.dim(event.Status))
		if strings.TrimSpace(event.Text) != "" {
			fmt.Fprintf(r.out, "  %s\n", r.dim(event.Text))
		}
	case events.KindReasoning:
		fmt.Fprintf(r.out, "%s %s\n", r.dim("·"), r.dim(event.Text))
	case events.KindContent:
		fmt.Fprintf(r.out, "%s %s\n", r.cyan("│"), event.Text)
	case events.KindArtifact:
		fmt.Fprintf(r.out, "%s %s\n", r.green("+"), event.Text)
	case events.KindSimulator:
		fmt.Fprintf(r.out, "%s %s\n", r.bold("sim"), event.Text)
	case events.KindInfo:
		fmt.Fprintf(r.out, "%s %s\n", r.dim("i"), event.Text)
	case events.KindDone:
		if event.Status == "completed" {
			fmt.Fprintf(r.out, "\n%s %s\n", r.green("✔ done"), r.bold(event.Text))
		} else {
			fmt.Fprintf(r.out, "\n%s %s\n", r.red("✖ failed"), r.bold(event.Text))
		}
	}
}

func (r *renderer) wrap(code, text string) string {
	if !r.color {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (r *renderer) bold(s string) string  { return r.wrap("1", s) }
func (r *renderer) dim(s string) string   { return r.wrap("2", s) }
func (r *renderer) green(s string) string { return r.wrap("32", s) }
func (r *renderer) red(s string) string   { return r.wrap("31", s) }
func (r *renderer) cyan(s string) string  { return r.wrap("36", s) }
