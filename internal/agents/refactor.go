package agents

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/asemoq/sv-multi-agent/internal/llm"
)

// Refactor transforms existing RTL according to an instruction while preserving
// behavior. It returns whole-file replacements under the original rtl/ or tb/
// paths, or localized block patches resolved against the current sources.
func Refactor(ctx context.Context, client *llm.LLMClient, opts Options, files map[string]string, instruction string) *Result {
	if opts.Mode == "mock" {
		return failed("Mock refactor has no automated transformation. Configure an LLM to enable refactoring.")
	}

	prompt := promptWithContext(opts,
		"Existing sources:\n"+sourcesContext(files, opts.SourceBudget, nil)+
			"\n\nRefactoring instruction:\n"+instruction+
			"\n\nReturn the transformed sources. Prefer complete-file replacements keyed by their original rtl/ or tb/ paths:\n"+
			`{"files":{"rtl/module.sv":"complete updated file"},"summary":"what changed and why"}`+
			"\nFor a small localized change you may instead return a block patch by id:\n"+
			`{"patches":[{"block_id":"rtl/module.sv:blk_0","new_code":"complete replacement for that block"}],"summary":"what changed and why"}`)

	data, err := client.ChatJSON(ctx, systemPrompt(refactorSystemPrompt, refactorExamples), prompt)
	if err != nil {
		return failed(err.Error())
	}
	return refactorResult(data, files)
}

// refactorResult normalizes a refactor response. Block patches are applied in
// memory; otherwise whole files are accepted under their original safe paths.
// An optional "delete" list records files removed by the refactor.
func refactorResult(data map[string]any, files map[string]string) *Result {
	if patches, ok := data["patches"].([]any); ok && len(patches) > 0 {
		if artifacts := ApplyBlockPatches(files, patches); len(artifacts) > 0 {
			result := refactorCompleted(data, artifacts)
			result.DeletePaths = deletedPaths(data)
			return result
		}
	}
	artifacts := preservedFiles(data)
	deletePaths := deletedPaths(data)
	if len(artifacts) == 0 && len(deletePaths) == 0 {
		return failed("Model response did not contain any valid files, patches or deletions")
	}
	result := refactorCompleted(data, artifacts)
	result.DeletePaths = deletePaths
	return result
}

func refactorCompleted(data map[string]any, artifacts []Artifact) *Result {
	summary := asString(data["summary"])
	if strings.TrimSpace(summary) == "" {
		summary = "Refactored " + strconv.Itoa(len(artifacts)) + " file(s)."
	}
	result := completed(summary)
	result.Artifacts = artifacts
	return result
}

// preservedFiles reads files/artifacts and keeps the original relative paths
// (unlike filesResult, which forces a default directory). Only safe relative
// paths under rtl/ or tb/ are accepted.
func preservedFiles(data map[string]any) []Artifact {
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
			items = append(items, map[string]any{"path": name, "content": typed[name]})
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
		if path == "" || content == "" || !validRefactorPath(path) {
			continue
		}
		artifacts = append(artifacts, Artifact{Path: path, Content: content})
	}
	return artifacts
}

// validRefactorPath accepts only safe relative paths under rtl/ or tb/.
func validRefactorPath(path string) bool {
	normalized := strings.ReplaceAll(strings.TrimSpace(path), "\\", "/")
	if normalized == "" || strings.HasPrefix(normalized, "/") {
		return false
	}
	if len(normalized) >= 2 && normalized[1] == ':' {
		return false
	}
	for _, part := range strings.Split(normalized, "/") {
		if part == ".." || part == "" {
			return false
		}
	}
	return strings.HasPrefix(normalized, "rtl/") || strings.HasPrefix(normalized, "tb/")
}

// deletedPaths reads the optional "delete" list (files removed by the refactor).
func deletedPaths(data map[string]any) []string {
	raw, _ := data["delete"].([]any)
	paths := []string{}
	for _, item := range raw {
		path := strings.TrimSpace(asString(item))
		if path == "" || !validRefactorPath(path) {
			continue
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}
