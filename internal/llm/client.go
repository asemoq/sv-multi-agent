package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/asemoq/sv-multi-agent/internal/events"
	"github.com/asemoq/sv-multi-agent/internal/logx"
)

// LLMClient handles communication with LLM providers
type LLMClient struct {
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float64
	Timeout     time.Duration
	HTTPClient  *http.Client
	// UserAgent identifies this agent to the provider (OpenCode Go rejects
	// generic HTTP-library user agents).
	UserAgent string
	// SessionID is the default x-opencode-session value; a per-task value can be
	// attached to the context with WithSession.
	SessionID string
	// MaxTokens caps the completion; reasoning models return empty content when
	// it is left at the provider default.
	MaxTokens int
}

// NewLLMClient creates a new LLM client
func NewLLMClient(baseURL, apiKey, model string, temperature float64, timeout time.Duration) *LLMClient {
	return &LLMClient{
		BaseURL:     baseURL,
		APIKey:      apiKey,
		Model:       model,
		Temperature: temperature,
		Timeout:     timeout,
		HTTPClient:  &http.Client{Timeout: timeout},
		UserAgent:   "sv-multi-agent/1.0",
		SessionID:   "svma-session",
		MaxTokens:   32768,
	}
}

type sessionKey struct{}

// WithSession attaches a stable conversation/session ID to ctx. The client
// forwards it as x-opencode-session so the gateway can route and cache per task.
func WithSession(ctx context.Context, sessionID string) context.Context {
	if strings.TrimSpace(sessionID) == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionKey{}, sessionID)
}

func sessionFromContext(ctx context.Context) string {
	if value, ok := ctx.Value(sessionKey{}).(string); ok {
		return value
	}
	return ""
}

// ChatCompletionRequest represents the request to LLM API
type ChatCompletionRequest struct {
	Model       string    `json:"model"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
	Messages    []Message `json:"messages"`
}

// Message represents a chat message
type Message struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

var (
	mdFenceOpenRe  = regexp.MustCompile("(?i)^```(?:json)?\\s*")
	mdFenceCloseRe = regexp.MustCompile("\\s*```$")
)

// ChatCompletionResponse represents the response from LLM API
type ChatCompletionResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// ChatJSON sends a system and user prompt to the LLM and returns parsed JSON response
func (c *LLMClient) ChatJSON(ctx context.Context, systemPrompt, userPrompt string) (map[string]any, error) {
	req := ChatCompletionRequest{
		Model:       c.Model,
		Temperature: c.Temperature,
		MaxTokens:   c.MaxTokens,
		Stream:      true,
		Messages: []Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}

	logx.Debugf("llm: request model=%s temperature=%.2f max_tokens=%d system_chars=%d user_chars=%d",
		c.Model, c.Temperature, c.MaxTokens, len(systemPrompt), len(userPrompt))
	logx.Debugf("llm: system prompt:\n%s", systemPrompt)
	logx.Debugf("llm: user prompt:\n%s", userPrompt)

	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/chat/completions", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create http request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	if c.UserAgent != "" {
		httpReq.Header.Set("User-Agent", c.UserAgent)
	}
	session := sessionFromContext(ctx)
	if session == "" {
		session = c.SessionID
	}
	if session != "" {
		httpReq.Header.Set("x-opencode-session", session)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		message := strings.TrimSpace(string(body))
		logx.Infof("llm: HTTP %d: %s", resp.StatusCode, message)
		return nil, fmt.Errorf("unexpected status code: %d: %s", resp.StatusCode, message)
	}

	var (
		content      string
		reasoning    string
		finishReason string
		totalTokens  int
	)
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		content, reasoning, finishReason, totalTokens, err = readStream(resp.Body, events.EmitterFrom(ctx))
		if err != nil {
			return nil, fmt.Errorf("read stream: %w", err)
		}
	} else {
		var respBody ChatCompletionResponse
		if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		if len(respBody.Choices) == 0 {
			return nil, fmt.Errorf("no choices in response")
		}
		choice := respBody.Choices[0]
		content = choice.Message.Content
		reasoning = choice.Message.ReasoningContent
		finishReason = choice.FinishReason
		totalTokens = respBody.Usage.TotalTokens
		if emitter := events.EmitterFrom(ctx); emitter != nil {
			if reasoning != "" {
				emitter.Reasoning(reasoning)
			}
			if content != "" {
				emitter.Content(content)
			}
		}
	}
	logx.Debugf("llm: response finish_reason=%q tokens=%d", finishReason, totalTokens)
	logx.Debugf("llm: reasoning:\n%s", reasoning)
	logx.Debugf("llm: content:\n%s", content)
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("empty response content (finish_reason=%q)", finishReason)
	}
	return parseJSONResponse(content)
}

// streamChunk is one SSE chunk of an OpenAI-compatible streaming response.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// readStream consumes an SSE chat-completions stream and returns the assembled
// content, reasoning and finish reason. Streaming keeps long reasoning requests
// alive past the gateway's non-streaming idle timeout.
func readStream(body io.Reader, emitter *events.Emitter) (string, string, string, int, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var content, reasoning strings.Builder
	var pendingReasoning, pendingContent strings.Builder
	finishReason := ""
	totalTokens := 0
	chunks := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return "", "", "", 0, fmt.Errorf("decode chunk: %w", err)
		}
		chunks++
		for _, choice := range chunk.Choices {
			if delta := choice.Delta.ReasoningContent; delta != "" {
				reasoning.WriteString(delta)
				pendingReasoning.WriteString(delta)
				if pendingReasoning.Len() >= streamFlushChars || strings.Contains(delta, "\n") {
					flushStream(&pendingReasoning, "think", emitter)
				}
			}
			if delta := choice.Delta.Content; delta != "" {
				content.WriteString(delta)
				pendingContent.WriteString(delta)
				if pendingContent.Len() >= streamFlushChars || strings.Contains(delta, "\n") {
					flushStream(&pendingContent, "write", emitter)
				}
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
		}
		if chunk.Usage != nil {
			totalTokens = chunk.Usage.TotalTokens
		}
		if chunks%50 == 0 {
			logx.Debugf("llm: streaming... chunks=%d content_chars=%d reasoning_chars=%d", chunks, content.Len(), reasoning.Len())
		}
	}
	flushStream(&pendingReasoning, "think", emitter)
	flushStream(&pendingContent, "write", emitter)
	if err := scanner.Err(); err != nil {
		return "", "", "", 0, err
	}
	return content.String(), reasoning.String(), finishReason, totalTokens, nil
}

// streamFlushChars controls how much streamed reasoning/content is buffered
// before it is written to the log.
const streamFlushChars = 200

func flushStream(buf *strings.Builder, label string, emitter *events.Emitter) {
	if buf.Len() == 0 {
		return
	}
	if text := strings.TrimSpace(buf.String()); text != "" {
		logx.Debugf("llm: %s: %s", label, text)
		if emitter != nil {
			if label == "think" {
				emitter.Reasoning(text)
			} else {
				emitter.Content(text)
			}
		}
	}
	buf.Reset()
}

// parseJSONResponse extracts JSON from LLM response (handles markdown code fences, etc.)
func parseJSONResponse(content string) (map[string]any, error) {
	// Handles markdown fences and surrounding prose, like the Python original.
	cleaned := content
	cleaned = strings.TrimSpace(cleaned)
	cleaned = mdFenceOpenRe.ReplaceAllString(cleaned, "")
	cleaned = mdFenceCloseRe.ReplaceAllString(cleaned, "")

	start := strings.Index(cleaned, "{")
	end := strings.LastIndex(cleaned, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object found in response")
	}

	jsonStr := cleaned[start : end+1]
	var result map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	return result, nil
}
