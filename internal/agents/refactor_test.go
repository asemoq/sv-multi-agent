package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/asemoq/sv-multi-agent/internal/sv/slicer"
)

func TestRefactorMockFails(t *testing.T) {
	result := Refactor(context.Background(), nil, Options{Mode: "mock"}, nil, "rename")
	if result.Status != "failed" {
		t.Fatalf("mock refactor should fail, got %q", result.Status)
	}
}

func TestRefactorResultPreservesPaths(t *testing.T) {
	data := map[string]any{
		"files": map[string]any{
			"rtl/foo.sv":   "module foo; endmodule",
			"tb/tb_foo.sv": "module tb_foo; endmodule",
			"/abs/x.sv":    "module abs; endmodule",
			"../up.sv":     "module up; endmodule",
			"notes.txt":    "not rtl",
		},
		"summary": "preserve paths",
	}
	result := refactorResult(data, nil)
	if result.Status != "completed" {
		t.Fatalf("status = %q", result.Status)
	}
	if len(result.Artifacts) != 2 {
		t.Fatalf("artifacts = %d, want 2 (only rtl/ and tb/ kept)", len(result.Artifacts))
	}
	paths := map[string]bool{}
	for _, artifact := range result.Artifacts {
		paths[artifact.Path] = true
	}
	if !paths["rtl/foo.sv"] || !paths["tb/tb_foo.sv"] {
		t.Fatalf("unexpected artifacts: %v", result.Artifacts)
	}
}

func TestRefactorResultPatches(t *testing.T) {
	const src = "module foo;\n  logic q, d;\n  always_ff @(posedge clk) begin\n    q <= d;\n  end\nendmodule\n"
	files := map[string]string{"rtl/foo.sv": src}

	blocks := slicer.ParseSources(files)
	var id string
	for _, block := range blocks {
		if block.Kind == "always_ff" {
			id = block.ID
			break
		}
	}
	if id == "" {
		t.Skip("no always_ff block parsed by slicer")
	}

	data := map[string]any{
		"patches": []any{
			map[string]any{
				"block_id": id,
				"new_code": "always_ff @(posedge clk) begin\n    q <= ~d;\n  end",
			},
		},
		"summary": "invert data",
	}
	result := refactorResult(data, files)
	if result.Status != "completed" {
		t.Fatalf("status = %q", result.Status)
	}
	if len(result.Artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(result.Artifacts))
	}
	if !strings.Contains(result.Artifacts[0].Content, "q <= ~d") {
		t.Fatalf("patch not applied:\n%s", result.Artifacts[0].Content)
	}
}

func TestRefactorResultNoFiles(t *testing.T) {
	result := refactorResult(map[string]any{"summary": "nothing"}, nil)
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
}

func TestRefactorResultDelete(t *testing.T) {
	data := map[string]any{
		"files": map[string]any{
			"rtl/new/engine.sv": "module engine; endmodule",
		},
		"delete":  []any{"rtl/old_engine.sv", "/abs/x.sv", "../up.sv", "notes.txt"},
		"summary": "moved engine",
	}
	result := refactorResult(data, nil)
	if result.Status != "completed" {
		t.Fatalf("status = %q", result.Status)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].Path != "rtl/new/engine.sv" {
		t.Fatalf("artifacts = %v", result.Artifacts)
	}
	if len(result.DeletePaths) != 1 || result.DeletePaths[0] != "rtl/old_engine.sv" {
		t.Fatalf("delete paths = %v (unsafe paths must be filtered)", result.DeletePaths)
	}
}

func TestRefactorResultDeleteOnly(t *testing.T) {
	data := map[string]any{
		"delete":  []any{"rtl/legacy.sv"},
		"summary": "drop legacy module",
	}
	result := refactorResult(data, nil)
	if result.Status != "completed" {
		t.Fatalf("status = %q", result.Status)
	}
	if len(result.DeletePaths) != 1 {
		t.Fatalf("delete paths = %v", result.DeletePaths)
	}
}

func TestValidRefactorPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"rtl/foo.sv", true},
		{"tb/tb_foo.sv", true},
		{"rtl/sub/bar.svh", true},
		{"/abs/x.sv", false},
		{"../x.sv", false},
		{"C:\\x.sv", false},
		{"notes.txt", false},
		{"rtl//double.sv", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := validRefactorPath(tc.path); got != tc.want {
			t.Errorf("validRefactorPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
