package agents

import (
	"context"
	"strings"
	"testing"
)

func containsValue(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestTopModuleName(t *testing.T) {
	if got := topModuleName(Options{TopModule: "tb_top"}); got != "tb_top" {
		t.Fatalf("got %q", got)
	}
	if got := topModuleName(Options{TopModule: "bad name;"}); got != "tb" {
		t.Fatalf("got %q", got)
	}
	if got := topModuleName(Options{}); got != "tb" {
		t.Fatalf("got %q", got)
	}
}

func TestBuildVerilatorCommand(t *testing.T) {
	command := buildVerilatorCommand(Options{}, "/tmp/build", []string{"rtl/counter.sv", "tb/tb_counter.sv"}, "tb_top")
	for _, flag := range []string{"--trace", "--sv", "--assert", "-Wall"} {
		if !containsValue(command, flag) {
			t.Fatalf("missing %s in %#v", flag, command)
		}
	}
	for index, value := range command {
		if value == "--top-module" {
			if index+1 >= len(command) || command[index+1] != "tb_top" {
				t.Fatalf("top module = %#v", command)
			}
			return
		}
	}
	t.Fatalf("missing --top-module in %#v", command)
}

func TestSimulatorMissingSources(t *testing.T) {
	result := Simulator(context.Background(), Options{}, map[string]string{
		"rtl/counter.sv": "module counter; endmodule\n",
	}, nil)
	if result.Status != "failed" {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(result.Summary, "rtl/") {
		t.Fatalf("summary = %q", result.Summary)
	}
}
