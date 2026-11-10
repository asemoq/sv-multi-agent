package mutate

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTextMutationKinds(t *testing.T) {
	cases := []struct {
		line        string
		want        string
		description string
	}{
		{"    if (rst) count <= 0;", "    if (!rst) count <= 0;", "invert reset polarity"},
		{"    if (!en) q <= d;", "    if (en) q <= d;", "invert enable gating"},
		{"assign eq = a == b;", "assign eq = a != b;", "flip comparison operator"},
		{"assign y = a & b;", "assign y = a | b;", "flip bitwise operator"},
		{"assign s = a + b;", "assign s = a - b;", "flip arithmetic operator"},
		{"assign n = count + 1;", "assign n = count + 2;", "mutate constant by one"},
	}
	for _, tc := range cases {
		got, desc, ok := textMutation(tc.line)
		if !ok || desc != tc.description || got != tc.want {
			t.Fatalf("textMutation(%q) = %q, %q, %v; want %q, %q", tc.line, got, desc, ok, tc.want, tc.description)
		}
	}
}

func TestDeleteBlock(t *testing.T) {
	content := "module m;\n    always_ff @(posedge clk) begin\n        q <= d;\n    end\nendmodule\n"
	mutated := deleteBlock(content, 2, 4)
	if strings.Contains(mutated, "q <= d") {
		t.Fatalf("block not removed:\n%s", mutated)
	}
	if !strings.Contains(mutated, "// [mutation] removed block") {
		t.Fatalf("no marker:\n%s", mutated)
	}
}

func TestGenerateProducesMutants(t *testing.T) {
	files := map[string]string{
		"rtl/counter.sv": "module counter(input logic clk, input logic rst, input logic en, output logic [7:0] count);\n" +
			"    always_ff @(posedge clk) begin\n" +
			"        if (rst) count <= 0;\n" +
			"        else if (en) count <= count + 1;\n" +
			"    end\n" +
			"endmodule\n",
	}
	mutants := Generate(files, 3, 42)
	if len(mutants) == 0 || len(mutants) > 3 {
		t.Fatalf("mutants = %d", len(mutants))
	}
	for _, mutant := range mutants {
		if mutant.File != "rtl/counter.sv" {
			t.Fatalf("file = %q", mutant.File)
		}
		if mutant.Files["rtl/counter.sv"] == files["rtl/counter.sv"] {
			t.Fatalf("mutant %s did not change the file", mutant.ID)
		}
	}
}

func TestRunReportsScore(t *testing.T) {
	mutants := []Mutant{
		{ID: "mut_1", Files: map[string]string{"rtl/a.sv": "x"}},
		{ID: "mut_2", Files: map[string]string{"rtl/a.sv": "y"}},
		{ID: "mut_3", Files: map[string]string{"rtl/a.sv": "z"}},
	}
	report := Run(mutants, func(files map[string]string) bool {
		return files["rtl/a.sv"] == "y" // only mut_2 survives
	})
	if report.Total != 3 || report.Killed != 2 || report.Score != 2.0/3.0 {
		t.Fatalf("report = %#v", report)
	}
	if len(report.Survived) != 1 || report.Survived[0].ID != "mut_2" {
		t.Fatalf("survived = %#v", report.Survived)
	}
}

func TestRunWithWorkersMatchesRun(t *testing.T) {
	mutants := make([]Mutant, 20)
	for i := range mutants {
		mutants[i] = Mutant{
			ID:          fmt.Sprintf("mut_%d", i+1),
			Description: "desc",
			File:        "rtl/a.sv",
			Lines:       []int{i + 1},
			Files:       map[string]string{"rtl/a.sv": fmt.Sprintf("%d", i)},
		}
	}
	// Even mutants survive, odd mutants are killed.
	run := func(files map[string]string) bool {
		var n int
		if _, err := fmt.Sscanf(files["rtl/a.sv"], "%d", &n); err != nil {
			return false
		}
		return n%2 == 0
	}

	seq := Run(mutants, run)
	par := RunWithWorkers(mutants, 4, run)

	if seq.Total != par.Total || seq.Killed != par.Killed || seq.Score != par.Score {
		t.Fatalf("sequential=%#v parallel=%#v", seq, par)
	}
	if len(seq.Survived) != len(par.Survived) {
		t.Fatalf("survivor count: seq=%d par=%d", len(seq.Survived), len(par.Survived))
	}
	for i := range seq.Survived {
		if seq.Survived[i].ID != par.Survived[i].ID {
			t.Fatalf("survivor order differs at %d: %q vs %q", i, seq.Survived[i].ID, par.Survived[i].ID)
		}
	}
}

func TestRunWithWorkersRunsConcurrently(t *testing.T) {
	mutants := make([]Mutant, 8)
	for i := range mutants {
		mutants[i] = Mutant{ID: fmt.Sprintf("mut_%d", i), Files: map[string]string{"rtl/a.sv": "x"}}
	}
	var mu sync.Mutex
	current, peak := 0, 0
	run := func(map[string]string) bool {
		mu.Lock()
		current++
		if current > peak {
			peak = current
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		current--
		mu.Unlock()
		return false
	}

	RunWithWorkers(mutants, 4, run)
	if peak < 2 {
		t.Fatalf("expected concurrent execution, peak=%d", peak)
	}
}
