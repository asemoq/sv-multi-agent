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
	"github.com/asemoq/sv-multi-agent/internal/sv/coverage"
)

func TestValidateSourceFiles(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{
			name:  "valid rtl only",
			files: map[string]string{"rtl/counter.sv": "module counter; endmodule"},
		},
		{
			name: "valid rtl and tb",
			files: map[string]string{
				"rtl/counter.sv":   "module counter; endmodule",
				"tb/tb_counter.sv": "module tb; endmodule",
				"rtl/include.svh":  "`define X 1",
			},
		},
		{name: "empty", files: nil, wantErr: "at least one RTL"},
		{name: "tb only", files: map[string]string{"tb/tb.sv": "module tb; endmodule"}, wantErr: "at least one SystemVerilog"},
		{name: "header only rtl", files: map[string]string{"rtl/defs.svh": "`define X 1"}, wantErr: "at least one SystemVerilog"},
		{name: "verilog rtl", files: map[string]string{"rtl/counter.v": "module counter; endmodule"}},
		{name: "traversal", files: map[string]string{"rtl/../secret.sv": "x"}, wantErr: "unsafe"},
		{name: "absolute", files: map[string]string{"/etc/passwd.sv": "x"}, wantErr: "unsafe"},
		{name: "wrong dir", files: map[string]string{"src/counter.sv": "x"}, wantErr: "rtl/ or tb/"},
		{name: "wrong extension", files: map[string]string{"rtl/counter.txt": "x"}, wantErr: "unsupported"},
		{name: "empty content", files: map[string]string{"rtl/counter.sv": "  "}, wantErr: "empty"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSourceFiles(tc.files)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestAnalyzeModeSkipsCoderAndVerifier(t *testing.T) {
	contractPayload := map[string]any{"contract": map[string]any{
		"module_name": "counter",
		"parameters":  map[string]any{"WIDTH": 8},
		"io": []any{
			map[string]any{"name": "clk", "dir": "input", "width": 1},
			map[string]any{"name": "rst", "dir": "input", "width": 1},
			map[string]any{"name": "count", "dir": "output", "width": "WIDTH"},
		},
		"clocking": map[string]any{
			"is_sequential": true, "clock": "clk", "edge": "posedge",
			"reset": "rst", "active": "high", "sync": true,
		},
		"timing":             map[string]any{"count": map[string]any{"latency_cycles": 1}},
		"functional_summary": "Existing counter RTL.",
		"corner_cases":       []any{"reset"},
		"test_plan":          []any{"reset", "count"},
		"guidance":           map[string]any{"coder": "keep behavior"},
	}}

	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		user := ""
		for _, message := range body.Messages {
			if message.Role == "user" {
				user = message.Content
			}
		}
		switch {
		case strings.HasPrefix(user, "User request:"):
			calls["architect"]++
		default:
			calls["other"]++
		}
		content, _ := json.Marshal(contractPayload)
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
		ArchitectMaxRevisions: 0,
		ContractOnly:          true,
		MaxSourceChars:        24000,
		DecomposeEnabled:      false,
		LintEnabled:           false,
		TopModule:             "tb",
		VerilatorBuildTimeout: 2,
		VerilatorRunTimeout:   2,
	}
	client := llm.NewLLMClient(server.URL, "test", "test", 0.1, 5*time.Second)
	manager, err := New(taskStore, root, client, cfg, fakeRAG{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	task, err := manager.SubmitTask(Submission{
		Prompt:        "Analyze the existing counter",
		MaxIterations: 1,
		SourceMode:    "analyze",
		DutTop:        "counter",
		Files: map[string]string{
			"rtl/counter.sv": "module counter(input logic clk, input logic rst, output logic [7:0] count);\n" +
				"    always_ff @(posedge clk) if (rst) count <= '0; else count <= count + 1'b1;\nendmodule\n",
			"tb/tb_counter.sv": "module tb; initial begin $display(\"SIMULATION PASSED\"); $finish; end endmodule\n",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var finished *store.Task
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, ok := taskStore.Get(task.ID)
		if ok && (snapshot.Status == "completed" || snapshot.Status == "failed") {
			finished = snapshot
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if finished == nil {
		t.Fatal("workflow did not finish")
	}

	names := []string{}
	for _, step := range finished.Steps {
		names = append(names, step.Name)
	}
	if indexOf(names, "ingest") < 0 || indexOf(names, "architect") < 0 {
		t.Fatalf("steps = %v", names)
	}
	for _, skipped := range []string{"planner", "coder", "verifier"} {
		if indexOf(names, skipped) >= 0 {
			t.Fatalf("analyze mode ran %s: %v", skipped, names)
		}
	}
	if calls["architect"] != 1 {
		t.Fatalf("architect calls = %d, want 1", calls["architect"])
	}
	if _, err := os.Stat(filepath.Join(finished.Workspace, "rtl", "counter.sv")); err != nil {
		t.Fatalf("ingested RTL missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(finished.Workspace, "tb", "tb_counter.sv")); err != nil {
		t.Fatalf("ingested TB missing: %v", err)
	}
}

func TestStepIngestWritesArtifacts(t *testing.T) {
	root := t.TempDir()
	taskStore, err := store.New(filepath.Join(root, "data", "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{store: taskStore, root: root, cfg: &config.Config{WorkspacePerm: 0o755}}
	task := &store.Task{ID: "task-test", Workspace: filepath.Join(root, "task-test"), Steps: []store.Step{}}
	if err := manager.stepIngest(task, map[string]string{"rtl/a.sv": "module a; endmodule\n"}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(task.Workspace, "rtl", "a.sv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "module a") {
		t.Fatalf("content = %q", content)
	}
	if len(task.Artifacts) != 1 || task.Artifacts[0].Role != "import" {
		t.Fatalf("artifacts = %#v", task.Artifacts)
	}
}

func TestRunCoverageLoopDisabled(t *testing.T) {
	manager := &Manager{cfg: &config.Config{CoverageEnabled: true, CoverageMin: 0.9, CoverageRounds: 1, LLMMode: "mock"}}
	simulation := &agents.Result{
		Passed: true,
		Coverage: &coverage.Report{
			LinesTotal:   10,
			LinesCovered: 5,
			LineCoverage: 0.5,
		},
	}
	result, passed, err := manager.runCoverageLoop(context.Background(), &store.Task{}, simulation)
	if err != nil {
		t.Fatal(err)
	}
	if !passed || result != simulation {
		t.Fatalf("result = %#v passed=%v", result, passed)
	}
}
