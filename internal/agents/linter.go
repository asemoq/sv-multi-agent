package agents

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	lintErrorRe   = regexp.MustCompile(`%Error(?:-([A-Z0-9_]+))?:\s*(.*)`)
	lintWarningRe = regexp.MustCompile(`%Warning-([A-Z0-9_]+):\s*(.*)`)
)

// Lint runs Verilator's static lint on the RTL sources without building or
// simulating. It never requires a testbench.
func Lint(ctx context.Context, opts Options, files map[string]string) *Result {
	rtl := map[string]string{}
	for path, content := range files {
		if strings.HasPrefix(path, "rtl/") && isHDLSource(path) {
			rtl[path] = content
		}
	}
	if len(rtl) == 0 {
		return failed("Lint requires at least one SystemVerilog source under rtl/")
	}
	if _, err := exec.LookPath("verilator"); err != nil {
		return failed("Verilator is not installed on PATH")
	}

	tmpDir, err := os.MkdirTemp("", "svma-lint-*")
	if err != nil {
		return failed("create temp dir: " + err.Error())
	}
	defer os.RemoveAll(tmpDir)

	paths := make([]string, 0, len(rtl))
	for path := range rtl {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	commandSources := make([]string, 0, len(paths))
	for _, path := range paths {
		target := filepath.Join(tmpDir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return failed(err.Error())
		}
		if err := os.WriteFile(target, []byte(rtl[path]), 0o644); err != nil {
			return failed(err.Error())
		}
		if isCompilationUnit(path) {
			commandSources = append(commandSources, path)
		}
	}
	if len(commandSources) == 0 {
		return failed("Lint requires at least one .sv compilation unit under rtl/")
	}

	command := buildLintCommand(opts, commandSources)
	logText, exitCode, timedOut := runCommand(ctx, tmpDir, command, opts.VerilatorBuildTimeout)
	issues := parseLintIssues(logText)

	if timedOut {
		result := failed("Verilator lint timed out")
		result.Issues = append(result.Issues, Issue{Severity: "error", Code: "lint.timeout", Message: "Verilator lint timed out"})
		result.Artifacts = []Artifact{{Path: "reports/lint.log", Content: truncateTail(logText, 50000)}}
		return result
	}

	passed := exitCode == 0
	summary := "Verilator lint passed."
	if warnings := countSeverity(issues, "warning"); warnings > 0 && passed {
		summary = "Verilator lint passed with " + strconv.Itoa(warnings) + " warning(s)."
	}
	if !passed {
		summary = "Verilator lint failed"
		if message := firstLintError(issues); message != "" {
			summary += ": " + message
		} else {
			summary += ". See reports/lint.log."
		}
	}

	status := "completed"
	if !passed {
		status = "failed"
	}
	report := map[string]any{
		"status":    status,
		"exit_code": exitCode,
		"command":   command,
		"issues":    issues,
	}
	serialized, _ := json.MarshalIndent(report, "", "  ")

	result := &Result{
		Status:  status,
		Summary: summary,
		Issues:  issues,
		Artifacts: []Artifact{
			{Path: "reports/lint.log", Content: truncateTail(logText, 50000)},
			{Path: "reports/lint.json", Content: string(serialized)},
		},
	}
	return result
}

func buildLintCommand(opts Options, sources []string) []string {
	command := []string{"verilator", "--lint-only", "--sv", "-Wall"}
	if !opts.LintFailOnError {
		command = append(command, "-Wno-fatal")
	}
	for _, code := range opts.LintErrorOn {
		code = strings.ToUpper(strings.TrimSpace(code))
		if code == "" {
			continue
		}
		command = append(command, "-Werror-"+code)
	}
	if top := strings.TrimSpace(opts.DutTop); top != "" && topModuleRe.MatchString(top) {
		command = append(command, "--top-module", top)
	}
	return append(command, sources...)
}

func parseLintIssues(logText string) []Issue {
	issues := []Issue{}
	seen := map[string]bool{}
	for _, line := range strings.Split(logText, "\n") {
		stripped := strings.TrimSpace(line)
		if stripped == "" {
			continue
		}
		if match := lintErrorRe.FindStringSubmatch(stripped); match != nil {
			code := strings.TrimSpace(match[1])
			if code == "" {
				code = "ERROR"
			}
			issue := Issue{Severity: "error", Code: "verilator.lint." + code, Message: limitText(match[2], 2000)}
			if !seen[issue.Code+issue.Message] {
				seen[issue.Code+issue.Message] = true
				issues = append(issues, issue)
			}
			continue
		}
		if match := lintWarningRe.FindStringSubmatch(stripped); match != nil {
			issue := Issue{Severity: "warning", Code: "verilator.lint." + strings.TrimSpace(match[1]), Message: limitText(match[2], 2000)}
			if !seen[issue.Code+issue.Message] {
				seen[issue.Code+issue.Message] = true
				issues = append(issues, issue)
			}
		}
	}
	return issues
}

func firstLintError(issues []Issue) string {
	for _, issue := range issues {
		if issue.Severity == "error" {
			return issue.Message
		}
	}
	return ""
}

func countSeverity(issues []Issue, severity string) int {
	count := 0
	for _, issue := range issues {
		if issue.Severity == severity {
			count++
		}
	}
	return count
}

func limitText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit]
}

func isHDLSource(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".sv", ".svh", ".sva":
		return true
	default:
		return false
	}
}

func isCompilationUnit(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".sv":
		return true
	default:
		return false
	}
}
