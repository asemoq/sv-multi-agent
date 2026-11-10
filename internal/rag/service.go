// Package rag implements retrieval-augmented generation on top of pgvector.
package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/asemoq/sv-multi-agent/internal/agents"
	"github.com/asemoq/sv-multi-agent/internal/embeddings"
	"github.com/asemoq/sv-multi-agent/internal/store"
)

// Options configures the RAG service.
type Options struct {
	DatabaseURL string
	Embedding   embeddings.Options
	TopK        int
	MaxChars    int
	MinScore    float64
	IndexTasks  bool
}

// Service bundles the vector store and the embeddings client.
type Service struct {
	Store    *Store
	Embedder *embeddings.Client
	opts     Options
}

// New connects to Postgres, migrates the schema and checks the embedding endpoint.
func New(ctx context.Context, opts Options) (*Service, error) {
	embedder, err := embeddings.New(opts.Embedding)
	if err != nil {
		return nil, err
	}
	vectorStore, err := Open(ctx, opts.DatabaseURL, opts.Embedding.Dim)
	if err != nil {
		return nil, err
	}
	if err := vectorStore.Migrate(ctx); err != nil {
		vectorStore.Close()
		return nil, err
	}
	if _, err := embedder.Check(ctx); err != nil {
		vectorStore.Close()
		return nil, fmt.Errorf("embedding endpoint: %w", err)
	}
	if opts.TopK <= 0 {
		opts.TopK = 6
	}
	if opts.MaxChars <= 0 {
		opts.MaxChars = 12000
	}
	return &Service{Store: vectorStore, Embedder: embedder, opts: opts}, nil
}

// Close releases resources.
func (s *Service) Close() {
	if s != nil && s.Store != nil {
		s.Store.Close()
	}
}

// Available reports whether the service is ready.
func (s *Service) Available() bool {
	return s != nil && s.Store != nil && s.Embedder != nil
}

// Model returns the embedding model ID.
func (s *Service) Model() string {
	if s == nil || s.Embedder == nil {
		return ""
	}
	return s.Embedder.Model()
}

// Check verifies the embedding endpoint and Postgres, returning the observed dimension.
func (s *Service) Check(ctx context.Context) (int, error) {
	if !s.Available() {
		return 0, fmt.Errorf("RAG is not available")
	}
	dim, err := s.Embedder.Check(ctx)
	if err != nil {
		return 0, err
	}
	if err := s.Store.Ping(ctx); err != nil {
		return 0, err
	}
	return dim, nil
}

// IndexTask indexes the artifacts of a completed (or failed) task.
func (s *Service) IndexTask(ctx context.Context, task *store.Task, contractData map[string]any, files map[string]string, simulation *agents.Result, fix *agents.Result) error {
	if !s.Available() || task == nil {
		return nil
	}

	documents := []Document{}
	moduleName := asString(contractData["module_name"])
	if moduleName == "" {
		moduleName = task.ID
	}

	documents = append(documents, Document{
		Kind:    "design",
		TaskID:  task.ID,
		Title:   moduleName,
		Content: "Prompt:\n" + task.Prompt + "\n\nContract:\n" + task.ContractJSON,
		Metadata: map[string]any{
			"task_id":     task.ID,
			"module_name": moduleName,
			"prompt":      task.Prompt,
		},
	})

	paths := sortedFilePaths(files)
	for _, path := range paths {
		if !strings.HasPrefix(path, "rtl/") || !strings.HasSuffix(strings.ToLower(path), ".sv") {
			continue
		}
		for _, chunk := range ChunkRTL(path, files[path]) {
			documents = append(documents, Document{
				Kind:    "rtl",
				TaskID:  task.ID,
				Title:   chunk.Title,
				Content: chunk.Content,
				Metadata: mergeMetadata(chunk.Metadata, map[string]any{
					"task_id": task.ID,
				}),
			})
		}
	}

	for _, path := range paths {
		if !strings.HasPrefix(path, "tb/") || !strings.HasSuffix(strings.ToLower(path), ".sv") {
			continue
		}
		documents = append(documents, Document{
			Kind:    "testbench",
			TaskID:  task.ID,
			Title:   path,
			Content: files[path],
			Metadata: map[string]any{
				"task_id": task.ID,
				"path":    path,
			},
		})
	}

	if simulation != nil && !simulation.Passed && simulation.TraceReport != nil {
		if content := ChunkTraceReport(simulation.TraceReport); strings.TrimSpace(content) != "" {
			documents = append(documents, Document{
				Kind:    "failure",
				TaskID:  task.ID,
				Title:   "failure " + task.ID,
				Content: content,
				Metadata: map[string]any{
					"task_id":      task.ID,
					"fail_outputs": simulation.FailOutputs,
				},
			})
		}
	}

	if fix != nil && fix.Status == "completed" && len(fix.Artifacts) > 0 {
		if content := ChunkFix(fix.Summary, fix.Artifacts); strings.TrimSpace(content) != "" {
			documents = append(documents, Document{
				Kind:    "fix",
				TaskID:  task.ID,
				Title:   "fix " + task.ID,
				Content: content,
				Metadata: map[string]any{
					"task_id": task.ID,
				},
			})
		}
	}

	if len(documents) == 0 {
		return nil
	}
	if err := s.embedAll(ctx, documents); err != nil {
		return err
	}
	if err := s.Store.DeleteByTask(ctx, task.ID); err != nil {
		return err
	}
	return s.Store.Insert(ctx, documents)
}

// IndexOptions controls a single IndexPath run.
type IndexOptions struct {
	// Kind is the knowledge kind stored with every chunk (default "docs").
	Kind string
	// Tags are stored in chunk metadata for later filtering.
	Tags []string
	// Excludes are file names or globs to skip (e.g. "ibex_core.sv", "*_tb.sv").
	Excludes []string
}

// maxIndexFileBytes skips files larger than this while indexing a repository.
const maxIndexFileBytes = 1 << 20 // 1 MiB

// IndexPath indexes a file or directory as documentation.
func (s *Service) IndexPath(ctx context.Context, root string, opts IndexOptions) (int, error) {
	if !s.Available() {
		return 0, fmt.Errorf("RAG is not available")
	}
	kind := strings.TrimSpace(opts.Kind)
	if kind == "" {
		kind = "docs"
	}

	info, err := os.Stat(root)
	if err != nil {
		return 0, err
	}
	files := []string{}
	if info.IsDir() {
		err = filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if shouldSkipDir(entry.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if isIndexable(current) && !matchesExclude(current, opts.Excludes) {
				files = append(files, current)
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	} else {
		if matchesExclude(root, opts.Excludes) {
			return 0, nil
		}
		files = append(files, root)
	}
	sort.Strings(files)
	log.Printf("rag: indexing %d file(s) from %s as %s", len(files), root, kind)

	documents := []Document{}
	sources := []string{}
	for fileIndex, file := range files {
		if fileIndex > 0 && fileIndex%100 == 0 {
			log.Printf("rag: %d/%d files", fileIndex, len(files))
		}
		fileInfo, err := os.Stat(file)
		if err != nil {
			return 0, err
		}
		if fileInfo.Size() > maxIndexFileBytes {
			log.Printf("rag: skip %s (%d bytes > %d)", file, fileInfo.Size(), maxIndexFileBytes)
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return 0, err
		}
		absolute, err := filepath.Abs(file)
		if err != nil {
			absolute = file
		}
		relative := filepath.ToSlash(file)
		baseMetadata := map[string]any{
			"source": absolute,
			"path":   relative,
			"tags":   opts.Tags,
		}
		sources = append(sources, absolute)

		if isSystemVerilog(file) {
			for _, chunk := range ChunkRTL(relative, string(data)) {
				documents = append(documents, Document{
					Kind:     kind,
					Title:    chunk.Title,
					Content:  chunk.Content,
					Metadata: mergeMetadata(baseMetadata, chunk.Metadata),
				})
			}
			continue
		}
		for chunkIndex, chunk := range ChunkText(string(data), 2000) {
			documents = append(documents, Document{
				Kind:     kind,
				Title:    fmt.Sprintf("%s #%d", relative, chunkIndex+1),
				Content:  chunk,
				Metadata: baseMetadata,
			})
		}
	}

	if len(documents) == 0 {
		log.Printf("rag: nothing to index from %s", root)
		return 0, nil
	}
	if err := s.embedAll(ctx, documents); err != nil {
		return 0, err
	}
	for _, source := range sources {
		if err := s.Store.DeleteBySource(ctx, source); err != nil {
			return 0, err
		}
	}
	if err := s.Store.Insert(ctx, documents); err != nil {
		return 0, err
	}
	log.Printf("rag: indexed %d chunk(s) from %d file(s)", len(documents), len(sources))
	return len(documents), nil
}

// Search embeds the query and returns the closest knowledge rows.
func (s *Service) Search(ctx context.Context, query string, kinds []string, topK int) ([]Hit, error) {
	if !s.Available() {
		return nil, fmt.Errorf("RAG is not available")
	}
	vector, err := s.Embedder.EmbedQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	if topK <= 0 {
		topK = s.opts.TopK
	}
	return s.Store.Search(ctx, vector, kinds, topK, s.opts.MinScore)
}

// Context renders the top-k hits as a prompt context block.
func (s *Service) Context(ctx context.Context, query string, kinds []string) (string, error) {
	hits, err := s.Search(ctx, query, kinds, s.opts.TopK)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "", nil
	}

	var builder strings.Builder
	for index, hit := range hits {
		entry := fmt.Sprintf("[%d] kind=%s score=%.3f title=%s\n%s\n\n",
			index+1, hit.Kind, hit.Score, hit.Title, hit.Content)
		if builder.Len()+len(entry) > s.opts.MaxChars {
			remaining := s.opts.MaxChars - builder.Len()
			if remaining > 0 {
				if remaining > len(entry) {
					remaining = len(entry)
				}
				builder.WriteString(entry[:remaining])
			}
			break
		}
		builder.WriteString(entry)
	}
	return strings.TrimSpace(builder.String()), nil
}

// Stats returns row counts per kind.
func (s *Service) Stats(ctx context.Context) (map[string]int, error) {
	if !s.Available() {
		return nil, fmt.Errorf("RAG is not available")
	}
	return s.Store.Stats(ctx)
}

func (s *Service) embedAll(ctx context.Context, documents []Document) error {
	texts := make([]string, len(documents))
	for index, document := range documents {
		texts[index] = document.Content
	}
	vectors, err := s.Embedder.EmbedDocuments(ctx, texts)
	if err != nil {
		return err
	}
	if len(vectors) != len(documents) {
		return fmt.Errorf("embeddings: expected %d vectors, got %d", len(documents), len(vectors))
	}
	for index := range documents {
		documents[index].Embedding = vectors[index]
	}
	return nil
}

// isSystemVerilog reports whether file has a SystemVerilog extension.
func isSystemVerilog(file string) bool {
	switch strings.ToLower(filepath.Ext(file)) {
	case ".sv", ".svh", ".sva":
		return true
	}
	return false
}

func isIndexable(file string) bool {
	if isSystemVerilog(file) {
		return true
	}
	switch strings.ToLower(filepath.Ext(file)) {
	case ".md", ".txt", ".rst", ".adoc":
		return true
	}
	return false
}

// shouldSkipDir reports whether a directory should be skipped while indexing a
// repository (VCS metadata, dependency and build trees).
func shouldSkipDir(name string) bool {
	switch strings.ToLower(name) {
	case ".git", ".hg", ".svn", "node_modules", "vendor",
		"dist", "build", "obj_dir", "out", "target",
		".idea", ".vscode", "__pycache__", ".venv", "venv":
		return true
	}
	return false
}

// matchesExclude reports whether file matches any exclude name or glob. A bare
// name matches the basename with or without its extension; globs match either
// the basename or the full slash-separated path.
func matchesExclude(file string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	base := filepath.Base(file)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	slash := filepath.ToSlash(file)
	for _, raw := range patterns {
		pattern := strings.TrimSpace(raw)
		if pattern == "" {
			continue
		}
		if pattern == base || pattern == slash || pattern == stem {
			return true
		}
		if ok, err := path.Match(pattern, base); err == nil && ok {
			return true
		}
		if ok, err := path.Match(pattern, slash); err == nil && ok {
			return true
		}
	}
	return false
}

func sortedFilePaths(files map[string]string) []string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func mergeMetadata(base, extra map[string]any) map[string]any {
	result := make(map[string]any, len(base)+len(extra))
	for key, value := range base {
		result[key] = value
	}
	for key, value := range extra {
		result[key] = value
	}
	return result
}

func asString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

// MarshalMetadata is a small helper for CLI output.
func MarshalMetadata(metadata map[string]any) string {
	data, err := json.Marshal(metadata)
	if err != nil {
		return "{}"
	}
	return string(data)
}
