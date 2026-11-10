// Package markers parses the machine-readable simulation marker protocol.
package markers

import (
	"regexp"
	"strconv"
	"strings"
)

// FailOutput describes a failing signal from a simulation hint.
type FailOutput struct {
	Signal     string `json:"signal"`
	Mismatches int    `json:"mismatches"`
	Time       int    `json:"time"`
}

// Result holds the structured result parsed from a simulation log.
type Result struct {
	PassedMarker  bool
	FailedMarker  bool
	MismatchCount *int
	Samples       *int
	FailTime      *int
	FailOutputs   []FailOutput
}

// Issue is a structured simulator issue.
type Issue struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

var (
	passRe       = regexp.MustCompile(`(?i)\bSIMULATION PASSED\b`)
	legacyPassRe = regexp.MustCompile(`\bPASS\b`)
	failedRe     = regexp.MustCompile(`(?i)SIMULATION FAILED\s*-\s*(?P<count>\d+)\s+MISMATCHES DETECTED[^\n]*?FIRST AT TIME\s*(?P<time>\d+)`)
	mismatchesRe = regexp.MustCompile(`(?im)^Mismatches:\s*(?P<count>\d+)\s+in\s+(?P<samples>\d+)\s+samples\b`)
	hintRe       = regexp.MustCompile(`(?i)Hint:\s+Output\s+'(?P<signal>[^']+)'\s+has\s+(?P<count>\d+)\s+mismatches\.\s+First mismatch occurred at time\s+(?P<time>\d+)`)
	errorRe      = regexp.MustCompile(`(?im)%Error|^Error\b|\bfatal\b`)
)

// LegacyPassRe matches a bare PASS marker.
var LegacyPassRe = legacyPassRe

// Parse extracts structured results from a simulation log.
func Parse(log string) Result {
	result := Result{
		PassedMarker: passRe.MatchString(log),
		FailOutputs:  []FailOutput{},
	}

	if match := failedRe.FindStringSubmatch(log); match != nil {
		result.FailedMarker = true
		count := groupInt(failedRe, match, "count")
		failTime := groupInt(failedRe, match, "time")
		result.MismatchCount = &count
		result.FailTime = &failTime
	}

	if match := mismatchesRe.FindStringSubmatch(log); match != nil {
		if result.MismatchCount == nil {
			count := groupInt(mismatchesRe, match, "count")
			result.MismatchCount = &count
		}
		samples := groupInt(mismatchesRe, match, "samples")
		result.Samples = &samples
	}

	seen := map[string]bool{}
	for _, match := range hintRe.FindAllStringSubmatch(log, -1) {
		signal := groupString(hintRe, match, "signal")
		if signal == "" || seen[signal] {
			continue
		}
		seen[signal] = true
		result.FailOutputs = append(result.FailOutputs, FailOutput{
			Signal:     signal,
			Mismatches: groupInt(hintRe, match, "count"),
			Time:       groupInt(hintRe, match, "time"),
		})
	}

	if result.MismatchCount == nil && len(result.FailOutputs) > 0 {
		total := 0
		for _, output := range result.FailOutputs {
			total += output.Mismatches
		}
		result.MismatchCount = &total
	}
	if result.FailTime == nil && len(result.FailOutputs) > 0 {
		earliest := result.FailOutputs[0].Time
		for _, output := range result.FailOutputs[1:] {
			if output.Time < earliest {
				earliest = output.Time
			}
		}
		result.FailTime = &earliest
	}
	return result
}

// ErrorLines returns the first few error-looking lines from a simulation log.
func ErrorLines(log string, limit int) []string {
	lines := []string{}
	for _, line := range strings.Split(log, "\n") {
		stripped := strings.TrimSpace(line)
		if stripped == "" {
			continue
		}
		if errorRe.MatchString(stripped) {
			if len(stripped) > 500 {
				stripped = stripped[:500]
			}
			lines = append(lines, stripped)
			if len(lines) >= limit {
				break
			}
		}
	}
	return lines
}

// HasErrors reports whether the log contains error-looking lines.
func HasErrors(log string) bool {
	return errorRe.MatchString(log)
}

// BuildIssue builds a single structured issue describing a failed run.
func BuildIssue(log string, result Result) Issue {
	if errors := ErrorLines(log, 5); len(errors) > 0 {
		return Issue{Severity: "error", Code: "simulation.error", Message: errors[0]}
	}
	if result.FailedMarker {
		return Issue{
			Severity: "error",
			Code:     "simulation.mismatch",
			Message:  "SIMULATION FAILED with " + intText(result.MismatchCount) + " mismatches; first mismatch at time " + intText(result.FailTime),
		}
	}
	return Issue{
		Severity: "error",
		Code:     "simulation.marker",
		Message:  "No SIMULATION PASSED marker found in the simulation log",
	}
}

func groupInt(re *regexp.Regexp, match []string, name string) int {
	value, _ := strconv.Atoi(groupString(re, match, name))
	return value
}

func groupString(re *regexp.Regexp, match []string, name string) string {
	for index, groupName := range re.SubexpNames() {
		if groupName == name && index < len(match) {
			return match[index]
		}
	}
	return ""
}

func intText(value *int) string {
	if value == nil {
		return "unknown"
	}
	return strconv.Itoa(*value)
}
