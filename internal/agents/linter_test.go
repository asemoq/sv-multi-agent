package agents

import (
	"context"
	"strings"
	"testing"
)

func TestBuildLintCommand(t *testing.T) {
	command := buildLintCommand(Options{
		DutTop:          "counter",
		LintFailOnError: true,
		LintErrorOn:     []string{"width", "LATCH"},
	}, []string{"rtl/counter.sv"})

	for _, flag := range []string{"--lint-only", "--sv", "-Wall", "-Werror-WIDTH", "-Werror-LATCH", "--top-module"} {
		if !containsValue(command, flag) {
			t.Fatalf("missing %s in %#v", flag, command)
		}
	}
	if containsValue(command, "-Wno-fatal") {
		t.Fatalf("strict lint must not add -Wno-fatal: %#v", command)
	}
}

func TestBuildLintCommandRelaxed(t *testing.T) {
	command := buildLintCommand(Options{}, []string{"rtl/counter.sv"})
	if !containsValue(command, "-Wno-fatal") {
		t.Fatalf("missing -Wno-fatal in %#v", command)
	}
}

func TestParseLintIssues(t *testing.T) {
	logText := strings.Join([]string{
		"%Warning-WIDTH: rtl/counter.sv:12:5: Operator ADD expects 8 bits on the LHS, but 9 bits on the RHS",
		"%Error: rtl/counter.sv:20: syntax error, unexpected endmodule",
		"%Warning-LATCH: rtl/counter.sv:30: Latch inferred for signal 'q'",
	}, "\n")

	issues := parseLintIssues(logText)
	if len(issues) != 3 {
		t.Fatalf("issues = %#v", issues)
	}
	if issues[0].Code != "verilator.lint.WIDTH" || issues[0].Severity != "warning" {
		t.Fatalf("first = %#v", issues[0])
	}
	if issues[1].Code != "verilator.lint.ERROR" || issues[1].Severity != "error" {
		t.Fatalf("second = %#v", issues[1])
	}
	if firstLintError(issues) == "" {
		t.Fatal("expected an error message")
	}
	if countSeverity(issues, "warning") != 2 {
		t.Fatalf("warnings = %d", countSeverity(issues, "warning"))
	}
}

func TestLintMissingSources(t *testing.T) {
	result := Lint(context.Background(), Options{}, map[string]string{
		"tb/tb_counter.sv": "module tb; endmodule\n",
	})
	if result.Status != "failed" || !strings.Contains(result.Summary, "rtl/") {
		t.Fatalf("result = %#v", result)
	}
}
