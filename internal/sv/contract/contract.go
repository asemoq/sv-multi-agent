// Package contract implements the design-contract schema, normalization and lint.
package contract

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// Issue is a single lint finding.
type Issue struct {
	Code     string `json:"code"`
	Field    string `json:"field"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

var (
	validDirs   = map[string]bool{"input": true, "output": true, "inout": true}
	validEdges  = map[string]bool{"posedge": true, "negedge": true}
	validActive = map[string]bool{"high": true, "low": true}

	contractOnlyKeys = []string{
		"module_name", "parameters", "io", "clocking", "timing",
		"functional_summary", "corner_cases", "guidance",
	}

	intPattern   = regexp.MustCompile(`^-?\d+$`)
	identPattern = regexp.MustCompile(`^[A-Za-z_]\w*$`)
)

// Parse returns the request contract as a normalized dictionary.
func Parse(request map[string]any) (map[string]any, error) {
	raw, ok := request["contract_json"]
	if !ok || raw == nil {
		return nil, errors.New("contract_json is required")
	}
	if m, ok := raw.(map[string]any); ok {
		return Normalize(m), nil
	}
	text, _ := raw.(string)
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("contract_json is required")
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		return nil, err
	}
	return Normalize(data), nil
}

// Normalize upgrades legacy contract shapes to the canonical schema.
func Normalize(data map[string]any) map[string]any {
	if data == nil {
		return map[string]any{}
	}
	c := cloneMap(data)
	c["parameters"] = normalizeParameters(c["parameters"])
	c["io"] = normalizeIO(c["io"], c["ports"])
	c["clocking"] = normalizeClocking(c["clocking"], c["clock"], c["reset"], asSlice(c["io"]))
	if _, ok := c["timing"].(map[string]any); !ok {
		c["timing"] = map[string]any{}
	}
	if strings.TrimSpace(asString(c["functional_summary"])) == "" {
		summary := firstNonEmpty(asString(c["description"]), asString(c["functional_description"]))
		if summary == "" {
			if reqs, ok := c["requirements"].([]any); ok && len(reqs) > 0 {
				parts := make([]string, 0, len(reqs))
				for _, item := range reqs {
					parts = append(parts, asString(item))
				}
				summary = strings.Join(parts, " ")
			}
		}
		if summary != "" {
			c["functional_summary"] = summary
		}
	}
	return c
}

func normalizeParameters(value any) map[string]any {
	result := map[string]any{}
	switch typed := value.(type) {
	case map[string]any:
		for name, def := range typed {
			result[name] = def
		}
	case []any:
		for _, item := range typed {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name := asString(entry["name"])
			if name == "" {
				continue
			}
			result[name] = entry["default"]
		}
	}
	return result
}

func normalizeIO(value, legacy any) []any {
	var source []any
	if list, ok := value.([]any); ok && len(list) > 0 {
		source = list
	} else if list, ok := legacy.([]any); ok {
		source = list
	} else {
		return []any{}
	}
	ports := make([]any, 0, len(source))
	for _, item := range source {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		port := cloneMap(entry)
		if _, ok := port["dir"]; !ok {
			if direction, exists := port["direction"]; exists {
				port["dir"] = direction
				delete(port, "direction")
			}
		}
		if _, ok := port["width"]; !ok {
			port["width"] = 1
		}
		ports = append(ports, port)
	}
	return ports
}

func normalizeClocking(value, clock, reset any, io []any) map[string]any {
	result := map[string]any{}
	if entry, ok := value.(map[string]any); ok {
		result = cloneMap(entry)
	}
	if len(result) == 0 {
		if clk, ok := clock.(map[string]any); ok {
			result["clock"] = firstNonEmpty(asString(clk["signal"]), asString(clk["name"]))
			if edge := asString(clk["edge"]); edge != "" {
				result["edge"] = edge
			}
			result["is_sequential"] = true
		} else if clk := asString(clock); clk != "" {
			result["clock"] = clk
			result["is_sequential"] = true
		}
		if rst, ok := reset.(map[string]any); ok {
			result["reset"] = firstNonEmpty(asString(rst["signal"]), asString(rst["name"]))
			result["active"] = firstNonEmpty(asString(rst["active_level"]), asString(rst["active"]))
			if sync, exists := rst["synchronous"]; exists {
				result["sync"] = sync
			} else if sync, exists := rst["sync"]; exists {
				result["sync"] = sync
			}
			if asString(result["reset"]) != "" {
				result["is_sequential"] = true
			}
		} else if rst := asString(reset); rst != "" {
			result["reset"] = rst
			result["is_sequential"] = true
		}
		if _, ok := result["is_sequential"]; !ok {
			result["is_sequential"] = false
		}
	}
	if _, ok := result["active"]; !ok {
		if level, exists := result["active_level"]; exists {
			result["active"] = level
		}
	}
	if _, ok := result["sync"]; !ok {
		if sync, exists := result["synchronous"]; exists {
			result["sync"] = sync
		}
	}
	if _, ok := result["edge"]; !ok {
		result["edge"] = "posedge"
	}
	if _, ok := result["active"]; !ok {
		result["active"] = "high"
	}
	if _, ok := result["sync"]; !ok {
		result["sync"] = true
	}
	return result
}

// ParameterInt reads an integer parameter or returns the default.
func ParameterInt(contract map[string]any, name string, fallback int) int {
	parameters, _ := contract["parameters"].(map[string]any)
	if parameters == nil {
		return fallback
	}
	if value, ok := asInt(parameters[name]); ok {
		return value
	}
	return fallback
}

func asInt(value any) (int, bool) {
	switch typed := value.(type) {
	case bool:
		return 0, false
	case int:
		return typed, true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case float64:
		if typed == float64(int(typed)) {
			return int(typed), true
		}
		return 0, false
	case float32:
		if typed == float32(int(typed)) {
			return int(typed), true
		}
		return 0, false
	case json.Number:
		if i, err := typed.Int64(); err == nil {
			return int(i), true
		}
		return 0, false
	case string:
		text := strings.TrimSpace(typed)
		if intPattern.MatchString(text) {
			if i, err := strconv.Atoi(text); err == nil {
				return i, true
			}
		}
	}
	return 0, false
}

func validWidth(value any, parameters map[string]any) bool {
	if width, ok := asInt(value); ok {
		return width > 0
	}
	text := strings.TrimSpace(asString(value))
	if text == "" {
		return false
	}
	if _, ok := parameters[text]; ok {
		return true
	}
	return identPattern.MatchString(text)
}

// Lint returns semantic issues found in the contract.
func Lint(contract map[string]any) []Issue {
	normalized := Normalize(contract)
	issues := []Issue{}
	add := func(code, message, field, severity string) {
		issues = append(issues, Issue{Code: code, Field: field, Severity: severity, Message: message})
	}

	if strings.TrimSpace(asString(normalized["module_name"])) == "" {
		add("contract.module_name", "module_name must be a non-empty string", "module_name", "error")
	}

	parameters, _ := normalized["parameters"].(map[string]any)
	if parameters == nil {
		parameters = map[string]any{}
	}
	io := asSlice(normalized["io"])
	names := map[string]map[string]any{}
	if len(io) == 0 {
		add("contract.io.empty", "io must contain at least one port", "io", "error")
	} else {
		for index, item := range io {
			port, ok := item.(map[string]any)
			if !ok {
				add("contract.io.entry", "io["+strconv.Itoa(index)+"] must be an object", "io", "error")
				continue
			}
			name := strings.TrimSpace(asString(port["name"]))
			if name == "" {
				add("contract.io.name", "io["+strconv.Itoa(index)+"] has no name", "io", "error")
			} else if _, exists := names[name]; exists {
				add("contract.io.duplicate", "duplicate port name '"+name+"'", "io", "error")
			} else {
				names[name] = port
			}
			direction := strings.ToLower(strings.TrimSpace(asString(port["dir"])))
			if !validDirs[direction] {
				add("contract.io.dir", "port '"+name+"' has invalid direction; expected one of input/output/inout", "io", "error")
			}
			if !validWidth(port["width"], parameters) {
				add("contract.io.width", "port '"+name+"' has invalid width", "io", "error")
			}
		}
	}

	clocking, _ := normalized["clocking"].(map[string]any)
	if clocking == nil {
		clocking = map[string]any{}
	}
	if truthy(clocking["is_sequential"]) {
		edge := strings.ToLower(strings.TrimSpace(asString(clocking["edge"])))
		if !validEdges[edge] {
			add("contract.clocking.edge", "clocking.edge must be posedge or negedge", "clocking", "error")
		}
		clock := asString(clocking["clock"])
		if clock == "" {
			add("contract.clocking.clock", "sequential contract must name a clock port", "clocking", "error")
		} else if port, ok := names[clock]; !ok {
			add("contract.clocking.clock", "clock port '"+clock+"' is not declared in io", "clocking", "error")
		} else if strings.ToLower(asString(port["dir"])) != "input" {
			add("contract.clocking.clock", "clock port '"+clock+"' must be an input", "clocking", "error")
		}
		if reset := asString(clocking["reset"]); reset != "" {
			if _, ok := names[reset]; !ok {
				add("contract.clocking.reset", "reset port '"+reset+"' is not declared in io", "clocking", "error")
			}
		}
		active := strings.ToLower(strings.TrimSpace(firstNonEmpty(asString(clocking["active"]), "high")))
		if !validActive[active] {
			add("contract.clocking.active", "clocking.active must be high or low", "clocking", "error")
		}
	}

	timing, _ := normalized["timing"].(map[string]any)
	if timing == nil {
		timing = map[string]any{}
	}
	for _, item := range io {
		port, ok := item.(map[string]any)
		if !ok || strings.ToLower(asString(port["dir"])) != "output" {
			continue
		}
		name := asString(port["name"])
		entry, ok := timing[name].(map[string]any)
		if !ok {
			add("contract.timing.missing", "output '"+name+"' has no timing entry", "timing", "error")
			continue
		}
		latency, ok := asInt(entry["latency_cycles"])
		if !ok || latency < 0 {
			add("contract.timing.latency", "output '"+name+"' must define a non-negative integer latency_cycles", "timing", "error")
		}
	}

	if strings.TrimSpace(asString(normalized["functional_summary"])) == "" {
		add("contract.summary", "functional_summary must describe the intended behavior", "functional_summary", "error")
	}

	guidance, ok := normalized["guidance"].(map[string]any)
	if !ok || len(guidance) == 0 {
		add("contract.guidance", "guidance is recommended for verifier/coder/debugger", "guidance", "warning")
	}

	return issues
}

// HasErrors reports whether any issue is more severe than a warning.
func HasErrors(issues []Issue) bool {
	for _, issue := range issues {
		if issue.Severity != "warning" {
			return true
		}
	}
	return false
}

// RenderIssues formats lint issues for an LLM prompt.
func RenderIssues(issues []Issue) string {
	if len(issues) == 0 {
		return "contract lint: no issues"
	}
	lines := []string{"contract lint issues:"}
	for _, issue := range issues {
		severity := issue.Severity
		if severity == "" {
			severity = "error"
		}
		lines = append(lines, "- ["+severity+"] "+issue.Code+": "+issue.Message)
	}
	return strings.Join(lines, "\n")
}

// OnlyEnabled reports whether downstream agents should see only the allowed subset.
func OnlyEnabled(request map[string]any) bool {
	if value, ok := request["contract_only"].(bool); ok {
		return value
	}
	return true
}

// Payload returns the contract subset that downstream agents are allowed to see.
func Payload(contract map[string]any, request map[string]any) map[string]any {
	if !OnlyEnabled(request) {
		return contract
	}
	result := map[string]any{}
	for _, key := range contractOnlyKeys {
		if value, ok := contract[key]; ok {
			result[key] = value
		}
	}
	return result
}

func asString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case nil:
		return ""
	default:
		return ""
	}
}

func asSlice(value any) []any {
	if list, ok := value.([]any); ok {
		return list
	}
	return nil
}

func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return typed == "true" || typed == "1"
	}
	return false
}
