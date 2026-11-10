package slicer

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Port is a module/interface port declaration.
type Port struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`  // input, output, inout (empty when unknown)
	Type string `json:"type"` // raw type text, e.g. "logic [7:0]"
	Raw  string `json:"raw"`
}

// Segment is a structural piece of a design unit: header, declarations,
// process block, function or task.
type Segment struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Writes    []string `json:"writes,omitempty"`
	Reads     []string `json:"reads,omitempty"`
	Code      string   `json:"code"`
}

// Unit is a top-level SystemVerilog design unit (module, interface, package or
// program) with its header and structural segments.
type Unit struct {
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	File      string    `json:"file"`
	StartLine int       `json:"start_line"`
	EndLine   int       `json:"end_line"`
	Header    string    `json:"header"`
	Params    []string  `json:"params,omitempty"`
	Ports     []Port    `json:"ports,omitempty"`
	Blocks    []Block   `json:"blocks,omitempty"`
	Segments  []Segment `json:"segments,omitempty"`
}

var (
	unitStartRe  = regexp.MustCompile(`^\s*(module|interface|package|program)\b\s+(?:automatic\s+|static\s+)?([A-Za-z_]\w*)`)
	unitEndRe    = regexp.MustCompile(`^\s*(endmodule|endinterface|endpackage|endprogram)\b`)
	funcStartRe  = regexp.MustCompile(`^\s*(function|task)\b`)
	funcEndRe    = regexp.MustCompile(`\b(endfunction|endtask)\b`)
	paramListRe  = regexp.MustCompile(`#\s*\(`)
	dirRe        = regexp.MustCompile(`\b(input|output|inout)\b`)
	identTokenRe = regexp.MustCompile(`[A-Za-z_]\w*`)
	bracketRe    = regexp.MustCompile(`\[[^\]]*\]`)
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineComment  = regexp.MustCompile(`//[^\n]*`)
)

var svNonNameTokens = map[string]bool{
	"input": true, "output": true, "inout": true,
	"logic": true, "wire": true, "reg": true, "bit": true, "byte": true,
	"int": true, "integer": true, "longint": true, "shortint": true,
	"signed": true, "unsigned": true, "var": true, "const": true,
	"struct": true, "union": true, "enum": true, "packed": true, "tagged": true,
	"automatic": true, "static": true, "void": true, "string": true,
	"time": true, "real": true, "realtime": true, "shortreal": true,
	"parameter": true, "localparam": true, "type": true,
}

type unitRange struct {
	kind  string
	name  string
	start int // 0-based, inclusive
	end   int // 0-based, exclusive
}

// ParseStructure splits a SystemVerilog source into design units with headers,
// ports and structural segments. Files without a recognizable unit return nil.
func ParseStructure(text, file string) []Unit {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	ranges := findUnits(lines)
	if len(ranges) == 0 {
		return nil
	}

	blocks := ParseBlocks(normalized, file)
	units := make([]Unit, 0, len(ranges))
	for _, r := range ranges {
		headerEnd := headerEndLine(lines, r.start, r.end)
		header := strings.Join(lines[r.start:headerEnd], "\n")
		params, ports := parseHeader(header)
		if len(ports) == 0 || allPortsHaveNoDir(ports) {
			if bodyPorts := parseBodyPorts(lines, headerEnd, r.end); len(bodyPorts) > 0 {
				ports = bodyPorts
			}
		}

		unit := Unit{
			Kind:      r.kind,
			Name:      r.name,
			File:      file,
			StartLine: r.start + 1,
			EndLine:   r.end,
			Header:    strings.TrimSpace(header),
			Params:    params,
			Ports:     ports,
		}

		for _, block := range blocks {
			if block.StartLine >= unit.StartLine && block.EndLine <= unit.EndLine {
				unit.Blocks = append(unit.Blocks, block)
				unit.Segments = append(unit.Segments, Segment{
					ID:        block.ID,
					Kind:      block.Kind,
					StartLine: block.StartLine,
					EndLine:   block.EndLine,
					Writes:    block.Writes,
					Reads:     block.Reads,
					Code:      block.Code,
				})
			}
		}
		unit.Segments = append(unit.Segments, structureSegments(lines, r, file, headerEnd)...)
		sort.SliceStable(unit.Segments, func(i, j int) bool {
			if unit.Segments[i].StartLine == unit.Segments[j].StartLine {
				return unit.Segments[i].EndLine < unit.Segments[j].EndLine
			}
			return unit.Segments[i].StartLine < unit.Segments[j].StartLine
		})
		unit.Segments = addDeclarationsSegment(lines, r, headerEnd, unit.Segments)
		sort.SliceStable(unit.Segments, func(i, j int) bool {
			if unit.Segments[i].StartLine == unit.Segments[j].StartLine {
				return unit.Segments[i].EndLine < unit.Segments[j].EndLine
			}
			return unit.Segments[i].StartLine < unit.Segments[j].StartLine
		})
		units = append(units, unit)
	}
	return units
}

// ParseSourcesStructure parses every source in a path-to-content map.
func ParseSourcesStructure(sources map[string]string) []Unit {
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	units := []Unit{}
	for _, path := range paths {
		units = append(units, ParseStructure(sources[path], path)...)
	}
	return units
}

// HeaderOf renders a compact one-line signature for a unit.
func HeaderOf(unit Unit) string {
	header := strings.TrimSpace(unit.Header)
	header = strings.TrimSuffix(header, ";")
	header = strings.Join(strings.Fields(header), " ")
	if header == "" {
		return unit.Kind + " " + unit.Name
	}
	return header
}

// StructureIndex renders a compact structural map suitable for LLM context.
func StructureIndex(units []Unit) string {
	var builder strings.Builder
	for _, unit := range units {
		fmt.Fprintf(&builder, "%s: %s\n", unit.File, HeaderOf(unit))
		for _, segment := range unit.Segments {
			if segment.Kind == "module_header" || segment.Kind == "declarations" {
				continue
			}
			fmt.Fprintf(&builder, "  - %s %s L%d-%d", segment.ID, segment.Kind, segment.StartLine, segment.EndLine)
			if len(segment.Writes) > 0 {
				builder.WriteString(" writes=[" + strings.Join(segment.Writes, ",") + "]")
			}
			if len(segment.Reads) > 0 {
				builder.WriteString(" reads=[" + strings.Join(segment.Reads, ",") + "]")
			}
			builder.WriteString("\n")
		}
	}
	return strings.TrimRight(builder.String(), "\n")
}

// CompactStructureIndex is a bounded variant of StructureIndex for prompts. It
// lists at most maxPerUnit segments per design unit and summarizes the rest.
func CompactStructureIndex(units []Unit, maxPerUnit int) string {
	if maxPerUnit <= 0 {
		maxPerUnit = 80
	}
	var builder strings.Builder
	for _, unit := range units {
		fmt.Fprintf(&builder, "%s: %s\n", unit.File, HeaderOf(unit))

		total := 0
		counts := map[string]int{}
		for _, segment := range unit.Segments {
			if segment.Kind == "module_header" || segment.Kind == "declarations" {
				continue
			}
			total++
			counts[segment.Kind]++
		}
		if total == 0 {
			continue
		}

		kinds := make([]string, 0, len(counts))
		for kind := range counts {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		parts := make([]string, 0, len(kinds))
		for _, kind := range kinds {
			parts = append(parts, fmt.Sprintf("%s=%d", kind, counts[kind]))
		}
		fmt.Fprintf(&builder, "  summary: %d segment(s), %d line(s) [%s]\n",
			total, unit.EndLine-unit.StartLine+1, strings.Join(parts, " "))

		listed := 0
		for _, segment := range unit.Segments {
			if segment.Kind == "module_header" || segment.Kind == "declarations" {
				continue
			}
			if listed >= maxPerUnit {
				break
			}
			fmt.Fprintf(&builder, "  %s %s L%d-%d", shortSegmentID(segment.ID, unit.File), segment.Kind, segment.StartLine, segment.EndLine)
			if len(segment.Writes) > 0 {
				builder.WriteString(" w=" + strings.Join(segment.Writes, ","))
			}
			if len(segment.Reads) > 0 {
				builder.WriteString(" r=" + strings.Join(segment.Reads, ","))
			}
			builder.WriteString("\n")
			listed++
		}
		if total > listed {
			fmt.Fprintf(&builder, "  ... %d more segment(s) omitted\n", total-listed)
		}
	}
	return strings.TrimRight(builder.String(), "\n")
}

func shortSegmentID(id, file string) string {
	if file != "" && strings.HasPrefix(id, file+":") {
		return strings.TrimPrefix(id, file+":")
	}
	if index := strings.LastIndex(id, ":"); index >= 0 {
		return id[index+1:]
	}
	return id
}

func findUnits(lines []string) []unitRange {
	ranges := []unitRange{}
	stack := []unitRange{}
	for index, line := range lines {
		if match := unitStartRe.FindStringSubmatch(line); match != nil {
			stack = append(stack, unitRange{kind: match[1], name: match[2], start: index})
			continue
		}
		if unitEndRe.MatchString(line) {
			if len(stack) == 0 {
				continue
			}
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			top.end = index + 1
			ranges = append(ranges, top)
		}
	}
	for _, open := range stack {
		open.end = len(lines)
		ranges = append(ranges, open)
	}
	sort.SliceStable(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	return ranges
}

func headerEndLine(lines []string, start, limit int) int {
	depth := 0
	inBlockComment := false
	for index := start; index < limit; index++ {
		line := lines[index]
		for pos := 0; pos < len(line); pos++ {
			ch := line[pos]
			if inBlockComment {
				if ch == '*' && pos+1 < len(line) && line[pos+1] == '/' {
					inBlockComment = false
					pos++
				}
				continue
			}
			if ch == '/' && pos+1 < len(line) {
				if line[pos+1] == '/' {
					break
				}
				if line[pos+1] == '*' {
					inBlockComment = true
					pos++
					continue
				}
			}
			if ch == '"' {
				pos++
				for pos < len(line) {
					if line[pos] == '\\' {
						pos += 2
						continue
					}
					if line[pos] == '"' {
						break
					}
					pos++
				}
				continue
			}
			switch ch {
			case '(':
				depth++
			case ')':
				if depth > 0 {
					depth--
				}
			case ';':
				if depth == 0 {
					return index + 1
				}
			}
		}
	}
	return start + 1
}

func parseHeader(header string) ([]string, []Port) {
	rest := header
	params := []string{}

	if loc := paramListRe.FindStringIndex(rest); loc != nil {
		open := strings.IndexByte(rest[loc[0]:], '(')
		if open >= 0 {
			if content, end, ok := parenContent(rest, loc[0]+open); ok {
				params = compactList(splitTopLevel(content, ','))
				rest = rest[:loc[0]] + " " + rest[end+1:]
			}
		}
	}

	portText := ""
	if open := strings.IndexByte(rest, '('); open >= 0 {
		if content, _, ok := parenContent(rest, open); ok {
			portText = content
		}
	}
	return params, parsePorts(portText)
}

func parsePorts(portText string) []Port {
	if strings.TrimSpace(portText) == "" {
		return nil
	}
	parts := splitTopLevel(portText, ',')
	ports := make([]Port, 0, len(parts))
	currentDir := ""
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		clean := stripComments(part)
		dir := ""
		if match := dirRe.FindStringSubmatch(clean); match != nil {
			dir = strings.ToLower(match[1])
			currentDir = dir
		} else if currentDir != "" {
			dir = currentDir
		}
		name := portName(clean)
		if name == "" {
			continue
		}
		ports = append(ports, Port{
			Name: name,
			Dir:  dir,
			Type: portType(clean, name),
			Raw:  part,
		})
	}
	return ports
}

func parseBodyPorts(lines []string, start, limit int) []Port {
	bodyPortRe := regexp.MustCompile(`^\s*(input|output|inout)\b`)
	ports := []Port{}
	for index := start; index < limit; index++ {
		match := bodyPortRe.FindStringSubmatch(lines[index])
		if match == nil {
			continue
		}
		for _, part := range splitTopLevel(stripComments(lines[index]), ',') {
			name := portName(part)
			if name == "" {
				continue
			}
			ports = append(ports, Port{
				Name: name,
				Dir:  strings.ToLower(match[1]),
				Type: portType(part, name),
				Raw:  strings.TrimSpace(part),
			})
		}
	}
	return ports
}

func structureSegments(lines []string, r unitRange, file string, headerEnd int) []Segment {
	segments := []Segment{{
		ID:        file + ":header",
		Kind:      "module_header",
		StartLine: r.start + 1,
		EndLine:   headerEnd,
		Code:      strings.Join(lines[r.start:headerEnd], "\n"),
	}}

	functionCount := 0
	taskCount := 0
	for index := headerEnd; index < r.end; index++ {
		match := funcStartRe.FindStringSubmatch(lines[index])
		if match == nil {
			continue
		}
		kind := match[1]
		end := index
		for end < r.end && !funcEndRe.MatchString(lines[end]) {
			end++
		}
		if end >= r.end {
			end = r.end - 1
		}
		code := strings.Join(lines[index:end+1], "\n")
		writes := extractWrites(code)
		id := ""
		if kind == "function" {
			id = fmt.Sprintf("%s:fn_%d", file, functionCount)
			functionCount++
		} else {
			id = fmt.Sprintf("%s:task_%d", file, taskCount)
			taskCount++
		}
		segments = append(segments, Segment{
			ID:        id,
			Kind:      kind,
			StartLine: index + 1,
			EndLine:   end + 1,
			Writes:    writes,
			Reads:     extractReads(code, toStringSet(writes)),
			Code:      code,
		})
		index = end
	}
	return segments
}

func addDeclarationsSegment(lines []string, r unitRange, headerEnd int, segments []Segment) []Segment {
	first := r.end
	for _, segment := range segments {
		if segment.Kind == "module_header" {
			continue
		}
		if segment.StartLine < first {
			first = segment.StartLine
		}
	}
	end := first - 1
	if end <= headerEnd {
		return segments
	}
	code := strings.Join(lines[headerEnd:end], "\n")
	if strings.TrimSpace(code) == "" {
		return segments
	}
	declarations := Segment{
		ID:        fileOf(segments) + ":decls",
		Kind:      "declarations",
		StartLine: headerEnd + 1,
		EndLine:   end,
		Code:      code,
	}
	result := make([]Segment, 0, len(segments)+1)
	result = append(result, declarations)
	result = append(result, segments...)
	return result
}

func fileOf(segments []Segment) string {
	for _, segment := range segments {
		if index := strings.LastIndex(segment.ID, ":"); index > 0 {
			return segment.ID[:index]
		}
	}
	return ""
}

func allPortsHaveNoDir(ports []Port) bool {
	for _, port := range ports {
		if port.Dir != "" {
			return false
		}
	}
	return len(ports) > 0
}

func portName(segment string) string {
	cleaned := bracketRe.ReplaceAllString(segment, " ")
	tokens := identTokenRe.FindAllString(cleaned, -1)
	for index := len(tokens) - 1; index >= 0; index-- {
		if !svNonNameTokens[tokens[index]] {
			return tokens[index]
		}
	}
	return ""
}

func portType(segment, name string) string {
	cleaned := dirRe.ReplaceAllString(segment, " ")
	if index := strings.LastIndex(cleaned, name); index >= 0 {
		cleaned = cleaned[:index]
	}
	return strings.Join(strings.Fields(cleaned), " ")
}

func stripComments(value string) string {
	value = blockComment.ReplaceAllString(value, " ")
	return lineComment.ReplaceAllString(value, " ")
}

func compactList(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// splitTopLevel splits on sep ignoring separators nested in (), [] and {}.
func splitTopLevel(text string, sep byte) []string {
	parts := []string{}
	depth := 0
	start := 0
	inString := false
	for index := 0; index < len(text); index++ {
		ch := text[index]
		if inString {
			if ch == '\\' {
				index++
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			continue
		}
		switch ch {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		default:
			if ch == sep && depth == 0 {
				parts = append(parts, strings.TrimSpace(text[start:index]))
				start = index + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(text[start:]))
	return parts
}

// parenContent returns the text inside the parentheses that open at openIdx.
func parenContent(text string, openIdx int) (string, int, bool) {
	if openIdx < 0 || openIdx >= len(text) || text[openIdx] != '(' {
		return "", 0, false
	}
	depth := 0
	inString := false
	for index := openIdx; index < len(text); index++ {
		ch := text[index]
		if inString {
			if ch == '\\' {
				index++
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			continue
		}
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return text[openIdx+1 : index], index, true
			}
		}
	}
	return "", 0, false
}
