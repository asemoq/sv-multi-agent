// Package mutate generates synthetic mutants of SystemVerilog sources so the
// workflow can measure how well the testbench detects injected bugs.
package mutate

import (
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/asemoq/sv-multi-agent/internal/sv/slicer"
)

// Mutant is a single mutated file bundle.
type Mutant struct {
	ID          string            `json:"id"`
	Description string            `json:"description"`
	File        string            `json:"file"`
	Lines       []int             `json:"lines,omitempty"`
	Files       map[string]string `json:"-"`
}

// Survivor is a mutant the testbench did not detect.
type Survivor struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	File        string `json:"file"`
	Lines       []int  `json:"lines,omitempty"`
}

// Report summarizes a mutation run.
type Report struct {
	Total    int        `json:"total"`
	Killed   int        `json:"killed"`
	Survived []Survivor `json:"survived"`
	Score    float64    `json:"score"`
}

var (
	reReset  = regexp.MustCompile(`(if\s*\(\s*)(!?)(rst|reset|rst_n|nreset|arst)(\s*\))`)
	reEnable = regexp.MustCompile(`(if\s*\(\s*)(!?)(en|enable)(\s*\))`)
	reCmp    = regexp.MustCompile(`([A-Za-z0-9_\)\]])\s*(==|!=)\s*([A-Za-z0-9_\(\['])`)
	reBit    = regexp.MustCompile(`([A-Za-z0-9_\)\]])\s*(&|\|)\s*([A-Za-z0-9_\(\['])`)
	reArith  = regexp.MustCompile(`([A-Za-z0-9_\)\]])\s*(\+|-)\s*([A-Za-z0-9_\(\['])`)
	reConst  = regexp.MustCompile(`\b(\d+)\b`)
)

// Generate creates up to budget deterministic mutants of the SystemVerilog
// files. Mutants are shuffled with seed so results are reproducible.
func Generate(files map[string]string, budget int, seed int64) []Mutant {
	if budget <= 0 {
		return nil
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		if strings.HasPrefix(path, "rtl/") && strings.HasSuffix(strings.ToLower(path), ".sv") {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)

	mutants := []Mutant{}
	idCounter := 0

	// Block deletions: removing a whole always_*/assign block is a strong mutant.
	sv := map[string]string{}
	for _, path := range paths {
		sv[path] = files[path]
	}
	blocks := slicer.ParseSources(sv)
	for _, block := range blocks {
		switch block.Kind {
		case "always_ff", "always_comb", "always_latch", "assign", "always":
		default:
			continue
		}
		content, ok := files[block.File]
		if !ok {
			continue
		}
		mutated := deleteBlock(content, block.StartLine, block.EndLine)
		if mutated == "" {
			continue
		}
		idCounter++
		mutants = append(mutants, Mutant{
			ID:          fmt.Sprintf("mut_%d", idCounter),
			Description: "delete " + block.Kind + " block " + block.ID,
			File:        block.File,
			Lines:       lineRange(block.StartLine, block.EndLine),
			Files:       withFile(files, block.File, mutated),
		})
	}

	// Line-level mutations: one change per line, first applicable kind wins.
	for _, path := range paths {
		content := files[path]
		lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
		for index, line := range lines {
			mutated, description, ok := textMutation(line)
			if !ok {
				continue
			}
			updated := append([]string{}, lines...)
			updated[index] = mutated
			idCounter++
			mutants = append(mutants, Mutant{
				ID:          fmt.Sprintf("mut_%d", idCounter),
				Description: description,
				File:        path,
				Lines:       []int{index + 1},
				Files:       withFile(files, path, strings.Join(updated, "\n")+"\n"),
			})
		}
	}

	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(mutants), func(i, j int) { mutants[i], mutants[j] = mutants[j], mutants[i] })
	if len(mutants) > budget {
		mutants = mutants[:budget]
	}
	return mutants
}

// Run executes every mutant and reports how many the testbench killed.
// run returns true when the mutated design still passes simulation.
func Run(mutants []Mutant, run func(files map[string]string) bool) Report {
	return RunWithWorkers(mutants, 1, run)
}

// RunWithWorkers executes mutants with up to workers parallel simulations.
// Results are collected by mutant index so the report (survivors, score) is
// deterministic regardless of completion order. workers <= 1 runs sequentially.
func RunWithWorkers(mutants []Mutant, workers int, run func(files map[string]string) bool) Report {
	survived := make([]bool, len(mutants))
	if workers <= 1 || len(mutants) <= 1 {
		for index, mutant := range mutants {
			survived[index] = run(mutant.Files)
		}
	} else {
		if workers > len(mutants) {
			workers = len(mutants)
		}
		var wg sync.WaitGroup
		sem := make(chan struct{}, workers)
		for index := range mutants {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				// Each goroutine writes its own slice element, so no lock is
				// needed for the results array.
				survived[i] = run(mutants[i].Files)
			}(index)
		}
		wg.Wait()
	}

	report := Report{Total: len(mutants), Survived: []Survivor{}}
	for index, mutant := range mutants {
		if survived[index] {
			report.Survived = append(report.Survived, Survivor{
				ID:          mutant.ID,
				Description: mutant.Description,
				File:        mutant.File,
				Lines:       mutant.Lines,
			})
		} else {
			report.Killed++
		}
	}
	if report.Total > 0 {
		report.Score = float64(report.Killed) / float64(report.Total)
	}
	return report
}

// SurvivorText renders survivors for a Verifier prompt.
func (r Report) SurvivorText(limit int) string {
	if len(r.Survived) == 0 {
		return ""
	}
	var builder strings.Builder
	for index, survivor := range r.Survived {
		if limit > 0 && index >= limit {
			break
		}
		parts := make([]string, 0, len(survivor.Lines))
		for _, line := range survivor.Lines {
			parts = append(parts, strconv.Itoa(line))
		}
		fmt.Fprintf(&builder, "- %s: lines %s (%s)\n", survivor.File, strings.Join(parts, ", "), survivor.Description)
	}
	return builder.String()
}

func textMutation(line string) (string, string, bool) {
	if changed, ok := mutatePattern(line, reReset, func(g []string) string {
		neg := "!"
		if g[2] == "!" {
			neg = ""
		}
		return g[1] + neg + g[3] + g[4]
	}); ok {
		return changed, "invert reset polarity", true
	}
	if changed, ok := mutatePattern(line, reEnable, func(g []string) string {
		neg := "!"
		if g[2] == "!" {
			neg = ""
		}
		return g[1] + neg + g[3] + g[4]
	}); ok {
		return changed, "invert enable gating", true
	}
	if changed, ok := mutatePattern(line, reCmp, func(g []string) string {
		replacement := "=="
		if g[2] == "==" {
			replacement = "!="
		}
		return g[1] + " " + replacement + " " + g[3]
	}); ok {
		return changed, "flip comparison operator", true
	}
	if changed, ok := mutatePattern(line, reBit, func(g []string) string {
		replacement := "|"
		if g[2] == "|" {
			replacement = "&"
		}
		return g[1] + " " + replacement + " " + g[3]
	}); ok {
		return changed, "flip bitwise operator", true
	}
	if strings.ContainsAny(line, "+-") {
		if changed, ok := mutatePattern(line, reConst, func(g []string) string {
			value, err := strconv.Atoi(g[1])
			if err != nil {
				return g[0]
			}
			return strconv.Itoa(value + 1)
		}); ok {
			return changed, "mutate constant by one", true
		}
	}
	if changed, ok := mutatePattern(line, reArith, func(g []string) string {
		replacement := "-"
		if g[2] == "-" {
			replacement = "+"
		}
		return g[1] + " " + replacement + " " + g[3]
	}); ok {
		return changed, "flip arithmetic operator", true
	}
	return line, "", false
}

// mutatePattern replaces the first match of re in line using build, which
// receives the captured groups (group 0 is the full match).
func mutatePattern(line string, re *regexp.Regexp, build func(groups []string) string) (string, bool) {
	index := re.FindStringSubmatchIndex(line)
	if index == nil {
		return line, false
	}
	groups := make([]string, 0, len(index)/2)
	for i := 0; i < len(index); i += 2 {
		if index[i] < 0 {
			groups = append(groups, "")
			continue
		}
		groups = append(groups, line[index[i]:index[i+1]])
	}
	return line[:index[0]] + build(groups) + line[index[1]:], true
}

func deleteBlock(content string, startLine, endLine int) string {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if startLine < 1 || endLine < startLine || startLine > len(lines) {
		return ""
	}
	end := endLine
	if end > len(lines) {
		end = len(lines)
	}
	updated := make([]string, 0, len(lines)-(end-startLine+1)+1)
	updated = append(updated, lines[:startLine-1]...)
	updated = append(updated, "// [mutation] removed block")
	updated = append(updated, lines[end:]...)
	return strings.Join(updated, "\n") + "\n"
}

func withFile(files map[string]string, path, content string) map[string]string {
	copyMap := make(map[string]string, len(files))
	for key, value := range files {
		copyMap[key] = value
	}
	copyMap[path] = content
	return copyMap
}

func lineRange(start, end int) []int {
	lines := make([]int, 0, end-start+1)
	for line := start; line <= end; line++ {
		lines = append(lines, line)
	}
	return lines
}
