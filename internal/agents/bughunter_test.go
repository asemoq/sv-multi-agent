package agents

import (
	"context"
	"strings"
	"testing"
)

func TestBugHunterMockFails(t *testing.T) {
	result := BugHunter(context.Background(), nil, Options{Mode: "mock"}, nil, "")
	if result.Status != "failed" {
		t.Fatalf("mock bug hunter should fail, got %q", result.Status)
	}
}

func TestBugHunterResult(t *testing.T) {
	data := map[string]any{
		"bugs": []any{
			map[string]any{
				"file":          "rtl/counter.sv",
				"line":          float64(8),
				"block_id":      "rtl/counter.sv:blk_0",
				"severity":      "error",
				"category":      "arithmetic",
				"title":         "counter wraps early",
				"description":   "no modulo applied",
				"suggested_fix": "wrap on WIDTH bits",
			},
			map[string]any{
				"file":     "rtl/ctrl.sv",
				"severity": "warning",
				"category": "latch",
				"title":    "inferred latch",
			},
		},
		"summary": "two bugs",
	}

	result := bugHunterResult(data, map[string]string{"rtl/counter.sv": "", "rtl/ctrl.sv": ""})
	if result.Status != "completed" {
		t.Fatalf("status = %q", result.Status)
	}
	if len(result.Issues) != 2 {
		t.Fatalf("issues = %d, want 2", len(result.Issues))
	}
	first := result.Issues[0]
	if first.Severity != "error" || first.Code != "bug.arithmetic" {
		t.Fatalf("first issue = %+v", first)
	}
	if !strings.Contains(first.Message, "counter wraps early") || !strings.Contains(first.Message, "rtl/counter.sv:8") {
		t.Fatalf("first message = %q", first.Message)
	}
	if !strings.Contains(result.Issues[1].Message, "rtl/ctrl.sv") {
		t.Fatalf("second message = %q", result.Issues[1].Message)
	}

	if len(result.Artifacts) != 2 {
		t.Fatalf("artifacts = %d, want 2 (bugs.json + bugs.md)", len(result.Artifacts))
	}
	var hasJSON, hasMD bool
	for _, artifact := range result.Artifacts {
		switch artifact.Path {
		case "reports/bugs.json":
			hasJSON = true
		case "reports/bugs.md":
			hasMD = true
		}
	}
	if !hasJSON || !hasMD {
		t.Fatalf("missing report artifacts: %v", result.Artifacts)
	}
}

func TestBugHunterResultNoBugs(t *testing.T) {
	result := bugHunterResult(map[string]any{"bugs": []any{}, "summary": ""}, map[string]string{"rtl/counter.sv": ""})
	if result.Status != "completed" {
		t.Fatalf("status = %q", result.Status)
	}
	if len(result.Issues) != 0 {
		t.Fatalf("issues = %d, want 0", len(result.Issues))
	}
	if !strings.Contains(result.Summary, "No bugs found") {
		t.Fatalf("summary = %q", result.Summary)
	}
}

func TestBugHunterDropsHallucinatedFiles(t *testing.T) {
	files := map[string]string{"rtl/counter.sv": "", "rtl/ctrl.sv": ""}
	data := map[string]any{
		"bugs": []any{
			map[string]any{"file": "rtl/sync_fifo.sv", "severity": "warning", "category": "arithmetic", "title": "hallucinated fifo"},
			map[string]any{"block_id": "rtl/ghost.sv:blk_0", "severity": "error", "category": "logic", "title": "ghost block"},
			map[string]any{"file": "rtl/counter.sv", "severity": "warning", "category": "logic", "title": "real bug"},
			map[string]any{"severity": "warning", "category": "logic", "title": "no location, keep"},
		},
	}
	result := bugHunterResult(data, files)
	if len(result.Issues) != 2 {
		t.Fatalf("issues = %d, want 2 (real + no-location; dropped 2 hallucinations)", len(result.Issues))
	}
	if !strings.Contains(result.Issues[0].Message, "real bug") {
		t.Fatalf("first issue = %q", result.Issues[0].Message)
	}
	if !strings.Contains(result.Issues[1].Message, "no location") {
		t.Fatalf("second issue = %q", result.Issues[1].Message)
	}
}
