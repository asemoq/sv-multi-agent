package agents

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asemoq/sv-multi-agent/internal/llm"
	"github.com/asemoq/sv-multi-agent/internal/sv/slicer"
)

// captureSystemPrompt runs call against a fake OpenAI-compatible server and
// returns the system prompt the agent sent.
func captureSystemPrompt(t *testing.T, call func(*llm.LLMClient) *Result) string {
	t.Helper()
	var system string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, message := range body.Messages {
			if message.Role == "system" {
				system = message.Content
			}
		}
		content, _ := json.Marshal(map[string]any{
			"files":   []any{map[string]any{"path": "rtl/counter.sv", "content": "module counter; endmodule"}},
			"patches": []any{},
			"summary": "ok",
		})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "content": string(content)},
			}},
		})
	}))
	defer server.Close()

	client := llm.NewLLMClient(server.URL, "test", "test", 0.1, 5*time.Second)
	_ = call(client)
	return system
}

func TestSystemPromptsIncludeFewShotExamples(t *testing.T) {
	cases := []struct {
		name string
		call func(*llm.LLMClient) *Result
		want []string
	}{
		{
			name: "coder",
			call: func(client *llm.LLMClient) *Result {
				opts := mockOptions()
				opts.Mode = "openai-compatible"
				return Coder(context.Background(), client, opts, map[string]any{"module_name": "counter"}, nil, "implement")
			},
			want: []string{"GOOD", "BAD", "always_ff", "WIDTH-1:0"},
		},
		{
			name: "verifier",
			call: func(client *llm.LLMClient) *Result {
				opts := mockOptions()
				opts.Mode = "openai-compatible"
				return Verifier(context.Background(), client, opts, map[string]any{"module_name": "counter"}, nil, "verify")
			},
			want: []string{"GOOD", "BAD", "SIMULATION PASSED", "SIMULATION FAILED"},
		},
		{
			name: "debugger",
			call: func(client *llm.LLMClient) *Result {
				opts := mockOptions()
				opts.Mode = "openai-compatible"
				return Debugger(context.Background(), client, opts, map[string]any{"module_name": "counter"}, nil, nil, "debug")
			},
			want: []string{"GOOD", "BAD", "block_id"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			system := captureSystemPrompt(t, tc.call)
			for _, want := range tc.want {
				if !strings.Contains(system, want) {
					t.Fatalf("%s system prompt is missing %q", tc.name, want)
				}
			}
		})
	}
}

// TestCoderGoodExampleParses ensures the baked-in GOOD RTL is structurally valid
// SystemVerilog: the slicer must find one module and its always_ff block.
func TestCoderGoodExampleParses(t *testing.T) {
	good := coderExamples
	if index := strings.Index(good, "module counter"); index >= 0 {
		good = good[index:]
	}
	if index := strings.Index(good, "\n\nBAD"); index >= 0 {
		good = good[:index]
	}
	if !strings.Contains(good, "always_ff") || !strings.Contains(good, "endmodule") {
		t.Fatalf("GOOD example is incomplete:\n%s", good)
	}

	units := slicer.ParseStructure(good, "rtl/counter.sv")
	if len(units) != 1 {
		t.Fatalf("units = %d, want 1", len(units))
	}
	if units[0].Name != "counter" {
		t.Fatalf("module name = %q, want counter", units[0].Name)
	}
	foundFF := false
	for _, block := range units[0].Blocks {
		if block.Kind == "always_ff" {
			foundFF = true
		}
	}
	if !foundFF {
		t.Fatalf("GOOD example has no always_ff block: %#v", units[0].Blocks)
	}
}
