package agents

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/asemoq/sv-multi-agent/internal/llm"
	"github.com/asemoq/sv-multi-agent/internal/sv/contract"
)

// PlanModule is a sub-module of a hierarchy plan.
type PlanModule struct {
	Name          string         `json:"name"`
	Contract      map[string]any `json:"contract"`
	EstimateLines int            `json:"estimate_lines,omitempty"`
}

// PlanInstance wires a sub-module into the top-level module.
type PlanInstance struct {
	Module      string            `json:"module"`
	Instance    string            `json:"instance"`
	Connections map[string]string `json:"connections,omitempty"`
}

// Plan is the module hierarchy produced by the Planner.
type Plan struct {
	Modules   []PlanModule   `json:"modules"`
	Instances []PlanInstance `json:"instances"`
	Summary   string         `json:"summary,omitempty"`
}

// Planner decomposes a large design contract into cohesive sub-modules and a
// top-level wiring plan.
func Planner(ctx context.Context, client *llm.LLMClient, opts Options, contractData map[string]any, instruction string) *Result {
	normalized := contract.Normalize(contractData)
	if opts.Mode == "mock" {
		result := completed("Mock planner keeps the contract as a single module.")
		result.Plan = &Plan{Summary: "single module"}
		return result
	}

	prompt := promptWithContext(opts, "Design contract:\n"+prettyJSON(normalized)+
		"\n\nTask instruction:\n"+instruction)
	data, err := client.ChatJSON(ctx, plannerSystemPrompt, prompt)
	if err != nil {
		return failed(err.Error())
	}

	plan := parsePlan(data)
	revisions := 0
	for {
		issues := lintPlan(plan)
		if len(issues) == 0 {
			break
		}
		if revisions >= opts.MaxRevisions {
			message := "Planner produced an invalid hierarchy plan: " + issues[0]
			result := failed(message)
			result.Plan = plan
			return result
		}
		revisions++
		revisionPrompt := promptWithContext(opts, "Design contract:\n"+prettyJSON(normalized)+
			"\n\nCurrent plan:\n"+prettyJSON(plan)+
			"\n\nPlan lint issues:\n- "+strings.Join(issues, "\n- ")+
			"\n\nReturn the complete corrected plan in the same JSON shape.")
		revision, err := client.ChatJSON(ctx, plannerRevisionPrompt, revisionPrompt)
		if err != nil {
			return failed(err.Error())
		}
		if candidate := parsePlan(revision); len(candidate.Modules) > 0 {
			plan = candidate
		}
	}

	if len(plan.Modules) == 0 {
		return failed("Planner returned no sub-modules")
	}
	summary := plan.Summary
	if strings.TrimSpace(summary) == "" {
		summary = fmt.Sprintf("Decomposed the design into %d sub-module(s).", len(plan.Modules))
	}
	result := completed(summary)
	result.Plan = plan
	return result
}

func parsePlan(data map[string]any) *Plan {
	plan := &Plan{}
	if data == nil {
		return plan
	}
	if nested, ok := data["plan"].(map[string]any); ok {
		data = nested
	}
	plan.Summary = asString(data["summary"])

	if modules, ok := data["modules"].([]any); ok {
		for _, raw := range modules {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name := strings.TrimSpace(asString(entry["name"]))
			moduleContract, _ := entry["contract"].(map[string]any)
			if moduleContract == nil {
				moduleContract = entry
			}
			normalized := contract.Normalize(moduleContract)
			if name == "" {
				name = asString(normalized["module_name"])
			}
			if name == "" {
				continue
			}
			normalized["module_name"] = name
			module := PlanModule{Name: name, Contract: normalized}
			if estimate, ok := entry["estimate_lines"].(float64); ok {
				module.EstimateLines = int(estimate)
			}
			plan.Modules = append(plan.Modules, module)
		}
	}

	if instances, ok := data["instances"].([]any); ok {
		for _, raw := range instances {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			instance := PlanInstance{
				Module:      strings.TrimSpace(asString(entry["module"])),
				Instance:    strings.TrimSpace(asString(entry["instance"])),
				Connections: map[string]string{},
			}
			if connections, ok := entry["connections"].(map[string]any); ok {
				for key, value := range connections {
					instance.Connections[key] = asString(value)
				}
			}
			if instance.Module != "" {
				plan.Instances = append(plan.Instances, instance)
			}
		}
	}
	return plan
}

func lintPlan(plan *Plan) []string {
	issues := []string{}
	if plan == nil || len(plan.Modules) == 0 {
		return []string{"plan.modules must contain at least one sub-module"}
	}
	names := map[string]bool{}
	for _, module := range plan.Modules {
		if names[module.Name] {
			issues = append(issues, "duplicate sub-module name '"+module.Name+"'")
			continue
		}
		names[module.Name] = true
		for _, issue := range contract.Lint(module.Contract) {
			if issue.Severity == "warning" {
				continue
			}
			issues = append(issues, module.Name+": "+issue.Code+": "+issue.Message)
		}
	}
	for _, instance := range plan.Instances {
		if !names[instance.Module] {
			issues = append(issues, "instance '"+instance.Instance+"' references unknown module '"+instance.Module+"'")
		}
	}
	return issues
}

func planSection(plan *Plan) string {
	if plan == nil || len(plan.Modules) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("\n\nHierarchy plan (top-level must instantiate these sub-modules):\n")
	if strings.TrimSpace(plan.Summary) != "" {
		builder.WriteString(plan.Summary)
		builder.WriteString("\n")
	}
	builder.WriteString("Sub-modules:\n")
	for _, module := range plan.Modules {
		builder.WriteString("- " + module.Name + " (" + portSummary(module.Contract) + ")\n")
	}
	if len(plan.Instances) > 0 {
		builder.WriteString("Instances:\n")
		for _, instance := range plan.Instances {
			builder.WriteString("- " + instance.Instance + " : " + instance.Module)
			if len(instance.Connections) > 0 {
				parts := make([]string, 0, len(instance.Connections))
				for port, signal := range instance.Connections {
					parts = append(parts, port+"="+signal)
				}
				sort.Strings(parts)
				builder.WriteString(" (" + strings.Join(parts, ", ") + ")")
			}
			builder.WriteString("\n")
		}
	}
	return builder.String()
}

func portSummary(contractData map[string]any) string {
	io, _ := contractData["io"].([]any)
	parts := make([]string, 0, len(io))
	for _, raw := range io {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := asString(entry["name"])
		if name == "" {
			continue
		}
		dir := asString(entry["dir"])
		if dir == "" {
			dir = "port"
		}
		parts = append(parts, dir+" "+name)
	}
	return strings.Join(parts, ", ")
}
