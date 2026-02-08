package codex

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrRateLimited is returned when codex hits a rate/token limit.
var ErrRateLimited = errors.New("codex: rate limited")

var rateLimitPatterns = []string{
	"rate_limit",
	"rate limit",
	"too many requests",
	"429",
	"quota exceeded",
	"tokens per min",
	"requests per min",
	"usage limit",
	"billing",
}

type Options struct {
	Model   string
	AddDirs []string
}

// Response is the parsed output from a codex exec invocation
type Response struct {
	Result    string
	SessionID string // empty if unknown
}

// Invoke starts a new codex exec session with the given prompt
func Invoke(prompt string, opts Options) (*Response, error) {
	return run(prompt, "", opts)
}

// Resume continues an existing codex session with a new prompt
func Resume(sessionID string, prompt string, opts Options) (*Response, error) {
	return run(prompt, sessionID, opts)
}

func run(prompt string, sessionID string, opts Options) (*Response, error) {
	tmpDir, err := os.MkdirTemp("", "fuckjira-codex-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	outFile := filepath.Join(tmpDir, "response.txt")

	var args []string
	if sessionID != "" {
		args = []string{"exec", "resume", sessionID, prompt}
	} else {
		args = []string{"exec", prompt}
	}

	args = append(args, "--full-auto", "-o", outFile)

	if opts.Model != "" {
		args = append(args, "-m", opts.Model)
	}
	for _, dir := range opts.AddDirs {
		args = append(args, "--add-dir", dir)
	}

	cmd := exec.Command("codex", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		combined := strings.TrimSpace(string(output))
		if isRateLimited(combined) {
			return nil, ErrRateLimited
		}
		detail := combined
		if len(detail) > 500 {
			detail = detail[:500] + "..."
		}
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("codex failed (exit %v): %s", err, detail)
	}

	result, err := os.ReadFile(outFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read codex response: %w", err)
	}

	return &Response{
		Result: string(result),
	}, nil
}

func isRateLimited(text string) bool {
	lower := strings.ToLower(text)
	for _, pattern := range rateLimitPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}
