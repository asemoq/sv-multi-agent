package workflow

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/asemoq/sv-multi-agent/internal/agents"
	"github.com/asemoq/sv-multi-agent/internal/store"
	"github.com/asemoq/sv-multi-agent/internal/sv/slicer"
)

// writeReport persists a human-readable and a JSON summary of the task to the
// workspace. It runs after the task has reached its final state.
func (m *Manager) writeReport(task *store.Task, simulation *agents.Result, fix *agents.Result) {
	if task == nil || task.Workspace == "" {
		return
	}
	markdown := buildReportMarkdown(task, simulation, fix)
	if err := writeArtifact(task.Workspace, "reports/report.md", markdown); err == nil {
		task.Artifacts = appendArtifact(task.Artifacts, store.Artifact{Path: "reports/report.md", Role: "report"})
	}
	if data, err := json.MarshalIndent(buildReportJSON(task, simulation), "", "  "); err == nil {
		if err := writeArtifact(task.Workspace, "reports/report.json", string(data)); err == nil {
			task.Artifacts = appendArtifact(task.Artifacts, store.Artifact{Path: "reports/report.json", Role: "report"})
		}
	}
	_ = m.store.Save(task)
}

func buildReportMarkdown(task *store.Task, simulation *agents.Result, _ *agents.Result) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Task %s\n\n", task.ID)
	fmt.Fprintf(&builder, "- status: %s\n", task.Status)
	fmt.Fprintf(&builder, "- stage: %s\n", task.Stage)
	fmt.Fprintf(&builder, "- mode: %s\n", task.SourceMode)
	fmt.Fprintf(&builder, "- prompt: %s\n", task.Prompt)
	if strings.TrimSpace(task.Summary) != "" {
		fmt.Fprintf(&builder, "- summary: %s\n", task.Summary)
	}
	if len(task.Issues) > 0 {
		builder.WriteString("\n## Issues\n\n")
		for _, issue := range task.Issues {
			fmt.Fprintf(&builder, "- [%v] %v: %v\n", issue["severity"], issue["code"], issue["message"])
		}
	}
	if simulation != nil {
		builder.WriteString("\n## Simulation\n\n")
		fmt.Fprintf(&builder, "- compile_exit_code: %d\n", simulation.CompileExitCode)
		fmt.Fprintf(&builder, "- simulation_exit_code: %d\n", simulation.SimulationExitCode)
		fmt.Fprintf(&builder, "- passed: %t\n", simulation.Passed)
		fmt.Fprintf(&builder, "- mismatch_count: %v\n", pointerText(simulation.MismatchCount))
		fmt.Fprintf(&builder, "- samples: %v\n", pointerText(simulation.Samples))
		fmt.Fprintf(&builder, "- fail_time: %v\n", pointerText(simulation.FailTime))
		if len(simulation.FailOutputs) > 0 {
			builder.WriteString("\n### Failing outputs\n\n")
			for _, output := range simulation.FailOutputs {
				fmt.Fprintf(&builder, "- %s: %d mismatches at time %d\n", output.Signal, output.Mismatches, output.Time)
			}
		}
		if simulation.Coverage != nil {
			rtl := simulation.Coverage.ForPrefix("rtl/")
			fmt.Fprintf(&builder, "\n### Coverage\n\n- RTL line coverage: %.1f%%\n", rtl.LineCoverage*100)
		}
		if simulation.TraceReport != nil {
			if suspects, ok := simulation.TraceReport["suspect_blocks"].([]slicer.Block); ok && len(suspects) > 0 {
				builder.WriteString("\n### Suspect blocks\n\n")
				for _, block := range suspects {
					fmt.Fprintf(&builder, "- %s (%s:%d-%d)\n", block.ID, block.File, block.StartLine, block.EndLine)
				}
			}
		}
	}
	if len(task.Steps) > 0 {
		builder.WriteString("\n## Steps\n\n")
		for _, step := range task.Steps {
			fmt.Fprintf(&builder, "- %s: %s", step.Name, step.Status)
			if strings.TrimSpace(step.Summary) != "" {
				fmt.Fprintf(&builder, " (%s)", step.Summary)
			}
			builder.WriteString("\n")
		}
	}
	return builder.String()
}

func buildReportJSON(task *store.Task, simulation *agents.Result) map[string]any {
	report := map[string]any{
		"id":        task.ID,
		"status":    task.Status,
		"stage":     task.Stage,
		"mode":      task.SourceMode,
		"prompt":    task.Prompt,
		"summary":   task.Summary,
		"issues":    task.Issues,
		"steps":     task.Steps,
		"workspace": task.Workspace,
	}
	if simulation != nil {
		report["simulation"] = map[string]any{
			"compile_exit_code":    simulation.CompileExitCode,
			"simulation_exit_code": simulation.SimulationExitCode,
			"passed":               simulation.Passed,
			"mismatch_count":       pointerAny(simulation.MismatchCount),
			"samples":              pointerAny(simulation.Samples),
			"fail_time":            pointerAny(simulation.FailTime),
			"fail_outputs":         simulation.FailOutputs,
			"trace_report":         simulation.TraceReport,
			"coverage":             simulation.Coverage,
		}
	}
	return report
}

func pointerText(value *int) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprintf("%d", *value)
}

func pointerAny(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
