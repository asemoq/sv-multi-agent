// Package embeddings implements an OpenAI-compatible embeddings client.
package embeddings

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// Options configures the embeddings client.
type Options struct {
	BaseURL            string
	APIKey             string
	Model              string
	Dim                int
	Timeout            time.Duration
	InsecureSkipVerify bool
	CAFile             string
	QueryPrefix        string
	BatchSize          int
}

// Client calls POST {BaseURL}/embeddings.
type Client struct {
	baseURL     string
	apiKey      string
	model       string
	dim         int
	queryPrefix string
	batchSize   int
	http        *http.Client
}

// New creates an embeddings client.
func New(opts Options) (*Client, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		return nil, errors.New("EMBEDDING_BASE_URL is required")
	}
	if strings.TrimSpace(opts.Model) == "" {
		return nil, errors.New("EMBEDDING_MODEL is required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	tlsConfig := &tls.Config{InsecureSkipVerify: opts.InsecureSkipVerify} //nolint:gosec // explicit opt-in
	if opts.CAFile != "" {
		pool, err := loadCA(opts.CAFile)
		if err != nil {
			return nil, err
		}
		tlsConfig.RootCAs = pool
	}

	batchSize := opts.BatchSize
	if batchSize < 1 {
		batchSize = 1
	}

	return &Client{
		baseURL:     baseURL,
		apiKey:      opts.APIKey,
		model:       opts.Model,
		dim:         opts.Dim,
		queryPrefix: strings.ReplaceAll(opts.QueryPrefix, "\\n", "\n"),
		batchSize:   batchSize,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig: tlsConfig,
				Proxy:           http.ProxyFromEnvironment,
			},
		},
	}, nil
}

// Model returns the configured model ID.
func (c *Client) Model() string { return c.model }

// Dim returns the configured embedding dimension (0 means unvalidated).
func (c *Client) Dim() int { return c.dim }

// EmbedQuery embeds a search query with the instruction prefix.
func (c *Client) EmbedQuery(ctx context.Context, query string) ([]float32, error) {
	text := query
	if strings.TrimSpace(c.queryPrefix) != "" {
		text = c.queryPrefix + query
	}
	vectors, err := c.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		return nil, errors.New("embeddings: empty response")
	}
	return vectors[0], nil
}

// EmbedDocuments embeds documents without the query prefix.
func (c *Client) EmbedDocuments(ctx context.Context, documents []string) ([][]float32, error) {
	return c.Embed(ctx, documents)
}

// Embed embeds texts in batches.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	result := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += c.batchSize {
		end := start + c.batchSize
		if end > len(texts) {
			end = len(texts)
		}
		vectors, err := c.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		result = append(result, vectors...)
	}
	return result, nil
}

// Check verifies the endpoint and returns the observed dimension.
func (c *Client) Check(ctx context.Context) (int, error) {
	vector, err := c.EmbedQuery(ctx, "health check")
	if err != nil {
		return 0, err
	}
	return len(vector), nil
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Model string `json:"model"`
	Data  []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: c.model, Input: texts})
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := wait(ctx, time.Duration(attempt)*time.Second); err != nil {
				return nil, err
			}
		}
		vectors, retry, err := c.doEmbed(ctx, body, len(texts))
		if err == nil {
			return vectors, nil
		}
		lastErr = err
		if !retry {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) doEmbed(ctx context.Context, body []byte, count int) ([][]float32, bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	request.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return nil, true, fmt.Errorf("embeddings request: %w", err)
	}
	defer response.Body.Close()

	data, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return nil, true, fmt.Errorf("embeddings read: %w", err)
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
		return nil, true, fmt.Errorf("embeddings HTTP %d: %s", response.StatusCode, truncate(string(data), 500))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, false, fmt.Errorf("embeddings HTTP %d: %s", response.StatusCode, truncate(string(data), 500))
	}

	var parsed embedResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, false, fmt.Errorf("embeddings decode: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, false, errors.New(parsed.Error.Message)
	}
	if len(parsed.Data) != count {
		return nil, false, fmt.Errorf("embeddings: expected %d vectors, got %d", count, len(parsed.Data))
	}

	sort.SliceStable(parsed.Data, func(i, j int) bool { return parsed.Data[i].Index < parsed.Data[j].Index })
	vectors := make([][]float32, len(parsed.Data))
	for index, item := range parsed.Data {
		if c.dim > 0 && len(item.Embedding) != c.dim {
			return nil, false, fmt.Errorf("embeddings: dimension mismatch: got %d, want %d", len(item.Embedding), c.dim)
		}
		vectors[index] = item.Embedding
	}
	return vectors, false, nil
}

func loadCA(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read EMBEDDING_CA_FILE: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("EMBEDDING_CA_FILE %q contains no certificates", path)
	}
	return pool, nil
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
