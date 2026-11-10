package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectSourceDir(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"a.sv":       "module a; endmodule\n",
		"sub/b.sv":   "module b; endmodule\n",
		"readme.md":  "ignore me",
		"header.svh": "`define X 1\n",
	}
	for name, content := range files {
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	bundle := map[string]string{}
	if err := collectSourceDir(root, "rtl", bundle); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"rtl/a.sv", "rtl/sub/b.sv", "rtl/header.svh"} {
		if _, ok := bundle[key]; !ok {
			t.Fatalf("missing %s in %#v", key, bundle)
		}
	}
	if _, ok := bundle["rtl/readme.md"]; ok {
		t.Fatalf("non-HDL file was included: %#v", bundle)
	}
}

func TestCollectSourceSingleFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "counter.sv")
	if err := os.WriteFile(path, []byte("module counter; endmodule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle := map[string]string{}
	if err := collectSourceDir(path, "rtl", bundle); err != nil {
		t.Fatal(err)
	}
	if _, ok := bundle["rtl/counter.sv"]; !ok {
		t.Fatalf("bundle = %#v", bundle)
	}
}

func TestCollectSourceRejectsNonHDL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := collectSourceDir(path, "rtl", map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "not a .sv") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckBundleSize(t *testing.T) {
	if err := checkBundleSize(map[string]string{"rtl/a.sv": "small"}); err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("x", maxSourceBundleBytes+1)
	if err := checkBundleSize(map[string]string{"rtl/a.sv": large}); err == nil {
		t.Fatal("expected size error")
	}
}
