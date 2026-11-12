package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/asemoq/sv-multi-agent/internal/logx"
	"github.com/asemoq/sv-multi-agent/internal/sv/coverage"
	"github.com/asemoq/sv-multi-agent/internal/sv/markers"
	"github.com/asemoq/sv-multi-agent/internal/sv/trace"
)

var topModuleRe = regexp.MustCompile(`^[A-Za-z_]\w*$`)

// Simulator compiles and runs the RTL/testbench with Verilator.
func Simulator(ctx context.Context, opts Options, files map[string]string, contractData map[string]any) *Result {
	rtl := map[string]string{}
	tb := map[string]string{}
	for path, content := range files {
		switch {
		case strings.HasPrefix(path, "rtl/"):
			rtl[path] = content
		case strings.HasPrefix(path, "tb/"):
			tb[path] = content
		}
	}
	if len(rtl) == 0 || len(tb) == 0 {
		return failed("Expected at least one SystemVerilog source under rtl/ and one testbench under tb/")
	}
	if err := checkSimTool(opts); err != nil {
		return failed(err.Error())
	}

	tmpDir, err := os.MkdirTemp("", "svma-sim-*")
	if err != nil {
		return failed("create temp dir: " + err.Error())
	}
	defer os.RemoveAll(tmpDir)

	sources := map[string]string{}
	for path, content := range rtl {
		sources[path] = content
	}
	for path, content := range tb {
		sources[path] = content
	}
	for path, content := range sources {
		target := filepath.Join(tmpDir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return failed(err.Error())
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return failed(err.Error())
		}
	}

	reportDir := filepath.Join(tmpDir, "reports")
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		return failed(err.Error())
	}

	relativeSources := make([]string, 0, len(sources))
	for path := range sources {
		if isCompilationUnit(path) {
			relativeSources = append(relativeSources, filepath.ToSlash(path))
		}
	}
	relativeSources = orderSourcesForVerilator(relativeSources, sources)
	if len(relativeSources) == 0 {
		return failed("Expected at least one .sv compilation unit under rtl/ or tb/")
	}

	topModule := topModuleName(opts)
	compileLog, compileCode, runLog, runCode, timedOut := compileAndRun(ctx, opts, tmpDir, relativeSources, topModule)
	combined := compileLog
	if compileCode == 0 {
		combined = compileLog + "\n--- SIMULATION ---\n" + runLog
	}
	logx.Debugf("simulator: compile exit=%d log:\n%s", compileCode, compileLog)
	logx.Debugf("simulator: run exit=%d log:\n%s", runCode, runLog)

	if timedOut {
		log := combined + "\nSimulation command timed out."
		_ = os.WriteFile(filepath.Join(reportDir, "verilator.log"), []byte(log), 0o644)
		result := failed("Simulation timed out")
		result.Passed = false
		result.Issues = []Issue{{Severity: "error", Code: "simulator.timeout", Message: "Simulation timed out"}}
		result.Artifacts = []Artifact{{Path: "reports/verilator.log", Content: truncateTail(log, 50000)}}
		return result
	}

	parsed := markers.Parse(runLog)
	applyLegacyPass(runLog, &parsed)
	compileOK := compileCode == 0
	runOK := runCode == 0
	mismatchOK := parsed.MismatchCount == nil || *parsed.MismatchCount == 0
	passed := compileOK && runOK && parsed.PassedMarker && !parsed.FailedMarker && mismatchOK && !markers.HasErrors(runLog)
	logx.Infof("simulator: passed=%t compile_exit=%d run_exit=%d", passed, compileCode, runCode)

	issues := []Issue{}
	if !passed {
		if !compileOK {
			issues = append(issues, extractIssue(combined))
		} else {
			issue := markers.BuildIssue(runLog, parsed)
			issues = append(issues, Issue{Severity: issue.Severity, Code: issue.Code, Message: issue.Message})
		}
	}

	var coverageReport *coverage.Report
	coverageArtifacts := []Artifact{}
	if opts.CoverageEnabled && compileOK {
		var report *coverage.Report
		var artifacts []Artifact
		var coverageIssue Issue
		if opts.SimTool == "questa" {
			report, artifacts, coverageIssue = collectCoverageQuesta(ctx, tmpDir, reportDir)
		} else {
			report, artifacts, coverageIssue = collectCoverage(ctx, tmpDir, reportDir)
		}
		coverageReport = report
		coverageArtifacts = artifacts
		if coverageIssue.Code != "" {
			issues = append(issues, coverageIssue)
		}
	}

	var traceReport map[string]any
	if !passed {
		traceReport = trace.BuildTraceReport(tmpDir, orString(runLog, combined), contractData, opts.TraceWindowEdges, 8, rtl)
		if traceReport != nil {
			if data, err := json.MarshalIndent(traceReport, "", "  "); err == nil {
				_ = os.WriteFile(filepath.Join(reportDir, "trace_report.json"), data, 0o644)
			}
		}
	}

	summary := summaryText(passed, compileOK, runLog, parsed)
	logx.Infof("simulator: %s", summary)
	report := map[string]any{
		"status":               statusFor(passed),
		"passed":               passed,
		"summary":              summary,
		"compile_exit_code":    compileCode,
		"simulation_exit_code": runCode,
		"top_module":           topModule,
		"mismatch_count":       pointerValue(parsed.MismatchCount),
		"samples":              pointerValue(parsed.Samples),
		"fail_time":            pointerValue(parsed.FailTime),
		"fail_outputs":         parsed.FailOutputs,
		"sources":              relativeSources,
		"issues":               issues,
		"coverage":             coverageReport,
		"trace_report":         traceReport,
	}
	serialized, _ := json.MarshalIndent(report, "", "  ")
	artifacts := []Artifact{
		{Path: "reports/verilator.log", Content: truncateTail(combined, 50000)},
		{Path: "reports/simulation.json", Content: string(serialized)},
	}
	artifacts = append(artifacts, coverageArtifacts...)
	if traceReport != nil {
		if data, err := json.MarshalIndent(traceReport, "", "  "); err == nil {
			artifacts = append(artifacts, Artifact{Path: "reports/trace_report.json", Content: string(data)})
		}
	}

	result := &Result{
		Status:             statusFor(passed),
		Summary:            summary,
		Artifacts:          artifacts,
		Issues:             issues,
		Passed:             passed,
		CompileExitCode:    compileCode,
		SimulationExitCode: runCode,
		TopModule:          topModule,
		MismatchCount:      parsed.MismatchCount,
		Samples:            parsed.Samples,
		FailTime:           parsed.FailTime,
		FailOutputs:        make([]FailOutput, 0, len(parsed.FailOutputs)),
		TraceReport:        traceReport,
		Coverage:           coverageReport,
	}
	for _, output := range parsed.FailOutputs {
		result.FailOutputs = append(result.FailOutputs, FailOutput{
			Signal:     output.Signal,
			Mismatches: output.Mismatches,
			Time:       output.Time,
		})
	}
	return result
}

func buildVerilatorCommand(opts Options, buildDir string, sources []string, topModule string) []string {
	command := []string{
		"verilator", "--binary", "--sv", "--timing", "--trace", "--assert",
		"-Wall", "-Wno-fatal", "-j", "0", "--top-module", topModule, "--Mdir", buildDir,
	}
	if opts.CoverageEnabled {
		command = append(command, "--coverage-line", "--coverage-toggle")
	}
	return append(command, sources...)
}

// checkSimTool verifies the selected simulator backend is available.
func checkSimTool(opts Options) error {
	if opts.SimTool == "questa" {
		for _, tool := range []string{"vlib", "vlog", "vsim"} {
			if _, err := exec.LookPath(tool); err != nil {
				return fmt.Errorf("QuestaSim tool %q is not installed on PATH", tool)
			}
		}
		return nil
	}
	if _, err := exec.LookPath("verilator"); err != nil {
		return fmt.Errorf("Verilator is not installed on PATH")
	}
	return nil
}

// compileAndRun dispatches to the selected simulator backend.
func compileAndRun(ctx context.Context, opts Options, tmpDir string, sources []string, topModule string) (compileLog string, compileCode int, runLog string, runCode int, timedOut bool) {
	if opts.SimTool == "questa" {
		return compileAndRunQuesta(ctx, opts, tmpDir, sources, topModule)
	}
	return compileAndRunVerilator(ctx, opts, tmpDir, sources, topModule)
}

func compileAndRunVerilator(ctx context.Context, opts Options, tmpDir string, sources []string, topModule string) (string, int, string, int, bool) {
	buildDir := filepath.Join(tmpDir, "build", "obj_dir")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return err.Error(), 1, "", 0, false
	}
	command := buildVerilatorCommand(opts, buildDir, sources, topModule)
	logx.Debugf("simulator: command: %s", strings.Join(command, " "))
	compileLog, compileCode, compileTimedOut := runCommand(ctx, tmpDir, command, opts.VerilatorBuildTimeout)
	if compileTimedOut || compileCode != 0 {
		return compileLog, compileCode, "", 0, compileTimedOut
	}
	executable := filepath.Join(buildDir, "V"+topModule)
	if _, statErr := os.Stat(executable); statErr != nil {
		return compileLog, compileCode, "Verilator build completed but build/obj_dir/V" + topModule + " was not created.", 127, false
	}
	runLog, runCode, runTimedOut := runCommand(ctx, tmpDir, []string{executable}, opts.VerilatorRunTimeout)
	return compileLog, compileCode, runLog, runCode, runTimedOut
}

func compileAndRunQuesta(ctx context.Context, opts Options, tmpDir string, sources []string, topModule string) (string, int, string, int, bool) {
	// Create the work library and compile all sources into it.
	if _, code, timedOut := runCommand(ctx, tmpDir, []string{"vlib", "work"}, 60); timedOut || code != 0 {
		return "vlib work failed", code, "", 0, timedOut
	}
	compileArgs := []string{"vlog", "-sv", "-suppress", "2244"}
	if opts.CoverageEnabled {
		compileArgs = append(compileArgs, "+cover=sb")
	}
	compileArgs = append(compileArgs, "-work", "work")
	compileArgs = append(compileArgs, sources...)
	logx.Debugf("simulator: command: %s", strings.Join(compileArgs, " "))
	compileLog, compileCode, compileTimedOut := runCommand(ctx, tmpDir, compileArgs, opts.VerilatorBuildTimeout)
	if compileTimedOut || compileCode != 0 {
		return compileLog, compileCode, "", 0, compileTimedOut
	}
	// Run the simulation in console (batch) mode and quit after run -all.
	simDo := "run -all; quit -f"
	simArgs := []string{"vsim"}
	if opts.CoverageEnabled {
		simArgs = append(simArgs, "-coverage")
		simDo = "coverage save -onexit cov.ucdb; run -all; quit -f"
	}
	simArgs = append(simArgs, "-c", "-do", simDo, "work."+topModule)
	logx.Debugf("simulator: command: %s", strings.Join(simArgs, " "))
	runLog, runCode, runTimedOut := runCommand(ctx, tmpDir, simArgs, opts.VerilatorRunTimeout)
	return compileLog, compileCode, normalizeQuestaTranscript(runLog), runCode, runTimedOut
}

// normalizeQuestaTranscript strips the "# " prefix QuestaSim adds to $display
// output so the marker protocol can be parsed as plain text.
func normalizeQuestaTranscript(log string) string {
	if !strings.Contains(log, "# ") {
		return log
	}
	lines := strings.Split(log, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(line, "# ")
	}
	return strings.Join(lines, "\n")
}

// collectCoverageQuesta converts a QuestaSim UCDB into a line-coverage report
// using `vcover report -details`.
func collectCoverageQuesta(ctx context.Context, tmpDir, reportDir string) (*coverage.Report, []Artifact, Issue) {
	ucdb := filepath.Join(tmpDir, "cov.ucdb")
	if _, err := os.Stat(ucdb); err != nil {
		return nil, nil, Issue{Severity: "warning", Code: "coverage.data", Message: "cov.ucdb was not produced; coverage collection skipped"}
	}
	tool, err := exec.LookPath("vcover")
	if err != nil {
		return nil, nil, Issue{Severity: "warning", Code: "coverage.tool", Message: "vcover is not installed on PATH; coverage collection skipped"}
	}
	txtPath := filepath.Join(reportDir, "coverage.txt")
	logText, exitCode, timedOut := runCommand(ctx, tmpDir, []string{tool, "report", "-details", "-output", txtPath, ucdb}, 120)
	if timedOut || exitCode != 0 {
		return nil, nil, Issue{Severity: "warning", Code: "coverage.convert", Message: "vcover report failed: " + truncateTail(logText, 1000)}
	}
	data, err := os.ReadFile(txtPath)
	if err != nil {
		return nil, nil, Issue{Severity: "warning", Code: "coverage.read", Message: "read coverage report: " + err.Error()}
	}
	report := coverage.ParseQuestaDetails(data)
	serialized, _ := json.MarshalIndent(report, "", "  ")
	artifacts := []Artifact{
		{Path: "reports/coverage.txt", Content: string(data)},
		{Path: "reports/coverage.json", Content: string(serialized)},
	}
	return &report, artifacts, Issue{}
}

func collectCoverage(ctx context.Context, tmpDir, reportDir string) (*coverage.Report, []Artifact, Issue) {
	dataPath := ""
	for _, candidate := range []string{
		filepath.Join(tmpDir, "coverage.dat"),
		filepath.Join(tmpDir, "build", "coverage.dat"),
		filepath.Join(tmpDir, "build", "obj_dir", "coverage.dat"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			dataPath = candidate
			break
		}
	}
	if dataPath == "" {
		return nil, nil, Issue{
			Severity: "warning",
			Code:     "coverage.data",
			Message:  "coverage.dat was not produced; coverage collection skipped",
		}
	}

	tool, err := exec.LookPath("verilator_coverage")
	if err != nil {
		return nil, nil, Issue{
			Severity: "warning",
			Code:     "coverage.tool",
			Message:  "verilator_coverage is not installed on PATH; coverage collection skipped",
		}
	}

	infoPath := filepath.Join(reportDir, "coverage.info")
	logText, exitCode, timedOut := runCommand(ctx, tmpDir, []string{tool, "--write-info", infoPath, dataPath}, 60)
	if timedOut || exitCode != 0 {
		message := "verilator_coverage failed"
		if timedOut {
			message += ": timed out"
		}
		if tail := strings.TrimSpace(truncateTail(logText, 1000)); tail != "" {
			message += ": " + tail
		}
		return nil, nil, Issue{Severity: "warning", Code: "coverage.convert", Message: message}
	}

	data, err := os.ReadFile(infoPath)
	if err != nil {
		return nil, nil, Issue{Severity: "warning", Code: "coverage.read", Message: "read coverage report: " + err.Error()}
	}
	report := coverage.ParseLCOV(data, tmpDir)
	serialized, _ := json.MarshalIndent(report, "", "  ")
	artifacts := []Artifact{
		{Path: "reports/coverage.info", Content: string(data)},
		{Path: "reports/coverage.json", Content: string(serialized)},
	}
	return &report, artifacts, Issue{}
}

func topModuleName(opts Options) string {
	candidate := strings.TrimSpace(opts.TopModule)
	if candidate == "" || !topModuleRe.MatchString(candidate) {
		return "tb"
	}
	return candidate
}

func runCommand(parent context.Context, dir string, command []string, timeoutSeconds int) (string, int, bool) {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 60
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(output) + "\nVerilator command timed out.", 124, true
	}
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if err != nil && code == 0 {
		code = 1
	}
	return string(output), code, false
}

func extractIssue(log string) Issue {
	for _, line := range strings.Split(log, "\n") {
		stripped := strings.TrimSpace(line)
		if stripped == "" {
			continue
		}
		lower := strings.ToLower(stripped)
		if strings.Contains(stripped, "%Error") || strings.Contains(lower, "error:") || strings.Contains(lower, "fatal") {
			if len(stripped) > 2000 {
				stripped = stripped[:2000]
			}
			return Issue{Severity: "error", Code: "verilator.compile", Message: stripped}
		}
	}
	return Issue{Severity: "error", Code: "verilator.compile", Message: "Verilator returned a non-zero exit code."}
}

func summaryText(passed, compileOK bool, runLog string, parsed markers.Result) string {
	if passed {
		return "Compile and simulation passed."
	}
	if !compileOK {
		return "Compilation failed. See reports/verilator.log."
	}
	if parsed.FailedMarker || (parsed.MismatchCount != nil && *parsed.MismatchCount > 0) {
		count := "unknown number of"
		if parsed.MismatchCount != nil {
			count = strconv.Itoa(*parsed.MismatchCount)
		}
		failTime := "unknown time"
		if parsed.FailTime != nil {
			failTime = strconv.Itoa(*parsed.FailTime)
		}
		return "Simulation failed: " + count + " mismatches, first mismatch at time " + failTime + ". See reports/trace_report.json."
	}
	if markers.HasErrors(runLog) {
		return "Simulation failed with errors. See reports/verilator.log."
	}
	return "Simulation did not report SIMULATION PASSED. See reports/verilator.log."
}

func applyLegacyPass(log string, parsed *markers.Result) {
	explicit := parsed.PassedMarker || parsed.FailedMarker || parsed.MismatchCount != nil ||
		parsed.Samples != nil || strings.Contains(strings.ToUpper(log), "SIMULATION FAILED")
	if !explicit && markers.LegacyPassRe.MatchString(log) {
		parsed.PassedMarker = true
	}
}

func statusFor(passed bool) string {
	if passed {
		return "passed"
	}
	return "failed"
}

func orString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func truncateTail(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[len(value)-limit:]
}
