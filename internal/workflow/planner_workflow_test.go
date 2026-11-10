package workflow

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asemoq/sv-multi-agent/internal/config"
	"github.com/asemoq/sv-multi-agent/internal/llm"
	"github.com/asemoq/sv-multi-agent/internal/store"
)

func bigPipelineContract() map[string]any {
	io := []any{
		map[string]any{"name": "clk", "dir": "input", "width": 1},
		map[string]any{"name": "rst", "dir": "input", "width": 1},
	}
	timing := map[string]any{}
	for index := 0; index < 30; index++ {
		io = append(io, map[string]any{"name": fmt.Sprintf("in_%d", index), "dir": "input", "width": 8})
	}
	for index := 0; index < 30; index++ {
		name := fmt.Sprintf("out_%d", index)
		io = append(io, map[string]any{"name": name, "dir": "output", "width": 8})
		timing[name] = map[string]any{"latency_cycles": 1}
	}
	return map[string]any{
		"module_name": "dsp_pipeline",
		"io":          io,
		"clocking": map[string]any{
			"is_sequential": true, "clock": "clk", "edge": "posedge",
			"reset": "rst", "active": "high", "sync": true,
		},
		"timing":             timing,
		"functional_summary": strings.Repeat("Multi-stage pipeline processing many channels with backpressure. ", 6),
		"corner_cases":       []any{"stall", "flush", "overflow"},
		"test_plan":          []any{"stream", "stall"},
		"guidance":           map[string]any{"coder": "keep it modular"},
	}
}

func oneModulePlan() map[string]any {
	return map[string]any{"plan": map[string]any{
		"summary": "one core module",
		"modules": []any{map[string]any{
			"name": "core",
			"contract": map[string]any{
				"module_name": "core",
				"io": []any{
					map[string]any{"name": "clk", "dir": "input", "width": 1},
					map[string]any{"name": "rst", "dir": "input", "width": 1},
					map[string]any{"name": "q", "dir": "output", "width": 8},
				},
				"clocking":           map[string]any{"is_sequential": true, "clock": "clk", "edge": "posedge", "reset": "rst", "active": "high", "sync": true},
				"timing":             map[string]any{"q": map[string]any{"latency_cycles": 1}},
				"functional_summary": "registered output stage",
				"guidance":           map[string]any{"coder": "use always_ff"},
			},
		}},
		"instances": []any{map[string]any{
			"module": "core", "instance": "u_core",
			"connections": map[string]any{"clk": "clk", "rst": "rst", "q": "out_0"},
		}},
	}}
}

func writeChatJSON(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	content, err := json.Marshal(payload)
	if err != nil {
		t.Errorf("marshal payload: %v", err)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": string(content)},
		}},
	})
}

func TestWorkflowRunsPlannerForLargeContract(t *testing.T) {
	architectPayload := map[string]any{"contract": bigPipelineContract()}
	coreFile := "module core(input logic clk, input logic rst, output logic [7:0] q);\n" +
		"    always_ff @(posedge clk) begin\n        if (rst) q <= '0; else q <= q + 1'b1;\n    end\nendmodule\n"
	topFile := "module dsp_pipeline(input logic clk, input logic rst, output logic [7:0] out_0);\n" +
		"    core u_core(.clk(clk), .rst(rst), .q(out_0));\nendmodule\n"
	tbFile := "module tb; initial begin $display(\"SIMULATION PASSED\"); $finish; end endmodule\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		user := ""
		for _, message := range body.Messages {
			if message.Role == "user" {
				user = message.Content
			}
		}

		switch {
		case strings.HasPrefix(user, "User request:"):
			writeChatJSON(t, w, architectPayload)
		case strings.HasPrefix(user, "Design contract:"):
			writeChatJSON(t, w, oneModulePlan())
		case strings.Contains(user, "RTL source:"):
			writeChatJSON(t, w, map[string]any{
				"files":   []any{map[string]any{"path": "tb/tb_dsp_pipeline.sv", "content": tbFile}},
				"summary": "testbench generated",
			})
		case strings.Contains(user, "sub-module core"):
			writeChatJSON(t, w, map[string]any{
				"files":   []any{map[string]any{"path": "rtl/core.sv", "content": coreFile}},
				"summary": "core generated",
			})
		case strings.Contains(user, "top-level module"):
			writeChatJSON(t, w, map[string]any{
				"files":   []any{map[string]any{"path": "rtl/dsp_pipeline.sv", "content": topFile}},
				"summary": "top generated",
			})
		case strings.Contains(user, "Simulation result:"):
			writeChatJSON(t, w, map[string]any{
				"files":   []any{map[string]any{"path": "rtl/dsp_pipeline.sv", "content": topFile}},
				"summary": "no change",
			})
		default:
			t.Errorf("unexpected prompt: %q", user)
			writeChatJSON(t, w, map[string]any{"files": []any{}})
		}
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
		ContractOnly:          true,
		MaxSourceChars:        24000,
		DecomposeEnabled:      true,
		DecomposeMinLines:     400,
		TopModule:             "tb",
		VerilatorBuildTimeout: 5,
		VerilatorRunTimeout:   5,
	}
	client := llm.NewLLMClient(server.URL, "test", "test", 0.1, 5*time.Second)
	manager, err := New(taskStore, root, client, cfg, fakeRAG{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	task, err := manager.Submit("Build a large multi-channel DSP pipeline", 1)
	if err != nil {
		t.Fatal(err)
	}

	var finished *store.Task
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, ok := taskStore.Get(task.ID)
		if ok && (snapshot.Status == "failed" || snapshot.Status == "completed") {
			finished = snapshot
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if finished == nil {
		t.Fatal("workflow did not finish")
	}

	steps := map[string]store.Step{}
	order := []string{}
	for _, step := range finished.Steps {
		steps[step.Name] = step
		order = append(order, step.Name)
	}
	if _, ok := steps["planner"]; !ok {
		t.Fatalf("planner step missing: %v", order)
	}
	if steps["planner"].Status != "completed" {
		t.Fatalf("planner = %#v", steps["planner"])
	}
	if steps["coder"].Status != "completed" {
		t.Fatalf("coder = %#v", steps["coder"])
	}
	if !strings.Contains(steps["coder"].Summary, "core:") || !strings.Contains(steps["coder"].Summary, "top:") {
		t.Fatalf("coder summary = %q", steps["coder"].Summary)
	}
	if indexOf(order, "planner") > indexOf(order, "coder") {
		t.Fatalf("step order = %v", order)
	}

	planPath := filepath.Join(finished.Workspace, "design", "plan.json")
	if _, err := os.Stat(planPath); err != nil {
		t.Fatalf("plan.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(finished.Workspace, "rtl", "core.sv")); err != nil {
		t.Fatalf("core.sv missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(finished.Workspace, "rtl", "dsp_pipeline.sv")); err != nil {
		t.Fatalf("top RTL missing: %v", err)
	}
}

func indexOf(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}
