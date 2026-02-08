package index

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// tfBlock represents a Terraform block definition.
type tfBlock struct {
	Kind string // "resource", "data", "module", "variable", "output", "provider", "locals"
	Type string // e.g., "aws_lambda_function" (empty for variable/output/module)
	Name string // e.g., "my_function"
	File string // relative path
}

// indexTerraform walks a repo and extracts Terraform block definitions using the HCL parser.
func indexTerraform(repoRoot string) ([]tfBlock, error) {
	var blocks []tfBlock

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

		if filepath.Ext(path) != ".tf" {
			return nil
		}

		rel, _ := filepath.Rel(repoRoot, path)
		fileBlocks := extractTFBlocks(path, rel)
		blocks = append(blocks, fileBlocks...)
		return nil
	})

	return blocks, err
}

func extractTFBlocks(path, relPath string) []tfBlock {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	file, diags := hclsyntax.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return nil // skip unparseable files
	}

	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil
	}

	var blocks []tfBlock
	for _, b := range body.Blocks {
		block := tfBlock{
			Kind: b.Type,
			File: relPath,
		}

		switch b.Type {
		case "resource", "data":
			if len(b.Labels) >= 2 {
				block.Type = b.Labels[0]
				block.Name = b.Labels[1]
			}
		case "module", "variable", "output", "provider":
			if len(b.Labels) >= 1 {
				block.Name = b.Labels[0]
			}
		default:
			continue // skip terraform{}, locals{}, etc.
		}

		blocks = append(blocks, block)
	}

	return blocks
}
