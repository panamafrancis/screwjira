package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"bufio"
	"io"

	"github.com/chzyer/readline"
	"github.com/fraud-zero/screwjira/internal/claude"
	"github.com/fraud-zero/screwjira/internal/codex"
	"github.com/fraud-zero/screwjira/internal/config"
	"github.com/fraud-zero/screwjira/internal/index"
	"github.com/fraud-zero/screwjira/internal/storage"
	"github.com/spf13/cobra"
)

var (
	enrichStatus        bool
	enrichReview        bool
	enrichIssueKey      string
	enrichLimit         int
	enrichReset         bool
	enrichUpgrade       bool
	enrichAgent         string
	enrichModel         string
	enrichParallel      int
	enrichRejectUnknown bool
	enrichFilter        string
	enrichSkipProjects  []string
	enrichNoLight       bool
	enrichConfidence    string
)

var enrichCmd = &cobra.Command{
	Use:   "enrich",
	Short: "Enrich issues with context from local repos",
	Long: `Use a Claude agent to scan local repositories and propose improvements
to issue titles, descriptions, and labels. Review changes in diff style.`,
	RunE: runEnrich,
}

func init() {
	rootCmd.AddCommand(enrichCmd)
	enrichCmd.Flags().BoolVar(&enrichStatus, "status", false, "show enrichment progress")
	enrichCmd.Flags().BoolVar(&enrichReview, "review", false, "interactively review enrichment proposals")
	enrichCmd.Flags().StringVar(&enrichIssueKey, "issue", "", "enrich a single issue")
	enrichCmd.Flags().IntVar(&enrichLimit, "limit", 0, "limit number of issues to process")
	enrichCmd.Flags().BoolVar(&enrichReset, "reset", false, "re-enrich already enriched issues")
	enrichCmd.Flags().BoolVar(&enrichUpgrade, "upgrade", false, "re-enrich low/none confidence issues with a stronger model")
	enrichCmd.Flags().StringVar(&enrichAgent, "agent", "", "agent to use: claude or codex (overrides config)")
	enrichCmd.Flags().StringVar(&enrichModel, "model", "", "model override for this run (e.g. claude-sonnet-4-6)")
	enrichCmd.Flags().IntVar(&enrichParallel, "parallel", 0, "concurrent workers (overrides config)")
	enrichCmd.Flags().BoolVar(&enrichRejectUnknown, "reject-unknown", false, "reject all enrichments with unknown confidence")
	enrichCmd.Flags().StringVar(&enrichFilter, "filter", "", "filter review by decision: pending, accepted, rejected, deferred")
	enrichCmd.Flags().StringSliceVar(&enrichSkipProjects, "skip-project", nil, "skip issues from these projects (e.g. --skip-project FOO,BAR)")
	enrichCmd.Flags().BoolVar(&enrichNoLight, "no-light", false, "disable light mode for discovery issues — use full enrichment instead")
	enrichCmd.Flags().StringVar(&enrichConfidence, "confidence", "", "filter review by confidence: high, medium, low, none (comma-separated)")
}

func runEnrich(cmd *cobra.Command, args []string) error {
	store, err := storage.New(getDataDir())
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}

	switch {
	case enrichStatus:
		return showEnrichStatus(store)
	case enrichRejectUnknown:
		return rejectUnknownEnrichments(store)
	case enrichReview:
		return interactiveEnrichReview(store, enrichIssueKey, enrichFilter, enrichConfidence)
	default:
		return runEnrichAgent(store)
	}
}

// === Agent enrichment ===

const maxRetries = 3

// logMu protects log output for clean parallel logging.
var logMu sync.Mutex

// logf prints a timestamped log line (thread-safe).
func logf(format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05")
	logMu.Lock()
	fmt.Printf("%s "+format, append([]interface{}{ts}, args...)...)
	logMu.Unlock()
}

// setupStopListener cancels ctx on the first Ctrl+C (SIGINT).
// The subprocess (claude/codex) is in the same process group and also receives
// the signal, so the in-progress agent call will terminate and the loop stops
// cleanly at the end of the current issue.
func setupStopListener(cancel context.CancelFunc) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT)
	go func() {
		if _, ok := <-ch; ok {
			logf("\nCtrl+C received — stopping after current issue.\n")
			cancel()
		}
	}()
	return func() { signal.Stop(ch); close(ch) }
}

// agentResult normalizes results from claude and codex into a common shape.
type agentResult struct {
	Agent            string // "claude" or "codex"
	Result           string
	SessionID        string
	CostUSD          float64
	InputTokens      int // fresh (uncached) input tokens
	CacheWriteTokens int // tokens written to cache (cache_creation)
	CacheReadTokens  int // tokens read from cache (cache_read) — cheap
	OutputTokens     int
	NumTurns         int
	ContextRemaining int // -1 if unknown
}

func otherAgent(agent string) string {
	if agent == "codex" {
		return "claude"
	}
	return "codex"
}

// loadIndex loads the index.md file and returns its contents, or "" if not found.
// Warns if the index is older than 24 hours.
func loadIndex(dataDir string) string {
	indexPath := filepath.Join(dataDir, "index.md")
	info, err := os.Stat(indexPath)
	if err != nil {
		return ""
	}

	if time.Since(info.ModTime()) > 24*time.Hour {
		logf("Warning: index.md is older than 24 hours. Run 'screwjira index' to refresh.\n")
	}

	data, err := os.ReadFile(indexPath)
	if err != nil {
		return ""
	}
	return string(data)
}

func runEnrichAgent(store *storage.Store) error {
	cfg, err := config.Load(getDataDir())
	if err != nil {
		if err := config.CreateDefault(getDataDir()); err != nil {
			return fmt.Errorf("failed to create default config: %w", err)
		}
		return fmt.Errorf("created default config at %s/config.toml — edit repo paths, then re-run", getDataDir())
	}

	if len(cfg.Enrich.Repos) == 0 {
		return fmt.Errorf("no repos configured — edit %s/config.toml", getDataDir())
	}

	for _, repo := range cfg.Enrich.Repos {
		if _, err := os.Stat(repo); err != nil {
			return fmt.Errorf("repo path not found: %s", repo)
		}
	}

	primaryAgent := cfg.Enrich.Agent
	if enrichAgent != "" {
		primaryAgent = enrichAgent
	}
	if primaryAgent != "claude" && primaryAgent != "codex" {
		return fmt.Errorf("unknown agent %q — must be \"claude\" or \"codex\"", primaryAgent)
	}

	if enrichModel != "" {
		switch primaryAgent {
		case "claude":
			cfg.Enrich.ClaudeModel = enrichModel
		case "codex":
			cfg.Enrich.CodexModel = enrichModel
		}
	}

	var toEnrich []*storage.StoredIssue

	if enrichIssueKey != "" {
		stored, err := store.LoadIssue(enrichIssueKey)
		if err != nil {
			return fmt.Errorf("issue not found: %w", err)
		}
		toEnrich = []*storage.StoredIssue{stored}
	} else if enrichReset {
		toEnrich, err = store.GetKeptIssues()
		if err != nil {
			return err
		}
	} else if enrichUpgrade {
		toEnrich, err = store.GetUpgradeableIssues()
		if err != nil {
			return err
		}
	} else {
		toEnrich, err = store.GetEnrichableIssues()
		if err != nil {
			return err
		}
	}

	if len(enrichSkipProjects) > 0 {
		skip := make(map[string]bool, len(enrichSkipProjects))
		for _, p := range enrichSkipProjects {
			skip[strings.ToUpper(p)] = true
		}
		filtered := toEnrich[:0]
		for _, issue := range toEnrich {
			if !skip[strings.ToUpper(issue.Issue.Project())] {
				filtered = append(filtered, issue)
			}
		}
		toEnrich = filtered
	}

	if len(toEnrich) == 0 {
		fmt.Println("No issues to enrich.")
		return showEnrichStatus(store)
	}

	sort.Slice(toEnrich, func(i, j int) bool {
		return toEnrich[i].Issue.Key < toEnrich[j].Issue.Key
	})

	if enrichLimit > 0 && len(toEnrich) > enrichLimit {
		toEnrich = toEnrich[:enrichLimit]
	}

	parallel := cfg.Enrich.Parallel
	if enrichParallel > 0 {
		parallel = enrichParallel
	}

	// Load index and glossary for system prompt
	indexContent := loadIndex(getDataDir())
	glossary := loadGlossary(getDataDir())

	systemPrompt := buildEnrichSystemPrompt(cfg.Enrich.Repos, indexContent, glossary)
	lightSystemPrompt := buildLightEnrichSystemPrompt(cfg.Enrich.DocsRepo, glossary)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopListening := setupStopListener(cancel)
	defer stopListening()

	logf("Enriching %d issues using %d repos (agent: %s [%s], fallback: %s [%s], parallel: %d)...\n",
		len(toEnrich), len(cfg.Enrich.Repos),
		primaryAgent, cfg.Enrich.ModelFor(primaryAgent),
		otherAgent(primaryAgent), cfg.Enrich.ModelFor(otherAgent(primaryAgent)),
		parallel)
	for _, r := range cfg.Enrich.Repos {
		fmt.Printf("         repo: %s\n", r)
	}
	if cfg.Enrich.DocsRepo != "" {
		fmt.Printf("         docs: %s (light mode for discovery issues)\n", cfg.Enrich.DocsRepo)
	}
	if indexContent != "" {
		fmt.Printf("         index: loaded (%d bytes)\n", len(indexContent))
	} else {
		fmt.Printf("         index: not found (run 'screwjira index' first for faster enrichment)\n")
	}
	if glossary != "" {
		fmt.Printf("         glossary: loaded (%d bytes)\n", len(glossary))
	}
	fmt.Printf("         Press Ctrl+C to stop after current issue.\n\n")

	if parallel <= 1 {
		return runSequential(ctx, store, cfg, toEnrich, primaryAgent, systemPrompt, lightSystemPrompt)
	}
	return runParallel(ctx, store, cfg, toEnrich, primaryAgent, systemPrompt, lightSystemPrompt, parallel)
}

func runSequential(ctx context.Context, store *storage.Store, cfg *config.Config, toEnrich []*storage.StoredIssue, primaryAgent, systemPrompt, lightSystemPrompt string) error {
	activeAgent := primaryAgent
	fallbackAgent := otherAgent(primaryAgent)
	fallbackAvailable := true

	totalCost := 0.0
	totalIn, totalOut := 0, 0

	for i, issue := range toEnrich {
		select {
		case <-ctx.Done():
			logf("Stopped.\n\n")
			return showEnrichStatus(store)
		default:
		}

		isLight := isLightModeIssue(issue, cfg)
		modeTag := ""
		if isLight {
			modeTag = " [light]"
		}
		logf("[%d/%d] %s — %s%s\n", i+1, len(toEnrich), issue.Issue.Key, truncate(issue.Issue.Summary(), 50), modeTag)

		sysPrompt, issuePrompt := selectPrompts(issue, store, systemPrompt, lightSystemPrompt, isLight)
		start := time.Now()

		var result *agentResult
		var err error
		succeeded := false
		skipIssue := false

		for attempt := 0; attempt < maxRetries; attempt++ {
			if ctx.Err() != nil {
				break
			}
			if attempt > 0 {
				logf("  retry %d/%d...\n", attempt, maxRetries-1)
			}

			result, err = invokeAgent(activeAgent, cfg, sysPrompt, issuePrompt, isLight)

			if err == nil {
				succeeded = true
				break
			}

			if errors.Is(err, claude.ErrMaxTurns) {
				logf("  max turns reached — skipping: %v\n", err)
				skipIssue = true
				break
			}

			if errors.Is(err, claude.ErrRateLimited) || errors.Is(err, codex.ErrRateLimited) {
				logf("  %s rate limited: %v\n", activeAgent, err)
				if fallbackAvailable {
					logf("  switching to %s\n", fallbackAgent)
					activeAgent, fallbackAgent = fallbackAgent, activeAgent
					fallbackAvailable = false
					continue
				}
				logf("\nBoth agents rate limited — stopping.\n\n")
				return showEnrichStatus(store)
			}

			logf("  Error: %v\n", err)
			// A crash (signal: killed) won't recover on retry — bail immediately.
			if strings.Contains(err.Error(), "crashed") {
				break
			}
		}

		if skipIssue {
			continue
		}

		if !succeeded {
			if ctx.Err() != nil {
				logf("  Stopped.\n\n")
			} else {
				logf("  exiting after %d failed attempts\n\n", maxRetries)
			}
			return showEnrichStatus(store)
		}

		totalCost += result.CostUSD
		totalIn += result.InputTokens + result.CacheWriteTokens + result.CacheReadTokens
		totalOut += result.OutputTokens

		enrichment := parseEnrichmentResponse(result.Result)
		enrichment.EnrichedBy = result.Agent
		issue.Enrichment = enrichment
		if isLight {
			issue.EnrichMode = "light"
		}
		if err := store.SaveStoredIssue(issue); err != nil {
			return fmt.Errorf("failed to save %s: %w", issue.Issue.Key, err)
		}

		elapsed := time.Since(start).Round(time.Second)
		agentTag := ""
		if result.Agent != primaryAgent {
			agentTag = fmt.Sprintf(" [%s]", result.Agent)
		}
		logf("  %s — %s%s\n", enrichment.Confidence, truncate(enrichment.Notes, 60), agentTag)
		if result.CostUSD > 0 {
			fmt.Printf("         (%d turns, %s, $%.4f — %dk fresh + %dk↑ + %dk↓ in, %dk out",
				result.NumTurns, elapsed, result.CostUSD,
				result.InputTokens/1000, result.CacheWriteTokens/1000, result.CacheReadTokens/1000,
				result.OutputTokens/1000)
			if result.ContextRemaining >= 0 {
				fmt.Printf(", %dk ctx left", result.ContextRemaining/1000)
			}
			fmt.Printf(")\n\n")
		} else {
			fmt.Printf("         (%s)\n\n", elapsed)
		}
	}

	if totalCost > 0 {
		logf("Done! Total: $%.4f, %dk in + %dk out\n\n", totalCost, totalIn/1000, totalOut/1000)
	} else {
		logf("Done!\n\n")
	}
	return showEnrichStatus(store)
}

func runParallel(ctx context.Context, store *storage.Store, cfg *config.Config, toEnrich []*storage.StoredIssue, primaryAgent, systemPrompt, lightSystemPrompt string, parallel int) error {
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup

	var totalCost atomic.Int64 // stored as microdollars (cost * 1e6)
	var totalIn, totalOut atomic.Int64
	var succeeded, failed atomic.Int64

	total := len(toEnrich)

	for i, issue := range toEnrich {
		select {
		case <-ctx.Done():
			logf("Ctrl+C — waiting for in-flight workers to finish...\n")
			wg.Wait()
			logf("Stopped.\n\n")
			return showEnrichStatus(store)
		default:
		}

		wg.Add(1)
		sem <- struct{}{} // acquire semaphore

		go func(idx int, issue *storage.StoredIssue) {
			defer wg.Done()
			defer func() { <-sem }() // release semaphore

			isLight := isLightModeIssue(issue, cfg)
			modeTag := ""
			if isLight {
				modeTag = " [light]"
			}
			logf("[%d/%d] %s — %s%s\n", idx+1, total, issue.Issue.Key, truncate(issue.Issue.Summary(), 50), modeTag)

			sysPrompt, issuePrompt := selectPrompts(issue, store, systemPrompt, lightSystemPrompt, isLight)
			start := time.Now()

			result, err := invokeWithBackoff(ctx, primaryAgent, cfg, sysPrompt, issuePrompt, isLight)
			if err != nil {
				logf("  %s FAILED: %v\n\n", issue.Issue.Key, err)
				failed.Add(1)
				return
			}

			enrichment := parseEnrichmentResponse(result.Result)
			enrichment.EnrichedBy = result.Agent
			issue.Enrichment = enrichment
			if isLight {
				issue.EnrichMode = "light"
			}
			if err := store.SaveStoredIssue(issue); err != nil {
				logf("  %s save error: %v\n", issue.Issue.Key, err)
				failed.Add(1)
				return
			}

			totalCost.Add(int64(result.CostUSD * 1e6))
			totalIn.Add(int64(result.InputTokens))
			totalOut.Add(int64(result.OutputTokens))
			succeeded.Add(1)

			elapsed := time.Since(start).Round(time.Second)
			agentTag := ""
			if result.Agent != primaryAgent {
				agentTag = fmt.Sprintf(" [%s]", result.Agent)
			}
			logf("  %s %s — %s%s\n", issue.Issue.Key, enrichment.Confidence, truncate(enrichment.Notes, 50), agentTag)
			if result.CostUSD > 0 {
				logMu.Lock()
				fmt.Printf("         (%d turns, %s, $%.4f, %dk in + %dk out)\n\n", result.NumTurns, elapsed, result.CostUSD, result.InputTokens/1000, result.OutputTokens/1000)
				logMu.Unlock()
			}
		}(i, issue)
	}

	wg.Wait()

	cost := float64(totalCost.Load()) / 1e6
	in := totalIn.Load()
	out := totalOut.Load()
	if cost > 0 {
		logf("Done! %d succeeded, %d failed. Total: $%.4f, %dk in + %dk out\n\n",
			succeeded.Load(), failed.Load(), cost, in/1000, out/1000)
	} else {
		logf("Done! %d succeeded, %d failed.\n\n", succeeded.Load(), failed.Load())
	}
	return showEnrichStatus(store)
}

// invokeWithBackoff tries the primary agent with exponential backoff on rate limits.
// Falls back to the other agent if the primary is rate limited.
func invokeWithBackoff(ctx context.Context, primaryAgent string, cfg *config.Config, systemPrompt, issuePrompt string, lightMode bool) (*agentResult, error) {
	backoffs := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}
	agent := primaryAgent

	for attempt := 0; attempt <= len(backoffs); attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		result, err := invokeAgent(agent, cfg, systemPrompt, issuePrompt, lightMode)
		if err == nil {
			return result, nil
		}

		if !errors.Is(err, claude.ErrRateLimited) && !errors.Is(err, codex.ErrRateLimited) {
			return nil, err
		}

		// Try fallback agent once
		if agent == primaryAgent {
			fallback := otherAgent(primaryAgent)
			logf("  %s rate limited, trying %s...\n", agent, fallback)
			result, err = invokeAgent(fallback, cfg, systemPrompt, issuePrompt, lightMode)
			if err == nil {
				return result, nil
			}
			if !errors.Is(err, claude.ErrRateLimited) && !errors.Is(err, codex.ErrRateLimited) {
				return nil, err
			}
		}

		if attempt < len(backoffs) {
			wait := backoffs[attempt]
			logf("  rate limited, backing off %s...\n", wait)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			agent = primaryAgent // retry primary after backoff
		}
	}

	return nil, fmt.Errorf("all retries exhausted (rate limited)")
}

func invokeAgent(agent string, cfg *config.Config, systemPrompt, issuePrompt string, lightMode bool) (*agentResult, error) {
	switch agent {
	case "claude":
		return invokeClaudeAgent(cfg, systemPrompt, issuePrompt, lightMode)
	case "codex":
		return invokeCodexAgent(cfg, systemPrompt, issuePrompt, lightMode)
	default:
		return nil, fmt.Errorf("unknown agent: %s", agent)
	}
}

func invokeClaudeAgent(cfg *config.Config, systemPrompt, issuePrompt string, lightMode bool) (*agentResult, error) {
	opts := claude.Options{
		Model:        cfg.Enrich.ModelFor("claude"),
		MaxTurns:     cfg.Enrich.MaxTurns,
		SystemPrompt: systemPrompt,
		AllowedTools: []string{"Read", "Glob", "Grep", "Bash"},
		AddDirs:      cfg.Enrich.Repos,
		MCPServers: map[string]claude.MCPServerDef{
			"gopls":                      {Command: "gopls", Args: []string{"mcp"}},
			"typescript-language-server": {Command: "typescript-language-server", Args: []string{"--stdio"}},
		},
	}

	if lightMode {
		opts.AllowedTools = []string{"Read"}
		opts.MCPServers = nil
		if cfg.Enrich.DocsRepo != "" {
			opts.AddDirs = []string{cfg.Enrich.DocsRepo}
		} else {
			opts.AddDirs = nil
		}
	}

	resp, err := claude.Invoke(issuePrompt, opts)
	if err != nil {
		return nil, err
	}

	logf("  [claude] turns=%d, result_len=%d, in=%d, out=%d\n",
		resp.NumTurns, len(resp.Result), resp.TotalInputTokens(), resp.TotalOutputTokens())
	if len(resp.Result) > 0 {
		logf("  [claude raw] %s\n", truncate(resp.Result, 300))
	} else {
		logf("  [claude] empty result! type=%s subtype=%s is_error=%v\n", resp.Type, resp.Subtype, resp.IsError)
	}

	return &agentResult{
		Agent:            "claude",
		Result:           resp.Result,
		SessionID:        resp.SessionID,
		CostUSD:          resp.TotalCostUSD,
		InputTokens:      resp.Usage.InputTokens,
		CacheWriteTokens: resp.Usage.CacheCreationInputTokens,
		CacheReadTokens:  resp.Usage.CacheReadInputTokens,
		OutputTokens:     resp.TotalOutputTokens(),
		NumTurns:         resp.NumTurns,
		ContextRemaining: resp.ContextRemaining(),
	}, nil
}

func invokeCodexAgent(cfg *config.Config, systemPrompt, issuePrompt string, lightMode bool) (*agentResult, error) {
	dirs := cfg.Enrich.Repos
	if lightMode {
		if cfg.Enrich.DocsRepo != "" {
			dirs = []string{cfg.Enrich.DocsRepo}
		} else {
			dirs = nil
		}
	}
	opts := codex.Options{
		Model:   cfg.Enrich.ModelFor("codex"),
		AddDirs: dirs,
	}

	prompt := systemPrompt + "\n\n" + issuePrompt

	var resp *codex.Response
	var err error

	sessionID := cfg.Enrich.Session("codex")
	if sessionID != "" {
		resp, err = codex.Resume(sessionID, prompt, opts)
		if err != nil {
			if errors.Is(err, codex.ErrRateLimited) {
				return nil, err
			}
			resp, err = codex.Invoke(prompt, opts)
		}
	} else {
		resp, err = codex.Invoke(prompt, opts)
	}

	if err != nil {
		return nil, err
	}

	return &agentResult{
		Agent:            "codex",
		Result:           resp.Result,
		SessionID:        resp.SessionID,
		ContextRemaining: -1,
	}, nil
}

// === Prompts ===

func buildEnrichSystemPrompt(repos []string, indexContent string, glossary string) string {
	var repoList string
	for _, r := range repos {
		repoList += fmt.Sprintf("- %s\n", r)
	}

	var indexSection string
	if indexContent != "" {
		indexSection = fmt.Sprintf(`
## Codebase Architecture Index

The following index contains the full architecture of the repos: packages, exported types,
functions, services, infrastructure, and file tree. Use it to orient yourself quickly.

%s
`, indexContent)
	}

	glossarySection := ""
	if glossary != "" {
		glossarySection = fmt.Sprintf("\n## Codebase Glossary\n%s\n", glossary)
	}

	searchInstructions := `I will give you Jira issues one at a time. For each issue:
1. Use the architecture index (if present) to identify which packages, files, and types are relevant
2. Explore the actual code — use Read, Grep, Glob, and MCP tools (gopls) freely
3. If the index includes a markdown (docs) repo, read the relevant doc files for domain context
4. Propose improvements grounded in what you find in the code, not just the index`

	return fmt.Sprintf(`You are a code enrichment agent helping migrate Jira issues to GitHub.

You have access to these local repositories:
%s
%s
%s
%s

Respond with ONLY a JSON object (no other text, no markdown fences):
{
  "proposed_title": "Improved, specific title",
  "proposed_labels": ["label1", "label2"],
  "proposed_description": "Short description grounded in codebase findings.",
  "notes": "Brief explanation of what you found in the code. Do NOT speculate about why a previous attempt was rejected.",
  "related_files": ["path/to/file.go", "path/to/test.go"],
  "confidence": "high|medium|low|none"
}

CRITICAL: Your response must be ONLY the JSON object above. No explanations, no thinking, no markdown.

CRITICAL RULES for proposed_description:
- If the original description already contains significant text (more than a couple sentences), KEEP the original content and append additional context from the codebase below it. Do not rewrite or summarize what's already there. The word limit does not apply in this case.
- Try not to deviate too much from the original title, i.e. if the title is high level then don't change the ticket to be about one tiny thing.
- Try and make the first paragraph a description of the issue and surrounding context.
- For new/short descriptions, keep it under 250 words. Engineers will read this — be concise.
- Write only about what you found in the code and what know about the subject
- understand that we are processing 10k RPS. So recommendations for external tools/architectures must reflect that.
- You can generalize only slightly.
- DO NOT write any code.
- Reference specific files, functions, or lines you found at the end of the description
- End with a short "Definition of Done" (2-4 bullet points).
- DO NOT include: success metrics, technical implementation plans, deployment steps, cost analysis, security considerations (unless the issue is specifically about security), or operational runbooks. These are filler.
- If you didn't find anything relevant in the code, say so. Don't pad the description.

Format for proposed_description:
  [1-3 paragraphs: what this is about, grounded in code you found]

  **Definition of Done:**
  - [ ] concrete deliverable
  - [ ] concrete deliverable

Other guidelines:
- proposed_title: Make it specific. "Fix bug" -> "Fix float64 rounding in invoice total"
- proposed_labels: Use lowercase kebab-case. Include area (billing, auth), type (bug, feature), etc.
- confidence: "high" = found direct code, "medium" = found related code, "low" = tangential, "none" = nothing found
- If confidence is "none", keep the original description mostly intact and just clean it up`, repoList, glossarySection, indexSection, searchInstructions)
}

// isLightModeIssue returns true when the issue's project has light_mode_type configured
// and the issue's type matches, unless --no-light was passed.
func isLightModeIssue(issue *storage.StoredIssue, cfg *config.Config) bool {
	if enrichNoLight {
		return false
	}
	proj := cfg.ProjectByKey(issue.Issue.Project())
	return proj != nil && proj.LightModeType != "" &&
		strings.EqualFold(issue.Issue.IssueType(), proj.LightModeType)
}

// selectPrompts picks the right system/issue prompts based on light mode.
func selectPrompts(issue *storage.StoredIssue, store *storage.Store, systemPrompt, lightSystemPrompt string, isLight bool) (string, string) {
	if !isLight {
		return systemPrompt, buildIssuePrompt(issue)
	}
	return lightSystemPrompt, buildHierarchicalIssuePrompt(issue, store)
}

// buildLightEnrichSystemPrompt builds the system prompt for DISC Idea light enrichment.
// In light mode the agent does not scan source code — only the docs repo (if configured).
func buildLightEnrichSystemPrompt(docsRepo string, glossary string) string {
	glossarySection := ""
	if glossary != "" {
		glossarySection = fmt.Sprintf("\n## Codebase Glossary\n%s\n", glossary)
	}

	repoSection := ""
	toolInstructions := "You have no repository access. Answer based solely on the provided issue context."
	if docsRepo != "" {
		repoSection = fmt.Sprintf("You have access to one documentation repository:\n- %s\n", docsRepo)
		toolInstructions = "You may Read files from the documentation repository for domain context. Do NOT scan source code."
	}

	return fmt.Sprintf(`You are a product enrichment agent helping migrate Jira issues to GitHub.
%s
%s
I will give you a product discovery issue. It may include a list of child epics and their tasks.
Your task is light enrichment — synthesise the hierarchy into a clear feature description:
1. Write a concise feature description that summarises the idea and its child epics/tasks
2. Propose a clear, specific title
3. Suggest appropriate GitHub labels (lowercase kebab-case)
4. %s
5. Do NOT scan source code — this is a product-level feature

Respond with ONLY a JSON object (no other text, no markdown fences):
{
  "proposed_title": "Improved, specific title",
  "proposed_labels": ["label1", "label2"],
  "proposed_description": "Synthesised feature description.",
  "notes": "Brief explanation of what you found. Do NOT speculate about why a previous attempt was rejected.",
  "related_files": [],
  "confidence": "high|medium|low|none"
}

CRITICAL RULES for proposed_description:
- Synthesise child epics and tasks into themes — do NOT list every ticket individually
- Keep it under 300 words
- End with a "Definition of Done" section (2-4 bullet points)
- If there are no child epics, improve the existing description with minor clarifications only`, repoSection, glossarySection, toolInstructions)
}

// buildHierarchicalIssuePrompt builds the issue prompt for a DISC Idea, including
// a summary of its child epics and their tasks/bugs.
func buildHierarchicalIssuePrompt(issue *storage.StoredIssue, store *storage.Store) string {
	base := buildIssuePrompt(issue)

	childEpics, err := store.GetChildIssues(issue.Issue.Key)
	if err != nil || len(childEpics) == 0 {
		return base
	}

	var sb strings.Builder
	sb.WriteString(base)
	sb.WriteString("\n\nChild epics and tasks:")

	const maxChars = 4000
	total := len(base)

	for _, epic := range childEpics {
		if total >= maxChars {
			sb.WriteString("\n  (... truncated)")
			break
		}
		line := fmt.Sprintf("\n  Epic %s: %s", epic.Issue.Key, truncate(epic.Issue.Summary(), 80))
		sb.WriteString(line)
		total += len(line)

		grandchildren, _ := store.GetChildIssues(epic.Issue.Key)
		for _, child := range grandchildren {
			if total >= maxChars {
				sb.WriteString("\n    (... truncated)")
				break
			}
			line := fmt.Sprintf("\n    - %s (%s): %s", child.Issue.Key, child.Issue.IssueType(), truncate(child.Issue.Summary(), 80))
			sb.WriteString(line)
			total += len(line)
		}
	}

	return sb.String()
}

// loadGlossary reads ~/.screwjira/glossary.toml and returns a compact string for the system prompt.
// Returns "" if the file doesn't exist or on error (not fatal).
func loadGlossary(dataDir string) string {
	path := filepath.Join(dataDir, "glossary.toml")
	g, err := index.LoadGlossary(path)
	if err != nil || len(g.Terms) == 0 {
		return ""
	}

	info, _ := os.Stat(path)
	if info != nil && time.Since(info.ModTime()) > 24*time.Hour {
		logf("Warning: glossary.toml is older than 24 hours. Run 'screwjira index' to refresh.\n")
	}

	return index.RenderGlossaryCompact(g, 2000)
}

func buildIssuePrompt(issue *storage.StoredIssue) string {
	var parts []string
	parts = append(parts, "Enrich this issue:")
	parts = append(parts, fmt.Sprintf("Key: %s", issue.Issue.Key))
	parts = append(parts, fmt.Sprintf("Title: %s", issue.Issue.Summary()))

	if t := issue.Issue.IssueType(); t != "" {
		parts = append(parts, fmt.Sprintf("Jira type: %s", t))
	}
	if issue.Classification != nil && issue.Classification.Type != "" {
		parts = append(parts, fmt.Sprintf("Classified as: %s", issue.Classification.Type))
	}

	// Show current labels from all sources
	var allLabels []string
	if issue.Classification != nil && len(issue.Classification.Labels) > 0 {
		allLabels = issue.Classification.Labels
	} else if labels := issue.Issue.Labels(); len(labels) > 0 {
		allLabels = labels
	}
	if len(allLabels) > 0 {
		parts = append(parts, fmt.Sprintf("Current labels: %s", strings.Join(allLabels, ", ")))
	}

	if comps := issue.Issue.Components(); len(comps) > 0 {
		parts = append(parts, fmt.Sprintf("Components: %s", strings.Join(comps, ", ")))
	}
	if s := issue.Issue.Status(); s != "" {
		parts = append(parts, fmt.Sprintf("Status: %s", s))
	}
	if a := issue.Issue.Assignee(); a != "" {
		parts = append(parts, fmt.Sprintf("Assignee: %s", a))
	}
	if p := issue.Issue.ParentKey(); p != "" {
		parts = append(parts, fmt.Sprintf("Parent: %s", p))
	}
	if links := issue.Issue.IssueLinks(); len(links) > 0 {
		parts = append(parts, fmt.Sprintf("Linked issues: %s", strings.Join(links, ", ")))
	}

	if desc := issue.Issue.Description(); desc != "" {
		if len(desc) > 2000 {
			desc = desc[:2000] + "\n..."
		}
		parts = append(parts, fmt.Sprintf("\nDescription:\n%s", desc))
	}

	// If previously rejected, include the feedback so the agent can improve
	if issue.Enrichment != nil && issue.Enrichment.Decision == storage.EnrichRejected {
		parts = append(parts, "\n⚠️ PREVIOUS ATTEMPT WAS REJECTED.")
		if issue.Enrichment.RejectionReason != "" {
			parts = append(parts, fmt.Sprintf("Reviewer feedback: %s", issue.Enrichment.RejectionReason))
			parts = append(parts, "Please try again, addressing the feedback above.")
		} else {
			parts = append(parts, "No specific feedback was provided. Please try again.")
		}
		parts = append(parts, fmt.Sprintf("Previous proposed title: %s", issue.Enrichment.ProposedTitle))
		if len(issue.Enrichment.ProposedLabels) > 0 {
			parts = append(parts, fmt.Sprintf("Previous proposed labels: %s", strings.Join(issue.Enrichment.ProposedLabels, ", ")))
		}
	}

	return strings.Join(parts, "\n")
}

// === Response parsing ===

func parseEnrichmentResponse(text string) *storage.Enrichment {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")

	if start != -1 && end > start {
		jsonStr := text[start : end+1]
		var enrichment storage.Enrichment
		if err := json.Unmarshal([]byte(jsonStr), &enrichment); err == nil {
			enrichment.Decision = storage.EnrichPending
			return &enrichment
		} else {
			logf("  [parse] JSON error: %v\n", err)
			logf("  [parse] JSON snippet: %s\n", truncate(jsonStr, 200))
		}
	} else {
		logf("  [parse] no JSON found in response (len=%d)\n", len(text))
		if len(text) > 0 {
			logf("  [parse] response: %s\n", truncate(text, 300))
		}
	}

	// Fallback: couldn't parse JSON
	notes := text
	if len(notes) > 500 {
		notes = notes[:500] + "..."
	}
	return &storage.Enrichment{
		Notes:      notes,
		Confidence: "unknown",
		Decision:   storage.EnrichPending,
	}
}

// === Reject unknown ===

func rejectUnknownEnrichments(store *storage.Store) error {
	issues, err := store.GetKeptIssues()
	if err != nil {
		return err
	}

	rejected := 0
	for _, issue := range issues {
		if issue.Enrichment != nil && issue.Enrichment.Confidence == "unknown" {
			issue.Enrichment.Decision = storage.EnrichRejected
			issue.Enrichment.RejectionReason = "auto-rejected: unknown confidence (unparseable agent response)"
			if err := store.SaveStoredIssue(issue); err != nil {
				return fmt.Errorf("failed to save %s: %w", issue.Issue.Key, err)
			}
			fmt.Printf("  rejected %s\n", issue.Issue.Key)
			rejected++
		}
	}

	fmt.Printf("\nRejected %d issues with unknown confidence.\n", rejected)
	return showEnrichStatus(store)
}

// === Interactive review ===

func interactiveEnrichReview(store *storage.Store, issueKey, filterDecision, filterConfidence string) error {
	var toReview []*storage.StoredIssue

	if issueKey != "" {
		stored, err := store.LoadIssue(issueKey)
		if err != nil {
			return fmt.Errorf("issue not found: %w", err)
		}
		if stored.Enrichment == nil {
			return fmt.Errorf("%s has not been enriched yet", issueKey)
		}
		toReview = []*storage.StoredIssue{stored}
	} else {
		issues, err := store.GetKeptIssues()
		if err != nil {
			return err
		}

		// Determine which decisions to show
		var wantDecisions map[storage.EnrichDecision]bool
		if filterDecision != "" {
			d := storage.EnrichDecision(filterDecision)
			switch d {
			case storage.EnrichPending, storage.EnrichAccepted, storage.EnrichRejected, storage.EnrichDeferred:
				wantDecisions = map[storage.EnrichDecision]bool{d: true}
			default:
				return fmt.Errorf("unknown filter %q — use: pending, accepted, rejected, deferred", filterDecision)
			}
		} else {
			// Default: show pending + deferred
			wantDecisions = map[storage.EnrichDecision]bool{
				storage.EnrichPending:  true,
				storage.EnrichDeferred: true,
			}
		}

		var wantConfidences map[string]bool
		if filterConfidence != "" {
			wantConfidences = make(map[string]bool)
			for _, c := range strings.Split(filterConfidence, ",") {
				c = strings.TrimSpace(strings.ToLower(c))
				switch c {
				case "high", "medium", "low", "none", "unknown":
					wantConfidences[c] = true
				default:
					return fmt.Errorf("unknown confidence %q — use: high, medium, low, none", c)
				}
			}
		}

		for _, issue := range issues {
			if issue.Enrichment == nil {
				continue
			}
			if !wantDecisions[issue.Enrichment.Decision] {
				continue
			}
			if wantConfidences != nil && !wantConfidences[issue.Enrichment.Confidence] {
				continue
			}
			toReview = append(toReview, issue)
		}
	}

	if len(toReview) == 0 {
		fmt.Println("No enrichments to review.")
		return showEnrichStatus(store)
	}

	sort.Slice(toReview, func(i, j int) bool {
		return toReview[i].Issue.Key < toReview[j].Issue.Key
	})

	filterLabel := "pending+deferred"
	if filterDecision != "" {
		filterLabel = filterDecision
	}
	if filterConfidence != "" {
		filterLabel += ", confidence=" + filterConfidence
	}
	fmt.Printf("Found %d enrichments to review (filter: %s).\n", len(toReview), filterLabel)
	fmt.Println("Commands: [a]ccept [r]eject [d]efer [s]kip [v]iew-full [q]uit [?]help")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)

	accepted, rejected, deferred, skipped := 0, 0, 0, 0

	for i := 0; i < len(toReview); {
		issue := toReview[i]
		displayEnrichmentDiff(issue, i+1, len(toReview))

		fmt.Print("\nAction: ")
		input, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		input = strings.TrimSpace(strings.ToLower(input))

		switch input {
		case "a", "accept":
			issue.Enrichment.Decision = storage.EnrichAccepted
			if err := store.SaveStoredIssue(issue); err != nil {
				return err
			}
			accepted++
			i++
			fmt.Println("✓ Accepted")

		case "r", "reject":
			reason, err := readLineWithEditing("Reason (helps the agent do better next time): ")
			if err == readline.ErrInterrupt || err == io.EOF {
				fmt.Println("  (cancelled)")
				continue
			}
			if err != nil {
				return err
			}
			issue.Enrichment.Decision = storage.EnrichRejected
			issue.Enrichment.RejectionReason = reason
			if err := store.SaveStoredIssue(issue); err != nil {
				return err
			}
			rejected++
			i++
			fmt.Println("✗ Rejected")

		case "d", "defer":
			issue.Enrichment.Decision = storage.EnrichDeferred
			if err := store.SaveStoredIssue(issue); err != nil {
				return err
			}
			deferred++
			i++
			fmt.Println("⏸ Deferred")

		case "s", "skip":
			reason, err := readLineWithEditing("Reason (optional): ")
			if err == readline.ErrInterrupt || err == io.EOF {
				fmt.Println("  (cancelled)")
				continue
			}
			if err != nil {
				return err
			}
			if err := store.UpdateDecision(issue.Issue.Key, storage.DecisionSkip, reason); err != nil {
				return err
			}
			skipped++
			i++
			fmt.Println("✗ Skipped (permanently)")

		case "v", "view":
			displayFullEnrichment(issue)

		case "q", "quit":
			fmt.Printf("\nSession ended. Accepted: %d, Rejected: %d, Deferred: %d, Skipped: %d\n", accepted, rejected, deferred, skipped)
			return nil

		case "?", "help":
			printEnrichReviewHelp()

		default:
			fmt.Println("Unknown command. Type ? for help.")
		}

		fmt.Println()
	}

	fmt.Printf("\nAll reviewed! Accepted: %d, Rejected: %d, Deferred: %d, Skipped: %d\n", accepted, rejected, deferred, skipped)
	return nil
}

// readLineWithEditing opens a short-lived readline instance for a single prompted
// input, giving the user arrow-key navigation and line editing. readline is closed
// immediately after the read so it does not hold raw mode while fmt.Printf output runs.
func readLineWithEditing(prompt string) (string, error) {
	rl, err := readline.New(prompt)
	if err != nil {
		return "", err
	}
	defer rl.Close()
	line, err := rl.Readline()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func displayEnrichmentDiff(issue *storage.StoredIssue, current, total int) {
	e := issue.Enrichment

	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("[%d/%d] %s  confidence: %s  enriched by %s\n", current, total, issue.Issue.Key, e.Confidence, e.EnrichedBy)
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")

	// Title diff
	oldTitle := issue.Issue.Summary()
	newTitle := e.ProposedTitle
	if newTitle != "" && newTitle != oldTitle {
		fmt.Println("\nTitle:")
		fmt.Printf("  \033[31m- %s\033[0m\n", oldTitle)
		fmt.Printf("  \033[32m+ %s\033[0m\n", newTitle)
	} else {
		fmt.Printf("\nTitle: %s (unchanged)\n", oldTitle)
	}

	// Labels diff
	var oldLabels []string
	if issue.Classification != nil && len(issue.Classification.Labels) > 0 {
		oldLabels = issue.Classification.Labels
	} else {
		oldLabels = issue.Issue.Labels()
	}
	newLabels := e.ProposedLabels

	if len(newLabels) > 0 {
		oldStr := "(none)"
		if len(oldLabels) > 0 {
			oldStr = strings.Join(oldLabels, ", ")
		}
		newStr := strings.Join(newLabels, ", ")
		if oldStr != newStr {
			fmt.Println("\nLabels:")
			fmt.Printf("  \033[31m- %s\033[0m\n", oldStr)
			fmt.Printf("  \033[32m+ %s\033[0m\n", newStr)
		} else {
			fmt.Printf("\nLabels: %s (unchanged)\n", oldStr)
		}
	}

	// Description diff
	oldDesc := strings.TrimSpace(issue.Issue.Description())
	newDesc := strings.TrimSpace(e.ProposedDescription)
	if newDesc != "" && newDesc != oldDesc {
		fmt.Println("\nDescription:")
		if oldDesc != "" {
			fmt.Printf("  \033[31m--- original (%d chars) ---\033[0m\n", len(oldDesc))
			for _, line := range strings.Split(oldDesc, "\n") {
				fmt.Printf("  \033[31m  %s\033[0m\n", line)
			}
		} else {
			fmt.Printf("  \033[31m  (empty)\033[0m\n")
		}
		fmt.Printf("  \033[32m--- proposed (%d chars) ---\033[0m\n", len(newDesc))
		for _, line := range strings.Split(newDesc, "\n") {
			fmt.Printf("  \033[32m  %s\033[0m\n", line)
		}
	} else if newDesc == "" {
		fmt.Println("\nDescription: (no changes proposed)")
	}

	// Related files
	if len(e.RelatedFiles) > 0 {
		fmt.Println("\nRelated files:")
		for _, f := range e.RelatedFiles {
			fmt.Printf("  %s\n", f)
		}
	}

	// Agent notes
	if e.Notes != "" {
		fmt.Println("\nAgent notes:")
		for _, line := range strings.Split(e.Notes, "\n") {
			fmt.Printf("  %s\n", line)
		}
	}
}

func displayFullEnrichment(issue *storage.StoredIssue) {
	e := issue.Enrichment

	fmt.Println("\n=== Full Enrichment Details ===")
	fmt.Printf("Key:        %s\n", issue.Issue.Key)
	fmt.Printf("Confidence: %s\n", e.Confidence)

	fmt.Println("\n--- Original Title ---")
	fmt.Println(issue.Issue.Summary())
	fmt.Println("\n--- Proposed Title ---")
	fmt.Println(e.ProposedTitle)

	fmt.Println("\n--- Original Labels ---")
	var oldLabels []string
	if issue.Classification != nil && len(issue.Classification.Labels) > 0 {
		oldLabels = issue.Classification.Labels
	} else {
		oldLabels = issue.Issue.Labels()
	}
	if len(oldLabels) > 0 {
		fmt.Println(strings.Join(oldLabels, ", "))
	} else {
		fmt.Println("(none)")
	}
	fmt.Println("\n--- Proposed Labels ---")
	if len(e.ProposedLabels) > 0 {
		fmt.Println(strings.Join(e.ProposedLabels, ", "))
	} else {
		fmt.Println("(none)")
	}

	fmt.Println("\n--- Original Description ---")
	desc := issue.Issue.Description()
	if desc != "" {
		fmt.Println(desc)
	} else {
		fmt.Println("(empty)")
	}

	fmt.Println("\n--- Proposed Description ---")
	if e.ProposedDescription != "" {
		fmt.Println(e.ProposedDescription)
	} else {
		fmt.Println("(none)")
	}

	if len(e.RelatedFiles) > 0 {
		fmt.Println("\n--- Related Files ---")
		for _, f := range e.RelatedFiles {
			fmt.Println(f)
		}
	}

	if e.Notes != "" {
		fmt.Println("\n--- Agent Notes ---")
		fmt.Println(e.Notes)
	}
}

func printEnrichReviewHelp() {
	fmt.Println(`
Commands:
  a, accept  - Accept proposed changes
  r, reject  - Reject proposed changes (re-queues for agent retry)
  d, defer   - Defer decision for later
  s, skip    - Permanently skip this issue (removes from migration)
  v, view    - Show full original + proposed side by side
  q, quit    - Save and quit
  ?, help    - Show this help`)
}

// === Status ===

func showEnrichStatus(store *storage.Store) error {
	issues, err := store.GetKeptIssues()
	if err != nil {
		return err
	}

	enriched := 0
	byConfidence := map[string]int{}
	byDecision := map[storage.EnrichDecision]int{}

	for _, issue := range issues {
		if issue.Enrichment != nil {
			enriched++
			byConfidence[issue.Enrichment.Confidence]++
			byDecision[issue.Enrichment.Decision]++
		}
	}

	fmt.Println("Enrichment Status:")
	fmt.Printf("  Enriched:  %d/%d\n", enriched, len(issues))
	for _, c := range []string{"high", "medium", "low", "none", "unknown"} {
		if n, ok := byConfidence[c]; ok {
			fmt.Printf("    %-8s %d\n", c+":", n)
		}
	}

	if enriched > 0 {
		fmt.Println("\n  Review:")
		for _, d := range []storage.EnrichDecision{storage.EnrichPending, storage.EnrichAccepted, storage.EnrichRejected, storage.EnrichDeferred} {
			if n, ok := byDecision[d]; ok {
				fmt.Printf("    %-10s %d\n", string(d)+":", n)
			}
		}
	}

	remaining := len(issues) - enriched
	pendingReview := byDecision[storage.EnrichPending] + byDecision[storage.EnrichDeferred]

	if remaining > 0 {
		fmt.Printf("\n  %d issues not yet enriched. Run 'screwjira enrich' to process.\n", remaining)
	}
	if pendingReview > 0 {
		fmt.Printf("  %d enrichments awaiting review. Run 'screwjira enrich --review' to review.\n", pendingReview)
	}

	return nil
}
