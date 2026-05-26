package index

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// tsExport represents an exported symbol from a TypeScript file.
type tsExport struct {
	Kind string // "function", "class", "interface", "type", "const", "enum", "default", "re-export"
	Name string
	File string // relative path
}

// tsIndexScript is a Node.js script that uses the TypeScript compiler API
// to extract exports from source files. File paths are read from stdin.
const tsIndexScript = `
const ts = require('typescript');
const fs = require('fs');

const input = fs.readFileSync(0, 'utf8');
const files = input.trim().split('\n').filter(Boolean);
const results = [];

const getMods = (node) => {
  if (typeof ts.canHaveModifiers === 'function') {
    return ts.canHaveModifiers(node) ? ts.getModifiers(node) : undefined;
  }
  return node.modifiers;
};

for (const filePath of files) {
  try {
    const source = fs.readFileSync(filePath, 'utf8');
    const sf = ts.createSourceFile(filePath, source, ts.ScriptTarget.Latest, true);
    const exports = [];

    for (const stmt of sf.statements) {
      const mods = getMods(stmt);
      const isExported = mods?.some(m => m.kind === ts.SyntaxKind.ExportKeyword);
      const isDefault = mods?.some(m => m.kind === ts.SyntaxKind.DefaultKeyword);

      if (isExported) {
        if (ts.isVariableStatement(stmt)) {
          for (const decl of stmt.declarationList.declarations) {
            if (ts.isIdentifier(decl.name)) {
              exports.push({ kind: 'const', name: decl.name.text });
            }
          }
        } else if (stmt.name && ts.isIdentifier(stmt.name)) {
          let kind = 'unknown';
          if (ts.isFunctionDeclaration(stmt)) kind = 'function';
          else if (ts.isClassDeclaration(stmt)) kind = 'class';
          else if (ts.isInterfaceDeclaration(stmt)) kind = 'interface';
          else if (ts.isTypeAliasDeclaration(stmt)) kind = 'type';
          else if (ts.isEnumDeclaration(stmt)) kind = 'enum';
          if (isDefault) kind = 'default';
          exports.push({ kind, name: stmt.name.text });
        }
      }

      // export { Foo, Bar } or export { Foo } from './module'
      if (ts.isExportDeclaration(stmt) && stmt.exportClause && ts.isNamedExports(stmt.exportClause)) {
        for (const el of stmt.exportClause.elements) {
          exports.push({ kind: 're-export', name: el.name.text });
        }
      }

      // export default <identifier>
      if (ts.isExportAssignment(stmt) && !stmt.isExportEquals) {
        if (ts.isIdentifier(stmt.expression)) {
          exports.push({ kind: 'default', name: stmt.expression.text });
        }
      }
    }

    if (exports.length > 0) {
      results.push({ file: filePath, exports });
    }
  } catch(e) {}
}

process.stdout.write(JSON.stringify(results));
`

// indexTypeScript walks a repo and extracts exports using the TypeScript compiler API via Node.js.
// Requires node and typescript in the repo's node_modules.
func indexTypeScript(repoRoot string) ([]tsExport, error) {
	// Verify typescript is available in node_modules
	tsDir := filepath.Join(repoRoot, "node_modules", "typescript")
	if _, err := os.Stat(tsDir); err != nil {
		return nil, fmt.Errorf("typescript not found in node_modules — run npm install in %s", repoRoot)
	}

	// Collect .ts/.tsx file paths
	var files []string
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

		ext := filepath.Ext(path)
		if ext != ".ts" && ext != ".tsx" {
			return nil
		}
		base := filepath.Base(path)
		if strings.HasSuffix(base, ".d.ts") ||
			strings.HasSuffix(base, ".test.ts") ||
			strings.HasSuffix(base, ".test.tsx") ||
			strings.HasSuffix(base, ".spec.ts") ||
			strings.HasSuffix(base, ".spec.tsx") {
			return nil
		}

		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(files) == 0 {
		return nil, nil
	}

	// Write script to temp file
	tmpFile, err := os.CreateTemp("", "screwjira-ts-index-*.js")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(tsIndexScript); err != nil {
		tmpFile.Close()
		return nil, err
	}
	tmpFile.Close()

	// Run node with file list on stdin
	cmd := exec.Command("node", tmpFile.Name())
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "NODE_PATH="+filepath.Join(repoRoot, "node_modules"))
	cmd.Stdin = strings.NewReader(strings.Join(files, "\n"))

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("node script failed: %s", string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("failed to run node: %w", err)
	}

	// Parse JSON output
	type exportEntry struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	type fileResult struct {
		File    string        `json:"file"`
		Exports []exportEntry `json:"exports"`
	}

	var results []fileResult
	if err := json.Unmarshal(output, &results); err != nil {
		return nil, fmt.Errorf("failed to parse node output: %w", err)
	}

	// Convert to flat tsExport slice with relative paths
	var exports []tsExport
	for _, r := range results {
		rel, _ := filepath.Rel(repoRoot, r.File)
		for _, e := range r.Exports {
			exports = append(exports, tsExport{
				Kind: e.Kind,
				Name: e.Name,
				File: rel,
			})
		}
	}

	return exports, nil
}
