package contract

import (
	"encoding/json"
	"testing"
)

func counterContract(width int) map[string]any {
	return map[string]any{
		"module_name": "counter",
		"parameters":  map[string]any{"WIDTH": width},
		"io": []any{
			map[string]any{"name": "clk", "dir": "input", "width": 1},
			map[string]any{"name": "rst", "dir": "input", "width": 1},
			map[string]any{"name": "en", "dir": "input", "width": 1},
			map[string]any{"name": "count", "dir": "output", "width": "WIDTH"},
		},
		"clocking": map[string]any{
			"is_sequential": true,
			"clock":         "clk",
			"edge":          "posedge",
			"reset":         "rst",
			"active":        "high",
			"sync":          true,
		},
		"timing":             map[string]any{"count": map[string]any{"latency_cycles": 1}},
		"functional_summary": "Synchronous counter with enable and reset.",
		"corner_cases":       []any{"wrap-around"},
		"guidance":           map[string]any{"verifier": "sample on the rising edge"},
	}
}

func issueCodes(issues []Issue) map[string]bool {
	codes := map[string]bool{}
	for _, issue := range issues {
		codes[issue.Code] = true
	}
	return codes
}

func TestLegacyContractIsUpgraded(t *testing.T) {
	legacy := map[string]any{
		"module_name": "counter",
		"parameters":  []any{map[string]any{"name": "WIDTH", "default": 4}},
		"ports": []any{
			map[string]any{"name": "clk", "direction": "input", "width": 1},
			map[string]any{"name": "rst", "direction": "input", "width": 1},
			map[string]any{"name": "count", "direction": "output", "width": "WIDTH"},
		},
		"clock":       map[string]any{"name": "clk", "edge": "posedge"},
		"reset":       map[string]any{"name": "rst", "active_level": "high", "synchronous": true},
		"description": "legacy counter",
	}
	normalized := Normalize(legacy)

	parameters := normalized["parameters"].(map[string]any)
	if parameters["WIDTH"] != 4 {
		t.Fatalf("WIDTH = %v, want 4", parameters["WIDTH"])
	}
	ports := normalized["io"].([]any)
	if ports[0].(map[string]any)["dir"] != "input" {
		t.Fatalf("dir = %v", ports[0].(map[string]any)["dir"])
	}
	clocking := normalized["clocking"].(map[string]any)
	if clocking["clock"] != "clk" || clocking["reset"] != "rst" || clocking["sync"] != true {
		t.Fatalf("clocking = %#v", clocking)
	}
	if normalized["functional_summary"] != "legacy counter" {
		t.Fatalf("functional_summary = %v", normalized["functional_summary"])
	}
}

func TestParseContractAcceptsJSONString(t *testing.T) {
	data, err := json.Marshal(counterContract(16))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(map[string]any{"contract_json": string(data)})
	if err != nil {
		t.Fatal(err)
	}
	if got := ParameterInt(parsed, "WIDTH", 1); got != 16 {
		t.Fatalf("ParameterInt = %d, want 16", got)
	}
}

func TestParameterIntRejectsBoolean(t *testing.T) {
	parsed := map[string]any{"parameters": map[string]any{"WIDTH": true}}
	if got := ParameterInt(parsed, "WIDTH", 8); got != 8 {
		t.Fatalf("ParameterInt = %d, want 8", got)
	}
}

func TestValidContractHasNoErrors(t *testing.T) {
	if HasErrors(Lint(counterContract(8))) {
		t.Fatalf("unexpected errors: %#v", Lint(counterContract(8)))
	}
}

func TestUnknownClockIsAnError(t *testing.T) {
	value := counterContract(8)
	value["clocking"].(map[string]any)["clock"] = "clock"
	issues := Lint(value)
	if !HasErrors(issues) || !issueCodes(issues)["contract.clocking.clock"] {
		t.Fatalf("issues = %#v", issues)
	}
}

func TestMissingOutputTimingIsAnError(t *testing.T) {
	value := counterContract(8)
	value["timing"] = map[string]any{}
	issues := Lint(value)
	if !HasErrors(issues) || !issueCodes(issues)["contract.timing.missing"] {
		t.Fatalf("issues = %#v", issues)
	}
}

func TestDuplicateIONamesAreErrors(t *testing.T) {
	value := counterContract(8)
	value["io"] = append(value["io"].([]any), map[string]any{"name": "clk", "dir": "input", "width": 1})
	if !issueCodes(Lint(value))["contract.io.duplicate"] {
		t.Fatal("expected contract.io.duplicate")
	}
}

func TestMissingGuidanceIsOnlyAWarning(t *testing.T) {
	value := counterContract(8)
	delete(value, "guidance")
	issues := Lint(value)
	if HasErrors(issues) {
		t.Fatalf("unexpected errors: %#v", issues)
	}
	if !issueCodes(issues)["contract.guidance"] {
		t.Fatalf("issues = %#v", issues)
	}
}

func TestPayloadIsRestrictedByDefault(t *testing.T) {
	value := counterContract(8)
	value["secret"] = "do not leak"
	value["test_plan"] = []any{"not allowed"}
	payload := Payload(value, map[string]any{})
	if _, ok := payload["secret"]; ok {
		t.Fatal("secret leaked")
	}
	if _, ok := payload["test_plan"]; ok {
		t.Fatal("test_plan leaked")
	}
	if _, ok := payload["module_name"]; !ok {
		t.Fatal("module_name missing")
	}
}

func TestPayloadIgnoresEnvironment(t *testing.T) {
	value := counterContract(8)
	value["secret"] = "do not leak"
	t.Setenv("CONTRACT_ONLY", "0")
	payload := Payload(value, map[string]any{})
	if _, ok := payload["secret"]; ok {
		t.Fatal("secret leaked via environment")
	}
	if _, ok := payload["module_name"]; !ok {
		t.Fatal("module_name missing")
	}
}

func TestRequestCanDisableContractOnly(t *testing.T) {
	value := counterContract(8)
	value["secret"] = "visible"
	payload := Payload(value, map[string]any{"contract_only": false})
	if payload["secret"] != "visible" {
		t.Fatalf("secret = %v", payload["secret"])
	}
}
