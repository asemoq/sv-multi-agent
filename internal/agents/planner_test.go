package agents

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asemoq/sv-multi-agent/internal/llm"
)

func TestPlannerMockKeepsSingleModule(t *testing.T) {
	result := Planner(context.Background(), nil, Options{Mode: "mock"}, map[string]any{
		"module_name": "counter",
	}, "decompose")
	if result.Status != "completed" || result.Plan == nil {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Plan.Modules) != 0 {
		t.Fatalf("mock plan modules = %#v", result.Plan.Modules)
	}
}

func TestParsePlan(t *testing.T) {
	data := map[string]any{
		"plan": map[string]any{
			"summary": "split core and wrapper",
			"modules": []any{
				map[string]any{
					"name": "counter_core",
					"contract": map[string]any{
						"module_name": "counter_core",
						"io": []any{
							map[string]any{"name": "clk", "dir": "input", "width": 1},
							map[string]any{"name": "count", "dir": "output", "width": 8},
						},
					},
				},
			},
			"instances": []any{
				map[string]any{
					"module":      "counter_core",
					"instance":    "u_core",
					"connections": map[string]any{"clk": "clk", "count": "count"},
				},
			},
		},
	}
	plan := parsePlan(data)
	if len(plan.Modules) != 1 || plan.Modules[0].Name != "counter_core" {
		t.Fatalf("modules = %#v", plan.Modules)
	}
	if plan.Modules[0].Contract["module_name"] != "counter_core" {
		t.Fatalf("contract = %#v", plan.Modules[0].Contract)
	}
	if len(plan.Instances) != 1 || plan.Instances[0].Connections["clk"] != "clk" {
		t.Fatalf("instances = %#v", plan.Instances)
	}
}

func TestLintPlanRejectsUnknownInstance(t *testing.T) {
	plan := &Plan{
		Modules: []PlanModule{{
			Name: "core",
			Contract: map[string]any{
				"module_name": "core",
				"io": []any{
					map[string]any{"name": "clk", "dir": "input", "width": 1},
					map[string]any{"name": "q", "dir": "output", "width": 8},
				},
				"clocking":           map[string]any{"is_sequential": true, "clock": "clk"},
				"timing":             map[string]any{"q": map[string]any{"latency_cycles": 1}},
				"functional_summary": "core",
			},
		}},
		Instances: []PlanInstance{{Module: "missing", Instance: "u_missing"}},
	}
	issues := lintPlan(plan)
	if len(issues) == 0 {
		t.Fatal("expected lint issues")
	}
	found := false
	for _, issue := range issues {
		if strings.Contains(issue, "unknown module") {
			found = true
		}
	}
	if !found {
		t.Fatalf("issues = %#v", issues)
	}
}

func TestPlanSectionListsModules(t *testing.T) {
	plan := &Plan{
		Summary: "split",
		Modules: []PlanModule{{
			Name: "core",
			Contract: map[string]any{
				"io": []any{
					map[string]any{"name": "clk", "dir": "input"},
					map[string]any{"name": "q", "dir": "output"},
				},
			},
		}},
		Instances: []PlanInstance{{Module: "core", Instance: "u_core", Connections: map[string]string{"clk": "clk"}}},
	}
	section := planSection(plan)
	for _, want := range []string{"Hierarchy plan", "core", "u_core", "clk=clk"} {
		if !strings.Contains(section, want) {
			t.Fatalf("section missing %q: %q", want, section)
		}
	}
}

func TestPlannerParsesLLMPlan(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		plan := map[string]any{
			"plan": map[string]any{
				"summary": "one sub-module",
				"modules": []any{map[string]any{
					"name": "core",
					"contract": map[string]any{
						"module_name": "core",
						"io": []any{
							map[string]any{"name": "clk", "dir": "input", "width": 1},
							map[string]any{"name": "q", "dir": "output", "width": 8},
						},
						"clocking":           map[string]any{"is_sequential": true, "clock": "clk", "edge": "posedge"},
						"timing":             map[string]any{"q": map[string]any{"latency_cycles": 1}},
						"functional_summary": "registered buffer",
					},
				}},
				"instances": []any{map[string]any{
					"module":      "core",
					"instance":    "u_core",
					"connections": map[string]any{"clk": "clk", "q": "q"},
				}},
			},
		}
		content, _ := json.Marshal(plan)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "content": string(content)},
			}},
		})
	}))
	defer server.Close()

	client := llm.NewLLMClient(server.URL, "test", "test", 0.1, 5*time.Second)
	result := Planner(context.Background(), client, Options{Mode: "openai-compatible", MaxRevisions: 2}, map[string]any{
		"module_name":        "top",
		"io":                 []any{map[string]any{"name": "clk", "dir": "input", "width": 1}},
		"functional_summary": "top level",
	}, "decompose")

	if result.Status != "completed" || result.Plan == nil {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Plan.Modules) != 1 || result.Plan.Modules[0].Name != "core" {
		t.Fatalf("modules = %#v", result.Plan.Modules)
	}
	if calls != 1 {
		t.Fatalf("llm calls = %d, want 1", calls)
	}
}
