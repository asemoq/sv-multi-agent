// Package api implements the sv-multi-agent HTTP/JSON control plane used by the
// CLI (svma/svctl) and by optional external tools. It replaces the former gRPC
// + protobuf transport.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/asemoq/sv-multi-agent/internal/events"
	"github.com/asemoq/sv-multi-agent/internal/llm"
	"github.com/asemoq/sv-multi-agent/internal/mcp"
	"github.com/asemoq/sv-multi-agent/internal/store"
	"github.com/asemoq/sv-multi-agent/internal/workflow"
)

// Handler is the HTTP control plane for the sv-multi-agent service.
type Handler struct {
	manager *workflow.Manager
	store   *store.Store
	llm     *llm.LLMClient
	mcp     *mcp.MCPManager
	hub     *events.Hub
}

// NewHandler builds the control-plane handler. llmClient and mcpManager are
// optional: when nil the corresponding proxy routes are not registered.
func NewHandler(manager *workflow.Manager, taskStore *store.Store, llmClient *llm.LLMClient, mcpManager *mcp.MCPManager, hub *events.Hub) *Handler {
	return &Handler{manager: manager, store: taskStore, llm: llmClient, mcp: mcpManager, hub: hub}
}

// Routes returns the HTTP mux for the control plane.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.health)
	mux.HandleFunc("POST /v1/tasks", h.submitTask)
	mux.HandleFunc("GET /v1/tasks", h.listTasks)
	mux.HandleFunc("GET /v1/tasks/{id}", h.getTask)
	mux.HandleFunc("GET /v1/tasks/{id}/report", h.getReport)
	mux.HandleFunc("GET /v1/tasks/{id}/events", h.taskEvents)
	if h.llm != nil {
		mux.HandleFunc("POST /v1/llm/chat", h.chatJSON)
	}
	if h.mcp != nil {
		mux.HandleFunc("GET /v1/mcp/tools", h.listTools)
		mux.HandleFunc("POST /v1/mcp/call", h.callTool)
	}
	return mux
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (h *Handler) submitTask(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Prompt        string            `json:"prompt"`
		MaxIterations int               `json:"max_iterations"`
		SourceMode    string            `json:"source_mode"`
		DutTop        string            `json:"dut_top"`
		Files         map[string]string `json:"files"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if request.MaxIterations == 0 {
		request.MaxIterations = 2
	}
	task, err := h.manager.SubmitTask(workflow.Submission{
		Prompt:        request.Prompt,
		MaxIterations: request.MaxIterations,
		SourceMode:    request.SourceMode,
		DutTop:        request.DutTop,
		Files:         request.Files,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"task_id":     task.ID,
		"status":      task.Status,
		"stage":       task.Stage,
		"workspace":   task.Workspace,
		"source_mode": task.SourceMode,
	})
}

func (h *Handler) getTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("task_id is required"))
		return
	}
	task, ok := h.store.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("task %q not found", id))
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (h *Handler) getReport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("task_id is required"))
		return
	}
	task, ok := h.store.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("task %q not found", id))
		return
	}
	data, err := os.ReadFile(filepath.Join(task.Workspace, "reports", "report.md"))
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("report for task %q not found", id))
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handler) listTasks(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": h.store.List(limit)})
}

// taskEvents streams a task's events as Server-Sent Events.
func (h *Handler) taskEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, errors.New("task_id is required"))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming is not supported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	offset := 0
	for {
		batch, done, err := h.hub.Wait(r.Context(), id, offset)
		if err != nil {
			return
		}
		for _, event := range batch {
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			offset++
		}
		flusher.Flush()
		if done {
			return
		}
	}
}

func (h *Handler) chatJSON(w http.ResponseWriter, r *http.Request) {
	var request struct {
		SystemPrompt string `json:"system_prompt"`
		UserPrompt   string `json:"user_prompt"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	result, err := h.llm.ChatJSON(r.Context(), request.SystemPrompt, request.UserPrompt)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

func (h *Handler) listTools(w http.ResponseWriter, r *http.Request) {
	tools, err := h.mcp.ListTools(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": tools})
}

func (h *Handler) callTool(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	result, err := h.mcp.CallTool(r.Context(), request.Name, request.Args)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 32<<20))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}
