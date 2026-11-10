// Command svma is the unified SV Multi-Agent command-line interface.
//
// The sv-multi-agent service and all agents run in-process in a single Go binary:
//
//	svma serve                 start the sv-multi-agent service
//	svma submit -prompt "..."  submit a generation task
//	svma analyze -rtl <path>   analyze existing RTL
//	svma get -task-id <id>     show a task
//	svma wait -task-id <id>    wait until the task finishes
//	svma list                  list tasks
//	svma setup                 configure the LLM provider
//	svma config                show the saved configuration
//	svma version               print the version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/asemoq/sv-multi-agent/internal/app"
	"github.com/asemoq/sv-multi-agent/internal/cli"
	"github.com/asemoq/sv-multi-agent/internal/config"
	"github.com/asemoq/sv-multi-agent/internal/embeddings"
	"github.com/asemoq/sv-multi-agent/internal/rag"
	"github.com/asemoq/sv-multi-agent/internal/setup"
	"github.com/asemoq/sv-multi-agent/internal/userconfig"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		runFirstSetupIfNeeded()
		usage()
		return
	}

	switch os.Args[1] {
	case "serve":
		serve()
	case "setup":
		runSetup()
	case "config":
		configCommand(os.Args[2:])
	case "rag":
		ragCommand(os.Args[2:])
	case "submit", "analyze", "get", "list", "wait", "report":
		applyClientConfig()
		if err := cli.Run(os.Args[1:]); err != nil {
			fail(err)
		}
	case "version", "-v", "--version":
		fmt.Printf("svma %s\n", version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func serve() {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	if err := fs.Parse(os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fail(err)
	}

	uc := loadOrSetupConfig(true)
	applyLLMConfig(uc)

	if err := app.Serve(config.Load()); err != nil {
		fail(err)
	}
}

func runSetup() {
	existing, err := userconfig.Load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fail(err)
	}
	runSetupWizard(existing)
}

func configCommand(args []string) {
	sub := "show"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "show":
		cfg, err := userconfig.Load()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Println("No configuration yet. Run `svma setup`.")
				return
			}
			fail(err)
		}
		setup.PrintSummary(os.Stdout, cfg)
		if path, err := userconfig.Path(); err == nil {
			fmt.Printf("  config:    %s\n", path)
		}
	case "path":
		path, err := userconfig.Path()
		if err != nil {
			fail(err)
		}
		fmt.Println(path)
	case "edit", "set", "reset":
		runSetup()
	default:
		fail(fmt.Errorf("unknown config command %q (use show, path or edit)", sub))
	}
}

func ragCommand(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: svma rag <migrate|check|index|search|stats>")
		return
	}
	switch args[0] {
	case "migrate":
		runRAGMigrate()
	case "check":
		runRAGCheck()
	case "index":
		runRAGIndex(args[1:])
	case "search":
		runRAGSearch(args[1:])
	case "stats":
		runRAGStats()
	default:
		fail(fmt.Errorf("unknown rag command %q", args[0]))
	}
}

func ragServiceOptions(cfg config.Config) rag.Options {
	return rag.Options{
		DatabaseURL: cfg.DatabaseURL,
		Embedding: embeddings.Options{
			BaseURL:            cfg.EmbeddingBaseURL,
			APIKey:             cfg.EmbeddingAPIKey,
			Model:              cfg.EmbeddingModel,
			Dim:                cfg.EmbeddingDim,
			Timeout:            time.Duration(cfg.EmbeddingTimeoutSeconds) * time.Second,
			InsecureSkipVerify: cfg.EmbeddingInsecureSkipVerify,
			CAFile:             cfg.EmbeddingCAFile,
			QueryPrefix:        cfg.EmbeddingQueryPrefix,
			BatchSize:          cfg.EmbeddingBatchSize,
		},
		TopK:       cfg.RAGTopK,
		MaxChars:   cfg.RAGMaxChars,
		MinScore:   cfg.RAGMinScore,
		IndexTasks: cfg.RAGIndexTasks,
	}
}

func newRAGService() (*rag.Service, config.Config) {
	cfg := config.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	service, err := rag.New(ctx, ragServiceOptions(cfg))
	if err != nil {
		fail(err)
	}
	return service, cfg
}

func runRAGMigrate() {
	cfg := config.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	vectorStore, err := rag.Open(ctx, cfg.DatabaseURL, cfg.EmbeddingDim)
	if err != nil {
		fail(err)
	}
	defer vectorStore.Close()
	if err := vectorStore.Migrate(ctx); err != nil {
		fail(err)
	}
	fmt.Printf("pgvector schema ready (dim=%d)\n", cfg.EmbeddingDim)
}

func runRAGCheck() {
	service, cfg := newRAGService()
	defer service.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dim, err := service.Check(ctx)
	if err != nil {
		fail(err)
	}
	stats, err := service.Stats(ctx)
	if err != nil {
		fail(err)
	}
	fmt.Printf("RAG OK: model=%s dim=%d\n", cfg.EmbeddingModel, dim)
	fmt.Printf("database: %s\n", cfg.DatabaseURL)
	printStats(stats)
}

func runRAGIndex(args []string) {
	fs := flag.NewFlagSet("rag index", flag.ContinueOnError)
	kind := fs.String("kind", "docs", "knowledge kind (docs, design, rtl, testbench, failure, fix)")
	tags := fs.String("tag", "", "comma-separated tags")
	excludes := fs.String("exclude", "", "comma-separated file names/globs to skip (e.g. ibex_core.sv)")
	timeout := fs.Duration("timeout", 2*time.Hour, "indexing timeout")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fail(err)
	}
	if fs.NArg() < 1 {
		fail(fmt.Errorf("rag index requires a file or directory path"))
	}
	path := fs.Arg(0)

	service, _ := newRAGService()
	defer service.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	count, err := service.IndexPath(ctx, path, rag.IndexOptions{
		Kind:     *kind,
		Tags:     splitTags(*tags),
		Excludes: splitTags(*excludes),
	})
	if err != nil {
		fail(err)
	}
	fmt.Printf("indexed %d chunk(s) from %s as %s\n", count, path, *kind)
}

func runRAGSearch(args []string) {
	fs := flag.NewFlagSet("rag search", flag.ContinueOnError)
	kind := fs.String("kind", "", "filter by kind")
	topK := fs.Int("k", 0, "number of results (default RAG_TOP_K)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fail(err)
	}
	query := strings.Join(fs.Args(), " ")
	if strings.TrimSpace(query) == "" {
		fail(fmt.Errorf("rag search requires a query"))
	}

	service, _ := newRAGService()
	defer service.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var kinds []string
	if strings.TrimSpace(*kind) != "" {
		kinds = []string{*kind}
	}
	hits, err := service.Search(ctx, query, kinds, *topK)
	if err != nil {
		fail(err)
	}
	if len(hits) == 0 {
		fmt.Println("no results")
		return
	}
	for index, hit := range hits {
		fmt.Printf("[%d] score=%.3f kind=%s title=%s\n%s\n\n",
			index+1, hit.Score, hit.Kind, hit.Title, truncateText(hit.Content, 300))
	}
}

func runRAGStats() {
	cfg := config.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	vectorStore, err := rag.Open(ctx, cfg.DatabaseURL, cfg.EmbeddingDim)
	if err != nil {
		fail(err)
	}
	defer vectorStore.Close()
	stats, err := vectorStore.Stats(ctx)
	if err != nil {
		fail(err)
	}
	printStats(stats)
}

func printStats(stats map[string]int) {
	if len(stats) == 0 {
		fmt.Println("knowledge base is empty")
		return
	}
	kinds := make([]string, 0, len(stats))
	total := 0
	for kind, count := range stats {
		kinds = append(kinds, kind)
		total += count
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		fmt.Printf("  %-10s %d\n", kind, stats[kind])
	}
	fmt.Printf("  %-10s %d\n", "total", total)
}

func splitTags(value string) []string {
	tags := []string{}
	for _, tag := range strings.Split(value, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

func truncateText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func runFirstSetupIfNeeded() {
	if userconfig.Exists() || !isTerminal() {
		return
	}
	runSetupWizard(userconfig.Config{})
}

func runSetupWizard(existing userconfig.Config) userconfig.Config {
	cfg, err := setup.New().Run(existing)
	if err != nil {
		fail(err)
	}
	path, err := userconfig.Save(cfg)
	if err != nil {
		fail(err)
	}
	setup.PrintSummary(os.Stdout, cfg)
	fmt.Printf("\nConfig saved to %s\n", path)
	return cfg
}

// loadOrSetupConfig loads the user config, running the wizard on first use when
// the process is attached to a terminal.
func loadOrSetupConfig(interactive bool) userconfig.Config {
	cfg, err := userconfig.Load()
	if err == nil {
		return cfg
	}
	if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "warning: read user config:", err)
		return userconfig.Config{}
	}
	if !interactive || !isTerminal() {
		return userconfig.Config{}
	}
	return runSetupWizard(userconfig.Config{})
}

// applyLLMConfig copies saved values into the environment without overriding
// explicit environment variables (environment always wins).
func applyLLMConfig(cfg userconfig.Config) {
	if cfg.LLM.Mode != "" {
		setIfUnset("LLM_MODE", cfg.LLM.Mode)
	}
	setIfUnset("LLM_BASE_URL", cfg.LLM.BaseURL)
	setIfUnset("LLM_API_KEY", cfg.LLM.APIKey)
	setIfUnset("LLM_MODEL", cfg.LLM.Model)
	setIfUnset("SVMA_ADDR", cfg.Addr)
}

func applyClientConfig() {
	cfg, err := userconfig.Load()
	if err != nil {
		return
	}
	setIfUnset("SVMA_ADDR", cfg.Addr)
}

func setIfUnset(key, value string) {
	if value == "" || os.Getenv(key) != "" {
		return
	}
	_ = os.Setenv(key, value)
}

func isTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func usage() {
	fmt.Println(`svma — SV Multi-Agent CLI

Usage:
  svma serve                    Start the sv-multi-agent service (agents run in-process)
  svma setup                    Configure the LLM provider (saved per user)
  svma config [show|path|edit]  Show or edit the saved configuration
  svma rag <cmd>                RAG: migrate|check|index|search|stats
  svma submit -prompt "<text>"  Submit a generation task
  svma analyze -rtl <path>      Analyze existing RTL (optional -tb, -spec, -top)
  svma get -task-id <id>        Show a task
  svma wait -task-id <id>       Wait until the task finishes
  svma report -task-id <id>     Show the task's bug report
  svma list [-limit N]          List tasks
  svma version                  Print version

Client flags:
  -addr <host:port>             sv-multi-agent address (default 127.0.0.1:50050)
  -max-iterations N             Debug loop budget for submit/analyze (1..5)
  -rtl <path>                   Existing RTL file or directory (analyze)
  -tb <path>                    Existing testbench file or directory (analyze)
  -spec <path>                  Specification file (analyze)
  -top <module>                 DUT top module for linting (analyze)
  -json                         Print raw JSON instead of the live view
  -detach                       Submit and return immediately (no live view)
  -interval 2s                  Poll interval for wait -json
  -timeout 30m                  Overall wait timeout

Environment overrides:
  LLM_MODE, LLM_BASE_URL, LLM_API_KEY, LLM_MODEL, SVMA_ADDR

The first run asks for an OpenAI-compatible provider (Ollama, LM Studio,
OpenCode/opencode-go or custom) and stores it in the user config directory:
  Windows: %AppData%\svma\config.json
  Linux:   ~/.config/svma/config.json
  macOS:   ~/Library/Application Support/svma/config.json

The simulator requires Verilator 5.x on PATH (or run the Docker image).`)
}
