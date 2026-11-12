package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/asemoq/sv-multi-agent/internal/store"
)

// FileEntry represents a file to pass to workers.
type FileEntry struct {
	Path    string
	Content string
}

// ReadSources reads SystemVerilog sources from the rtl/ and tb/ directories.
// Missing directories are treated as empty, so a fresh workspace is valid.
func ReadSources(workspace string) ([]FileEntry, error) {
	return readFiles(workspace, []string{"rtl", "tb"}, true)
}

// ReadArtifacts reads previously generated artifacts (contract, reports).
// The rtl/ and tb/ trees are intentionally excluded here because they are
// already provided by ReadSources, which keeps the worker bundle duplicate-free.
func ReadArtifacts(workspace string) ([]FileEntry, error) {
	return readFiles(workspace, []string{"design", "reports"}, false)
}

func readFiles(workspace string, dirs []string, onlySystemVerilog bool) ([]FileEntry, error) {
	entries := []FileEntry{}
	ws := filepath.Clean(workspace)

	for _, dir := range dirs {
		targetDir := filepath.Join(ws, dir)
		info, err := os.Stat(targetDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if !info.IsDir() {
			continue
		}
		if err := filepath.WalkDir(targetDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if onlySystemVerilog && !isHDLSource(path) {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			rel, err := filepath.Rel(ws, path)
			if err != nil {
				return fmt.Errorf("relpath %s: %w", path, err)
			}
			entries = append(entries, FileEntry{
				Path:    filepath.ToSlash(rel),
				Content: string(content),
			})
			return nil
		}); err != nil {
			return nil, err
		}
	}

	return entries, nil
}

// BundleForWorker creates a complete file bundle for a worker request.
func BundleForWorker(task *store.Task) ([]FileEntry, error) {
	sources, err := ReadSources(task.Workspace)
	if err != nil {
		return nil, fmt.Errorf("read sources: %w", err)
	}
	artifacts, err := ReadArtifacts(task.Workspace)
	if err != nil {
		return nil, fmt.Errorf("read artifacts: %w", err)
	}
	return append(sources, artifacts...), nil
}

func isHDLSource(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".sv", ".svh", ".sva", ".v", ".vh":
		return true
	default:
		return false
	}
}
