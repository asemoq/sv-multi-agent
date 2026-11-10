package trace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asemoq/sv-multi-agent/internal/sv/markers"
	"github.com/asemoq/sv-multi-agent/internal/sv/slicer"
)

const rtlSource = `module counter (
    input  logic clk,
    input  logic rst,
    output logic [7:0] count
);
    always_ff @(posedge clk) begin
        if (rst) begin
            count <= '0;
        end else begin
            count <= count + 1'b1;
        end
    end
endmodule
`

const failedLog = `
Mismatches: 2 in 4 samples
SIMULATION FAILED - 2 MISMATCHES DETECTED, FIRST AT TIME 25
Hint: Output 'count' has 2 mismatches. First mismatch occurred at time 25.
`

const vcdText = `$date today $end
$timescale 1ns $end
$scope module tb $end
$var wire 1 ! clk $end
$var wire 8 # count $end
$var wire 8 $ expected $end
$upscope $end
$enddefinitions $end
#0
0!
b0 #
b0 $
#5
1!
#10
0!
#15
1!
b1 #
b1 $
#20
0!
#25
1!
b2 #
b2 $
#30
0!
`

func intPtr(value int) *int {
	return &value
}

func intValues(values ...int) []*int {
	result := make([]*int, 0, len(values))
	for _, value := range values {
		result = append(result, intPtr(value))
	}
	return result
}

func rateValue(t *testing.T, diagnosis map[string]any, key string) float64 {
	t.Helper()
	value, ok := diagnosis[key].(*float64)
	if !ok || value == nil {
		t.Fatalf("%s is not a rate: %#v", key, diagnosis[key])
	}
	return *value
}

func TestCurrentAlignment(t *testing.T) {
	diagnosis := DiagnoseAlignment(intValues(0, 1, 2), intValues(0, 1, 2), nil)
	if diagnosis["best_alignment"] != "current" {
		t.Fatalf("best = %v", diagnosis["best_alignment"])
	}
	if got := rateValue(t, diagnosis, "match_rate_current"); got != 1.0 {
		t.Fatalf("current = %v", got)
	}
}

func TestDUTLagIsDetected(t *testing.T) {
	diagnosis := DiagnoseAlignment(intValues(1, 2, 3, 4, 5), intValues(0, 1, 2, 3, 4), nil)
	if diagnosis["best_alignment"] != "dut_lag1" {
		t.Fatalf("best = %v", diagnosis["best_alignment"])
	}
	if got := rateValue(t, diagnosis, "match_rate_dut_lag1"); got != 1.0 {
		t.Fatalf("lag = %v", got)
	}
	if got := rateValue(t, diagnosis, "match_rate_current"); got != 0.0 {
		t.Fatalf("current = %v", got)
	}
}

func TestDUTLeadIsDetected(t *testing.T) {
	diagnosis := DiagnoseAlignment(intValues(0, 1, 2, 3), intValues(1, 2, 3, 4), nil)
	if diagnosis["best_alignment"] != "dut_lead1" {
		t.Fatalf("best = %v", diagnosis["best_alignment"])
	}
	if got := rateValue(t, diagnosis, "match_rate_dut_lead1"); got != 1.0 {
		t.Fatalf("lead = %v", got)
	}
}

func TestEdgeSpecificRates(t *testing.T) {
	diagnosis := DiagnoseAlignment(
		intValues(0, 0, 1, 1),
		intValues(0, 1, 1, 0),
		[]string{"posedge", "negedge", "posedge", "negedge"},
	)
	if got := rateValue(t, diagnosis, "match_rate_posedge"); got != 1.0 {
		t.Fatalf("posedge = %v", got)
	}
	if got := rateValue(t, diagnosis, "match_rate_negedge"); got != 0.0 {
		t.Fatalf("negedge = %v", got)
	}
	if diagnosis["samples"] != 4 {
		t.Fatalf("samples = %v", diagnosis["samples"])
	}
}

func TestNoneSamplesAreIgnored(t *testing.T) {
	expected := append(intValues(0, 1), nil)
	diagnosis := DiagnoseAlignment(expected, intValues(0, 1, 5), nil)
	if got := rateValue(t, diagnosis, "match_rate_current"); got != 1.0 {
		t.Fatalf("current = %v", got)
	}
	if diagnosis["samples"] != 3 {
		t.Fatalf("samples = %v", diagnosis["samples"])
	}
}

func TestReportWithoutVCDStillHasSuspectBlocks(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "rtl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rtl", "counter.sv"), []byte(rtlSource), 0o644); err != nil {
		t.Fatal(err)
	}

	report := BuildTraceReport(dir, failedLog, nil, 10, 8, nil)
	if report["vcd_available"] != false {
		t.Fatalf("vcd_available = %v", report["vcd_available"])
	}
	if report["mismatch_count"] != 2 || report["samples"] != 4 || report["fail_time"] != 25 {
		t.Fatalf("markers = %#v", report)
	}
	outputs := report["fail_outputs"].([]markers.FailOutput)
	if len(outputs) == 0 || outputs[0].Signal != "count" {
		t.Fatalf("fail_outputs = %#v", outputs)
	}
	reason, _ := report["reason"].(string)
	if !strings.Contains(reason, "wave.vcd") {
		t.Fatalf("reason = %q", reason)
	}
	blocks := report["suspect_blocks"].([]slicer.Block)
	if len(blocks) == 0 || blocks[0].ID != "rtl/counter.sv:blk_0" {
		t.Fatalf("suspect_blocks = %#v", blocks)
	}
}

func TestContractOutputsAreUsedWhenHintsAreAbsent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "rtl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rtl", "counter.sv"), []byte(rtlSource), 0o644); err != nil {
		t.Fatal(err)
	}
	contract := map[string]any{
		"io":       []any{map[string]any{"name": "count", "dir": "output", "width": 8}},
		"clocking": map[string]any{"clock": "clk"},
	}
	log := "SIMULATION FAILED - 1 MISMATCHES DETECTED, FIRST AT TIME 10\n"
	report := BuildTraceReport(dir, log, contract, 10, 8, nil)
	blocks := report["suspect_blocks"].([]slicer.Block)
	if len(blocks) == 0 {
		t.Fatal("expected suspect blocks")
	}
	found := false
	for _, write := range blocks[0].Writes {
		if write == "count" {
			found = true
		}
	}
	if !found {
		t.Fatalf("writes = %#v", blocks[0].Writes)
	}
}

func TestReportPicksUpVCDWhenAvailable(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "rtl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rtl", "counter.sv"), []byte(rtlSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wave.vcd"), []byte(vcdText), 0o644); err != nil {
		t.Fatal(err)
	}

	report := BuildTraceReport(dir, failedLog, nil, 10, 8, nil)
	if report["vcd_available"] != true {
		t.Fatalf("vcd_available = %v (reason=%v)", report["vcd_available"], report["reason"])
	}
	if report["clock"] != "clk" {
		t.Fatalf("clock = %v", report["clock"])
	}
	if window := report["window"].([]any); len(window) != 5 {
		t.Fatalf("window = %d", len(window))
	}
	diagnoses := report["alignment_diagnosis"].(map[string]any)
	diagnosis, ok := diagnoses["count"].(map[string]any)
	if !ok {
		t.Fatalf("diagnosis = %#v", diagnoses)
	}
	if diagnosis["best_alignment"] != "current" {
		t.Fatalf("best = %v", diagnosis["best_alignment"])
	}
	if got := rateValue(t, diagnosis, "match_rate_current"); got != 1.0 {
		t.Fatalf("current = %v", got)
	}
	if _, ok := report["timing_hints"].([]any); !ok {
		t.Fatalf("timing_hints = %#v", report["timing_hints"])
	}
}
