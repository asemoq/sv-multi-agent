package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	lintErrorRe   = regexp.MustCompile(`%Error(?:-([A-Z0-9_]+))?:\s*(.*)`)
	lintWarningRe = regexp.MustCompile(`%Warning-([A-Z0-9_]+):\s*(.*)`)
	packageDeclRe = regexp.MustCompile(`(?m)^\s*package\s+[A-Za-z_]\w*\s*;`)
)

// orderSourcesForVerilator reorders sources so files declaring a package come
// first. Verilator requires a package to be predeclared before any file that
// imports it, and plain alphabetical order can violate that (e.g. control.sv
// importing eth_common before eth_common.sv is seen).
func orderSourcesForVerilator(sources []string, files map[string]string) []string {
	packages := []string{}
	rest := []string{}
	for _, path := range sources {
		if packageDeclRe.MatchString(files[path]) {
			packages = append(packages, path)
		} else {
			rest = append(rest, path)
		}
	}
	sort.Strings(packages)
	sort.Strings(rest)
	return append(packages, rest...)
}

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
	if err := checkSimTool(opts); err != nil {
		return failed(err.Error())
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
	commandSources = orderSourcesForVerilator(commandSources, rtl)

	command, logText, exitCode, timedOut, issues := lintRun(ctx, opts, tmpDir, commandSources)

	toolName := "Verilator"
	if opts.SimTool == "questa" {
		toolName = "QuestaSim"
	}

	if timedOut {
		result := failed(toolName + " lint timed out")
		result.Issues = append(result.Issues, Issue{Severity: "error", Code: "lint.timeout", Message: toolName + " lint timed out"})
		result.Artifacts = []Artifact{{Path: "reports/lint.log", Content: truncateTail(logText, 50000)}}
		return result
	}

	passed := exitCode == 0
	summary := toolName + " lint passed."
	if warnings := countSeverity(issues, "warning"); warnings > 0 && passed {
		summary = toolName + " lint passed with " + strconv.Itoa(warnings) + " warning(s)."
	}
	if !passed {
		summary = toolName + " lint failed"
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

// lintRun dispatches lint to the selected backend and returns the command,
// log, exit code and parsed issues.
func lintRun(ctx context.Context, opts Options, tmpDir string, sources []string) ([]string, string, int, bool, []Issue) {
	if opts.SimTool == "questa" {
		if _, code, timedOut := runCommand(ctx, tmpDir, []string{"vlib", "work"}, 60); timedOut || code != 0 {
			return []string{"vlib", "work"}, "vlib work failed", code, timedOut, []Issue{{Severity: "error", Code: "questa.lint.vlib", Message: "vlib work failed"}}
		}
		command := append([]string{"vlog", "-sv", "-suppress", "2244", "-work", "work"}, sources...)
		logText, exitCode, timedOut := runCommand(ctx, tmpDir, command, opts.VerilatorBuildTimeout)
		return command, logText, exitCode, timedOut, parseQuestaIssues(logText)
	}
	command := buildLintCommand(opts, sources)
	logText, exitCode, timedOut := runCommand(ctx, tmpDir, command, opts.VerilatorBuildTimeout)
	return command, logText, exitCode, timedOut, parseLintIssues(logText)
}

var (
	questaErrorRe   = regexp.MustCompile(`\*\*\s*Error[^:]*:\s*(.*)`)
	questaWarningRe = regexp.MustCompile(`\*\*\s*Warning[^:]*:\s*(.*)`)
)

// parseQuestaIssues parses QuestaSim vlog/vsim "** Error"/"** Warning" lines.
func parseQuestaIssues(logText string) []Issue {
	issues := []Issue{}
	seen := map[string]bool{}
	for _, line := range strings.Split(logText, "\n") {
		stripped := strings.TrimSpace(line)
		if stripped == "" {
			continue
		}
		if match := questaErrorRe.FindStringSubmatch(stripped); match != nil {
			issue := Issue{Severity: "error", Code: "questa.lint.error", Message: limitText(match[1], 2000)}
			if !seen[issue.Code+issue.Message] {
				seen[issue.Code+issue.Message] = true
				issues = append(issues, issue)
			}
			continue
		}
		if match := questaWarningRe.FindStringSubmatch(stripped); match != nil {
			issue := Issue{Severity: "warning", Code: "questa.lint.warning", Message: limitText(match[1], 2000)}
			if !seen[issue.Code+issue.Message] {
				seen[issue.Code+issue.Message] = true
				issues = append(issues, issue)
			}
		}
	}
	return issues
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
	case ".sv", ".svh", ".sva", ".v", ".vh":
		return true
	default:
		return false
	}
}

func isCompilationUnit(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".sv", ".v":
		return true
	default:
		return false
	}
}
