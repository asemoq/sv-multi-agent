package contract

import (
	"strconv"
	"testing"
)

func smallCounterContract() map[string]any {
	return map[string]any{
		"module_name": "counter",
		"parameters":  map[string]any{"WIDTH": 8},
		"io": []any{
			map[string]any{"name": "clk", "dir": "input", "width": 1},
			map[string]any{"name": "rst", "dir": "input", "width": 1},
			map[string]any{"name": "en", "dir": "input", "width": 1},
			map[string]any{"name": "count", "dir": "output", "width": "WIDTH"},
		},
		"functional_summary": "Increment while enabled, hold otherwise.",
		"corner_cases":       []any{"wrap-around", "reset priority"},
		"test_plan":          []any{"reset", "increment", "hold"},
	}
}

func largeContract(ports int) map[string]any {
	io := make([]any, 0, ports)
	for index := 0; index < ports; index++ {
		io = append(io, map[string]any{
			"name":  "signal_" + strconv.Itoa(index),
			"dir":   "input",
			"width": 8,
		})
	}
	return map[string]any{
		"module_name":        "dsp_pipeline",
		"io":                 io,
		"functional_summary": "Multi-stage pipeline with many independent channels and complex control.",
		"corner_cases":       []any{"stall", "flush", "backpressure", "saturation", "overflow"},
		"test_plan":          []any{"stream", "stall", "flush"},
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func TestShouldDecomposeSmallContract(t *testing.T) {
	if ShouldDecompose(smallCounterContract(), 400) {
		t.Fatalf("counter estimated at %d lines", EstimateLines(smallCounterContract()))
	}
}

func TestShouldDecomposeLargeContract(t *testing.T) {
	large := largeContract(60)
	if !ShouldDecompose(large, 400) {
		t.Fatalf("large design estimated at %d lines", EstimateLines(large))
	}
}

func TestShouldDecomposeDefaultThreshold(t *testing.T) {
	large := largeContract(60)
	if !ShouldDecompose(large, 0) {
		t.Fatal("zero threshold should fall back to 400")
	}
}
