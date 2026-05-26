package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// MCPServerDef defines a single MCP server to pass via --mcp-config.
type MCPServerDef struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// ErrRateLimited is returned when claude hits a rate/token limit.
var ErrRateLimited = errors.New("claude: rate limited")

// ErrMaxTurns is returned when claude hits the max-turns limit.
// Retrying won't help — the issue needs more turns than configured.
var ErrMaxTurns = errors.New("claude: max turns reached")

var rateLimitPatterns = []string{
	"rate_limit",
	"rate limit",
	"overloaded",
	"too many requests",
	"429",
	"usage limit",
	"billing",
}

// Usage tracks token consumption from the Claude API
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// ModelUsageEntry tracks per-model token usage
type ModelUsageEntry struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	ContextWindow  int `json:"contextWindow"`
	MaxOutputTokens int `json:"maxOutputTokens"`
}

// Response is the JSON output from `claude --output-format json`
type Response struct {
	Type         string                    `json:"type"`
	Subtype      string                    `json:"subtype"`
	Result       string                    `json:"result"`
	SessionID    string                    `json:"session_id"`
	TotalCostUSD float64                   `json:"total_cost_usd"`
	DurationMs   int                       `json:"duration_ms"`
	NumTurns     int                       `json:"num_turns"`
	IsError      bool                      `json:"is_error"`
	Usage        Usage                     `json:"usage"`
	ModelUsage   map[string]ModelUsageEntry `json:"modelUsage"`
}

// TotalInputTokens returns the total input tokens (direct + cache)
func (r *Response) TotalInputTokens() int {
	return r.Usage.InputTokens + r.Usage.CacheCreationInputTokens + r.Usage.CacheReadInputTokens
}

// TotalOutputTokens returns the total output tokens
func (r *Response) TotalOutputTokens() int {
	return r.Usage.OutputTokens
}

// ContextRemaining returns remaining context window tokens for the primary model.
// Returns -1 if unknown.
func (r *Response) ContextRemaining() int {
	for _, m := range r.ModelUsage {
		if m.ContextWindow > 0 {
			used := r.TotalInputTokens() + r.TotalOutputTokens()
			return m.ContextWindow - used
		}
	}
	return -1
}

type Options struct {
	Model        string
	MaxTurns     int
	AllowedTools []string
	AddDirs      []string
	SystemPrompt string
	MCPServers   map[string]MCPServerDef
}

// Invoke starts a new claude session with the given prompt
func Invoke(prompt string, opts Options) (*Response, error) {
	args := buildArgs(prompt, "", opts)
	return run(args)
}

// Resume continues an existing session with a new prompt
func Resume(sessionID string, prompt string, opts Options) (*Response, error) {
	args := buildArgs(prompt, sessionID, opts)
	return run(args)
}

func buildArgs(prompt string, sessionID string, opts Options) []string {
	args := []string{"-p", prompt, "--output-format", "json"}

	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", opts.MaxTurns))
	}
	for _, tool := range opts.AllowedTools {
		args = append(args, "--allowedTools", tool)
	}
	for _, dir := range opts.AddDirs {
		args = append(args, "--add-dir", dir)
	}
	if opts.SystemPrompt != "" {
		args = append(args, "--system-prompt", opts.SystemPrompt)
	}
	if len(opts.MCPServers) > 0 {
		cfg, err := json.Marshal(map[string]any{"mcpServers": opts.MCPServers})
		if err == nil {
			args = append(args, "--mcp-config", string(cfg))
		}
	}

	return args
}

func run(args []string) (*Response, error) {
	cmd := exec.Command("claude", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// claude exits non-zero for structured errors (e.g. error_max_turns) but
		// still emits valid JSON. Try to parse it before falling back to raw error.
		var resp Response
		if jsonErr := json.Unmarshal(output, &resp); jsonErr == nil && resp.Type == "result" {
			if resp.Subtype == "error_max_turns" {
				return &resp, fmt.Errorf("%w (%d turns used, $%.4f)", ErrMaxTurns, resp.NumTurns, resp.TotalCostUSD)
			}
			if pattern := rateLimitMatch(resp.Result); pattern != "" {
				return &resp, fmt.Errorf("%w (matched %q)", ErrRateLimited, pattern)
			}
			return &resp, fmt.Errorf("claude error: %s", resp.Result)
		}
		combined := strings.TrimSpace(string(output))
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
		return nil, fmt.Errorf("claude failed (exit %v): %s", err, detail)
	}

	// Log raw response for debugging
	if len(output) < 2000 {
		fmt.Fprintf(os.Stderr, "[claude debug] raw: %s\n", string(output))
	} else {
		fmt.Fprintf(os.Stderr, "[claude debug] raw (%d bytes): %s...\n", len(output), string(output[:500]))
	}

	var resp Response
	if err := json.Unmarshal(output, &resp); err != nil {
		preview := string(output)
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		return nil, fmt.Errorf("failed to parse claude response: %w (output: %s)", err, preview)
	}

	if resp.IsError {
		if pattern := rateLimitMatch(resp.Result); pattern != "" {
			return &resp, fmt.Errorf("%w (matched %q in: %s)", ErrRateLimited, pattern, truncate(resp.Result, 200))
		}
		return &resp, fmt.Errorf("claude error: %s", resp.Result)
	}

	return &resp, nil
}

// rateLimitMatch returns the matched pattern if text looks like a rate limit error, or "" otherwise.
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
