package agents

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/asemoq/sv-multi-agent/internal/llm"
	"github.com/asemoq/sv-multi-agent/internal/sv/contract"
)

var (
	counterRe = regexp.MustCompile(`(?i)counter`)
	widthRe   = regexp.MustCompile(`(?i)(\d+)\s*-?\s*bit`)
)

// Architect produces a lint-clean design contract.
func Architect(ctx context.Context, client *llm.LLMClient, opts Options, prompt, instruction string) *Result {
	if opts.Mode == "mock" {
		return mockArchitect(prompt)
	}

	userPrompt := "User request:\n" + prompt + "\n\n"
	if strings.TrimSpace(opts.SourcesContext) != "" {
		userPrompt += "Existing RTL sources (derive the contract from them):\n" + opts.SourcesContext + "\n\n"
	}
	userPrompt += "Task instruction:\n" + instruction
	userPrompt = promptWithContext(opts, userPrompt)
	data, err := client.ChatJSON(ctx, architectSystemPrompt, userPrompt)
	if err != nil {
		return failed(err.Error())
	}
	contractData := extractContract(data)
	issues := contract.Lint(contractData)

	revisions := 0
	for contract.HasErrors(issues) && revisions < opts.MaxRevisions {
		revisions++
		revisionPrompt := promptWithContext(opts, "User request:\n"+prompt+"\n\nCurrent contract:\n"+
			prettyJSON(contract.Normalize(contractData))+"\n\n"+contract.RenderIssues(issues))
		revision, err := client.ChatJSON(ctx, architectRevisionPrompt, revisionPrompt)
		if err != nil {
			return failed(err.Error())
		}
		if candidate := extractContract(revision); len(candidate) > 0 {
			contractData = candidate
		}
		issues = contract.Lint(contractData)
	}

	normalized := contract.Normalize(contractData)
	contractJSON := prettyJSON(normalized)
	if contract.HasErrors(issues) {
		result := failed("Design contract failed lint after " + strconv.Itoa(revisions) + " revision(s): " + firstError(issues))
		result.ContractJSON = contractJSON
		result.ContractIssues = issues
		result.ContractRevisions = revisions
		return result
	}
	summary := "Created design contract."
	if revisions > 0 {
		summary = "Created design contract after " + strconv.Itoa(revisions) + " lint-driven revision(s)."
	}
	result := completed(summary)
	result.ContractJSON = contractJSON
	result.ContractIssues = issues
	result.ContractRevisions = revisions
	return result
}

// Coder generates synthesizable SystemVerilog from the contract.
func Coder(ctx context.Context, client *llm.LLMClient, opts Options, contractData map[string]any, files map[string]string, instruction string) *Result {
	if opts.Mode == "mock" {
		if asString(contractData["module_name"]) != "counter" {
			return failed("Mock coder supports the counter example only")
		}
		width := contract.ParameterInt(contractData, "WIDTH", 8)
		result := completed("Generated synthesizable counter RTL.")
		result.Artifacts = []Artifact{{Path: "rtl/counter.sv", Content: mockCounterRTL(width)}}
		return result
	}

	payload := contract.Payload(contractData, map[string]any{"contract_only": opts.ContractOnly})
	userPrompt := promptWithContext(opts, "Contract:\n"+prettyJSON(payload)+
		"\n\nExisting source (if any):\n"+sourcesContext(files, opts.SourceBudget, nil)+
		planSection(opts.Plan)+
		"\n\nTask instruction:\n"+instruction)
	data, err := client.ChatJSON(ctx, systemPrompt(coderSystemPrompt, coderExamples), userPrompt)
	if err != nil {
		return failed(err.Error())
	}
	return filesResult(data, "rtl")
}

// Verifier generates a self-checking testbench.
func Verifier(ctx context.Context, client *llm.LLMClient, opts Options, contractData map[string]any, files map[string]string, instruction string) *Result {
	if opts.Mode == "mock" {
		if asString(contractData["module_name"]) != "counter" {
			return failed("Mock verifier supports the counter example only")
		}
		width := contract.ParameterInt(contractData, "WIDTH", 8)
		result := completed("Generated a self-checking testbench for reset, increment, hold and wrap behavior.")
		result.Artifacts = []Artifact{{Path: "tb/tb_counter.sv", Content: mockCounterTB(width)}}
		return result
	}

	payload := contract.Payload(contractData, map[string]any{"contract_only": opts.ContractOnly})
	userPrompt := promptWithContext(opts, "Contract:\n"+prettyJSON(payload)+
		"\n\nRTL source:\n"+sourcesContext(files, opts.SourceBudget, nil)+
		"\n\nTask instruction:\n"+instruction+
		"\n\nThe testbench top-level module MUST be named '"+topModuleName(opts)+"'.")
	data, err := client.ChatJSON(ctx, systemPrompt(verifierSystemPrompt, verifierExamples), userPrompt)
	if err != nil {
		return failed(err.Error())
	}
	return filesResult(data, "tb")
}

// Debugger diagnoses a failing simulation and applies block patches in memory.
func Debugger(ctx context.Context, client *llm.LLMClient, opts Options, contractData map[string]any, files map[string]string, simulation *Result, instruction string) *Result {
	if opts.Mode == "mock" {
		return failed("Mock debugger has no automatic patch strategy. Configure an LLM to enable repair iterations.")
	}

	traceReport := map[string]any{}
	if simulation != nil && simulation.TraceReport != nil {
		traceReport = simulation.TraceReport
	}
	payload := contract.Payload(contractData, map[string]any{"contract_only": opts.ContractOnly})
	prompt := promptWithContext(opts, "Contract:\n"+prettyJSON(payload)+
		"\n\nSimulation result:\n"+prettyJSON(simulationDigest(simulation))+
		"\n\nTrace report:\n"+prettyJSON(traceReport)+
		"\n\nAlignment diagnosis:\n"+prettyJSON(traceReport["alignment_diagnosis"])+
		"\n\nSuspect RTL blocks:\n"+prettyJSON(traceReport["suspect_blocks"])+
		"\n\nSources:\n"+sourcesContext(files, opts.SourceBudget, nil))

	data, err := client.ChatJSON(ctx, systemPrompt(debuggerSystemPrompt, debuggerExamples), prompt)
	if err != nil {
		return failed(err.Error())
	}

	patches, _ := data["patches"].([]any)
	if len(patches) > 0 {
		if artifacts := ApplyBlockPatches(files, patches); len(artifacts) > 0 {
			summary := asString(data["summary"])
			if strings.TrimSpace(summary) == "" {
				summary = "Patched " + strconv.Itoa(len(artifacts)) + " RTL file(s)."
			}
			result := completed(summary)
			result.Artifacts = artifacts
			return result
		}
		if data["files"] == nil && data["artifacts"] == nil {
			return failed("Debugger returned block patches that did not match any RTL block")
		}
	}
	return filesResult(data, "rtl")
}

func promptWithContext(opts Options, prompt string) string {
	if strings.TrimSpace(opts.Context) == "" {
		return prompt
	}
	return "Retrieved context (use only if relevant):\n" + opts.Context + "\n\n" + prompt
}

func mockArchitect(prompt string) *Result {
	if !counterRe.MatchString(prompt) {
		return failed("Mock mode supports the example counter task only. Configure an OpenAI-compatible LLM endpoint for arbitrary RTL tasks.")
	}
	width := 8
	if match := widthRe.FindStringSubmatch(prompt); match != nil {
		if parsed, err := strconv.Atoi(match[1]); err == nil {
			width = parsed
		}
	}
	if width < 1 || width > 64 {
		return failed("Mock counter width must be between 1 and 64 bits")
	}

	contractData := contract.Normalize(map[string]any{
		"source_of_truth": "spec",
		"module_name":     "counter",
		"parameters":      map[string]any{"WIDTH": width},
		"io": []any{
			map[string]any{"name": "clk", "dir": "input", "width": 1, "notes": "clock"},
			map[string]any{"name": "rst", "dir": "input", "width": 1, "notes": "synchronous reset"},
			map[string]any{"name": "en", "dir": "input", "width": 1, "notes": "count enable"},
			map[string]any{"name": "count", "dir": "output", "width": "WIDTH", "notes": "counter value"},
		},
		"clocking": map[string]any{
			"is_sequential": true,
			"clock":         "clk",
			"edge":          "posedge",
			"reset":         "rst",
			"active":        "high",
			"sync":          true,
		},
		"timing": map[string]any{"count": map[string]any{"latency_cycles": 1}},
		"functional_summary": "On each rising clock edge, a high reset clears the counter to zero. " +
			"When reset is low and enable is high, the counter increments by one modulo 2**WIDTH. " +
			"When reset is low and enable is low, the counter holds its value.",
		"corner_cases": []any{
			"wrap-around at 2**WIDTH - 1",
			"reset has priority over enable",
			"enable deasserted for several cycles",
		},
		"test_plan": []any{
			"check synchronous reset",
			"check increment while enabled",
			"check hold while disabled",
			"check wrap-around behavior",
		},
		"guidance": map[string]any{
			"verifier": "Drive inputs on the falling edge and sample on the rising edge after a small delay.",
			"coder":    "Use always_ff with nonblocking assignments and asynchronous-free synchronous reset.",
			"debugger": "Check reset priority and enable gating first.",
		},
	})

	issues := contract.Lint(contractData)
	if contract.HasErrors(issues) {
		result := failed("Mock design contract failed lint.")
		result.ContractIssues = issues
		return result
	}
	result := completed("Created a counter design contract (mock mode).")
	result.ContractJSON = prettyJSON(contractData)
	result.ContractIssues = issues
	return result
}
