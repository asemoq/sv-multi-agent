// Package setup implements the interactive first-run configuration wizard.
package setup

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/asemoq/sv-multi-agent/internal/userconfig"
)

// Runner reads answers from the terminal and returns an updated user config.
type Runner struct {
	in  *bufio.Reader
	out *os.File
}

// New creates a wizard bound to stdin/stdout.
func New() *Runner {
	return &Runner{in: bufio.NewReader(os.Stdin), out: os.Stdout}
}

// Run prompts for an OpenAI-compatible LLM provider and connection details.
func (r *Runner) Run(existing userconfig.Config) (userconfig.Config, error) {
	cfg := existing
	providers := []string{
		"Ollama (local, http://localhost:11434/v1)",
		"LM Studio (local, http://localhost:1234/v1)",
		"OpenCode / opencode-go (custom endpoint)",
		"Custom OpenAI-compatible endpoint",
		"Mock (no LLM; counter demo only)",
	}

	fmt.Fprintln(r.out, "")
	fmt.Fprintln(r.out, "SV Multi-Agent - LLM setup")
	fmt.Fprintln(r.out, "==========================")
	fmt.Fprintln(r.out, "Choose an OpenAI-compatible provider:")
	for i, provider := range providers {
		fmt.Fprintf(r.out, "  %d) %s\n", i+1, provider)
	}

	defaultChoice := 1
	switch cfg.LLM.Provider {
	case "lmstudio":
		defaultChoice = 2
	case "opencode":
		defaultChoice = 3
	case "custom":
		defaultChoice = 4
	case "mock":
		defaultChoice = 5
	}

	raw := r.input(fmt.Sprintf("Select [1-%d]", len(providers)), strconv.Itoa(defaultChoice))
	index, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || index < 1 || index > len(providers) {
		return userconfig.Config{}, fmt.Errorf("invalid selection %q", raw)
	}

	switch index {
	case 1:
		cfg.LLM.Provider = "ollama"
		cfg.LLM.Mode = "openai-compatible"
		cfg.LLM.BaseURL = r.input("Base URL", orDefault(cfg.LLM.BaseURL, "http://localhost:11434/v1"))
		cfg.LLM.Model = r.input("Model", orDefault(cfg.LLM.Model, "qwen2.5-coder:14b"))
		cfg.LLM.APIKey = r.secret("API key", orDefault(cfg.LLM.APIKey, "ollama"))
	case 2:
		cfg.LLM.Provider = "lmstudio"
		cfg.LLM.Mode = "openai-compatible"
		cfg.LLM.BaseURL = r.input("Base URL", orDefault(cfg.LLM.BaseURL, "http://localhost:1234/v1"))
		cfg.LLM.Model = r.input("Model", orDefault(cfg.LLM.Model, "local-model"))
		cfg.LLM.APIKey = r.secret("API key (LM Studio ignores it)", orDefault(cfg.LLM.APIKey, "lm-studio"))
	case 3:
		cfg.LLM.Provider = "opencode"
		cfg.LLM.Mode = "openai-compatible"
		fmt.Fprintln(r.out, "Enter the OpenAI-compatible base URL of your OpenCode/opencode-go endpoint.")
		cfg.LLM.BaseURL = r.input("Base URL (e.g. https://<host>/v1)", cfg.LLM.BaseURL)
		cfg.LLM.Model = r.input("Model", cfg.LLM.Model)
		cfg.LLM.APIKey = r.secret("API key (Enter to keep/empty)", cfg.LLM.APIKey)
	case 4:
		cfg.LLM.Provider = "custom"
		cfg.LLM.Mode = "openai-compatible"
		cfg.LLM.BaseURL = r.input("Base URL (OpenAI-compatible root, e.g. http://localhost:8000/v1)", cfg.LLM.BaseURL)
		cfg.LLM.Model = r.input("Model", cfg.LLM.Model)
		cfg.LLM.APIKey = r.secret("API key (Enter to keep/empty)", cfg.LLM.APIKey)
	case 5:
		cfg.LLM = userconfig.LLM{Provider: "mock", Mode: "mock"}
	}

	cfg.Addr = r.input(
		"sv-multi-agent HTTP address",
		orDefault(cfg.Addr, "127.0.0.1:50050"),
	)

	if err := validate(cfg); err != nil {
		return userconfig.Config{}, err
	}
	return cfg, nil
}

func validate(cfg userconfig.Config) error {
	if cfg.LLM.Mode == "mock" {
		return nil
	}
	if strings.TrimSpace(cfg.LLM.BaseURL) == "" {
		return fmt.Errorf("base URL is required for provider %q", cfg.LLM.Provider)
	}
	if strings.TrimSpace(cfg.LLM.Model) == "" {
		return fmt.Errorf("model is required for provider %q", cfg.LLM.Provider)
	}
	return nil
}

func (r *Runner) input(prompt, defaultValue string) string {
	if defaultValue != "" {
		fmt.Fprintf(r.out, "%s [%s]: ", prompt, defaultValue)
	} else {
		fmt.Fprintf(r.out, "%s: ", prompt)
	}
	line, _ := r.in.ReadString('\n')
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "\ufeff") // tolerate a UTF-8 BOM from piped stdin
	if line == "" {
		return defaultValue
	}
	return line
}

func (r *Runner) secret(prompt, defaultValue string) string {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return r.input(prompt, defaultValue)
	}
	if defaultValue != "" {
		fmt.Fprintf(r.out, "%s [keep existing]: ", prompt)
	} else {
		fmt.Fprintf(r.out, "%s (hidden): ", prompt)
	}
	data, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(r.out)
	if err != nil {
		return defaultValue
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return defaultValue
	}
	return value
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

// PrintSummary prints the saved configuration (masking the API key).
func PrintSummary(out *os.File, cfg userconfig.Config) {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Saved configuration:")
	if cfg.LLM.Mode == "mock" {
		fmt.Fprintln(out, "  provider: mock (no LLM)")
	} else {
		fmt.Fprintf(out, "  provider: %s\n", cfg.LLM.Provider)
		fmt.Fprintf(out, "  base URL: %s\n", cfg.LLM.BaseURL)
		fmt.Fprintf(out, "  model:    %s\n", cfg.LLM.Model)
		fmt.Fprintf(out, "  api key:  %s\n", mask(cfg.LLM.APIKey))
	}
	fmt.Fprintf(out, "  service:   %s\n", cfg.Addr)
}

func mask(value string) string {
	if value == "" {
		return "(none)"
	}
	if len(value) <= 4 {
		return "****"
	}
	return value[:2] + strings.Repeat("*", len(value)-4) + value[len(value)-2:]
}
