package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asemoq/sv-multi-agent/internal/agents"
	"github.com/asemoq/sv-multi-agent/internal/config"
	"github.com/asemoq/sv-multi-agent/internal/store"
	"github.com/asemoq/sv-multi-agent/internal/sv/coverage"
	"github.com/asemoq/sv-multi-agent/internal/sv/mutate"
)

func TestBuildMutationMarkdown(t *testing.T) {
	report := mutate.Report{
		Total:  4,
		Killed: 3,
		Score:  0.75,
		Survived: []mutate.Survivor{{
			ID:          "mut_1",
			Description: "invert reset polarity",
			File:        "rtl/counter.sv",
			Lines:       []int{4},
		}},
	}
	markdown := buildMutationMarkdown(report)
	for _, want := range []string{"# Mutation report", "score: 75%", "invert reset polarity", "rtl/counter.sv"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestBuildReportMarkdown(t *testing.T) {
	task := &store.Task{
		ID:         "task-1",
		Status:     "failed",
		Stage:      "simulation",
		SourceMode: "generate",
		Prompt:     "counter",
		Summary:    "simulation still failing",
		Issues: []map[string]any{{
			"severity": "error",
			"code":     "workflow.simulation",
			"message":  "mismatches",
		}},
	}
	simulation := &agents.Result{
		Passed:             false,
		CompileExitCode:    0,
		SimulationExitCode: 0,
		FailOutputs:        []agents.FailOutput{{Signal: "count", Mismatches: 2, Time: 25}},
		Coverage: &coverage.Report{
			Files: []coverage.File{{Path: "rtl/counter.sv", LinesTotal: 4, LinesCovered: 3}},
		},
	}
	markdown := buildReportMarkdown(task, simulation, nil)
	for _, want := range []string{"# Task task-1", "status: failed", "count", "Coverage", "Issues"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestWriteReport(t *testing.T) {
	root := t.TempDir()
	taskStore, err := store.New(filepath.Join(root, "data", "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{store: taskStore, cfg: &config.Config{WorkspacePerm: 0o755}}
	workspaceDir := filepath.Join(root, "task-1")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	task := &store.Task{ID: "task-1", Workspace: workspaceDir, Status: "completed", Stage: "done", Summary: "passed"}

	manager.writeReport(task, &agents.Result{Passed: true}, nil)

	for _, path := range []string{"reports/report.md", "reports/report.json"} {
		if _, err := os.Stat(filepath.Join(workspaceDir, filepath.FromSlash(path))); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
	}
	if len(task.Artifacts) < 2 {
		t.Fatalf("artifacts = %#v", task.Artifacts)
	}
}

func TestStepMutationSkipsWithoutVerilator(t *testing.T) {
	root := t.TempDir()
	taskStore, err := store.New(filepath.Join(root, "data", "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		store: taskStore,
		cfg:   &config.Config{MutationEnabled: true, LLMMode: "openai-compatible", WorkspacePerm: 0o755},
	}
	workspaceDir := filepath.Join(root, "task-1")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	task := &store.Task{ID: "task-1", Workspace: workspaceDir, Steps: []store.Step{}}

	manager.stepMutation(context.Background(), task)

	if len(task.Steps) != 1 || task.Steps[0].Name != "mutation" {
		t.Fatalf("steps = %#v", task.Steps)
	}
	if task.Steps[0].Status != "failed" || !strings.Contains(task.Steps[0].Summary, "Verilator") {
		t.Fatalf("step = %#v", task.Steps[0])
	}
}
