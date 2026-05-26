package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fraud-zero/screwjira/internal/config"
	"github.com/fraud-zero/screwjira/internal/index"
	"github.com/spf13/cobra"
)

var indexCmd = &cobra.Command{
	Use:   "index",
	Short: "Index local repos for enrichment context",
	Long: `Analyze configured repositories and generate a compact architecture index.
The index is saved to ~/.screwjira/index.md and used as context during enrichment
to eliminate per-issue codebase searches.`,
	RunE: runIndex,
}

func init() {
	rootCmd.AddCommand(indexCmd)
}

func runIndex(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(getDataDir())
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if len(cfg.Enrich.Repos) == 0 {
		return fmt.Errorf("no repos configured — edit %s/config.toml", getDataDir())
	}

	var repos []*index.RepoIndex
	start := time.Now()

	for _, repoPath := range cfg.Enrich.Repos {
		if _, err := os.Stat(repoPath); err != nil {
			return fmt.Errorf("repo path not found: %s", repoPath)
		}

		fmt.Printf("Indexing %s...\n", repoPath)
		idx, err := index.IndexRepo(repoPath)
		if err != nil {
			return fmt.Errorf("failed to index %s: %w", repoPath, err)
		}

		fmt.Printf("  Language: %s\n", idx.Language)
		switch idx.Language {
		case index.LangGo:
			fmt.Printf("  Packages: %d\n", len(idx.GoPackages))
		case index.LangTypeScript:
			fmt.Printf("  Exports: %d\n", len(idx.TSExports))
		case index.LangTerraform:
			fmt.Printf("  Blocks: %d\n", len(idx.TFBlocks))
		case index.LangDataform:
			fmt.Printf("  Models: %d\n", len(idx.DFModels))
		}
		if len(idx.ProtoFiles) > 0 {
			total := 0
			for _, pf := range idx.ProtoFiles {
				total += len(pf.Messages) + len(pf.Services) + len(pf.Enums)
			}
			fmt.Printf("  Proto: %d files, %d declarations\n", len(idx.ProtoFiles), total)
		}

		repos = append(repos, idx)
	}

	markdown := index.RenderMarkdown(repos)

	outPath := filepath.Join(getDataDir(), "index.md")
	if err := os.MkdirAll(getDataDir(), 0755); err != nil {
		return fmt.Errorf("failed to create data dir: %w", err)
	}
	if err := os.WriteFile(outPath, []byte(markdown), 0644); err != nil {
		return fmt.Errorf("failed to write index: %w", err)
	}

	elapsed := time.Since(start).Round(time.Millisecond)
	fmt.Printf("\nIndex written to %s (%d bytes, %s)\n", outPath, len(markdown), elapsed)

	// Build / refresh glossary
	glossaryPath := filepath.Join(getDataDir(), "glossary.toml")
	existing, err := index.LoadGlossary(glossaryPath)
	if err != nil {
		fmt.Printf("Warning: failed to load existing glossary: %v\n", err)
		existing = &index.Glossary{}
	}
	// Seed with hardcoded entries on first run (empty file).
	if len(existing.Terms) == 0 {
		existing.Terms = index.SeedGlossary()
	}
	freshTerms := index.ExtractGlossary(cfg.Enrich.Repos)
	merged := index.MergeGlossary(existing, freshTerms)
	if err := index.SaveGlossary(glossaryPath, merged); err != nil {
		fmt.Printf("Warning: failed to save glossary: %v\n", err)
	} else {
		auto, manual := 0, 0
		for _, t := range merged.Terms {
			if t.Source == "manual" {
				manual++
			} else {
				auto++
			}
		}
		fmt.Printf("Glossary: %d terms (%d auto, %d manual) written to %s\n", len(merged.Terms), auto, manual, glossaryPath)
	}

	return nil
}
