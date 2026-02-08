package index

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Language represents a detected repo language.
type Language string

const (
	LangGo         Language = "go"
	LangTypeScript Language = "typescript"
	LangTerraform  Language = "terraform"
	LangDataform   Language = "dataform"
	LangUnknown    Language = "unknown"
)

// maxIndexBytes is the hard cap on rendered index size (~15k tokens).
const maxIndexBytes = 60000

// RepoIndex holds the indexed data for a single repository.
type RepoIndex struct {
	Path       string
	Name       string // last path component
	Language   Language
	FileTree   string
	GoPackages []goPackage
	TSExports  []tsExport
	TFBlocks   []tfBlock
	DFModels   []dfModel
	ProtoFiles []protoFile // always scanned, regardless of primary language
}

// IndexRepo auto-detects the repo language and indexes accordingly.
// Proto files are always scanned as an overlay since they commonly coexist with other languages.
func IndexRepo(repoRoot string) (*RepoIndex, error) {
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}

	idx := &RepoIndex{
		Path: repoRoot,
		Name: filepath.Base(repoRoot),
	}

	// Build file tree (always)
	tree, err := buildFileTree(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("file tree: %w", err)
	}
	idx.FileTree = tree

	// Detect language and index
	idx.Language = detectLanguage(repoRoot)

	switch idx.Language {
	case LangGo:
		pkgs, err := indexGo(repoRoot)
		if err != nil {
			return nil, fmt.Errorf("go index: %w", err)
		}
		idx.GoPackages = pkgs
	case LangTypeScript:
		exports, err := indexTypeScript(repoRoot)
		if err != nil {
			return nil, fmt.Errorf("ts index: %w", err)
		}
		idx.TSExports = exports
	case LangTerraform:
		blocks, err := indexTerraform(repoRoot)
		if err != nil {
			return nil, fmt.Errorf("tf index: %w", err)
		}
		idx.TFBlocks = blocks
	case LangDataform:
		models, err := indexDataform(repoRoot)
		if err != nil {
			return nil, fmt.Errorf("dataform index: %w", err)
		}
		idx.DFModels = models
	}

	// Proto overlay: always scan for .proto files
	protos, _ := indexProto(repoRoot)
	idx.ProtoFiles = protos

	return idx, nil
}

// detectLanguage checks for language-specific files in the repo root.
func detectLanguage(repoRoot string) Language {
	// Go: go.mod exists
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err == nil {
		return LangGo
	}
	// TypeScript: package.json + tsconfig.json
	_, pkgErr := os.Stat(filepath.Join(repoRoot, "package.json"))
	_, tsErr := os.Stat(filepath.Join(repoRoot, "tsconfig.json"))
	if pkgErr == nil && tsErr == nil {
		return LangTypeScript
	}
	// Terraform: *.tf files in root
	tfMatches, _ := filepath.Glob(filepath.Join(repoRoot, "*.tf"))
	if len(tfMatches) > 0 {
		return LangTerraform
	}
	// Dataform: dataform.json or workflow_settings.yaml
	if _, err := os.Stat(filepath.Join(repoRoot, "dataform.json")); err == nil {
		return LangDataform
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "workflow_settings.yaml")); err == nil {
		return LangDataform
	}
	return LangUnknown
}

// RenderMarkdown renders all repo indexes into a compact markdown string.
// Output is capped at ~15k tokens to fit in a system prompt.
func RenderMarkdown(repos []*RepoIndex) string {
	var b strings.Builder
	b.WriteString("# Codebase Architecture Index\n\n")

	for _, repo := range repos {
		fmt.Fprintf(&b, "## %s (%s)\n\n", repo.Name, repo.Language)

		switch repo.Language {
		case LangGo:
			renderGoCompact(&b, repo)
		case LangTypeScript:
			renderTSCompact(&b, repo)
		case LangTerraform:
			renderTFCompact(&b, repo)
		case LangDataform:
			renderDFCompact(&b, repo)
		default:
			// Unknown language: render file tree only
			b.WriteString("```\n")
			b.WriteString(repo.FileTree)
			b.WriteString("```\n\n")
		}
		// File tree is only included for unknown-language repos.
		// For detected languages, the structured data (packages, exports, etc.) is more useful and compact.

		if len(repo.ProtoFiles) > 0 {
			renderProtoCompact(&b, repo)
		}

		// Check budget after each repo
		if b.Len() > maxIndexBytes {
			b.WriteString("\n(index truncated — run fewer repos or use `fuckjira index` to inspect)\n")
			break
		}
	}

	result := b.String()
	if len(result) > maxIndexBytes {
		result = result[:maxIndexBytes] + "\n\n(truncated)\n"
	}
	return result
}

// renderGoCompact renders Go packages in one line each: names only, no signatures.
// e.g. `**handler** (internal/handler) — Types: Service, Config | Funcs: New, Validate | Methods: Service.{Handle, Start}`
func renderGoCompact(b *strings.Builder, repo *RepoIndex) {
	pkgs := make([]goPackage, len(repo.GoPackages))
	copy(pkgs, repo.GoPackages)
	sort.Slice(pkgs, func(i, j int) bool {
		return pkgs[i].Dir < pkgs[j].Dir
	})

	for _, pkg := range pkgs {
		var parts []string

		if len(pkg.Types) > 0 {
			parts = append(parts, "Types: "+strings.Join(pkg.Types, ", "))
		}
		if len(pkg.Interfaces) > 0 {
			parts = append(parts, "Ifaces: "+strings.Join(shortInterfaceNames(pkg.Interfaces), ", "))
		}
		if len(pkg.Functions) > 0 {
			parts = append(parts, "Funcs: "+strings.Join(shortFuncNames(pkg.Functions), ", "))
		}
		if len(pkg.Methods) > 0 {
			parts = append(parts, "Methods: "+compactMethods(pkg.Methods))
		}

		if len(parts) == 0 {
			continue
		}

		fmt.Fprintf(b, "**%s** (`%s`) — %s\n", pkg.Name, pkg.Dir, strings.Join(parts, " | "))
	}
	b.WriteString("\n")
}

// shortFuncNames strips signatures: "New(cfg Config) *Service" -> "New"
func shortFuncNames(funcs []string) []string {
	out := make([]string, len(funcs))
	for i, f := range funcs {
		if idx := strings.Index(f, "("); idx > 0 {
			out[i] = f[:idx]
		} else {
			out[i] = f
		}
	}
	return out
}

// shortInterfaceNames strips method lists: "Handler { ServeHTTP }" -> "Handler"
func shortInterfaceNames(ifaces []string) []string {
	out := make([]string, len(ifaces))
	for i, f := range ifaces {
		if idx := strings.Index(f, " {"); idx > 0 {
			out[i] = f[:idx]
		} else {
			out[i] = f
		}
	}
	return out
}

// compactMethods groups methods by receiver: "Service.{Handle, Start}, Config.{Validate}"
func compactMethods(methods []string) string {
	byRecv := map[string][]string{}
	var order []string
	for _, m := range methods {
		parts := strings.SplitN(m, ".", 2)
		if len(parts) != 2 {
			continue
		}
		recv := parts[0]
		name := parts[1]
		if idx := strings.Index(name, "("); idx > 0 {
			name = name[:idx]
		}
		if _, seen := byRecv[recv]; !seen {
			order = append(order, recv)
		}
		byRecv[recv] = append(byRecv[recv], name)
	}

	var groups []string
	for _, recv := range order {
		names := byRecv[recv]
		if len(names) == 1 {
			groups = append(groups, recv+"."+names[0])
		} else {
			groups = append(groups, recv+".{"+strings.Join(names, ", ")+"}")
		}
	}
	return strings.Join(groups, ", ")
}

// renderTSCompact renders TS exports grouped by directory, one line each.
func renderTSCompact(b *strings.Builder, repo *RepoIndex) {
	// Group by directory
	byDir := map[string][]string{}
	for _, e := range repo.TSExports {
		dir := filepath.Dir(e.File)
		byDir[dir] = append(byDir[dir], e.Name)
	}

	var dirs []string
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		names := byDir[dir]
		fmt.Fprintf(b, "**%s/** — %s\n", dir, strings.Join(names, ", "))
	}
	b.WriteString("\n")
}

// renderTFCompact renders Terraform blocks grouped by kind, compact.
func renderTFCompact(b *strings.Builder, repo *RepoIndex) {
	byKind := map[string][]string{}
	for _, block := range repo.TFBlocks {
		label := block.Name
		if block.Type != "" {
			label = block.Type + "." + block.Name
		}
		byKind[block.Kind] = append(byKind[block.Kind], label)
	}

	for _, kind := range []string{"resource", "data", "module", "variable", "output", "provider"} {
		items, ok := byKind[kind]
		if !ok {
			continue
		}
		fmt.Fprintf(b, "**%ss**: %s\n", kind, strings.Join(items, ", "))
	}
	b.WriteString("\n")
}

// renderDFCompact renders Dataform models grouped by type, compact.
func renderDFCompact(b *strings.Builder, repo *RepoIndex) {
	byType := map[string][]string{}
	for _, m := range repo.DFModels {
		byType[m.Type] = append(byType[m.Type], m.Name)
	}

	for _, t := range []string{"table", "incremental", "view", "assertion", "operation"} {
		names, ok := byType[t]
		if !ok {
			continue
		}
		fmt.Fprintf(b, "**%ss**: %s\n", t, strings.Join(names, ", "))
	}
	b.WriteString("\n")
}

// renderProtoCompact renders protobuf declarations grouped by package, compact.
func renderProtoCompact(b *strings.Builder, repo *RepoIndex) {
	byPkg := map[string][]protoFile{}
	for _, pf := range repo.ProtoFiles {
		pkg := pf.Package
		if pkg == "" {
			pkg = "(default)"
		}
		byPkg[pkg] = append(byPkg[pkg], pf)
	}

	var pkgs []string
	for p := range byPkg {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	b.WriteString("**Proto:** ")
	var pkgParts []string
	for _, pkg := range pkgs {
		files := byPkg[pkg]
		var msgs, enums []string
		var svcParts []string
		for _, pf := range files {
			msgs = append(msgs, pf.Messages...)
			enums = append(enums, pf.Enums...)
			for _, svc := range pf.Services {
				if len(svc.Methods) > 0 {
					svcParts = append(svcParts, svc.Name+"{"+strings.Join(svc.Methods, ", ")+"}")
				} else {
					svcParts = append(svcParts, svc.Name)
				}
			}
		}

		var parts []string
		if len(msgs) > 0 {
			parts = append(parts, "msg: "+strings.Join(msgs, ", "))
		}
		if len(svcParts) > 0 {
			parts = append(parts, "svc: "+strings.Join(svcParts, ", "))
		}
		if len(enums) > 0 {
			parts = append(parts, "enum: "+strings.Join(enums, ", "))
		}
		if len(parts) > 0 {
			pkgParts = append(pkgParts, pkg+" ("+strings.Join(parts, " | ")+")")
		}
	}
	b.WriteString(strings.Join(pkgParts, "; "))
	b.WriteString("\n\n")
}
