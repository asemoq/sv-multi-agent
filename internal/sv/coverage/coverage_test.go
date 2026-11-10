package coverage

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLCOV(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sim")
	data := strings.Join([]string{
		"TN:",
		"SF:" + filepath.Join(root, "rtl", "counter.sv"),
		"DA:10,1",
		"DA:11,0",
		"DA:12,3",
		"LF:3",
		"LH:2",
		"end_of_record",
		"TN:",
		"SF:" + filepath.Join(root, "tb", "tb_counter.sv"),
		"DA:5,1",
		"end_of_record",
	}, "\n")

	report := ParseLCOV([]byte(data), root)
	if report.LinesTotal != 4 || report.LinesCovered != 3 {
		t.Fatalf("report = %#v", report)
	}
	if report.LineCoverage != 0.75 {
		t.Fatalf("coverage = %v, want 0.75", report.LineCoverage)
	}
	if len(report.Files) != 2 {
		t.Fatalf("files = %#v", report.Files)
	}
	if report.Files[0].Path != "rtl/counter.sv" {
		t.Fatalf("path = %q", report.Files[0].Path)
	}
}

func TestUncovered(t *testing.T) {
	report := ParseLCOV([]byte(strings.Join([]string{
		"SF:rtl/counter.sv",
		"DA:4,0",
		"DA:5,1",
		"DA:6,0",
		"end_of_record",
	}, "\n")), "")

	items := report.Uncovered(0)
	if len(items) != 1 || len(items[0].Lines) != 2 {
		t.Fatalf("items = %#v", items)
	}
	if items[0].Lines[0] != 4 || items[0].Lines[1] != 6 {
		t.Fatalf("lines = %#v", items[0].Lines)
	}
	text := report.UncoveredText(1)
	if !strings.Contains(text, "rtl/counter.sv") || !strings.Contains(text, "4") {
		t.Fatalf("text = %q", text)
	}
	if strings.Contains(text, "6") {
		t.Fatalf("limit was ignored: %q", text)
	}
	if filtered := report.UncoveredTextFor("tb/", 0); filtered != "" {
		t.Fatalf("prefix filter = %q, want empty", filtered)
	}
	if scoped := report.ForPrefix("rtl/"); scoped.LinesTotal != 3 || scoped.LinesCovered != 1 {
		t.Fatalf("scoped = %#v", scoped)
	}
}

func TestParseLCOVEmpty(t *testing.T) {
	report := ParseLCOV(nil, "")
	if report.LinesTotal != 0 || report.LineCoverage != 0 || len(report.Files) != 0 {
		t.Fatalf("report = %#v", report)
	}
}
