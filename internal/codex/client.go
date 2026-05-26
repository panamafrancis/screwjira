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
	tmpDir, err := os.MkdirTemp("", "screwjira-codex-*")
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
		// A killed process is a crash, not a rate limit — don't retry.
		if isCrash(err) {
			return nil, fmt.Errorf("codex crashed (%v) — is codex installed and configured? output: %s", err, truncate(combined, 200))
		}
		if pattern := rateLimitMatch(combined); pattern != "" {
			return nil, fmt.Errorf("%w (matched %q in: %s)", ErrRateLimited, pattern, truncate(combined, 200))
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

// isCrash returns true if the error indicates the process was killed by a signal
// (SIGKILL / SIGTERM), which means it crashed rather than exiting cleanly.
func isCrash(err error) bool {
	return strings.Contains(err.Error(), "signal: killed") ||
		strings.Contains(err.Error(), "signal: terminated")
}

// rateLimitMatch returns the matched pattern if text looks like a rate limit error, or "".
func rateLimitMatch(text string) string {
	lower := strings.ToLower(text)
	for _, pattern := range rateLimitPatterns {
		if strings.Contains(lower, pattern) {
			return pattern
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
