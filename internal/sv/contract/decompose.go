package contract

import "strings"

// EstimateLines estimates the RTL size implied by a design contract. The
// estimate is deliberately rough: it only decides whether a design is large
// enough to be worth decomposing.
func EstimateLines(contract map[string]any) int {
	normalized := Normalize(contract)
	io := asSlice(normalized["io"])
	parameters, _ := normalized["parameters"].(map[string]any)
	corners := asSlice(normalized["corner_cases"])
	tests := asSlice(normalized["test_plan"])
	summaryWords := len(strings.Fields(asString(normalized["functional_summary"])))

	return 40 +
		len(io)*8 +
		len(parameters)*6 +
		len(corners)*12 +
		len(tests)*6 +
		summaryWords/2
}

// ShouldDecompose reports whether a contract is large enough to justify a
// hierarchical decomposition. minLines <= 0 falls back to 400.
func ShouldDecompose(contract map[string]any, minLines int) bool {
	if minLines <= 0 {
		minLines = 400
	}
	return EstimateLines(contract) >= minLines
}
