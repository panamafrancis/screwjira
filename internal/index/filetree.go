package index

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// skipDirs are directories to skip during indexing.
var skipDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	".terraform":   true,
	"dist":         true,
	"build":        true,
	"testdata":     true,
	".git":         true,
	".idea":        true,
	".vscode":      true,
	"__pycache__":  true,
	".next":        true,
	".nuxt":        true,
	"coverage":     true,
}

// dirEntry represents a directory in the file tree with its file count.
type dirEntry struct {
	Path  string // relative to repo root
	Files int    // number of non-directory files
}

// buildFileTree walks the repo and returns a compact file tree string.
// It skips hidden directories (starting with .) and known skip dirs.
func buildFileTree(repoRoot string) (string, error) {
	var dirs []dirEntry

	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable
		}

		if !d.IsDir() {
			return nil
		}

		rel, _ := filepath.Rel(repoRoot, path)
		if rel == "." {
			// count files in root
			count := countFiles(path)
			if count > 0 {
				dirs = append(dirs, dirEntry{Path: ".", Files: count})
			}
			return nil
		}

		name := d.Name()

		// Skip hidden dirs (except repo root)
		if strings.HasPrefix(name, ".") {
			return filepath.SkipDir
		}

		if skipDirs[name] {
			return filepath.SkipDir
		}

		count := countFiles(path)
		if count > 0 || hasSubdirs(path) {
			dirs = append(dirs, dirEntry{Path: rel, Files: count})
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	sort.Slice(dirs, func(i, j int) bool {
		return dirs[i].Path < dirs[j].Path
	})

	var b strings.Builder
	for _, d := range dirs {
		if d.Files > 0 {
			b.WriteString(fmt.Sprintf("%s/ (%d files)\n", d.Path, d.Files))
		} else {
			b.WriteString(fmt.Sprintf("%s/\n", d.Path))
		}
	}
	return b.String(), nil
}

// countFiles returns the number of non-directory entries in a single directory (non-recursive).
func countFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			count++
		}
	}
	return count
}

// hasSubdirs returns true if the directory has any non-skipped subdirectories.
func hasSubdirs(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && !skipDirs[e.Name()] && !strings.HasPrefix(e.Name(), ".") {
			return true
		}
	}
	return false
}
