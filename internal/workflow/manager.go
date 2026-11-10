package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/asemoq/sv-multi-agent/internal/agents"
	"github.com/asemoq/sv-multi-agent/internal/config"
	eventbus "github.com/asemoq/sv-multi-agent/internal/events"
	"github.com/asemoq/sv-multi-agent/internal/llm"
	"github.com/asemoq/sv-multi-agent/internal/logx"
	"github.com/asemoq/sv-multi-agent/internal/store"
	"github.com/asemoq/sv-multi-agent/internal/sv/contract"
	"github.com/asemoq/sv-multi-agent/internal/sv/mutate"
	"github.com/asemoq/sv-multi-agent/internal/workspace"
)

// ragClient is the subset of rag.Service the workflow depends on. It lets tests
// substitute a stub without a database.
type ragClient interface {
	Available() bool
	Context(ctx context.Context, query string, kinds []string) (string, error)
	IndexTask(ctx context.Context, task *store.Task, contractData map[string]any, files map[string]string, simulation *agents.Result, fix *agents.Result) error
}

// Manager runs the multi-agent workflow for a submission.
//
// All agents run in-process; only the sv-multi-agent service reads and writes the
// workspace and owns the LLM connection.
type Manager struct {
	store *store.Store
	root  string
	llm   *llm.LLMClient
	cfg   *config.Config
	rag   ragClient
	hub   *eventbus.Hub
	// ctx is the service lifetime context; cancelling it stops in-flight
	// workflow runs (LLM calls, Verilator subprocesses).
	ctx context.Context
}

// New creates a workflow manager rooted at root. A nil ctx falls back to
// context.Background(); a nil hub disables event streaming.
func New(s *store.Store, root string, llmClient *llm.LLMClient, cfg *config.Config, ragService ragClient, ctx context.Context, hub *eventbus.Hub) (*Manager, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, cfg.WorkspacePerm); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &Manager{store: s, root: abs, llm: llmClient, cfg: cfg, rag: ragService, hub: hub, ctx: ctx}, nil
}

func (m *Manager) emitter(task *store.Task) *eventbus.Emitter {
	return m.hub.Emitter(task.ID)
}

// Submission is a task request. SourceMode is "generate" (default) or
// "analyze"; Files is used only by analyze mode and contains relative
// rtl/... and tb/... paths.
type Submission struct {
	Prompt        string
	MaxIterations int
	SourceMode    string
	DutTop        string
	Files         map[string]string
}

// Submit validates the prompt, creates a task and starts the workflow asynchronously.
func (m *Manager) Submit(prompt string, maxIterations int) (*store.Task, error) {
	return m.SubmitTask(Submission{Prompt: prompt, MaxIterations: maxIterations})
}

// SubmitTask creates a task from a full submission and starts it asynchronously.
func (m *Manager) SubmitTask(submission Submission) (*store.Task, error) {
	prompt := strings.TrimSpace(submission.Prompt)
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	maxIterations := submission.MaxIterations
	if maxIterations < 1 {
		maxIterations = 1
	}
	if maxIterations > 5 {
		maxIterations = 5
	}

	sourceMode := strings.TrimSpace(submission.SourceMode)
	if sourceMode == "" {
		sourceMode = "generate"
	}
	if sourceMode != "generate" && sourceMode != "analyze" {
		return nil, fmt.Errorf("source_mode must be generate or analyze")
	}
	files := submission.Files
	if sourceMode == "generate" && len(files) > 0 {
		return nil, fmt.Errorf("source files are only supported in analyze mode")
	}
	if sourceMode == "analyze" {
		if err := validateSourceFiles(files); err != nil {
			return nil, err
		}
	}

	id := newTaskID()
	workspacePath := filepath.Join(m.root, id)
	if m.cfg.AutoCreateWorkspaces {
		if err := os.MkdirAll(workspacePath, m.cfg.WorkspacePerm); err != nil {
			return nil, fmt.Errorf("create workspace: %w", err)
		}
	}

	now := time.Now().UTC()
	task := &store.Task{
		ID:            id,
		Prompt:        prompt,
		Workspace:     workspacePath,
		SourceMode:    sourceMode,
		DutTop:        strings.TrimSpace(submission.DutTop),
		Status:        "queued",
		Stage:         "queued",
		Iteration:     0,
		MaxIterations: maxIterations,
		CreatedAt:     now,
		UpdatedAt:     now,
		Steps:         []store.Step{},
		Artifacts:     []store.Artifact{},
	}
	if err := m.store.Save(task); err != nil {
		return nil, err
	}

	go m.run(task, files)

	// Return a consistent snapshot: run() mutates the live task concurrently,
	// so never hand the caller the live pointer.
	snapshot, ok := m.store.Get(id)
	if !ok {
		return nil, fmt.Errorf("task %s was not persisted", id)
	}
	return snapshot, nil
}

func validateSourceFiles(files map[string]string) error {
	if len(files) == 0 {
		return fmt.Errorf("analyze mode requires at least one RTL file")
	}
	hasRTL := false
	for path, content := range files {
		normalized := strings.ReplaceAll(strings.TrimSpace(path), "\\", "/")
		if !safeSourcePath(normalized) {
			return fmt.Errorf("unsafe source path %q", path)
		}
		if !isHDLPath(normalized) {
			return fmt.Errorf("unsupported source file %q (expected .sv, .svh or .sva)", path)
		}
		if !strings.HasPrefix(normalized, "rtl/") && !strings.HasPrefix(normalized, "tb/") {
			return fmt.Errorf("source path %q must be under rtl/ or tb/", path)
		}
		if strings.TrimSpace(content) == "" {
			return fmt.Errorf("source file %q is empty", path)
		}
		if strings.HasPrefix(normalized, "rtl/") && isCompilationPath(normalized) {
			hasRTL = true
		}
	}
	if !hasRTL {
		return fmt.Errorf("analyze mode requires at least one SystemVerilog compilation unit (.sv) under rtl/")
	}
	return nil
}

func safeSourcePath(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") {
		return false
	}
	if len(path) >= 2 && path[1] == ':' {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func isHDLPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".sv", ".svh", ".sva":
		return true
	default:
		return false
	}
}

func isCompilationPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".sv":
		return true
	default:
		return false
	}
}

func hasRTL(files map[string]string) bool {
	for path := range files {
		if strings.HasPrefix(path, "rtl/") && isCompilationPath(path) {
			return true
		}
	}
	return false
}

func hasTB(files map[string]string) bool {
	for path := range files {
		if strings.HasPrefix(path, "tb/") && isCompilationPath(path) {
			return true
		}
	}
	return false
}

func (m *Manager) run(task *store.Task, sourceFiles map[string]string) {
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	// Stable per-conversation session ID for gateway routing / prompt caching.
	ctx = llm.WithSession(ctx, task.ID)
	emitter := m.emitter(task)
	ctx = eventbus.WithEmitter(ctx, emitter)
	emitter.TaskStart(task.Prompt)
	logx.Infof("workflow: task=%s mode=%s max_iterations=%d prompt=%q", task.ID, task.SourceMode, task.MaxIterations, task.Prompt)
	var (
		simulation *agents.Result
		fix        *agents.Result
		passed     bool
		err        error
	)
	defer func() {
		m.indexTask(task, simulation, fix)
		m.writeReport(task, simulation, fix)
	}()

	if task.SourceMode == "analyze" {
		m.setStatus(task, "running", "ingest")
		if err := m.stepIngest(task, sourceFiles); err != nil {
			m.fail(task, "ingest", err)
			return
		}
	}

	m.setStatus(task, "running", "architect")
	files, err := m.filesMap(task)
	if err != nil {
		m.fail(task, "architect", err)
		return
	}
	if err := m.stepArchitect(ctx, task, files); err != nil {
		m.fail(task, "architect", err)
		return
	}

	var plan *agents.Plan
	if task.SourceMode != "analyze" && m.shouldDecompose(task) {
		plan, err = m.stepPlanner(ctx, task)
		if err != nil {
			m.fail(task, "planner", err)
			return
		}
	}

	if task.SourceMode == "analyze" {
		if !hasRTL(files) {
			m.fail(task, "coder", fmt.Errorf("analyze mode requires at least one RTL source under rtl/"))
			return
		}
	} else if err := m.stepCoder(ctx, task, plan); err != nil {
		m.fail(task, "coder", err)
		return
	}

	if err := m.stepLint(ctx, task); err != nil {
		m.fail(task, "lint", err)
		return
	}

	if task.SourceMode != "analyze" || !hasTB(files) {
		if err := m.stepVerifier(ctx, task); err != nil {
			m.fail(task, "verifier", err)
			return
		}
	}

	simulation, passed, err = m.stepSimulator(ctx, task)
	if err != nil {
		m.fail(task, "simulator", err)
		return
	}

	for iteration := 1; !passed && iteration <= task.MaxIterations; iteration++ {
		task.Iteration = iteration
		m.setStatus(task, "running", "debugger")
		fix, err = m.stepDebugger(ctx, task, simulation)
		if err != nil {
			m.fail(task, "debugger", err)
			return
		}
		simulation, passed, err = m.stepSimulator(ctx, task)
		if err != nil {
			m.fail(task, "simulator", err)
			return
		}
	}

	if passed {
		simulation, passed, err = m.runCoverageLoop(ctx, task, simulation)
		if err != nil {
			m.fail(task, "coverage", err)
			return
		}
	}

	if passed {
		m.stepMutation(ctx, task)
	}

	if passed {
		summary := simulation.Summary
		if strings.TrimSpace(summary) == "" {
			summary = "Simulation passed."
		}
		m.complete(task, summary)
		return
	}
	m.fail(task, "simulation", fmt.Errorf("simulation still failing after %d debug iteration(s)", task.MaxIterations))
}

func (m *Manager) stepIngest(task *store.Task, files map[string]string) error {
	index := m.beginStep(task, "ingest")
	if len(files) == 0 {
		err := fmt.Errorf("analyze mode requires source files")
		m.finishStep(task, index, "failed", err.Error())
		return err
	}

	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		normalized := strings.ReplaceAll(path, "\\", "/")
		if err := writeArtifact(task.Workspace, normalized, files[path]); err != nil {
			m.finishStep(task, index, "failed", err.Error())
			return err
		}
		task.Artifacts = appendArtifact(task.Artifacts, store.Artifact{Path: normalized, Role: "import"})
	}
	m.finishStep(task, index, "completed", fmt.Sprintf("Imported %d source file(s).", len(paths)))
	return m.store.Save(task)
}

func (m *Manager) stepArchitect(ctx context.Context, task *store.Task, files map[string]string) error {
	index := m.beginStep(task, "architect")
	opts := m.agentOptions()
	contextText, err := m.ragContext(ctx, task.Prompt, []string{"design", "docs"})
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return err
	}
	opts.Context = contextText
	if task.SourceMode == "analyze" {
		opts.SourcesContext = agents.SourceBundle(files, opts.SourceBudget)
	}
	instruction := "Produce a complete, lint-clean design contract for the user request."
	if task.SourceMode == "analyze" {
		instruction = "Derive a complete, lint-clean design contract that describes the existing RTL and the user specification."
	}
	result := agents.Architect(ctx, m.llm, opts, task.Prompt, instruction)
	if result.Status == "failed" {
		m.finishStep(task, index, "failed", result.Summary)
		return fmt.Errorf("architect step failed: %s", result.Summary)
	}
	task.ContractJSON = result.ContractJSON
	logx.Debugf("workflow: contract:\n%s", result.ContractJSON)
	if err := writeArtifact(task.Workspace, "design/contract.json", result.ContractJSON); err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return err
	}
	task.Artifacts = appendArtifact(task.Artifacts, store.Artifact{Path: "design/contract.json", Role: "architect"})
	m.finishStep(task, index, "completed", result.Summary)
	return m.store.Save(task)
}

func (m *Manager) shouldDecompose(task *store.Task) bool {
	if !m.cfg.DecomposeEnabled {
		return false
	}
	contractData, err := m.contractMap(task)
	if err != nil {
		return false
	}
	return contract.ShouldDecompose(contractData, m.cfg.DecomposeMinLines)
}

func (m *Manager) stepPlanner(ctx context.Context, task *store.Task) (*agents.Plan, error) {
	index := m.beginStep(task, "planner")
	contractData, err := m.contractMap(task)
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return nil, err
	}
	opts := m.agentOptions()
	contextText, err := m.ragContext(ctx, contractQuery(contractData), []string{"design", "docs"})
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return nil, err
	}
	opts.Context = contextText
	result := agents.Planner(ctx, m.llm, opts, contractData,
		"Decompose the contract into cohesive sub-modules and a top-level wiring plan.")
	if result.Status == "failed" || result.Plan == nil {
		m.finishStep(task, index, "failed", result.Summary)
		return nil, fmt.Errorf("planner step failed: %s", result.Summary)
	}
	data, err := json.MarshalIndent(result.Plan, "", "  ")
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return nil, err
	}
	logx.Debugf("workflow: plan:\n%s", string(data))
	if err := writeArtifact(task.Workspace, "design/plan.json", string(data)+"\n"); err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return nil, err
	}
	task.Artifacts = appendArtifact(task.Artifacts, store.Artifact{Path: "design/plan.json", Role: "planner"})
	m.finishStep(task, index, "completed", result.Summary)
	if err := m.store.Save(task); err != nil {
		return nil, err
	}
	return result.Plan, nil
}

func (m *Manager) stepCoder(ctx context.Context, task *store.Task, plan *agents.Plan) error {
	index := m.beginStep(task, "coder")
	contractData, err := m.contractMap(task)
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return err
	}
	files, err := m.filesMap(task)
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return err
	}
	opts := m.agentOptions()
	contextText, err := m.ragContext(ctx, contractQuery(contractData), []string{"rtl", "docs"})
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return err
	}
	opts.Context = contextText

	if plan == nil || len(plan.Modules) == 0 {
		result := agents.Coder(ctx, m.llm, opts, contractData, files,
			"Implement the design contract as synthesizable SystemVerilog under rtl/.")
		if result.Status == "failed" {
			m.finishStep(task, index, "failed", result.Summary)
			return fmt.Errorf("coder step failed: %s", result.Summary)
		}
		if err := m.applyResultArtifacts(task, result, "coder"); err != nil {
			m.finishStep(task, index, "failed", err.Error())
			return err
		}
		m.finishStep(task, index, "completed", result.Summary)
		return nil
	}

	moduleOpts := opts
	moduleOpts.Plan = nil
	results := make([]*agents.Result, len(plan.Modules))
	workers := m.cfg.CoderConcurrency
	if workers < 1 {
		workers = 1
	}
	if workers > len(plan.Modules) {
		workers = len(plan.Modules)
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for i, module := range plan.Modules {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, module agents.PlanModule) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = agents.Coder(ctx, m.llm, moduleOpts, module.Contract, files,
				"Implement sub-module "+module.Name+" exactly as its contract specifies in rtl/"+module.Name+".sv.")
		}(i, module)
	}
	wg.Wait()

	summaries := []string{}
	for i, module := range plan.Modules {
		result := results[i]
		if result == nil {
			result = &agents.Result{Status: "failed", Summary: "coder produced no result"}
		}
		if result.Status == "failed" {
			m.finishStep(task, index, "failed", result.Summary)
			return fmt.Errorf("coder step failed for %s: %s", module.Name, result.Summary)
		}
		if err := m.applyResultArtifacts(task, result, "coder"); err != nil {
			m.finishStep(task, index, "failed", err.Error())
			return err
		}
		summaries = append(summaries, module.Name+": "+result.Summary)
	}
	if refreshed, err := m.filesMap(task); err == nil {
		files = refreshed
	}

	opts.Plan = plan
	result := agents.Coder(ctx, m.llm, opts, contractData, files,
		"Implement the top-level module that instantiates the sub-modules from the hierarchy plan.")
	if result.Status == "failed" {
		m.finishStep(task, index, "failed", result.Summary)
		return fmt.Errorf("coder step failed for the top-level module: %s", result.Summary)
	}
	if err := m.applyResultArtifacts(task, result, "coder"); err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return err
	}
	summaries = append(summaries, "top: "+result.Summary)
	m.finishStep(task, index, "completed", strings.Join(summaries, "; "))
	return nil
}

func (m *Manager) stepLint(ctx context.Context, task *store.Task) error {
	if !m.cfg.LintEnabled {
		return nil
	}
	index := m.beginStep(task, "lint")
	files, err := m.filesMap(task)
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return err
	}
	opts := m.agentOptions()
	opts.DutTop = task.DutTop
	result := agents.Lint(ctx, opts, files)
	if err := m.applyResultArtifacts(task, result, "lint"); err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return err
	}
	for _, issue := range result.Issues {
		task.Issues = append(task.Issues, map[string]any{
			"severity": issue.Severity,
			"code":     issue.Code,
			"message":  issue.Message,
		})
	}
	if result.Status == "failed" {
		m.finishStep(task, index, "failed", result.Summary)
		return fmt.Errorf("lint step failed: %s", result.Summary)
	}
	m.finishStep(task, index, "completed", result.Summary)
	return nil
}

func (m *Manager) stepVerifier(ctx context.Context, task *store.Task) error {
	index := m.beginStep(task, "verifier")
	contractData, err := m.contractMap(task)
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return err
	}
	files, err := m.filesMap(task)
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return err
	}
	query := contractQuery(contractData)
	for path := range files {
		if strings.HasPrefix(path, "rtl/") {
			query += "\n" + path
		}
	}
	opts := m.agentOptions()
	contextText, err := m.ragContext(ctx, query, []string{"testbench", "docs"})
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return err
	}
	opts.Context = contextText
	result := agents.Verifier(ctx, m.llm, opts, contractData, files,
		"Create a self-checking testbench under tb/ that prints the simulation markers.")
	if result.Status == "failed" {
		m.finishStep(task, index, "failed", result.Summary)
		return fmt.Errorf("verifier step failed: %s", result.Summary)
	}
	if err := m.applyResultArtifacts(task, result, "verifier"); err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return err
	}
	m.finishStep(task, index, "completed", result.Summary)
	return nil
}

func (m *Manager) stepSimulator(ctx context.Context, task *store.Task) (*agents.Result, bool, error) {
	index := m.beginStep(task, "simulator")
	contractData, err := m.contractMap(task)
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return nil, false, err
	}
	files, err := m.filesMap(task)
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return nil, false, err
	}
	result := agents.Simulator(ctx, m.agentOptions(), files, contractData)
	m.emitter(task).Simulator(result.Summary)
	if err := m.applyResultArtifacts(task, result, "simulator"); err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return result, false, err
	}
	for _, issue := range result.Issues {
		task.Issues = append(task.Issues, map[string]any{
			"severity": issue.Severity,
			"code":     issue.Code,
			"message":  issue.Message,
		})
	}
	if result.Passed || result.Status == "passed" {
		m.finishStep(task, index, "completed", result.Summary)
		return result, true, nil
	}
	m.finishStep(task, index, "failed", result.Summary)
	return result, false, nil
}

func (m *Manager) runCoverageLoop(ctx context.Context, task *store.Task, simulation *agents.Result) (*agents.Result, bool, error) {
	if simulation == nil {
		return nil, false, nil
	}
	if !m.cfg.CoverageEnabled || m.cfg.LLMMode == "mock" || m.cfg.CoverageRounds <= 0 || !simulation.Passed {
		return simulation, simulation.Passed, nil
	}

	for round := 1; round <= m.cfg.CoverageRounds; round++ {
		if simulation.Coverage == nil {
			break
		}
		rtlCoverage := simulation.Coverage.ForPrefix("rtl/")
		if rtlCoverage.LinesTotal == 0 || rtlCoverage.LineCoverage >= m.cfg.CoverageMin {
			break
		}
		index := m.beginStep(task, "coverage")
		contractData, err := m.contractMap(task)
		if err != nil {
			m.finishStep(task, index, "failed", err.Error())
			return simulation, simulation.Passed, err
		}
		files, err := m.filesMap(task)
		if err != nil {
			m.finishStep(task, index, "failed", err.Error())
			return simulation, simulation.Passed, err
		}
		instruction := fmt.Sprintf(
			"Extend the existing testbench to cover the uncovered RTL lines below. Keep all existing checks, mismatch counting and the exact simulation markers intact. Do not shorten or rewrite existing tests.\n\nUncovered lines:\n%s",
			rtlCoverage.UncoveredText(40),
		)
		tbBackup := map[string]string{}
		for path, content := range files {
			if strings.HasPrefix(path, "tb/") {
				tbBackup[path] = content
			}
		}
		result := agents.Verifier(ctx, m.llm, m.agentOptions(), contractData, files, instruction)
		if result.Status == "failed" {
			m.finishStep(task, index, "failed", result.Summary)
			task.Issues = append(task.Issues, map[string]any{
				"severity": "warning",
				"code":     "coverage.verifier",
				"message":  result.Summary,
			})
			_ = m.store.Save(task)
			break
		}
		if err := m.applyResultArtifacts(task, result, "coverage"); err != nil {
			m.finishStep(task, index, "failed", err.Error())
			return simulation, simulation.Passed, err
		}
		m.finishStep(task, index, "completed", result.Summary)

		next, passed, simErr := m.stepSimulator(ctx, task)
		if simErr != nil {
			return next, false, simErr
		}
		if !passed {
			// The augmented testbench regressed; restore the original one.
			for path, content := range tbBackup {
				_ = writeArtifact(task.Workspace, path, content)
			}
			task.Issues = append(task.Issues, map[string]any{
				"severity": "warning",
				"code":     "coverage.regression",
				"message":  "The coverage-augmented testbench failed simulation; the original testbench was restored.",
			})
			_ = m.store.Save(task)
			break
		}
		simulation = next
	}

	if simulation.Coverage != nil {
		rtlCoverage := simulation.Coverage.ForPrefix("rtl/")
		if rtlCoverage.LinesTotal > 0 && rtlCoverage.LineCoverage < m.cfg.CoverageMin {
			task.Issues = append(task.Issues, map[string]any{
				"severity": "warning",
				"code":     "coverage.low",
				"message": fmt.Sprintf("RTL line coverage %.1f%% is below the %.1f%% target",
					rtlCoverage.LineCoverage*100, m.cfg.CoverageMin*100),
			})
			_ = m.store.Save(task)
		}
	}
	return simulation, simulation.Passed, nil
}

// stepMutation measures testbench quality by injecting mutants and checking
// whether the simulation still passes. It is best-effort and never fails the
// task: a passing simulation stays completed with a mutation.weak warning.
func (m *Manager) stepMutation(ctx context.Context, task *store.Task) {
	if !m.cfg.MutationEnabled || m.cfg.LLMMode == "mock" {
		return
	}
	index := m.beginStep(task, "mutation")
	if _, err := exec.LookPath("verilator"); err != nil {
		m.finishStep(task, index, "failed", "Verilator is not installed on PATH")
		return
	}
	contractData, err := m.contractMap(task)
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return
	}
	files, err := m.filesMap(task)
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return
	}

	mutants := mutate.Generate(files, m.cfg.MutationBudget, fnvHash(task.ID))
	if len(mutants) == 0 {
		m.finishStep(task, index, "completed", "No mutation sites found.")
		return
	}

	opts := m.agentOptions()
	report := mutate.RunWithWorkers(mutants, m.cfg.MutationConcurrency, func(mutated map[string]string) bool {
		return agents.Simulator(ctx, opts, mutated, contractData).Passed
	})

	summary := fmt.Sprintf("Mutation score %.0f%%: %d/%d mutants killed.",
		report.Score*100, report.Killed, report.Total)

	if data, err := json.MarshalIndent(report, "", "  "); err == nil {
		_ = writeArtifact(task.Workspace, "reports/mutation.json", string(data))
		task.Artifacts = appendArtifact(task.Artifacts, store.Artifact{Path: "reports/mutation.json", Role: "mutation"})
	}
	_ = writeArtifact(task.Workspace, "reports/mutation.md", buildMutationMarkdown(report))
	task.Artifacts = appendArtifact(task.Artifacts, store.Artifact{Path: "reports/mutation.md", Role: "mutation"})
	_ = m.store.Save(task)

	if len(report.Survived) > 0 {
		task.Issues = append(task.Issues, map[string]any{
			"severity": "warning",
			"code":     "mutation.weak",
			"message":  fmt.Sprintf("%d mutant(s) survived: the testbench misses some bugs", len(report.Survived)),
		})
		_ = m.store.Save(task)
		if m.cfg.MutationRounds > 0 {
			m.augmentForSurvivors(ctx, task, contractData, report)
		}
	}
	m.finishStep(task, index, "completed", summary)
}

// augmentForSurvivors asks the Verifier to add tests that detect surviving
// mutants, then re-runs the simulation. If the new testbench regresses, the
// original testbench files are restored.
func (m *Manager) augmentForSurvivors(ctx context.Context, task *store.Task, contractData map[string]any, report mutate.Report) {
	files, err := m.filesMap(task)
	if err != nil {
		return
	}
	tbBackup := map[string]string{}
	for path, content := range files {
		if strings.HasPrefix(path, "tb/") {
			tbBackup[path] = content
		}
	}

	instruction := "Add tests that detect the following mutation survivors. Keep all existing checks, mismatch counting and the exact simulation markers intact.\n\n" +
		report.SurvivorText(20)
	result := agents.Verifier(ctx, m.llm, m.agentOptions(), contractData, files, instruction)
	if result.Status == "failed" {
		task.Issues = append(task.Issues, map[string]any{
			"severity": "warning",
			"code":     "mutation.verifier",
			"message":  result.Summary,
		})
		_ = m.store.Save(task)
		return
	}
	if err := m.applyResultArtifacts(task, result, "mutation"); err != nil {
		return
	}

	refreshed, err := m.filesMap(task)
	if err != nil {
		return
	}
	sim := agents.Simulator(ctx, m.agentOptions(), refreshed, contractData)
	if !sim.Passed {
		for path, content := range tbBackup {
			_ = writeArtifact(task.Workspace, path, content)
		}
		task.Issues = append(task.Issues, map[string]any{
			"severity": "warning",
			"code":     "mutation.regression",
			"message":  "The augmented testbench failed simulation; the original testbench was restored.",
		})
		_ = m.store.Save(task)
	}
}

func buildMutationMarkdown(report mutate.Report) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Mutation report\n\n")
	fmt.Fprintf(&builder, "- score: %.0f%%\n", report.Score*100)
	fmt.Fprintf(&builder, "- killed: %d\n", report.Killed)
	fmt.Fprintf(&builder, "- survived: %d\n", len(report.Survived))
	if len(report.Survived) > 0 {
		builder.WriteString("\n## Survivors\n\n")
		builder.WriteString(report.SurvivorText(0))
	}
	return builder.String()
}

func fnvHash(value string) int64 {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(value))
	return int64(hasher.Sum64())
}

func (m *Manager) stepDebugger(ctx context.Context, task *store.Task, simulation *agents.Result) (*agents.Result, error) {
	index := m.beginStep(task, "debugger")
	contractData, err := m.contractMap(task)
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return nil, err
	}
	files, err := m.filesMap(task)
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return nil, err
	}
	opts := m.agentOptions()
	contextText, err := m.ragContext(ctx, failureQuery(simulation), []string{"failure", "fix"})
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return nil, err
	}
	opts.Context = contextText
	result := agents.Debugger(ctx, m.llm, opts, contractData, files, simulation,
		"Diagnose the failing simulation and return the smallest valid RTL fix.")
	if result.Status == "failed" {
		m.finishStep(task, index, "failed", result.Summary)
		return result, fmt.Errorf("debugger step failed: %s", result.Summary)
	}
	if err := m.applyResultArtifacts(task, result, "debugger"); err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return result, err
	}
	m.finishStep(task, index, "completed", result.Summary)
	return result, nil
}

func (m *Manager) agentOptions() agents.Options {
	return agents.Options{
		Mode:                  m.cfg.LLMMode,
		MaxRevisions:          m.cfg.ArchitectMaxRevisions,
		ContractOnly:          m.cfg.ContractOnly,
		TopModule:             m.cfg.TopModule,
		VerilatorBuildTimeout: m.cfg.VerilatorBuildTimeout,
		VerilatorRunTimeout:   m.cfg.VerilatorRunTimeout,
		TraceWindowEdges:      m.cfg.TraceWindowEdges,
		LintEnabled:           m.cfg.LintEnabled,
		LintFailOnError:       m.cfg.LintFailOnError,
		LintErrorOn:           m.cfg.LintErrorOn,
		CoverageEnabled:       m.cfg.CoverageEnabled,
		CoverageMin:           m.cfg.CoverageMin,
		CoverageRounds:        m.cfg.CoverageRounds,
		SourceBudget:          m.cfg.MaxSourceChars,
	}
}

func (m *Manager) ragContext(ctx context.Context, query string, kinds []string) (string, error) {
	if strings.TrimSpace(query) == "" {
		return "", nil
	}
	if m.rag == nil || !m.rag.Available() {
		return "", fmt.Errorf("RAG is unavailable")
	}
	ragCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	text, err := m.rag.Context(ragCtx, query, kinds)
	if err != nil {
		return "", fmt.Errorf("RAG retrieval failed: %w", err)
	}
	return text, nil
}

func (m *Manager) indexTask(task *store.Task, simulation *agents.Result, fix *agents.Result) {
	if m.rag == nil || !m.cfg.RAGIndexTasks || !m.rag.Available() {
		return
	}
	contractData, err := m.contractMap(task)
	if err != nil {
		contractData = map[string]any{}
	}
	files, err := m.filesMap(task)
	if err != nil {
		log.Printf("rag: bundle files for %s: %v", task.ID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := m.rag.IndexTask(ctx, task, contractData, files, simulation, fix); err != nil {
		log.Printf("rag: index task %s: %v", task.ID, err)
		task.Issues = append(task.Issues, map[string]any{
			"severity": "error",
			"code":     "rag.index",
			"message":  err.Error(),
		})
		_ = m.store.Save(task)
	}
}

func (m *Manager) contractMap(task *store.Task) (map[string]any, error) {
	return contract.Parse(map[string]any{"contract_json": task.ContractJSON})
}

func (m *Manager) filesMap(task *store.Task) (map[string]string, error) {
	entries, err := workspace.BundleForWorker(task)
	if err != nil {
		return nil, fmt.Errorf("bundle files: %w", err)
	}
	files := make(map[string]string, len(entries))
	for _, entry := range entries {
		files[entry.Path] = entry.Content
	}
	return files, nil
}

func (m *Manager) applyResultArtifacts(task *store.Task, result *agents.Result, role string) error {
	for _, artifact := range result.Artifacts {
		if artifact.Path == "" {
			continue
		}
		if err := writeArtifact(task.Workspace, artifact.Path, artifact.Content); err != nil {
			return err
		}
		logx.Infof("workflow: wrote %s (%d bytes) role=%s", artifact.Path, len(artifact.Content), role)
		m.emitter(task).Artifact(artifact.Path)
		task.Artifacts = appendArtifact(task.Artifacts, store.Artifact{Path: artifact.Path, Role: role})
	}
	return m.store.Save(task)
}

func (m *Manager) beginStep(task *store.Task, name string) int {
	task.Stage = name
	task.Steps = append(task.Steps, store.Step{
		Name:      name,
		Status:    "running",
		StartedAt: time.Now().UTC(),
	})
	task.UpdatedAt = time.Now().UTC()
	_ = m.store.Save(task)
	logx.Infof("workflow: step %s started", name)
	m.emitter(task).StepStart(name)
	return len(task.Steps) - 1
}

func (m *Manager) finishStep(task *store.Task, index int, status, summary string) {
	if index < 0 || index >= len(task.Steps) {
		return
	}
	task.Steps[index].Status = status
	task.Steps[index].Summary = summary
	task.Steps[index].EndedAt = time.Now().UTC()
	task.UpdatedAt = time.Now().UTC()
	_ = m.store.Save(task)
	logx.Infof("workflow: step %s %s: %s", task.Steps[index].Name, status, summary)
	m.emitter(task).StepEnd(task.Steps[index].Name, status, summary)
}

func (m *Manager) setStatus(task *store.Task, status, stage string) {
	task.Status = status
	task.Stage = stage
	task.UpdatedAt = time.Now().UTC()
	_ = m.store.Save(task)
}

func (m *Manager) complete(task *store.Task, summary string) {
	task.Status = "completed"
	task.Stage = "done"
	task.Summary = summary
	task.UpdatedAt = time.Now().UTC()
	_ = m.store.Save(task)
	logx.Infof("workflow: task %s completed: %s", task.ID, summary)
	m.emitter(task).Done("completed", summary)
}

func (m *Manager) fail(task *store.Task, stage string, err error) {
	task.Status = "failed"
	task.Stage = stage
	task.Summary = err.Error()
	task.Issues = append(task.Issues, map[string]any{
		"severity": "error",
		"code":     "workflow." + stage,
		"message":  err.Error(),
	})
	task.UpdatedAt = time.Now().UTC()
	_ = m.store.Save(task)
	logx.Infof("workflow: task %s failed at %s: %v", task.ID, stage, err)
	m.emitter(task).Done("failed", err.Error())
}

func writeArtifact(workspaceDir, relativePath, content string) error {
	clean := filepath.Clean(filepath.FromSlash(relativePath))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("unsafe artifact path %q", relativePath)
	}
	target := filepath.Join(workspaceDir, clean)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, []byte(content), 0o644)
}

func appendArtifact(artifacts []store.Artifact, artifact store.Artifact) []store.Artifact {
	for i := range artifacts {
		if artifacts[i].Path == artifact.Path {
			artifacts[i].Role = artifact.Role
			return artifacts
		}
	}
	return append(artifacts, artifact)
}

func newTaskID() string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("task-%d-%s", time.Now().UTC().Unix(), hex.EncodeToString(buf[:]))
}

func contractQuery(contractData map[string]any) string {
	module := stringValue(contractData["module_name"])
	summary := stringValue(contractData["functional_summary"])
	return strings.TrimSpace(module + "\n" + summary)
}

func failureQuery(simulation *agents.Result) string {
	if simulation == nil {
		return ""
	}
	parts := []string{simulation.Summary}
	for _, output := range simulation.FailOutputs {
		parts = append(parts, fmt.Sprintf("%s: %d mismatches at time %d", output.Signal, output.Mismatches, output.Time))
	}
	return strings.Join(parts, "\n")
}

func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}
