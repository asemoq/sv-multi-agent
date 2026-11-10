package workflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asemoq/sv-multi-agent/internal/agents"
	"github.com/asemoq/sv-multi-agent/internal/config"
	"github.com/asemoq/sv-multi-agent/internal/llm"
	"github.com/asemoq/sv-multi-agent/internal/store"
)

func TestStepCoderWithPlanGeneratesSubmoduleAndTop(t *testing.T) {
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		user := ""
		for _, message := range body.Messages {
			if message.Role == "user" {
				user = message.Content
			}
		}
		requests = append(requests, user)

		var payload map[string]any
		switch {
		case strings.Contains(user, "sub-module core"):
			payload = map[string]any{
				"files": []any{map[string]any{
					"path":    "rtl/core.sv",
					"content": "module core(input logic clk, output logic q);\n    assign q = clk;\nendmodule\n",
				}},
				"summary": "core generated",
			}
		case strings.Contains(user, "top-level module"):
			payload = map[string]any{
				"files": []any{map[string]any{
					"path":    "rtl/top.sv",
					"content": "module top(input logic clk, output logic q);\n    core u_core(.clk(clk), .q(q));\nendmodule\n",
				}},
				"summary": "top generated",
			}
		default:
			t.Errorf("unexpected coder prompt: %q", user)
			payload = map[string]any{"files": []any{}}
		}
		content, _ := json.Marshal(payload)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "content": string(content)},
			}},
		})
	}))
	defer server.Close()

	root := t.TempDir()
	taskStore, err := store.New(filepath.Join(root, "data", "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		WorkspaceRoot:         root,
		DataDir:               filepath.Join(root, "data"),
		WorkspacePerm:         0o755,
		AutoCreateWorkspaces:  true,
		LLMMode:               "openai-compatible",
		ArchitectMaxRevisions: 2,
		MaxSourceChars:        24000,
		DecomposeEnabled:      true,
		DecomposeMinLines:     400,
		VerilatorBuildTimeout: 10,
		VerilatorRunTimeout:   10,
	}
	client := llm.NewLLMClient(server.URL, "test", "test", 0.1, 5*time.Second)
	manager, err := New(taskStore, root, client, cfg, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	task := &store.Task{
		ID:        "task-test",
		Workspace: filepath.Join(root, "task-test"),
		ContractJSON: `{
			"module_name": "top",
			"io": [
				{"name": "clk", "dir": "input", "width": 1},
				{"name": "q", "dir": "output", "width": 1}
			],
			"functional_summary": "top wires core"
		}`,
		Status: "running",
		Steps:  []store.Step{},
	}
	if err := os.MkdirAll(task.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.Save(task); err != nil {
		t.Fatal(err)
	}

	plan := &agents.Plan{
		Modules: []agents.PlanModule{{
			Name: "core",
			Contract: map[string]any{
				"module_name": "core",
				"io": []any{
					map[string]any{"name": "clk", "dir": "input", "width": 1},
					map[string]any{"name": "q", "dir": "output", "width": 1},
				},
				"functional_summary": "buffer",
			},
		}},
		Instances: []agents.PlanInstance{{
			Module:      "core",
			Instance:    "u_core",
			Connections: map[string]string{"clk": "clk", "q": "q"},
		}},
	}
	if err := manager.stepCoder(context.Background(), task, plan); err != nil {
		t.Fatal(err)
	}

	if len(requests) != 2 {
		t.Fatalf("coder calls = %d, want 2", len(requests))
	}
	for _, path := range []string{"rtl/core.sv", "rtl/top.sv"} {
		if _, err := os.Stat(filepath.Join(task.Workspace, filepath.FromSlash(path))); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
	}
	if len(task.Steps) == 0 || task.Steps[len(task.Steps)-1].Status != "completed" {
		t.Fatalf("steps = %#v", task.Steps)
	}
}
