package index

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type mdDoc struct {
	File     string
	Sections []mdSection
}

type mdSection struct {
	Level   int
	Heading string
	Snippet string // first paragraph text, truncated
}

func indexMarkdown(repoRoot string) ([]mdDoc, error) {
	var docs []mdDoc

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
		if strings.ToLower(filepath.Ext(path)) != ".md" {
			return nil
		}

		rel, _ := filepath.Rel(repoRoot, path)
		doc, err := parseMarkdownDoc(path, rel)
		if err != nil || len(doc.Sections) == 0 {
			return nil
		}
		docs = append(docs, *doc)
		return nil
	})

	return docs, err
}

func parseMarkdownDoc(path, rel string) (*mdDoc, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	doc := &mdDoc{File: rel}
	var currentSection *mdSection
	var paraLines []string
	inCodeBlock := false

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

		if strings.HasPrefix(line, "#") {
			// Finalize previous section's snippet
			if currentSection != nil && currentSection.Snippet == "" && len(paraLines) > 0 {
				currentSection.Snippet = joinSnippet(paraLines)
				paraLines = nil
			}

			level := 0
			for _, c := range line {
				if c == '#' {
					level++
				} else {
					break
				}
			}
			if level > 3 {
				continue
			}
			heading := strings.TrimSpace(line[level:])
			if heading == "" {
				continue
			}
			doc.Sections = append(doc.Sections, mdSection{Level: level, Heading: heading})
			currentSection = &doc.Sections[len(doc.Sections)-1]
			continue
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if currentSection != nil && currentSection.Snippet == "" && len(paraLines) > 0 {
				currentSection.Snippet = joinSnippet(paraLines)
				paraLines = nil
			}
		} else if currentSection != nil && currentSection.Snippet == "" {
			// Skip image lines, HTML tags, and badge-only lines
			if !strings.HasPrefix(trimmed, "![") && !strings.HasPrefix(trimmed, "<") && !strings.HasPrefix(trimmed, "---") {
				paraLines = append(paraLines, trimmed)
			}
		}
	}

	if currentSection != nil && currentSection.Snippet == "" && len(paraLines) > 0 {
		currentSection.Snippet = joinSnippet(paraLines)
	}

	return doc, scanner.Err()
}

// joinSnippet joins paragraph lines, strips basic markdown formatting, and truncates.
func joinSnippet(lines []string) string {
	text := strings.Join(lines, " ")
	r := strings.NewReplacer("**", "", "*", "", "`", "", "__", "", "_", "")
	text = r.Replace(text)
	text = strings.TrimSpace(text)
	if len(text) > 160 {
		return text[:160] + "..."
	}
	return text
}

func renderMDCompact(b *strings.Builder, repo *RepoIndex) {
	for _, doc := range repo.MDDocs {
		// File path as anchor; append H1 title if present
		if len(doc.Sections) > 0 && doc.Sections[0].Level == 1 {
			fmt.Fprintf(b, "**%s** — %s\n", doc.File, doc.Sections[0].Heading)
		} else {
			fmt.Fprintf(b, "**%s**\n", doc.File)
		}

		for _, sec := range doc.Sections {
			if sec.Level == 1 {
				if sec.Snippet != "" {
					fmt.Fprintf(b, "  %s\n", sec.Snippet)
				}
				continue
			}
			prefix := strings.Repeat("#", sec.Level)
			if sec.Snippet != "" {
				fmt.Fprintf(b, "  %s %s: %s\n", prefix, sec.Heading, sec.Snippet)
			} else {
				fmt.Fprintf(b, "  %s %s\n", prefix, sec.Heading)
			}
		}
		b.WriteString("\n")
	}
}
