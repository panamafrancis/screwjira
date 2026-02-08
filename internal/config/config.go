package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Enrich EnrichConfig `toml:"enrich"`
}

type EnrichConfig struct {
	Agent         string   `toml:"agent"`          // "claude" or "codex"
	Repos         []string `toml:"repos"`
	Model         string   `toml:"model"`           // deprecated, use claude_model/codex_model
	ClaudeModel   string   `toml:"claude_model"`
	CodexModel    string   `toml:"codex_model"`
	MaxTurns      int      `toml:"max_turns"`       // claude only
	Parallel      int      `toml:"parallel"`        // concurrent workers (default 1)
	ClaudeSession string   `toml:"claude_session"`
	CodexSession  string   `toml:"codex_session"`
}

// ModelFor returns the model configured for the given agent.
func (c *EnrichConfig) ModelFor(agent string) string {
	if agent == "codex" && c.CodexModel != "" {
		return c.CodexModel
	}
	if agent == "claude" && c.ClaudeModel != "" {
		return c.ClaudeModel
	}
	return c.Model // fallback to legacy single model field
}

// Session returns the session ID for the given agent.
func (c *EnrichConfig) Session(agent string) string {
	if agent == "codex" {
		return c.CodexSession
	}
	return c.ClaudeSession
}

// SetSession updates the session ID for the given agent and persists to disk.
func (cfg *Config) SetSession(dataDir, agent, sessionID string) error {
	switch agent {
	case "codex":
		cfg.Enrich.CodexSession = sessionID
	default:
		cfg.Enrich.ClaudeSession = sessionID
	}

	key := agent + "_session"
	return upsertConfigValue(filepath.Join(dataDir, "config.toml"), key, sessionID)
}

// upsertConfigValue updates a key under [enrich] in config.toml, preserving
// all other content and comments. Appends the key if it doesn't exist.
func upsertConfigValue(path, key, value string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	lines := strings.Split(string(data), "\n")
	target := key + " ="
	found := false

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, target) || strings.HasPrefix(trimmed, key+" =") {
			lines[i] = fmt.Sprintf("%s = %q", key, value)
			found = true
			break
		}
	}

	if !found {
		// Insert before the last blank line or at end of [enrich] section
		inserted := false
		for i := len(lines) - 1; i >= 0; i-- {
			trimmed := strings.TrimSpace(lines[i])
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "[") {
				// Insert after the last non-empty, non-comment, non-section line
				newLine := fmt.Sprintf("%s = %q", key, value)
				lines = append(lines[:i+1], append([]string{newLine}, lines[i+1:]...)...)
				inserted = true
				break
			}
		}
		if !inserted {
			lines = append(lines, fmt.Sprintf("%s = %q", key, value))
		}
	}

	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}

func Load(dataDir string) (*Config, error) {
	path := filepath.Join(dataDir, "config.toml")

	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config not found at %s", path)
		}
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	for i, repo := range cfg.Enrich.Repos {
		cfg.Enrich.Repos[i] = expandHome(repo)
	}

	if cfg.Enrich.Agent == "" {
		cfg.Enrich.Agent = "codex"
	}
	if cfg.Enrich.CodexModel == "" && cfg.Enrich.Model == "" {
		cfg.Enrich.CodexModel = "o3"
	}
	if cfg.Enrich.ClaudeModel == "" && cfg.Enrich.Model == "" {
		cfg.Enrich.ClaudeModel = "sonnet"
	}
	if cfg.Enrich.MaxTurns == 0 {
		cfg.Enrich.MaxTurns = 5
	}
	if cfg.Enrich.Parallel == 0 {
		cfg.Enrich.Parallel = 1
	}

	return &cfg, nil
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

// CreateDefault creates a default config.toml if one doesn't exist
func CreateDefault(dataDir string) error {
	path := filepath.Join(dataDir, "config.toml")

	if _, err := os.Stat(path); err == nil {
		return nil // already exists
	}

	return os.WriteFile(path, []byte(defaultConfig), 0644)
}

const defaultConfig = `# fuckjira configuration

[enrich]
# Default agent: "codex" or "claude" (falls back to the other on rate limit)
agent = "codex"

# Local repository paths to scan for issue-related code
repos = [
  "~/code/go/src/github.com/fraud-zero/keystone-api/",
]

# Per-agent model configuration
codex_model = "o3"
claude_model = "sonnet"

# Maximum agent turns per issue (claude only, default: 5)
max_turns = 5

# Concurrent workers for enrichment (default: 1)
# parallel = 3
`
