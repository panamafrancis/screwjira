package index

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// protoFile holds extracted declarations from a .proto file.
type protoFile struct {
	Package  string
	File     string // relative path
	Messages []string
	Services []protoService
	Enums    []string
}

// protoService holds a service name and its RPC methods.
type protoService struct {
	Name    string
	Methods []string
}

var (
	protoPackageRe = regexp.MustCompile(`^package\s+([\w.]+)\s*;`)
	protoMessageRe = regexp.MustCompile(`^message\s+(\w+)\s*\{`)
	protoServiceRe = regexp.MustCompile(`^service\s+(\w+)\s*\{`)
	protoEnumRe    = regexp.MustCompile(`^enum\s+(\w+)\s*\{`)
	protoRPCRe     = regexp.MustCompile(`^\s*rpc\s+(\w+)\s*\(`)
)

// indexProto walks a repo and extracts declarations from .proto files.
func indexProto(repoRoot string) ([]protoFile, error) {
	var files []protoFile

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

		if filepath.Ext(path) != ".proto" {
			return nil
		}

		rel, _ := filepath.Rel(repoRoot, path)
		if pf := extractProtoDecls(path, rel); pf != nil {
			files = append(files, *pf)
		}
		return nil
	})

	return files, err
}

func extractProtoDecls(path, relPath string) *protoFile {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	pf := &protoFile{File: relPath}
	scanner := bufio.NewScanner(f)

	depth := 0
	var currentService *protoService

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Track brace depth to only capture top-level declarations
		for _, ch := range trimmed {
			switch ch {
			case '{':
				depth++
			case '}':
				depth--
				if depth <= 1 && currentService != nil {
					pf.Services = append(pf.Services, *currentService)
					currentService = nil
				}
			}
		}

		// Package — always top-level
		if m := protoPackageRe.FindStringSubmatch(trimmed); m != nil {
			pf.Package = m[1]
			continue
		}

		// Top-level declarations only (depth 0 before the opening brace on this line)
		lineDepthBefore := depth
		// Adjust for braces on this line: count opening braces
		for _, ch := range trimmed {
			if ch == '{' {
				lineDepthBefore--
			}
		}

		if lineDepthBefore == 0 {
			if m := protoMessageRe.FindStringSubmatch(trimmed); m != nil {
				pf.Messages = append(pf.Messages, m[1])
				continue
			}
			if m := protoServiceRe.FindStringSubmatch(trimmed); m != nil {
				currentService = &protoService{Name: m[1]}
				continue
			}
			if m := protoEnumRe.FindStringSubmatch(trimmed); m != nil {
				pf.Enums = append(pf.Enums, m[1])
				continue
			}
		}

		// RPC methods inside a service block
		if currentService != nil {
			if m := protoRPCRe.FindStringSubmatch(trimmed); m != nil {
				currentService.Methods = append(currentService.Methods, m[1])
			}
		}
	}

	// Flush any remaining service
	if currentService != nil {
		pf.Services = append(pf.Services, *currentService)
	}

	if pf.Package == "" && len(pf.Messages) == 0 && len(pf.Services) == 0 && len(pf.Enums) == 0 {
		return nil
	}

	return pf
}
