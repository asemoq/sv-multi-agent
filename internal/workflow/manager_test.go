package workflow

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/asemoq/sv-multi-agent/internal/config"
	"github.com/asemoq/sv-multi-agent/internal/store"
)

func TestRAGContextRequiresService(t *testing.T) {
	manager := &Manager{cfg: &config.Config{}}
	if _, err := manager.ragContext(context.Background(), "counter", nil); err == nil {
		t.Fatal("expected RAG unavailable error")
	}
}

func TestRAGContextEmptyQuery(t *testing.T) {
	manager := &Manager{cfg: &config.Config{}}
	text, err := manager.ragContext(context.Background(), "  ", nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if text != "" {
		t.Fatalf("text = %q", text)
	}
}

func TestShouldDecomposeRespectsConfig(t *testing.T) {
	io := make([]any, 0, 60)
	for index := 0; index < 60; index++ {
		io = append(io, map[string]any{"name": "sig_ports_" + string(rune('a'+index%26)), "dir": "input", "width": 8})
	}
	data, err := json.Marshal(map[string]any{
		"module_name":        "dsp_pipeline",
		"io":                 io,
		"functional_summary": "Multi-stage pipeline with many independent channels.",
		"corner_cases":       []any{"stall", "flush", "backpressure"},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := &store.Task{ContractJSON: string(data)}

	manager := &Manager{cfg: &config.Config{DecomposeEnabled: false, DecomposeMinLines: 400}}
	if manager.shouldDecompose(task) {
		t.Fatal("decomposition should be disabled")
	}

	manager.cfg.DecomposeEnabled = true
	if !manager.shouldDecompose(task) {
		t.Fatal("large contract should be decomposed")
	}

	manager.cfg.DecomposeMinLines = 100000
	if manager.shouldDecompose(task) {
		t.Fatal("threshold should prevent decomposition")
	}
}

func TestShouldDecomposeSmallContract(t *testing.T) {
	task := &store.Task{ContractJSON: `{
		"module_name": "counter",
		"parameters": {"WIDTH": 8},
		"io": [
			{"name": "clk", "dir": "input", "width": 1},
			{"name": "rst", "dir": "input", "width": 1},
			{"name": "en", "dir": "input", "width": 1},
			{"name": "count", "dir": "output", "width": "WIDTH"}
		],
		"functional_summary": "Increment while enabled.",
		"corner_cases": ["wrap-around"]
	}`}
	manager := &Manager{cfg: &config.Config{DecomposeEnabled: true, DecomposeMinLines: 400}}
	if manager.shouldDecompose(task) {
		t.Fatal("counter should not be decomposed")
	}
}
