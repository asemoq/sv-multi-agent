package agents

import (
	"embed"
	"strings"
)

// System prompts live in ./prompts/*.md so they can be reviewed and edited as
// text rather than as Go string concatenation. go:embed keeps them inside the
// single binary, so deployments do not need to ship a separate prompt folder.
//
//go:embed prompts/*.md
var promptFiles embed.FS

func loadPrompt(name string) string {
	data, err := promptFiles.ReadFile("prompts/" + name)
	if err != nil {
		panic("agents: embedded prompt " + name + " is missing: " + err.Error())
	}
	return strings.TrimSpace(string(data))
}

// systemPrompt appends few-shot examples to a base system prompt.
func systemPrompt(base string, examples ...string) string {
	parts := []string{strings.TrimSpace(base)}
	for _, example := range examples {
		if trimmed := strings.TrimSpace(example); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, "\n\n")
}

var (
	architectSystemPrompt   = loadPrompt("architect.md")
	architectRevisionPrompt = loadPrompt("architect_revision.md")
	coderSystemPrompt       = loadPrompt("coder.md")
	coderExamples           = loadPrompt("coder_examples.md")
	verifierSystemPrompt    = loadPrompt("verifier.md")
	verifierExamples        = loadPrompt("verifier_examples.md")
	debuggerSystemPrompt    = loadPrompt("debugger.md")
	debuggerExamples        = loadPrompt("debugger_examples.md")
	plannerSystemPrompt     = loadPrompt("planner.md")
	plannerRevisionPrompt   = loadPrompt("planner_revision.md")
	refactorSystemPrompt    = loadPrompt("refactor.md")
	refactorExamples        = loadPrompt("refactor_examples.md")
	bugHunterSystemPrompt   = loadPrompt("bughunter.md")
	bugHunterExamples       = loadPrompt("bughunter_examples.md")
)
