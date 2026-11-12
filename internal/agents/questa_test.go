package agents

import (
	"context"
	"os/exec"
	"testing"

	"github.com/asemoq/sv-multi-agent/internal/sv/mutate"
)

func questaAvailable() bool {
	for _, tool := range []string{"vlib", "vlog", "vsim"} {
		if _, err := exec.LookPath(tool); err != nil {
			return false
		}
	}
	return true
}

// TestQuestaBackend exercises the QuestaSim lint + simulation path end-to-end.
// It skips when QuestaSim is not installed on PATH.
func TestQuestaBackend(t *testing.T) {
	if !questaAvailable() {
		t.Skip("QuestaSim (vlib/vlog/vsim) is not on PATH")
	}

	files := map[string]string{
		"rtl/counter.sv":   mockCounterRTL(8),
		"tb/tb_counter.sv": mockCounterTB(8),
	}
	opts := Options{
		SimTool:               "questa",
		TopModule:             "tb",
		VerilatorBuildTimeout: 120,
		VerilatorRunTimeout:   60,
	}

	lint := Lint(context.Background(), opts, files)
	if lint.Status != "completed" {
		t.Fatalf("questa lint failed: %s", lint.Summary)
	}

	sim := Simulator(context.Background(), opts, files, nil)
	if !sim.Passed {
		t.Fatalf("questa simulation did not pass: %s\nissues=%v", sim.Summary, sim.Issues)
	}
	if sim.MismatchCount == nil || *sim.MismatchCount != 0 {
		t.Fatalf("expected 0 mismatches, got %v", sim.MismatchCount)
	}
}

// TestQuestaCoverage checks that QuestaSim coverage (vsim -coverage + vcover)
// produces a line-coverage report.
func TestQuestaCoverage(t *testing.T) {
	if !questaAvailable() {
		t.Skip("QuestaSim (vlib/vlog/vsim) is not on PATH")
	}
	files := map[string]string{
		"rtl/counter.sv":   mockCounterRTL(8),
		"tb/tb_counter.sv": mockCounterTB(8),
	}
	opts := Options{
		SimTool:               "questa",
		TopModule:             "tb",
		CoverageEnabled:       true,
		VerilatorBuildTimeout: 120,
		VerilatorRunTimeout:   60,
	}
	sim := Simulator(context.Background(), opts, files, nil)
	if !sim.Passed {
		t.Fatalf("simulation did not pass: %s\nissues=%v", sim.Summary, sim.Issues)
	}
	if sim.Coverage == nil {
		t.Fatalf("expected a coverage report, got nil")
	}
	rtl := sim.Coverage.ForPrefix("rtl/")
	if rtl.LinesTotal == 0 {
		t.Fatalf("no RTL lines in coverage: %#v", sim.Coverage)
	}
	t.Logf("QuestaSim RTL line coverage: %.1f%% (%d/%d)", rtl.LineCoverage*100, rtl.LinesCovered, rtl.LinesTotal)
}

// TestQuestaMutation validates mutation testing on top of the QuestaSim backend.
func TestQuestaMutation(t *testing.T) {
	if !questaAvailable() {
		t.Skip("QuestaSim (vlib/vlog/vsim) is not on PATH")
	}
	files := map[string]string{
		"rtl/counter.sv":   mockCounterRTL(8),
		"tb/tb_counter.sv": mockCounterTB(8),
	}
	opts := Options{SimTool: "questa", TopModule: "tb", VerilatorBuildTimeout: 120, VerilatorRunTimeout: 60}
	scope := map[string]bool{"rtl/counter.sv": true}
	mutants := mutate.GenerateScoped(files, 20, 42, scope)
	if len(mutants) == 0 {
		t.Fatal("no mutants generated")
	}
	report := mutate.RunWithWorkers(mutants, 2, func(mutated map[string]string) bool {
		return Simulator(context.Background(), opts, mutated, nil).Passed
	})
	t.Logf("QuestaSim mutation: score=%.0f%% killed=%d/%d survived=%d",
		report.Score*100, report.Killed, report.Total, len(report.Survived))
	if report.Total == 0 {
		t.Fatal("no mutants executed")
	}
}
