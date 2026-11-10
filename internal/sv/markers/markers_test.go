package markers

import (
	"strings"
	"testing"
)

const failedLog = `
Mismatches: 3 in 12 samples
SIMULATION FAILED - 3 MISMATCHES DETECTED, FIRST AT TIME 45
Hint: Output 'count' has 2 mismatches. First mismatch occurred at time 45.
Hint: Output 'valid' has 1 mismatches. First mismatch occurred at time 60.
`

func TestPassedMarker(t *testing.T) {
	result := Parse("Mismatches: 0 in 10 samples\nSIMULATION PASSED\n")
	if !result.PassedMarker || result.FailedMarker {
		t.Fatalf("result = %#v", result)
	}
	if result.MismatchCount == nil || *result.MismatchCount != 0 {
		t.Fatalf("mismatch = %v", result.MismatchCount)
	}
	if result.Samples == nil || *result.Samples != 10 {
		t.Fatalf("samples = %v", result.Samples)
	}
}

func TestFailedMarkers(t *testing.T) {
	result := Parse(failedLog)
	if result.PassedMarker || !result.FailedMarker {
		t.Fatalf("result = %#v", result)
	}
	if result.MismatchCount == nil || *result.MismatchCount != 3 {
		t.Fatalf("mismatch = %v", result.MismatchCount)
	}
	if result.Samples == nil || *result.Samples != 12 {
		t.Fatalf("samples = %v", result.Samples)
	}
	if result.FailTime == nil || *result.FailTime != 45 {
		t.Fatalf("fail_time = %v", result.FailTime)
	}
	if len(result.FailOutputs) != 2 {
		t.Fatalf("fail_outputs = %#v", result.FailOutputs)
	}
	if result.FailOutputs[0].Signal != "count" || result.FailOutputs[0].Mismatches != 2 || result.FailOutputs[0].Time != 45 {
		t.Fatalf("fail_outputs[0] = %#v", result.FailOutputs[0])
	}
}

func TestDuplicateHintsAreCollapsed(t *testing.T) {
	log := failedLog + "Hint: Output 'count' has 9 mismatches. First mismatch occurred at time 1.\n"
	result := Parse(log)
	if len(result.FailOutputs) != 2 {
		t.Fatalf("fail_outputs = %#v", result.FailOutputs)
	}
}

func TestLegacyPassDoesNotMatchSimulationPassed(t *testing.T) {
	if !LegacyPassRe.MatchString(`$display("PASS")`) {
		t.Fatal("expected legacy PASS match")
	}
	if LegacyPassRe.MatchString("SIMULATION PASSED") {
		t.Fatal("SIMULATION PASSED must not match legacy PASS")
	}
}

func TestErrorsAreDetected(t *testing.T) {
	log := "%Error: Cannot find file\n%Warning: unused signal\nError: bad value\n"
	if !HasErrors(log) {
		t.Fatal("expected errors")
	}
	lines := ErrorLines(log, 5)
	if len(lines) != 2 {
		t.Fatalf("lines = %#v", lines)
	}
	if !strings.Contains(lines[0], "%Error") {
		t.Fatalf("lines[0] = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "Error:") {
		t.Fatalf("lines[1] = %q", lines[1])
	}
}

func TestFailureIssueUsesMarkerDetails(t *testing.T) {
	result := Parse(failedLog)
	issue := BuildIssue(failedLog, result)
	if issue.Code != "simulation.mismatch" {
		t.Fatalf("code = %q", issue.Code)
	}
	if !strings.Contains(issue.Message, "3 mismatches") {
		t.Fatalf("message = %q", issue.Message)
	}
}

func TestMissingPassMarkerIssue(t *testing.T) {
	issue := BuildIssue("nothing happened\n", Parse("nothing happened\n"))
	if issue.Code != "simulation.marker" {
		t.Fatalf("code = %q", issue.Code)
	}
}
