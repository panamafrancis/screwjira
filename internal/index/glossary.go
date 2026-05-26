package index

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// GlossaryTerm is a single glossary entry.
type GlossaryTerm struct {
	Term       string   `toml:"term"`
	Aliases    []string `toml:"aliases,omitempty"`
	Definition string   `toml:"definition"`
	Source     string   `toml:"source"` // "auto" or "manual"
}

// Glossary holds all terms.
type Glossary struct {
	Terms []GlossaryTerm `toml:"terms"`
}

// LoadGlossary loads a glossary from path, returning empty if the file doesn't exist.
func LoadGlossary(path string) (*Glossary, error) {
	var g Glossary
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return &g, nil
	}
	if _, err := toml.DecodeFile(path, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// SaveGlossary writes the glossary to path.
func SaveGlossary(path string, g *Glossary) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(g)
}

// SeedGlossary returns built-in glossary seed entries. Add project-specific terms
// here or populate the glossary file manually with `screwjira index --glossary-add`.
func SeedGlossary() []GlossaryTerm {
	return nil
}

// MergeGlossary merges freshly-extracted auto terms into an existing glossary.
// Manual entries are always preserved; auto entries are replaced.
func MergeGlossary(existing *Glossary, fresh []GlossaryTerm) *Glossary {
	out := &Glossary{}

	manualKeys := make(map[string]bool)
	for _, t := range existing.Terms {
		if t.Source == "manual" {
			out.Terms = append(out.Terms, t)
			manualKeys[strings.ToLower(t.Term)] = true
		}
	}

	for _, t := range fresh {
		if !manualKeys[strings.ToLower(t.Term)] {
			out.Terms = append(out.Terms, t)
		}
	}

	sort.Slice(out.Terms, func(i, j int) bool {
		return out.Terms[i].Term < out.Terms[j].Term
	})
	return out
}

// RenderGlossaryCompact returns a compact string for inclusion in a system prompt.
// Format per line: "Term (alias1, alias2): definition"
// Total output is capped at maxBytes.
func RenderGlossaryCompact(g *Glossary, maxBytes int) string {
	var sb strings.Builder
	for _, t := range g.Terms {
		header := t.Term
		if len(t.Aliases) > 0 {
			header += " (" + strings.Join(t.Aliases, ", ") + ")"
		}
		line := header + ": " + t.Definition + "\n"
		if sb.Len()+len(line) > maxBytes {
			break
		}
		sb.WriteString(line)
	}
	return sb.String()
}

// ExtractGlossary scans markdown files in the given repos for H2/H3 term headings
// (≤5 words, noun-phrase heuristic) and uses the first paragraph as the definition.
func ExtractGlossary(repoPaths []string) []GlossaryTerm {
	var terms []GlossaryTerm
	seen := make(map[string]bool)

	for _, repoPath := range repoPaths {
		_ = filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
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
			if strings.ToLower(filepath.Ext(path)) != ".md" {
				return nil
			}
			for _, t := range extractTermsFromMarkdown(path) {
				key := strings.ToLower(t.Term)
				if !seen[key] {
					seen[key] = true
					terms = append(terms, t)
				}
			}
			return nil
		})
	}
	return terms
}

func extractTermsFromMarkdown(path string) []GlossaryTerm {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var terms []GlossaryTerm
	var currentTerm string
	var paraLines []string
	inCodeBlock := false

	flush := func() {
		if currentTerm == "" || len(paraLines) == 0 {
			currentTerm = ""
			paraLines = nil
			return
		}
		def := joinSnippet(paraLines)
		if len(def) >= 10 {
			terms = append(terms, GlossaryTerm{
				Term:       currentTerm,
				Definition: def,
				Source:     "auto",
			})
		}
		currentTerm = ""
		paraLines = nil
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			continue
		}

		// H1 heading: reset current term, no glossary extraction
		if strings.HasPrefix(line, "# ") || line == "#" {
			flush()
			continue
		}

		// H2 or H3 heading (but not H4+)
		if (strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ")) {
			flush()
			level := 0
			for _, c := range line {
				if c == '#' {
					level++
				} else {
					break
				}
			}
			heading := strings.TrimSpace(line[level:])
			if isTermHeading(heading) {
				currentTerm = heading
			}
			continue
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if len(paraLines) > 0 {
				flush()
			}
		} else if currentTerm != "" && len(paraLines) == 0 {
			if !strings.HasPrefix(trimmed, "![") && !strings.HasPrefix(trimmed, "<") && !strings.HasPrefix(trimmed, "---") {
				paraLines = append(paraLines, trimmed)
			}
		}
	}
	flush()
	return terms
}

// isTermHeading returns true if the heading looks like a noun phrase (≤5 words, not a verb phrase).
func isTermHeading(heading string) bool {
	words := strings.Fields(heading)
	if len(words) == 0 || len(words) > 5 {
		return false
	}
	skipWords := map[string]bool{
		"how": true, "why": true, "when": true, "where": true,
		"see": true, "get": true, "use": true, "using": true,
		"building": true, "running": true, "testing": true,
		"deploying": true, "overview": true, "introduction": true,
		"contents": true, "table": true, "license": true,
		"contributing": true, "changelog": true, "faq": true,
		"todo": true, "notes": true, "setup": true, "installation": true,
	}
	return !skipWords[strings.ToLower(words[0])]
}
