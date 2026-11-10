// Package trace turns a failed simulation into an actionable diagnosis:
// markers, a window of clock edges, VCD alignment diagnosis and suspect RTL
// blocks from a static backward slice.
package trace

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/asemoq/sv-multi-agent/internal/sv/markers"
	"github.com/asemoq/sv-multi-agent/internal/sv/slicer"
)

type edge struct {
	Time int64
	Kind string
}

type pair struct {
	expected *int
	actual   *int
}

// BuildTraceReport creates a JSON-serializable trace report for a failed run.
// When files is non-nil, it supplies the RTL sources; otherwise the workspace
// rtl/ directory is read.
func BuildTraceReport(workspace, runLog string, contract map[string]any, windowEdges, maxBlocks int, files map[string]string) map[string]any {
	var blocks []slicer.Block
	if files != nil {
		rtl := map[string]string{}
		for path, content := range files {
			if strings.HasSuffix(strings.ToLower(path), ".sv") {
				rtl[path] = content
			}
		}
		blocks = slicer.ParseSources(rtl)
	} else {
		blocks = parseWorkspaceSources(workspace)
	}

	result := markers.Parse(runLog)
	failingSignals := []string{}
	for _, output := range result.FailOutputs {
		failingSignals = append(failingSignals, output.Signal)
	}
	if len(failingSignals) == 0 {
		failingSignals = contractOutputs(contract)
	}

	report := map[string]any{
		"mismatch_count":      intPointerAny(result.MismatchCount),
		"samples":             intPointerAny(result.Samples),
		"fail_time":           intPointerAny(result.FailTime),
		"fail_outputs":        result.FailOutputs,
		"vcd_available":       false,
		"vcd_path":            nil,
		"reason":              nil,
		"clock":               nil,
		"window":              []any{},
		"alignment_diagnosis": map[string]any{},
		"timing_hints":        []any{},
		"suspect_blocks":      []slicer.Block{},
		"slice_signals":       []string{},
	}

	suspects, sliceSignals := suspectBlocks(blocks, failingSignals, maxBlocks)
	report["suspect_blocks"] = suspects
	report["slice_signals"] = sliceSignals

	if len(failingSignals) == 0 {
		report["reason"] = "no failing outputs were reported by the simulation"
		return report
	}

	vcdPath := findVCD(workspace)
	if vcdPath == "" {
		report["reason"] = "wave.vcd not found under the workspace"
		return report
	}
	report["vcd_path"] = relativePath(workspace, vcdPath)

	vcd, err := parseVCD(vcdPath)
	if err != nil {
		report["reason"] = "failed to read VCD: " + err.Error()
		return report
	}

	byLeaf := signalsByLeaf(vcd.Signals)
	clockName := "clk"
	if clocking, ok := contract["clocking"].(map[string]any); ok {
		if name := asString(clocking["clock"]); name != "" {
			clockName = name
		}
	}
	clockFull := findSignal(byLeaf, clockName, []string{"dut", "u_", "top"})
	if clockFull == "" {
		report["reason"] = "clock signal '" + clockName + "' not found in the waveform"
		return report
	}
	clockTimes, clockValues := series(vcd, clockFull)
	if len(clockTimes) == 0 {
		report["reason"] = "clock signal '" + clockName + "' has no transitions in the waveform"
		return report
	}
	edges := clockEdges(clockTimes, clockValues, result.FailTime, windowEdges)
	if len(edges) == 0 {
		report["reason"] = "no clock edges found before the first mismatch"
		return report
	}

	report["vcd_available"] = true
	report["clock"] = clockName
	edgeKinds := make([]string, len(edges))
	for i, item := range edges {
		edgeKinds[i] = item.Kind
	}

	type sampledValues struct {
		expected []*int
		actual   []*int
	}
	sampled := map[string]sampledValues{}
	diagnoses := map[string]any{}
	hints := []any{}
	sampleTimes := edgeTimes(edges)

	for _, signal := range failingSignals {
		actualFull := findSignal(byLeaf, signal, []string{"dut", "dut_", "u_"})
		if actualFull == "" {
			hints = append(hints, "No signal named '"+signal+"' found in the waveform.")
			continue
		}
		expectedFull := findExpected(byLeaf, signal)
		actual := sample(vcd, actualFull, sampleTimes)
		var expected []*int
		if expectedFull != "" {
			expected = sample(vcd, expectedFull, sampleTimes)
		}
		sampled[signal] = sampledValues{expected: expected, actual: actual}

		if expectedFull != "" {
			diagnosis := DiagnoseAlignment(expected, actual, edgeKinds)
			diagnoses[signal] = diagnosis
			for _, hint := range timingHints(signal, diagnosis) {
				hints = append(hints, hint)
			}
		} else {
			hints = append(hints, "No reference signal for '"+signal+"'; alignment diagnosis skipped.")
		}
	}

	window := []any{}
	for index, item := range edges {
		row := map[string]any{"time": item.Time, "edge": item.Kind}
		row[clockName] = valueAt(clockTimes, clockValues, item.Time)
		for signal, values := range sampled {
			if index < len(values.actual) {
				row[signal] = values.actual[index]
			}
			if index < len(values.expected) {
				row[signal+"_expected"] = values.expected[index]
			}
		}
		window = append(window, row)
	}
	report["window"] = window
	report["alignment_diagnosis"] = diagnoses
	report["timing_hints"] = hints

	if best := bestAlignment(diagnoses); best != "" {
		report["best_alignment"] = best
	}
	return report
}

// DiagnoseAlignment compares expected/actual samples under current, lag and lead
// alignment.
func DiagnoseAlignment(expected, actual []*int, edgeKinds []string) map[string]any {
	if len(edgeKinds) == 0 {
		edgeKinds = make([]string, len(actual))
		for i := range edgeKinds {
			edgeKinds[i] = "sample"
		}
	}
	current := zip(expected, actual)
	lagPairs := zip(limit(expected, len(expected)-1), offset(actual, 1))
	leadPairs := zip(offset(expected, 1), limit(actual, len(actual)-1))
	lag2Pairs := zip(limit(expected, len(expected)-2), offset(actual, 2))
	lead2Pairs := zip(offset(expected, 2), limit(actual, len(actual)-2))

	posedge := []pair{}
	negedge := []pair{}
	for index, item := range current {
		kind := "sample"
		if index < len(edgeKinds) {
			kind = edgeKinds[index]
		}
		switch kind {
		case "posedge":
			posedge = append(posedge, item)
		case "negedge":
			negedge = append(negedge, item)
		}
	}

	rates := map[string]*float64{
		"current":   rate(current),
		"dut_lag1":  rate(lagPairs),
		"dut_lag2":  rate(lag2Pairs),
		"dut_lead1": rate(leadPairs),
		"dut_lead2": rate(lead2Pairs),
	}
	best := ""
	bestRate := -1.0
	for _, name := range []string{"current", "dut_lag1", "dut_lag2", "dut_lead1", "dut_lead2"} {
		if value := rates[name]; value != nil && *value > bestRate {
			best, bestRate = name, *value
		}
	}

	return map[string]any{
		"best_alignment":       best,
		"match_rate_current":   round(rates["current"]),
		"match_rate_dut_lag1":  round(rates["dut_lag1"]),
		"match_rate_dut_lag2":  round(rates["dut_lag2"]),
		"match_rate_dut_lead1": round(rates["dut_lead1"]),
		"match_rate_dut_lead2": round(rates["dut_lead2"]),
		"match_rate_posedge":   round(rate(posedge)),
		"match_rate_negedge":   round(rate(negedge)),
		"samples":              len(current),
	}
}

func parseWorkspaceSources(workspace string) []slicer.Block {
	sources := map[string]string{}
	root := filepath.Join(workspace, "rtl")
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".sv") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		relative, relErr := filepath.Rel(workspace, path)
		if relErr != nil {
			return nil
		}
		sources[filepath.ToSlash(relative)] = string(data)
		return nil
	})
	return slicer.ParseSources(sources)
}

func contractOutputs(contract map[string]any) []string {
	outputs := []string{}
	io, _ := contract["io"].([]any)
	for _, item := range io {
		port, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if strings.ToLower(asString(port["dir"])) == "output" {
			if name := asString(port["name"]); name != "" {
				outputs = append(outputs, name)
			}
		}
	}
	return outputs
}

func suspectBlocks(blocks []slicer.Block, failingSignals []string, maxBlocks int) ([]slicer.Block, []string) {
	if len(blocks) == 0 {
		return []slicer.Block{}, []string{}
	}
	sliced, signals := slicer.DynamicSlice(blocks, failingSignals, 3)
	if len(sliced) == 0 {
		sliced = blocks
	}
	if maxBlocks > 0 && len(sliced) > maxBlocks {
		sliced = sliced[:maxBlocks]
	}
	return sliced, signals
}

func findVCD(workspace string) string {
	direct := filepath.Join(workspace, "wave.vcd")
	if _, err := os.Stat(direct); err == nil {
		return direct
	}
	candidates := []string{}
	_ = filepath.WalkDir(workspace, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !entry.IsDir() && strings.HasSuffix(strings.ToLower(path), ".vcd") {
			candidates = append(candidates, path)
		}
		return nil
	})
	sort.Strings(candidates)
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

func relativePath(root, path string) string {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(relative)
}

func leaf(name string) string {
	parts := strings.Split(name, ".")
	last := parts[len(parts)-1]
	if index := strings.Index(last, "["); index >= 0 {
		last = last[:index]
	}
	return last
}

func signalsByLeaf(signals []string) map[string][]string {
	byLeaf := map[string][]string{}
	for _, name := range signals {
		key := leaf(name)
		byLeaf[key] = append(byLeaf[key], name)
	}
	return byLeaf
}

type signalKey struct {
	preferred int
	dots      int
	length    int
}

func keyFor(name string, preferred []string) signalKey {
	lowered := strings.ToLower(name)
	isPreferred := 1
	for _, token := range preferred {
		if strings.Contains(lowered, token) {
			isPreferred = 0
			break
		}
	}
	return signalKey{preferred: isPreferred, dots: strings.Count(name, "."), length: len(name)}
}

func findSignal(byLeaf map[string][]string, name string, preferred []string) string {
	candidates := append([]string{}, byLeaf[name]...)
	if len(candidates) == 0 {
		return ""
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := keyFor(candidates[i], preferred), keyFor(candidates[j], preferred)
		if a.preferred != b.preferred {
			return a.preferred < b.preferred
		}
		if a.dots != b.dots {
			return a.dots < b.dots
		}
		return a.length < b.length
	})
	return candidates[0]
}

func findExpected(byLeaf map[string][]string, signal string) string {
	candidates := []string{
		signal + "_ref",
		signal + "_expected",
		"ref_" + signal,
		"expected_" + signal,
		signal + "_golden",
		"expected",
		"reference",
	}
	for _, candidate := range candidates {
		if found := findSignal(byLeaf, candidate, []string{"dut", "u_", "top"}); found != "" {
			return found
		}
	}
	return ""
}

func decode(value string, radix byte) *int {
	text := strings.TrimSpace(value)
	if text == "" {
		return nil
	}
	if strings.ContainsAny(text, "xXzZ") {
		return nil
	}
	// Prefer the radix recorded in the VCD, then fall back to the other base.
	bases := []int{10, 2}
	if radix == 'b' || radix == 's' || radix == 0 {
		bases = []int{2, 10}
	}
	for _, base := range bases {
		if number, err := strconv.ParseInt(text, base, 64); err == nil {
			result := int(number)
			return &result
		}
	}
	return nil
}

func series(vcd *vcdData, fullName string) ([]int64, []*int) {
	values := vcd.series[fullName]
	times := make([]int64, 0, len(values))
	decoded := make([]*int, 0, len(values))
	for _, item := range values {
		times = append(times, item.Time)
		decoded = append(decoded, decode(item.Value, item.Radix))
	}
	return times, decoded
}

func sample(vcd *vcdData, fullName string, sampleTimes []int64) []*int {
	times, values := series(vcd, fullName)
	result := make([]*int, 0, len(sampleTimes))
	for _, at := range sampleTimes {
		result = append(result, valueAt(times, values, at))
	}
	return result
}

func valueAt(times []int64, values []*int, at int64) *int {
	index := sort.Search(len(times), func(i int) bool { return times[i] > at }) - 1
	if index < 0 {
		return nil
	}
	return values[index]
}

func clockEdges(times []int64, values []*int, failTime *int, limit int) []edge {
	edges := []edge{}
	var previous *int
	for index, value := range values {
		if value == nil {
			continue
		}
		if previous != nil && *value != *previous {
			if *previous == 0 && *value == 1 {
				edges = append(edges, edge{Time: times[index], Kind: "posedge"})
			} else if *previous == 1 && *value == 0 {
				edges = append(edges, edge{Time: times[index], Kind: "negedge"})
			}
		}
		previous = value
	}
	if failTime != nil {
		filtered := edges[:0]
		for _, item := range edges {
			if item.Time <= int64(*failTime) {
				filtered = append(filtered, item)
			}
		}
		edges = filtered
	}
	if limit < 1 {
		limit = 1
	}
	if len(edges) > limit {
		edges = edges[len(edges)-limit:]
	}
	return edges
}

func edgeTimes(edges []edge) []int64 {
	times := make([]int64, len(edges))
	for index, item := range edges {
		times[index] = item.Time
	}
	return times
}

func zip(expected, actual []*int) []pair {
	length := len(expected)
	if len(actual) < length {
		length = len(actual)
	}
	result := make([]pair, 0, length)
	for index := 0; index < length; index++ {
		result = append(result, pair{expected: expected[index], actual: actual[index]})
	}
	return result
}

func rate(pairs []pair) *float64 {
	compared := 0
	matches := 0
	for _, item := range pairs {
		if item.expected != nil && item.actual != nil {
			compared++
			if *item.expected == *item.actual {
				matches++
			}
		}
	}
	if compared == 0 {
		return nil
	}
	value := float64(matches) / float64(compared)
	return &value
}

func round(value *float64) *float64 {
	if value == nil {
		return nil
	}
	rounded := math.Round(*value*1000) / 1000
	return &rounded
}

func timingHints(signal string, diagnosis map[string]any) []string {
	hints := []string{}
	best, _ := diagnosis["best_alignment"].(string)
	switch best {
	case "dut_lag1":
		hints = append(hints, "Output '"+signal+"' matches the reference one cycle later: possible off-by-one-cycle latency.")
	case "dut_lag2":
		hints = append(hints, "Output '"+signal+"' matches the reference two cycles later: possible extra pipeline stage.")
	case "dut_lead1":
		hints = append(hints, "Output '"+signal+"' matches the reference one cycle earlier: possible premature update.")
	case "dut_lead2":
		hints = append(hints, "Output '"+signal+"' matches the reference two cycles earlier: possible missing pipeline stage.")
	}
	posedge, posedgeOK := floatPointer(diagnosis["match_rate_posedge"])
	negedge, negedgeOK := floatPointer(diagnosis["match_rate_negedge"])
	if posedgeOK && negedgeOK && *posedge-*negedge >= 0.25 {
		hints = append(hints, "Output '"+signal+"' matches much better on posedge than negedge: check the clock edge or reset type.")
	}
	return hints
}

func bestAlignment(diagnoses map[string]any) string {
	best := ""
	bestScore := 0.0
	alignments := []string{"dut_lag1", "dut_lag2", "dut_lead1", "dut_lead2"}
	for _, value := range diagnoses {
		diagnosis, ok := value.(map[string]any)
		if !ok {
			continue
		}
		for _, alignment := range alignments {
			if diagnosis["best_alignment"] != alignment {
				continue
			}
			score, ok := floatPointer(diagnosis["match_rate_"+alignment])
			if ok && *score > bestScore {
				best, bestScore = alignment, *score
			}
		}
	}
	return best
}

func floatPointer(value any) (*float64, bool) {
	if pointer, ok := value.(*float64); ok && pointer != nil {
		return pointer, true
	}
	return nil, false
}

func limit(values []*int, end int) []*int {
	if end < 0 {
		end = 0
	}
	if end > len(values) {
		end = len(values)
	}
	return values[:end]
}

func offset(values []*int, start int) []*int {
	if start < 0 {
		start = 0
	}
	if start > len(values) {
		start = len(values)
	}
	return values[start:]
}

func intPointerAny(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func asString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}
