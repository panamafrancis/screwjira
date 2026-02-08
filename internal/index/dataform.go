package index

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// dfModel represents a Dataform model (table, view, assertion, etc.).
type dfModel struct {
	Name string // from config name or filename
	Type string // "table", "view", "assertion", "operation", "incremental"
	File string // relative path
}

var (
	dfTypeRe = regexp.MustCompile(`type:\s*"(\w+)"`)
	dfNameRe = regexp.MustCompile(`name:\s*"([^"]+)"`)
)

// indexDataform walks a repo and extracts model definitions from .sqlx files.
func indexDataform(repoRoot string) ([]dfModel, error) {
	var models []dfModel

	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}

		if filepath.Ext(path) != ".sqlx" {
			return nil
		}

		rel, _ := filepath.Rel(repoRoot, path)
		if m := extractDataformModel(path, rel); m != nil {
			models = append(models, *m)
		}
		return nil
	})

	return models, err
}

func extractDataformModel(path, relPath string) *dfModel {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	content := string(data)

	// Find config block: config { ... }
	configStart := strings.Index(content, "config")
	if configStart < 0 {
		return nil
	}

	// Find the opening brace after "config"
	braceStart := strings.Index(content[configStart:], "{")
	if braceStart < 0 {
		return nil
	}
	braceStart += configStart

	// Match braces to find the end of the config block
	depth := 0
	configEnd := -1
outer:
	for i := braceStart; i < len(content); i++ {
		switch content[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				configEnd = i
				break outer
			}
		}
	}
	if configEnd < 0 {
		return nil
	}

	configBlock := content[braceStart : configEnd+1]

	model := &dfModel{File: relPath}

	// Extract type
	if m := dfTypeRe.FindStringSubmatch(configBlock); m != nil {
		model.Type = m[1]
	} else {
		model.Type = "unknown"
	}

	// Extract name (or derive from filename)
	if m := dfNameRe.FindStringSubmatch(configBlock); m != nil {
		model.Name = m[1]
	} else {
		base := filepath.Base(relPath)
		model.Name = strings.TrimSuffix(base, ".sqlx")
	}

	return model
}
