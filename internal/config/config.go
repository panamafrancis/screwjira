package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// ProjectConfig describes one Jira project to ingest.
type ProjectConfig struct {
	Key              string   `toml:"key"`               // Jira project key, e.g. "F0"
	Name             string   `toml:"name"`              // human-readable label
	JQL              string   `toml:"jql"`               // JQL query for ingestion
	ExcludeStatuses  []string `toml:"exclude_statuses"`  // statuses to auto-skip on sync
	LightModeType    string   `toml:"light_mode_type"`   // issue type that uses light enrichment, e.g. "Idea"
	CollapseChildType string  `toml:"collapse_child_type"` // issue type to collapse under LightModeType parents, e.g. "Epic"
}

type PostConfig struct {
	Repo           string            `toml:"repo"`            // "owner/repo"
	ProjectNumber  int               `toml:"project_number"`  // 0 = skip
	ProjectOwner   string            `toml:"project_owner"`   // required if project_number > 0
	MigrationLabel string            `toml:"migration_label"` // label added to every issue
	StatusMap      map[string]string `toml:"status_map"`      // jira status (lowercase) -> github project status
}

type Config struct {
	JiraURL  string            `toml:"jira_url"`  // e.g. "https://your-site.atlassian.net"
	Projects []ProjectConfig   `toml:"projects"`
	Enrich   EnrichConfig      `toml:"enrich"`
	Post     PostConfig        `toml:"post"`
	UserMap  map[string]string `toml:"user_map"` // jira display name (lowercase) -> github login
}

// ProjectByKey returns the ProjectConfig for the given Jira project key (case-insensitive), or nil.
func (c *Config) ProjectByKey(key string) *ProjectConfig {
	key = strings.ToUpper(key)
	for i := range c.Projects {
		if strings.ToUpper(c.Projects[i].Key) == key {
			return &c.Projects[i]
		}
	}
	return nil
}

type EnrichConfig struct {
	Agent         string   `toml:"agent"`         // "claude" or "codex"
	Repos         []string `toml:"repos"`
	DocsRepo      string   `toml:"docs_repo"`     // docs-only repo for DISC Idea light enrichment
	Model         string   `toml:"model"`         // deprecated, use claude_model/codex_model
	ClaudeModel   string   `toml:"claude_model"`
	CodexModel    string   `toml:"codex_model"`
	MaxTurns      int      `toml:"max_turns"`     // claude only
	Parallel      int      `toml:"parallel"`      // concurrent workers (default 1)
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
	cfg.Enrich.DocsRepo = expandHome(cfg.Enrich.DocsRepo)

	// Normalise project keys to uppercase.
	for i := range cfg.Projects {
		cfg.Projects[i].Key = strings.ToUpper(cfg.Projects[i].Key)
	}

	// Normalise UserMap keys to lowercase for case-insensitive lookup.
	if len(cfg.UserMap) > 0 {
		normalised := make(map[string]string, len(cfg.UserMap))
		for k, v := range cfg.UserMap {
			normalised[strings.ToLower(k)] = v
		}
		cfg.UserMap = normalised
	}

	if cfg.Post.ProjectNumber > 0 && cfg.Post.ProjectOwner == "" {
		return nil, fmt.Errorf("post.project_owner is required when post.project_number is set")
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

const defaultConfig = `# screwjira configuration

# Your Atlassian site URL (used in issue footer links)
# jira_url = "https://your-site.atlassian.net"

# Jira projects to ingest — add one [[projects]] block per project.
# [[projects]]
# key  = "ENG"
# name = "Engineering"
# jql  = "project = \"ENG\" AND status NOT IN (\"Done\", \"Won't Do\") ORDER BY created ASC"
# exclude_statuses = ["Done", "Won't Do"]
#
# [[projects]]
# key  = "DISC"
# name = "Discovery (JPD)"
# jql  = "project = \"DISC\" AND status NOT IN (\"Released\", \"Abandoned\") ORDER BY created ASC"
# exclude_statuses = ["Released", "Abandoned"]
# light_mode_type    = "Idea"   # issues of this type use docs-only light enrichment
# collapse_child_type = "Epic"  # epics under a kept Idea are collapsed into the parent

[enrich]
# Default agent: "codex" or "claude" (falls back to the other on rate limit)
agent = "codex"

# Local repository paths to scan for issue-related code
repos = [
  "~/code/your-repo/",
]

# Optional: docs-only repo for light enrichment (no source-code scan)
# docs_repo = "~/code/docs"

# Per-agent model configuration
codex_model = "o3"
claude_model = "sonnet"

# Maximum agent turns per issue (claude only, default: 5)
max_turns = 5

# Concurrent workers for enrichment (default: 1)
# parallel = 3

[post]
# Target GitHub repo for issue creation (owner/name)
# repo = "your-org/your-repo"

# Optional: GitHub Project v2 number and owner to add issues to
# project_number = 12
# project_owner  = "your-org"

# Optional: label applied to every migrated issue
# migration_label = "from-jira"

# Optional: override Jira status -> GitHub Project status mapping
# [post.status_map]
# "blocked" = "Blocked"

[user_map]
# Maps Jira display name (lowercase) to GitHub username
# "jane doe" = "janedoe"
`
