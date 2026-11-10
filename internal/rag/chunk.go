package rag

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/asemoq/sv-multi-agent/internal/agents"
	"github.com/asemoq/sv-multi-agent/internal/sv/slicer"
)

// Chunk is a single embeddable unit.
type Chunk struct {
	Title    string
	Content  string
	Metadata map[string]any
}

// maxUnitChars is the size above which a design unit is split into structural
// chunks instead of being embedded as one block.
const maxUnitChars = 6000

// ChunkText splits prose into chunks of at most maxChars characters.
func ChunkText(text string, maxChars int) []string {
	if maxChars <= 0 {
		maxChars = 2000
	}
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	paragraphs := strings.Split(normalized, "\n\n")
	chunks := []string{}
	var current strings.Builder

	flush := func() {
		if current.Len() > 0 {
			chunks = append(chunks, strings.TrimSpace(current.String()))
			current.Reset()
		}
	}

	for _, paragraph := range paragraphs {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph == "" {
			continue
		}
		if len(paragraph) > maxChars {
			flush()
			for len(paragraph) > maxChars {
				chunks = append(chunks, strings.TrimSpace(paragraph[:maxChars]))
				paragraph = paragraph[maxChars:]
			}
			if strings.TrimSpace(paragraph) != "" {
				chunks = append(chunks, strings.TrimSpace(paragraph))
			}
			continue
		}
		if current.Len() > 0 && current.Len()+len(paragraph)+2 > maxChars {
			flush()
		}
		if current.Len() > 0 {
			current.WriteString("\n\n")
		}
		current.WriteString(paragraph)
	}
	flush()

	if len(chunks) == 0 && strings.TrimSpace(normalized) != "" {
		chunks = append(chunks, strings.TrimSpace(normalized))
	}
	return chunks
}

// ChunkRTL splits a SystemVerilog file by design unit. Large units are split
// into a header/declarations chunk and one chunk per process block, function or
// task so embeddings stay focused.
func ChunkRTL(path, content string) []Chunk {
	units := slicer.ParseStructure(content, path)
	if len(units) == 0 {
		return []Chunk{{
			Title:   path,
			Content: content,
			Metadata: map[string]any{
				"path": filepath.ToSlash(path),
			},
		}}
	}

	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	chunks := []Chunk{}
	for _, unit := range units {
		unitCode := sliceLines(lines, unit.StartLine, unit.EndLine)
		if len(unitCode) <= maxUnitChars || len(unit.Segments) <= 1 {
			chunks = append(chunks, Chunk{
				Title:   path + ":" + unit.Name,
				Content: unitCode,
				Metadata: map[string]any{
					"path":        filepath.ToSlash(path),
					"module_name": unit.Name,
					"unit_kind":   unit.Kind,
				},
			})
			continue
		}

		preludeEnd := unit.StartLine
		for _, segment := range unit.Segments {
			if segment.Kind != "module_header" && segment.Kind != "declarations" {
				break
			}
			if segment.EndLine > preludeEnd {
				preludeEnd = segment.EndLine
			}
		}

		chunks = append(chunks, Chunk{
			Title:   path + ":" + unit.Name + ":header",
			Content: sliceLines(lines, unit.StartLine, preludeEnd),
			Metadata: map[string]any{
				"path":        filepath.ToSlash(path),
				"module_name": unit.Name,
				"unit_kind":   unit.Kind,
				"segment":     "header",
			},
		})

		for _, segment := range unit.Segments {
			if segment.Kind == "module_header" || segment.Kind == "declarations" {
				continue
			}
			if strings.TrimSpace(segment.Code) == "" {
				continue
			}
			chunks = append(chunks, Chunk{
				Title:   segment.ID,
				Content: segment.Code,
				Metadata: map[string]any{
					"path":         filepath.ToSlash(path),
					"module_name":  unit.Name,
					"unit_kind":    unit.Kind,
					"block_id":     segment.ID,
					"segment_kind": segment.Kind,
					"start_line":   segment.StartLine,
					"end_line":     segment.EndLine,
				},
			})
		}
	}
	return chunks
}

func sliceLines(lines []string, startLine, endLine int) string {
	if startLine < 1 {
		startLine = 1
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	if endLine < startLine {
		return ""
	}
	return strings.Join(lines[startLine-1:endLine], "\n")
}

// ChunkTraceReport turns a trace report into a compact text digest.
func ChunkTraceReport(report map[string]any) string {
	digest := map[string]any{
		"mismatch_count":      report["mismatch_count"],
		"samples":             report["samples"],
		"fail_time":           report["fail_time"],
		"fail_outputs":        report["fail_outputs"],
		"alignment_diagnosis": report["alignment_diagnosis"],
		"timing_hints":        report["timing_hints"],
		"suspect_blocks":      report["suspect_blocks"],
	}
	data, err := json.MarshalIndent(digest, "", "  ")
	if err != nil {
		return ""
	}
	return truncateString(string(data), 8000)
}

// ChunkFix formats a debugger fix for indexing.
func ChunkFix(summary string, artifacts []agents.Artifact) string {
	var builder strings.Builder
	builder.WriteString("Summary: ")
	builder.WriteString(strings.TrimSpace(summary))
	builder.WriteString("\n\n")
	for _, artifact := range artifacts {
		builder.WriteString("--- ")
		builder.WriteString(artifact.Path)
		builder.WriteString(" ---\n")
		builder.WriteString(artifact.Content)
		builder.WriteString("\n\n")
	}
	return truncateString(builder.String(), 12000)
}

func truncateString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
