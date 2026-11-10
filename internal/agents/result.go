// Package agents implements the Architect, Coder, Verifier, Simulator and
// Debugger in-process. It is the Go replacement for the former Python workers.
package agents

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/asemoq/sv-multi-agent/internal/sv/contract"
	"github.com/asemoq/sv-multi-agent/internal/sv/slicer"
)

// Artifact is a generated file to persist in the workspace.
type Artifact struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Issue is a structured agent issue.
type Issue struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// FailOutput mirrors the simulator marker hint.
type FailOutput struct {
	Signal     string `json:"signal"`
	Mismatches int    `json:"mismatches"`
	Time       int    `json:"time"`
}

// Result is the shared agent response.
type Result struct {
	Status             string
	Summary            string
	Artifacts          []Artifact
	Issues             []Issue
	ContractJSON       string
	ContractIssues     []contract.Issue
	ContractRevisions  int
	Passed             bool
	CompileExitCode    int
	SimulationExitCode int
	TopModule          string
	MismatchCount      *int
	Samples            *int
	FailTime           *int
	FailOutputs        []FailOutput
	TraceReport        map[string]any
	// Plan is set by the Planner when the design is decomposed.
	Plan *Plan
}

// Options carries the service-owned runtime configuration.
type Options struct {
	Mode                  string
	MaxRevisions          int
	ContractOnly          bool
	TopModule             string
	VerilatorBuildTimeout int
	VerilatorRunTimeout   int
	TraceWindowEdges      int
	// Context is optional retrieved RAG context prepended to LLM prompts.
	Context string
	// SourceBudget caps how many characters of source context reach the LLM.
	SourceBudget int
	// Plan is an optional module hierarchy produced by the Planner.
	Plan *Plan
}

func completed(summary string) *Result {
	return &Result{Status: "completed", Summary: summary}
}

func failed(summary string) *Result {
	return &Result{Status: "failed", Summary: summary}
}

// filesResult normalizes a model response containing files/artifacts.
func filesResult(data map[string]any, defaultDir string) *Result {
	filesValue := data["files"]
	if filesValue == nil {
		filesValue = data["artifacts"]
	}

	var items []any
	switch typed := filesValue.(type) {
	case map[string]any:
		names := make([]string, 0, len(typed))
		for name := range typed {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			items = append(items, map[string]any{
				"path":    name,
				"content": typed[name],
			})
		}
	case []any:
		items = typed
	}

	artifacts := []Artifact{}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		path := strings.TrimSpace(asString(entry["path"]))
		content := asString(entry["content"])
		if path == "" || content == "" {
			continue
		}
		if strings.HasPrefix(path, "/") || filepathIsAbs(path) || hasParent(path) {
			continue
		}
		if !strings.HasPrefix(path, defaultDir+"/") {
			path = defaultDir + "/" + baseName(path)
		}
		artifacts = append(artifacts, Artifact{Path: path, Content: content})
	}

	if len(artifacts) == 0 {
		return failed("Model response did not contain any valid files")
	}
	summary := asString(data["summary"])
	if strings.TrimSpace(summary) == "" {
		summary = "Generated " + strconv.Itoa(len(artifacts)) + " file(s)."
	}
	result := completed(summary)
	result.Artifacts = artifacts
	return result
}

// ApplyBlockPatches replaces block ranges in memory and returns full-file artifacts.
func ApplyBlockPatches(files map[string]string, patches []any) []Artifact {
	if len(files) == 0 {
		return nil
	}
	systemVerilog := map[string]string{}
	for path, content := range files {
		if strings.HasSuffix(strings.ToLower(path), ".sv") {
			systemVerilog[path] = content
		}
	}
	blocks := slicer.ParseSources(systemVerilog)
	byID := map[string]slicer.Block{}
	for _, block := range blocks {
		byID[block.ID] = block
	}

	type edit struct {
		start int
		end   int
		code  string
	}
	edits := map[string][]edit{}
	for _, raw := range patches {
		patch, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id := strings.TrimSpace(asString(patch["block_id"]))
		newCode, ok := patch["new_code"].(string)
		if id == "" || !ok || strings.TrimSpace(newCode) == "" {
			continue
		}
		block, ok := byID[id]
		if !ok {
			continue
		}
		edits[block.File] = append(edits[block.File], edit{start: block.StartLine, end: block.EndLine, code: newCode})
	}

	artifacts := []Artifact{}
	for file, fileEdits := range edits {
		content, ok := files[file]
		if !ok {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
		sort.SliceStable(fileEdits, func(i, j int) bool { return fileEdits[i].start > fileEdits[j].start })
		for _, item := range fileEdits {
			if item.start < 1 || item.end < item.start || item.start > len(lines) {
				continue
			}
			end := item.end
			if end > len(lines) {
				end = len(lines)
			}
			replacement := strings.Split(strings.ReplaceAll(item.code, "\r\n", "\n"), "\n")
			updated := make([]string, 0, len(lines)-(end-(item.start-1))+len(replacement))
			updated = append(updated, lines[:item.start-1]...)
			updated = append(updated, replacement...)
			updated = append(updated, lines[end:]...)
			lines = updated
		}
		artifacts = append(artifacts, Artifact{Path: file, Content: strings.Join(lines, "\n") + "\n"})
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	return artifacts
}

func prettyJSON(value any) string {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return ""
	}
	return string(data)
}

func filesContext(files map[string]string) string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	chunks := []string{}
	for _, path := range paths {
		content := files[path]
		if path != "" && content != "" {
			chunks = append(chunks, "--- "+path+" ---\n"+content)
		}
	}
	return strings.Join(chunks, "\n\n")
}

// sourcesContext renders the source bundle within budget. Small bundles are
// sent in full; large ones are reduced to a structural index plus, optionally,
// the full code of focus blocks.
func sourcesContext(files map[string]string, budget int, focus []slicer.Block) string {
	if budget <= 0 {
		budget = 24000
	}
	full := filesContext(files)
	if len(full) <= budget {
		return full
	}

	sv := map[string]string{}
	others := []string{}
	for path, content := range files {
		if strings.TrimSpace(content) == "" {
			continue
		}
		if strings.HasSuffix(strings.ToLower(path), ".sv") {
			sv[path] = content
			continue
		}
		others = append(others, path)
	}
	units := slicer.ParseSourcesStructure(sv)

	var builder strings.Builder
	builder.WriteString("Source structure (large files are summarized; full bodies omitted to fit the context budget):\n")
	if index := slicer.CompactStructureIndex(units, 80); index != "" {
		builder.WriteString(index)
		builder.WriteString("\n")
	}

	if len(focus) > 0 && builder.Len() < budget {
		builder.WriteString("\nRelevant blocks (full code):\n")
		seen := map[string]bool{}
		for _, block := range focus {
			if seen[block.ID] || builder.Len() >= budget {
				continue
			}
			seen[block.ID] = true
			builder.WriteString("--- " + block.ID + " (" + block.Kind + ") ---\n")
			builder.WriteString(block.Code)
			builder.WriteString("\n\n")
		}
	}

	sort.Strings(others)
	for _, path := range others {
		if builder.Len() >= budget {
			break
		}
		entry := "--- " + path + " ---\n" + files[path] + "\n\n"
		if builder.Len()+len(entry) > budget {
			builder.WriteString("--- " + path + " --- [omitted: context budget]\n")
			continue
		}
		builder.WriteString(entry)
	}
	return truncateContext(builder.String(), budget)
}

func truncateContext(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "\n... [truncated to fit the context budget]"
}

// asString returns the string value of a JSON-decoded value.
func asString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func filepathIsAbs(path string) bool {
	return strings.HasPrefix(path, "/") || strings.HasPrefix(path, "\\") ||
		(len(path) >= 2 && path[1] == ':')
}

func hasParent(path string) bool {
	for _, part := range strings.Split(strings.ReplaceAll(path, "\\", "/"), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func baseName(path string) string {
	normalized := strings.ReplaceAll(path, "\\", "/")
	parts := strings.Split(normalized, "/")
	return parts[len(parts)-1]
}

func firstError(issues []contract.Issue) string {
	for _, issue := range issues {
		if issue.Severity != "warning" {
			return issue.Message
		}
	}
	return "contract lint error"
}

func extractContract(data map[string]any) map[string]any {
	if nested, ok := data["contract"].(map[string]any); ok {
		return nested
	}
	return data
}

func simulationDigest(simulation *Result) map[string]any {
	if simulation == nil {
		return map[string]any{}
	}
	digest := map[string]any{
		"status":               simulation.Status,
		"passed":               simulation.Passed,
		"summary":              simulation.Summary,
		"compile_exit_code":    simulation.CompileExitCode,
		"simulation_exit_code": simulation.SimulationExitCode,
		"mismatch_count":       pointerValue(simulation.MismatchCount),
		"samples":              pointerValue(simulation.Samples),
		"fail_time":            pointerValue(simulation.FailTime),
		"fail_outputs":         simulation.FailOutputs,
		"issues":               simulation.Issues,
	}
	return digest
}

func pointerValue(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
