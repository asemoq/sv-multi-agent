package agents

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/asemoq/sv-multi-agent/internal/llm"
)

// BugHunter reviews existing RTL for functional bugs (reset polarity, clocking,
// arithmetic, latches, race conditions, width/truncation, CDC, ...) and returns
// them as Issues without modifying the sources. A review focus may be supplied
// via instruction.
func BugHunter(ctx context.Context, client *llm.LLMClient, opts Options, files map[string]string, instruction string) *Result {
	if opts.Mode == "mock" {
		return failed("Mock bug hunter does not analyze RTL. Configure an LLM to enable bug hunting.")
	}

	focus := strings.TrimSpace(instruction)
	if focus != "" {
		focus = "\n\nReview focus:\n" + focus
	}
	prompt := promptWithContext(opts,
		"Existing sources:\n"+sourcesContext(files, opts.SourceBudget, nil)+
			focus+
			"\n\nFind functional bugs in the RTL. For each bug return file, line (optional), block_id (optional), severity (error or warning), category, a short title, a description, and a suggested fix:\n"+
			`{"bugs":[{"file":"rtl/module.sv","line":12,"block_id":"rtl/module.sv:blk_0","severity":"warning","category":"clocking","title":"...","description":"...","suggested_fix":"..."}],"summary":"one-line summary"}`)

	data, err := client.ChatJSON(ctx, systemPrompt(bugHunterSystemPrompt, bugHunterExamples), prompt)
	if err != nil {
		return failed(err.Error())
	}
	return bugHunterResult(data, files)
}

// bugHunterResult converts a bug list into Issues and a report artifact.
func bugHunterResult(data map[string]any, files map[string]string) *Result {
	raw, _ := data["bugs"].([]any)
	issues := []Issue{}
	bugs := []map[string]any{}

	for _, item := range raw {
		bug, ok := item.(map[string]any)
		if !ok {
			continue
		}
		title := strings.TrimSpace(asString(bug["title"]))
		if title == "" {
			continue
		}
		file := strings.TrimSpace(asString(bug["file"]))
		blockID := strings.TrimSpace(asString(bug["block_id"]))
		// Drop references to files not in the reviewed bundle (the model can
		// hallucinate file names).
		if !validBugLocation(file, blockID, files) {
			continue
		}
		severity := strings.ToLower(strings.TrimSpace(asString(bug["severity"])))
		if severity != "error" && severity != "warning" {
			severity = "warning"
		}
		category := strings.TrimSpace(asString(bug["category"]))
		if category == "" {
			category = "logic"
		}
		line := 0
		if number, ok := bug["line"].(float64); ok {
			line = int(number)
		}
		description := strings.TrimSpace(asString(bug["description"]))
		suggested := strings.TrimSpace(asString(bug["suggested_fix"]))

		location := file
		if line > 0 {
			if location != "" {
				location = location + ":" + strconv.Itoa(line)
			} else {
				location = "line " + strconv.Itoa(line)
			}
		}
		if location == "" {
			location = blockID
		}
		if location == "" {
			location = "unknown location"
		}

		message := "[" + category + "] " + title
		if description != "" {
			message += ": " + description
		}
		if suggested != "" {
			message += " (fix: " + suggested + ")"
		}
		message += " — " + location

		issues = append(issues, Issue{Severity: severity, Code: "bug." + category, Message: message})
		bugs = append(bugs, map[string]any{
			"file":          file,
			"line":          line,
			"block_id":      blockID,
			"severity":      severity,
			"category":      category,
			"title":         title,
			"description":   description,
			"suggested_fix": suggested,
		})
	}

	summary := strings.TrimSpace(asString(data["summary"]))
	if summary == "" {
		if len(issues) == 0 {
			summary = "No bugs found."
		} else {
			summary = "Found " + strconv.Itoa(len(issues)) + " suspected bug(s)."
		}
	}
	result := completed(summary)
	result.Issues = issues

	report := map[string]any{"summary": summary, "count": len(issues), "bugs": bugs}
	if serialized, err := json.MarshalIndent(report, "", "  "); err == nil {
		result.Artifacts = append(result.Artifacts, Artifact{Path: "reports/bugs.json", Content: string(serialized)})
	}
	result.Artifacts = append(result.Artifacts, Artifact{Path: "reports/bugs.md", Content: renderBugsMarkdown(summary, bugs)})
	return result
}

// validBugLocation reports whether the bug points at a file present in the
// reviewed bundle, or carries no location at all.
func validBugLocation(file, blockID string, files map[string]string) bool {
	if file != "" {
		_, ok := files[file]
		return ok
	}
	if blockID != "" {
		idx := strings.Index(blockID, ":")
		if idx > 0 {
			_, ok := files[blockID[:idx]]
			return ok
		}
		return false
	}
	return true
}

func renderBugsMarkdown(summary string, bugs []map[string]any) string {
	var builder strings.Builder
	builder.WriteString("# Bug report\n\n")
	builder.WriteString(summary + "\n")
	for _, bug := range bugs {
		severity := asString(bug["severity"])
		title := asString(bug["title"])
		file := asString(bug["file"])
		category := asString(bug["category"])
		description := asString(bug["description"])
		suggested := asString(bug["suggested_fix"])
		line := 0
		if number, ok := bug["line"].(int); ok {
			line = number
		}

		builder.WriteString("\n## [" + severity + "] " + title + "\n\n")
		location := file
		if line > 0 {
			location = file + ":" + strconv.Itoa(line)
		}
		builder.WriteString("- location: " + location + "\n")
		builder.WriteString("- category: " + category + "\n")
		if description != "" {
			builder.WriteString("- description: " + description + "\n")
		}
		if suggested != "" {
			builder.WriteString("- suggested fix: " + suggested + "\n")
		}
	}
	return builder.String()
}
