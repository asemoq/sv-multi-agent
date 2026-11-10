package workflow

import (
	"context"

	"github.com/asemoq/sv-multi-agent/internal/agents"
	"github.com/asemoq/sv-multi-agent/internal/store"
)

// fakeRAG is a stub ragClient for tests that don't exercise retrieval.
type fakeRAG struct{}

func (fakeRAG) Available() bool { return true }
func (fakeRAG) Context(context.Context, string, []string) (string, error) {
	return "", nil
}
func (fakeRAG) IndexTask(context.Context, *store.Task, map[string]any, map[string]string, *agents.Result, *agents.Result) error {
	return nil
}
