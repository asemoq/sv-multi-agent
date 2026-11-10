package rag

import "testing"

func TestIsIndexable(t *testing.T) {
	indexable := []string{
		"rtl/ibex_alu.sv", "rtl/ibex_pkg.svh",
		"rtl/baz.sva", "docs/readme.md", "docs/spec.txt", "docs/notes.rst",
		"docs/manual.adoc",
	}
	for _, file := range indexable {
		if !isIndexable(file) {
			t.Errorf("isIndexable(%q) = false, want true", file)
		}
	}
	skipped := []string{"main.go", "image.png", "config.json", "Makefile", "rtl/foo.sv.bak", "rtl/foo.v", "rtl/bar.vh"}
	for _, file := range skipped {
		if isIndexable(file) {
			t.Errorf("isIndexable(%q) = true, want false", file)
		}
	}
}

func TestIsSystemVerilog(t *testing.T) {
	for _, file := range []string{"a.sv", "a.svh", "a.sva"} {
		if !isSystemVerilog(file) {
			t.Errorf("isSystemVerilog(%q) = false, want true", file)
		}
	}
	for _, file := range []string{"a.md", "a.sv.bak", "a"} {
		if isSystemVerilog(file) {
			t.Errorf("isSystemVerilog(%q) = true, want false", file)
		}
	}
}

func TestShouldSkipDir(t *testing.T) {
	for _, name := range []string{".git", "node_modules", "vendor", "dist", "build", "obj_dir", ".idea", "__pycache__", ".venv"} {
		if !shouldSkipDir(name) {
			t.Errorf("shouldSkipDir(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"rtl", "src", "docs", "tb"} {
		if shouldSkipDir(name) {
			t.Errorf("shouldSkipDir(%q) = true, want false", name)
		}
	}
}

func TestMatchesExclude(t *testing.T) {
	cases := []struct {
		file     string
		patterns []string
		want     bool
	}{
		{"rtl/ibex_core.sv", []string{"ibex_core.sv"}, true},
		{"rtl/ibex_core.sv", []string{"ibex_core"}, true},
		{"rtl/ibex_core.sv", []string{"*.sv"}, true},
		{"rtl/ibex_core.sv", []string{"rtl/ibex_core.sv"}, true},
		{"rtl/ibex_core.sv", []string{"ibex_alu.sv"}, false},
		{"rtl/ibex_core.sv", nil, false},
	}
	for _, tc := range cases {
		if got := matchesExclude(tc.file, tc.patterns); got != tc.want {
			t.Errorf("matchesExclude(%q, %v) = %v, want %v", tc.file, tc.patterns, got, tc.want)
		}
	}
}
