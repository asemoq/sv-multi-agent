package tui

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// maxBundleBytes mirrors the CLI/server limit for analyze-mode uploads.
const maxBundleBytes = 24 << 20

// collectSources reads SystemVerilog sources from root and keys them as
// prefix/<relative-path> with POSIX separators. Hidden directories are skipped.
func collectSources(root, prefix string) (map[string]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	files := map[string]string{}
	if !info.IsDir() {
		if !isHDL(root) {
			return nil, fmt.Errorf("%s is not a .sv or .svh file", root)
		}
		data, err := os.ReadFile(root)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", root, err)
		}
		files[prefix+"/"+filepath.Base(root)] = string(data)
		return files, nil
	}

	count := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !isHDL(path) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		files[prefix+"/"+filepath.ToSlash(rel)] = string(data)
		count++
		return nil
	})
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, fmt.Errorf("no SystemVerilog sources found in %s", root)
	}
	return files, nil
}

func isHDL(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".sv", ".svh", ".sva", ".v", ".vh":
		return true
	default:
		return false
	}
}

func checkBundleSize(files map[string]string) error {
	total := 0
	for _, content := range files {
		total += len(content)
	}
	if total > maxBundleBytes {
		return fmt.Errorf("source bundle is %d bytes; limit is %d bytes", total, maxBundleBytes)
	}
	return nil
}
