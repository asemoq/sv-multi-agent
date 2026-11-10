package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asemoq/sv-multi-agent/internal/agents"
	"github.com/asemoq/sv-multi-agent/internal/config"
	eventbus "github.com/asemoq/sv-multi-agent/internal/events"
	"github.com/asemoq/sv-multi-agent/internal/llm"
	"github.com/asemoq/sv-multi-agent/internal/logx"
	"github.com/asemoq/sv-multi-agent/internal/rag"
	"github.com/asemoq/sv-multi-agent/internal/store"
	"github.com/asemoq/sv-multi-agent/internal/sv/contract"
	"github.com/asemoq/sv-multi-agent/internal/workspace"
)

// Manager runs the multi-agent workflow for a submission.
//
// All agents run in-process; only the sv-multi-agent service reads and writes the
// workspace and owns the LLM connection.
type Manager struct {
	store *store.Store
	root  string
	llm   *llm.LLMClient
	cfg   *config.Config
	rag   *rag.Service
	hub   *eventbus.Hub
	// ctx is the service lifetime context; cancelling it stops in-flight
	// workflow runs (LLM calls, Verilator subprocesses).
	ctx context.Context
}

// New creates a workflow manager rooted at root. A nil ctx falls back to
// context.Background(); a nil hub disables event streaming.
func New(s *store.Store, root string, llmClient *llm.LLMClient, cfg *config.Config, ragService *rag.Service, ctx context.Context, hub *eventbus.Hub) (*Manager, error) {
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

// Submit validates the prompt, creates a task and starts the workflow asynchronously.
func (m *Manager) Submit(prompt string, maxIterations int) (*store.Task, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	if maxIterations < 1 {
		maxIterations = 1
	}
	if maxIterations > 5 {
		maxIterations = 5
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

	go m.run(task)

	// Return a consistent snapshot: run() mutates the live task concurrently,
	// so never hand the caller the live pointer.
	snapshot, ok := m.store.Get(id)
	if !ok {
		return nil, fmt.Errorf("task %s was not persisted", id)
	}
	return snapshot, nil
}

func (m *Manager) run(task *store.Task) {
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	// Stable per-conversation session ID for gateway routing / prompt caching.
	ctx = llm.WithSession(ctx, task.ID)
	emitter := m.emitter(task)
	ctx = eventbus.WithEmitter(ctx, emitter)
	emitter.TaskStart(task.Prompt)
	logx.Infof("workflow: task=%s max_iterations=%d prompt=%q", task.ID, task.MaxIterations, task.Prompt)
	var (
		simulation *agents.Result
		fix        *agents.Result
		passed     bool
		err        error
	)
	defer func() { m.indexTask(task, simulation, fix) }()

	m.setStatus(task, "running", "architect")

	if err := m.stepArchitect(ctx, task); err != nil {
		m.fail(task, "architect", err)
		return
	}

	var plan *agents.Plan
	if m.shouldDecompose(task) {
		plan, err = m.stepPlanner(ctx, task)
		if err != nil {
			m.fail(task, "planner", err)
			return
		}
	}

	if err := m.stepCoder(ctx, task, plan); err != nil {
		m.fail(task, "coder", err)
		return
	}
	if err := m.stepVerifier(ctx, task); err != nil {
		m.fail(task, "verifier", err)
		return
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
		summary := simulation.Summary
		if strings.TrimSpace(summary) == "" {
			summary = "Simulation passed."
		}
		m.complete(task, summary)
		return
	}
	m.fail(task, "simulation", fmt.Errorf("simulation still failing after %d debug iteration(s)", task.MaxIterations))
}

func (m *Manager) stepArchitect(ctx context.Context, task *store.Task) error {
	index := m.beginStep(task, "architect")
	opts := m.agentOptions()
	contextText, err := m.ragContext(ctx, task.Prompt, []string{"design", "docs"})
	if err != nil {
		m.finishStep(task, index, "failed", err.Error())
		return err
	}
	opts.Context = contextText
	result := agents.Architect(ctx, m.llm, opts, task.Prompt,
		"Produce a complete, lint-clean design contract for the user request.")
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

	summaries := []string{}
	for _, module := range plan.Modules {
		moduleOpts := opts
		moduleOpts.Plan = nil
		result := agents.Coder(ctx, m.llm, moduleOpts, module.Contract, files,
			"Implement sub-module "+module.Name+" exactly as its contract specifies in rtl/"+module.Name+".sv.")
		if result.Status == "failed" {
			m.finishStep(task, index, "failed", result.Summary)
			return fmt.Errorf("coder step failed for %s: %s", module.Name, result.Summary)
		}
		if err := m.applyResultArtifacts(task, result, "coder"); err != nil {
			m.finishStep(task, index, "failed", err.Error())
			return err
		}
		summaries = append(summaries, module.Name+": "+result.Summary)
		if refreshed, err := m.filesMap(task); err == nil {
			files = refreshed
		}
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
		SourceBudget:          m.cfg.MaxSourceChars,
	}
}

func (m *Manager) ragContext(ctx context.Context, query string, kinds []string) (string, error) {
	if strings.TrimSpace(query) == "" {
		return "", nil
	}
	if m.rag == nil || !m.rag.Available() {
		if m.cfg.RAGRequired {
			return "", fmt.Errorf("RAG is required but unavailable")
		}
		return "", nil
	}
	ragCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	text, err := m.rag.Context(ragCtx, query, kinds)
	if err != nil {
		if m.cfg.RAGRequired {
			return "", fmt.Errorf("RAG retrieval failed: %w", err)
		}
		log.Printf("rag: retrieve: %v", err)
		return "", nil
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
		if m.cfg.RAGRequired {
			task.Issues = append(task.Issues, map[string]any{
				"severity": "error",
				"code":     "rag.index",
				"message":  err.Error(),
			})
			_ = m.store.Save(task)
		}
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
