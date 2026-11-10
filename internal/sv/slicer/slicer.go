// Package slicer splits SystemVerilog sources into editable blocks and computes
// a static backward slice from failing outputs.
package slicer

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Block is an editable SystemVerilog block.
type Block struct {
	ID        string   `json:"id"`
	File      string   `json:"file"`
	Kind      string   `json:"kind"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Clocking  string   `json:"clocking"`
	Writes    []string `json:"writes"`
	Reads     []string `json:"reads"`
	Code      string   `json:"code"`
}

var keywords = map[string]bool{
	"always": true, "always_ff": true, "always_comb": true, "always_latch": true,
	"assign": true, "automatic": true, "begin": true, "bit": true, "byte": true,
	"case": true, "casex": true, "casez": true, "const": true, "default": true,
	"else": true, "end": true, "endcase": true, "endfunction": true,
	"endgenerate": true, "endmodule": true, "endpackage": true, "endtask": true,
	"enum": true, "export": true, "final": true, "for": true, "foreach": true,
	"function": true, "generate": true, "genvar": true, "if": true, "import": true,
	"initial": true, "inout": true, "input": true, "int": true, "integer": true,
	"interface": true, "localparam": true, "logic": true, "longint": true,
	"module": true, "negedge": true, "output": true, "package": true,
	"parameter": true, "posedge": true, "priority": true, "reg": true,
	"repeat": true, "return": true, "shortint": true, "signed": true,
	"string": true, "struct": true, "task": true, "time": true, "typedef": true,
	"unique": true, "unsigned": true, "var": true, "void": true, "while": true,
	"wire": true,
}

var (
	startRe      = regexp.MustCompile(`^\s*(always_ff|always_comb|always_latch|always|assign)\b`)
	edgeRe       = regexp.MustCompile(`@\s*\(\s*(posedge|negedge)\s+([A-Za-z_]\w*)`)
	conditionRe  = regexp.MustCompile(`\b(if|while|for|repeat|case|assert|assume|cover)\s*\(`)
	assignRe     = regexp.MustCompile(`([A-Za-z_]\w*(?:\s*\[[^\]]*\])?)\s*(<=|=)`)
	identRe      = regexp.MustCompile(`[A-Za-z_]\w*`)
	literalRe    = regexp.MustCompile(`\d*'[sS]?[bBoOdDhH][0-9a-fA-FxXzZ_]+|'[01xXzZ]|\b\d+\b`)
	beginEndRe   = regexp.MustCompile(`\b(begin|end)\b|;`)
	arrayIndexRe = regexp.MustCompile(`\[[^\]]*\]`)
	elseRe       = regexp.MustCompile(`^\s*else\b`)
)

// ParseBlocks splits a source file into editable blocks.
func ParseBlocks(text, file string) []Block {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	blocks := []Block{}
	index := 0
	line := 0
	for line < len(lines) {
		match := startRe.FindStringSubmatch(lines[line])
		if match == nil {
			line++
			continue
		}
		end := findBlockEnd(lines, line)
		if end <= line {
			line++
			continue
		}
		kind := match[1]
		code := strings.Join(lines[line:end], "\n")
		clocking := "combinational"
		if edge := edgeRe.FindStringSubmatch(code); edge != nil {
			if kind != "assign" && kind != "always_comb" && kind != "always_latch" {
				clocking = edge[1] + " " + edge[2]
			}
		}
		writes := extractWrites(code)
		reads := extractReads(code, toStringSet(writes))
		id := "blk_" + strconv.Itoa(index)
		if file != "" {
			id = file + ":" + id
		}
		blocks = append(blocks, Block{
			ID:        id,
			File:      file,
			Kind:      kind,
			StartLine: line + 1,
			EndLine:   end,
			Clocking:  clocking,
			Writes:    writes,
			Reads:     reads,
			Code:      code,
		})
		index++
		line = end
	}

	if len(blocks) == 0 && strings.TrimSpace(text) != "" {
		body := strings.Trim(text, "\n")
		end := len(lines)
		if end < 1 {
			end = 1
		}
		id := "blk_0"
		if file != "" {
			id = file + ":blk_0"
		}
		writes := extractWrites(body)
		blocks = append(blocks, Block{
			ID:        id,
			File:      file,
			Kind:      "module_body",
			StartLine: 1,
			EndLine:   end,
			Clocking:  "unknown",
			Writes:    writes,
			Reads:     extractReads(body, toStringSet(writes)),
			Code:      body,
		})
	}
	return blocks
}

func findBlockEnd(lines []string, start int) int {
	depth := 0
	seenBegin := false
	for line := start; line < len(lines); line++ {
		for _, loc := range beginEndRe.FindAllStringIndex(lines[line], -1) {
			token := lines[line][loc[0]:loc[1]]
			switch token {
			case ";":
				if depth == 0 && !seenBegin {
					rest := lines[line][loc[1]:]
					if continuesWithElse(rest, lines, line) {
						continue
					}
					return line + 1
				}
			case "begin":
				seenBegin = true
				depth++
			case "end":
				depth--
				if seenBegin && depth <= 0 {
					return line + 1
				}
			}
		}
	}
	return len(lines)
}

func continuesWithElse(rest string, lines []string, line int) bool {
	if elseRe.MatchString(rest) {
		return true
	}
	for next := line + 1; next < len(lines); next++ {
		stripped := strings.TrimSpace(lines[next])
		if stripped == "" {
			continue
		}
		return strings.HasPrefix(stripped, "else")
	}
	return false
}

func stripConditions(code string) string {
	var builder strings.Builder
	cursor := 0
	for cursor < len(code) {
		loc := conditionRe.FindStringIndex(code[cursor:])
		if loc == nil {
			builder.WriteString(code[cursor:])
			break
		}
		start := cursor + loc[0]
		end := cursor + loc[1]
		builder.WriteString(code[cursor:start])
		depth := 1
		index := end
		for index < len(code) && depth > 0 {
			switch code[index] {
			case '(':
				depth++
			case ')':
				depth--
			}
			index++
		}
		cursor = index
		builder.WriteByte(' ')
	}
	return builder.String()
}

func extractWrites(code string) []string {
	clean := stripConditions(code)
	writes := map[string]bool{}
	for _, loc := range assignRe.FindAllStringSubmatchIndex(clean, -1) {
		fullStart, fullEnd := loc[0], loc[1]
		lhsStart, lhsEnd := loc[2], loc[3]
		opStart, opEnd := loc[4], loc[5]
		if fullStart > 0 && strings.ContainsRune("=!<>+-*/", rune(clean[fullStart-1])) {
			continue
		}
		operator := clean[opStart:opEnd]
		if operator == "=" && fullEnd < len(clean) && clean[fullEnd] == '=' {
			continue
		}
		lhs := arrayIndexRe.ReplaceAllString(clean[lhsStart:lhsEnd], "")
		parts := strings.Split(strings.TrimSpace(lhs), ".")
		name := strings.TrimSpace(parts[len(parts)-1])
		if name != "" && !keywords[name] {
			writes[name] = true
		}
	}
	return sortedKeys(writes)
}

func extractReads(code string, writes map[string]bool) []string {
	clean := literalRe.ReplaceAllString(code, " ")
	reads := map[string]bool{}
	for _, name := range identRe.FindAllString(clean, -1) {
		if keywords[name] || writes[name] {
			continue
		}
		reads[name] = true
	}
	return sortedKeys(reads)
}

// BuildDriverMap maps each written signal to the block IDs that drive it.
func BuildDriverMap(blocks []Block) map[string][]string {
	drivers := map[string][]string{}
	for _, block := range blocks {
		for _, signal := range block.Writes {
			drivers[signal] = append(drivers[signal], block.ID)
		}
	}
	return drivers
}

// DynamicSlice computes a backward slice from the target signals.
func DynamicSlice(blocks []Block, targets []string, depth int) ([]Block, []string) {
	byID := map[string]Block{}
	for _, block := range blocks {
		byID[block.ID] = block
	}
	drivers := BuildDriverMap(blocks)
	selected := map[string]int{}
	frontier := map[string]bool{}
	visited := map[string]bool{}
	for _, target := range targets {
		frontier[target] = true
		visited[target] = true
	}
	if depth < 1 {
		depth = 1
	}
	for level := 0; level < depth; level++ {
		next := map[string]bool{}
		for signal := range frontier {
			for _, blockID := range drivers[signal] {
				if _, exists := selected[blockID]; exists {
					continue
				}
				selected[blockID] = level
				for _, read := range byID[blockID].Reads {
					next[read] = true
				}
			}
		}
		for signal := range next {
			if visited[signal] {
				delete(next, signal)
			}
		}
		for signal := range next {
			visited[signal] = true
		}
		frontier = next
		if len(frontier) == 0 {
			break
		}
	}
	result := []Block{}
	for _, block := range blocks {
		if _, ok := selected[block.ID]; ok {
			result = append(result, block)
		}
	}
	return result, sortedKeys(visited)
}

// ParseSources parses RTL sources from a path-to-content map.
func ParseSources(sources map[string]string) []Block {
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	blocks := []Block{}
	for _, path := range paths {
		blocks = append(blocks, ParseBlocks(sources[path], path)...)
	}
	return blocks
}

func toStringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
