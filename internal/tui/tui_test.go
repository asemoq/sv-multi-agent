package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/asemoq/sv-multi-agent/internal/events"
	"github.com/asemoq/sv-multi-agent/internal/store"
)

func TestRenderEventStepEnd(t *testing.T) {
	ok := renderEvent(events.Event{Kind: events.KindStepEnd, Step: "coder", Status: "completed"})
	if !strings.Contains(ok, "coder") || !strings.Contains(ok, "completed") {
		t.Fatalf("completed step missing fields: %q", ok)
	}
	bad := renderEvent(events.Event{Kind: events.KindStepEnd, Step: "simulator", Status: "failed"})
	if !strings.Contains(bad, "simulator") || !strings.Contains(bad, "failed") {
		t.Fatalf("failed step missing fields: %q", bad)
	}
}

func TestRenderMarkdown(t *testing.T) {
	rendered := renderMarkdown("# Title\n\n- one\n- two\n\n## Section\n")
	for _, want := range []string{"Title", "one", "two", "Section"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered markdown missing %q:\n%s", want, rendered)
		}
	}
}

func TestViewSmoke(t *testing.T) {
	model := New("127.0.0.1:50050")
	model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	model.tasks = []store.Task{
		{
			ID:        "task-1",
			Status:    "running",
			Stage:     "coder",
			CreatedAt: time.Now(),
			Steps: []store.Step{
				{Name: "architect", Status: "completed", Summary: "contract ok"},
				{Name: "coder", Status: "running"},
			},
		},
		{ID: "task-2", Status: "completed", Stage: "done"},
	}
	model.cursor = 0
	model.selected = "task-1"
	model.events = []events.Event{
		{Kind: events.KindTaskStart, Text: "build a fifo"},
		{Kind: events.KindStepStart, Step: "architect"},
		{Kind: events.KindStepEnd, Step: "architect", Status: "completed"},
		{Kind: events.KindContent, Text: "writing rtl…"},
	}
	model.refreshViewport()

	view := model.View()
	if strings.TrimSpace(view) == "" {
		t.Fatal("view is empty")
	}
	for _, want := range []string{"sv-multi-agent", "Tasks", "task-1", "Live"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q", want)
		}
	}
}

func TestSelectTaskSubscribes(t *testing.T) {
	model := New("127.0.0.1:50050")
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	cmd := model.selectTask("task-x")
	if model.selected != "task-x" {
		t.Fatalf("selected = %q", model.selected)
	}
	if model.subID != "task-x" {
		t.Fatalf("subID = %q", model.subID)
	}
	if cmd == nil {
		t.Fatal("expected a subscription command")
	}
	if model.sseCancel != nil {
		model.sseCancel()
	}
}

func TestToggleMode(t *testing.T) {
	model := New("127.0.0.1:50050")
	model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	model.toggleMode()
	if model.mode != 1 || model.focus != 2 {
		t.Fatalf("after toggle: mode=%d focus=%d", model.mode, model.focus)
	}
	model.toggleMode()
	if model.mode != 0 || model.focus != 1 {
		t.Fatalf("after second toggle: mode=%d focus=%d", model.mode, model.focus)
	}
}

func TestCollectSources(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("top.sv", "module top; endmodule")
	write("sub/child.svh", "// child")
	write("notes.txt", "ignore me")
	write(".git/hidden.sv", "module hidden; endmodule")

	files, err := collectSources(dir, "rtl")
	if err != nil {
		t.Fatalf("collectSources: %v", err)
	}
	if _, ok := files["rtl/top.sv"]; !ok {
		t.Fatalf("missing rtl/top.sv: %v", keys(files))
	}
	if _, ok := files["rtl/sub/child.svh"]; !ok {
		t.Fatalf("missing rtl/sub/child.svh: %v", keys(files))
	}
	if _, ok := files["rtl/notes.txt"]; ok {
		t.Fatalf("non-HDL file was included: %v", keys(files))
	}
	if _, ok := files["rtl/.git/hidden.sv"]; ok {
		t.Fatalf("hidden directory was included: %v", keys(files))
	}
}

func TestCollectSourcesEmpty(t *testing.T) {
	if _, err := collectSources(t.TempDir(), "rtl"); err == nil {
		t.Fatal("expected an error for an empty directory")
	}
}

func TestSubmitAnalyzePayload(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tasks" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"task_id":"task-analyze-1"}`)
	}))
	defer server.Close()

	client := NewClient(server.URL)
	id, err := client.SubmitAnalyze(context.Background(), "check the fifo", 5, map[string]string{
		"rtl/fifo.sv": "module fifo; endmodule",
	})
	if err != nil {
		t.Fatalf("SubmitAnalyze: %v", err)
	}
	if id != "task-analyze-1" {
		t.Fatalf("id = %q", id)
	}
	if payload["source_mode"] != "analyze" {
		t.Fatalf("source_mode = %v", payload["source_mode"])
	}
	if payload["prompt"] != "check the fifo" {
		t.Fatalf("prompt = %v", payload["prompt"])
	}
	files, ok := payload["files"].(map[string]any)
	if !ok || files["rtl/fifo.sv"] != "module fifo; endmodule" {
		t.Fatalf("files = %v", payload["files"])
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}
