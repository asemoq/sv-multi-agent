// Package coverage parses Verilator/verilator_coverage LCOV reports.
package coverage

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Line is one source line and its execution count.
type Line struct {
	Number int `json:"number"`
	Count  int `json:"count"`
}

// File is the coverage of a single source file.
type File struct {
	Path         string `json:"path"`
	Lines        []Line `json:"lines,omitempty"`
	LinesTotal   int    `json:"lines_total"`
	LinesCovered int    `json:"lines_covered"`
}

// Report is a line-coverage summary for a simulation run.
type Report struct {
	Files        []File  `json:"files"`
	LinesTotal   int     `json:"lines_total"`
	LinesCovered int     `json:"lines_covered"`
	LineCoverage float64 `json:"line_coverage"`
}

// Uncovered is a list of unexecuted lines in one file.
type Uncovered struct {
	Path  string `json:"path"`
	Lines []int  `json:"lines"`
}

// ParseLCOV parses the subset of LCOV emitted by verilator_coverage --write-info.
// root is used to rewrite absolute source paths back to workspace-relative POSIX
// paths; an empty root keeps the recorded path as-is.
func ParseLCOV(data []byte, root string) Report {
	files := map[string][]Line{}
	var current string
	for _, raw := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "SF:"):
			current = normalizePath(strings.TrimPrefix(line, "SF:"), root)
			if _, ok := files[current]; !ok {
				files[current] = []Line{}
			}
		case strings.HasPrefix(line, "DA:"):
			if current == "" {
				continue
			}
			parts := strings.Split(strings.TrimPrefix(line, "DA:"), ",")
			if len(parts) < 2 {
				continue
			}
			number, err := strconv.Atoi(strings.TrimSpace(parts[0]))
			if err != nil {
				continue
			}
			count, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil {
				continue
			}
			files[current] = append(files[current], Line{Number: number, Count: count})
		case line == "end_of_record":
			current = ""
		}
	}

	report := Report{Files: []File{}}
	for path, lines := range files {
		sort.Slice(lines, func(i, j int) bool { return lines[i].Number < lines[j].Number })
		file := File{Path: path, Lines: lines, LinesTotal: len(lines)}
		for _, entry := range lines {
			if entry.Count > 0 {
				file.LinesCovered++
			}
		}
		report.LinesTotal += file.LinesTotal
		report.LinesCovered += file.LinesCovered
		report.Files = append(report.Files, file)
	}
	sort.Slice(report.Files, func(i, j int) bool { return report.Files[i].Path < report.Files[j].Path })
	if report.LinesTotal > 0 {
		report.LineCoverage = float64(report.LinesCovered) / float64(report.LinesTotal)
	}
	return report
}

// ForPrefix returns a report restricted to files under prefix.
func (r Report) ForPrefix(prefix string) Report {
	result := Report{Files: []File{}}
	for _, file := range r.Files {
		if prefix != "" && !strings.HasPrefix(file.Path, prefix) {
			continue
		}
		result.Files = append(result.Files, file)
		result.LinesTotal += file.LinesTotal
		result.LinesCovered += file.LinesCovered
	}
	if result.LinesTotal > 0 {
		result.LineCoverage = float64(result.LinesCovered) / float64(result.LinesTotal)
	}
	return result
}

// Uncovered returns files with unexecuted lines. A limit <= 0 returns all lines.
func (r Report) Uncovered(limit int) []Uncovered {
	return r.uncovered(limit, "")
}

// UncoveredFor returns files under prefix with unexecuted lines.
func (r Report) UncoveredFor(prefix string, limit int) []Uncovered {
	return r.uncovered(limit, prefix)
}

func (r Report) uncovered(limit int, prefix string) []Uncovered {
	result := []Uncovered{}
	remaining := limit
	for _, file := range r.Files {
		if prefix != "" && !strings.HasPrefix(file.Path, prefix) {
			continue
		}
		lines := []int{}
		for _, line := range file.Lines {
			if line.Count > 0 {
				continue
			}
			if remaining > 0 && len(lines) >= remaining {
				break
			}
			lines = append(lines, line.Number)
		}
		if len(lines) == 0 {
			continue
		}
		if remaining > 0 {
			remaining -= len(lines)
		}
		result = append(result, Uncovered{Path: file.Path, Lines: lines})
		if remaining == 0 && limit > 0 {
			break
		}
	}
	return result
}

// UncoveredText renders uncovered lines for a Verifier prompt.
func (r Report) UncoveredText(limit int) string {
	return r.UncoveredTextFor("", limit)
}

// UncoveredTextFor renders uncovered lines under prefix for a Verifier prompt.
func (r Report) UncoveredTextFor(prefix string, limit int) string {
	items := r.UncoveredFor(prefix, limit)
	if len(items) == 0 {
		return ""
	}
	var builder strings.Builder
	for _, item := range items {
		parts := make([]string, 0, len(item.Lines))
		for _, line := range item.Lines {
			parts = append(parts, strconv.Itoa(line))
		}
		fmt.Fprintf(&builder, "- %s: lines %s\n", item.Path, strings.Join(parts, ", "))
	}
	return builder.String()
}

func normalizePath(path, root string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if root != "" {
		if rel, err := filepath.Rel(root, filepath.Clean(path)); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			path = rel
		}
	}
	return filepath.ToSlash(path)
}
