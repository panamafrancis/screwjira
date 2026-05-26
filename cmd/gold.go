package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panamafrancis/screwjira/internal/claude"
	"github.com/panamafrancis/screwjira/internal/storage"
	"github.com/spf13/cobra"
)

// The gold command runs a cheap post-enrichment classifier (Haiku) over kept
// issues whose enrichments were accepted, and flags ones where the agent
// found a concrete bug or fix worth a maintainer's attention. The verdict is
// persisted to the issue's JSON; the `agent-found-gold` label is added to
// the issue's labels on the next `post --apply` run (see buildIssueContent).

const (
	goldLabel             = "agent-found-gold"
	goldDefaultModel      = "claude-haiku-4-5-20251001"
	goldOriginalDescCap   = 2000
	goldProposedDescCap   = 2000
	goldNotesCap          = 2000
)

var (
	goldIssueKey    string
	goldJiraProject string
	goldLimit       int
	goldForce       bool
	goldStatus      bool
	goldParallel    int
	goldModel       string
)

var goldCmd = &cobra.Command{
	Use:   "gold",
	Short: "Flag enriched issues where the agent found a real bug or fix",
	Long: `Review issues with accepted enrichments and decide whether the agent
identified a concrete bug or fix in the codebase. The verdict is saved to
the issue and the ` + "`agent-found-gold`" + ` label is applied on the next
post --apply run.

Only issues with decision=keep and enrichment.decision=accepted are eligible.
Already-reviewed issues are skipped unless --force is set.`,
	RunE: runGold,
}

func init() {
	rootCmd.AddCommand(goldCmd)
	goldCmd.Flags().StringVar(&goldIssueKey, "issue", "", "review a single issue by key")
	goldCmd.Flags().StringVar(&goldJiraProject, "jira-project", "", "filter by Jira project key (e.g. F0)")
	goldCmd.Flags().IntVar(&goldLimit, "limit", 0, "max issues to review (0 = all)")
	goldCmd.Flags().BoolVar(&goldForce, "force", false, "re-review issues that already have a verdict")
	goldCmd.Flags().BoolVar(&goldStatus, "status", false, "show gold review progress and exit")
	goldCmd.Flags().IntVar(&goldParallel, "parallel", 1, "concurrent workers")
	goldCmd.Flags().StringVar(&goldModel, "model", goldDefaultModel, "Claude model to use for classification")
}

func runGold(cmd *cobra.Command, args []string) error {
	store, err := storage.New(getDataDir())
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}

	if goldStatus {
		return showGoldStatus(store)
	}

	toReview, err := buildGoldList(store)
	if err != nil {
		return err
	}

	if len(toReview) == 0 {
		fmt.Println("No issues to review.")
		return showGoldStatus(store)
	}

	return reviewGoldIssues(store, toReview)
}

func buildGoldList(store *storage.Store) ([]*storage.StoredIssue, error) {
	issues, err := store.GetKeptIssues()
	if err != nil {
		return nil, err
	}

	var result []*storage.StoredIssue
	for _, issue := range issues {
		if issue.Enrichment == nil || issue.Enrichment.Decision != storage.EnrichAccepted {
			continue
		}
		if issue.Gold != nil && !goldForce {
			continue
		}
		if goldIssueKey != "" && issue.Issue.Key != goldIssueKey {
			continue
		}
		if goldJiraProject != "" && !strings.EqualFold(issue.Issue.Project(), goldJiraProject) {
			continue
		}
		result = append(result, issue)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Issue.Key < result[j].Issue.Key
	})

	if goldLimit > 0 && len(result) > goldLimit {
		result = result[:goldLimit]
	}

	return result, nil
}

func reviewGoldIssues(store *storage.Store, issues []*storage.StoredIssue) error {
	parallel := goldParallel
	if parallel < 1 {
		parallel = 1
	}
	if parallel > 1 {
		fmt.Printf("Running %d workers in parallel (model: %s).\n\n", parallel, goldModel)
	} else {
		fmt.Printf("Reviewing %d issue(s) with %s.\n\n", len(issues), goldModel)
	}

	var gold, notGold, failed atomic.Int64
	var saveErr atomic.Value

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	total := len(issues)

	for i, issue := range issues {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}

		go func(idx int, issue *storage.StoredIssue) {
			defer wg.Done()
			defer func() { <-sem }()

			if ctx.Err() != nil {
				return
			}

			var buf bytes.Buffer
			fmt.Fprintf(&buf, "[%d/%d] %s — %s\n", idx+1, total, issue.Issue.Key, truncate(issue.Issue.Summary(), 55))

			verdict, err := classifyGold(issue)
			if err != nil {
				fmt.Fprintf(&buf, "  FAILED: %v\n\n", err)
				failed.Add(1)
				flushLog(&buf)
				return
			}

			issue.Gold = verdict
			if err := store.SaveStoredIssue(issue); err != nil {
				fmt.Fprintf(&buf, "  CRITICAL: classified but failed to save: %v\n", err)
				fmt.Fprintf(&buf, "  Cancelling further workers to avoid burning LLM calls we can't persist.\n\n")
				saveErr.CompareAndSwap(nil, fmt.Errorf("failed to save gold verdict for %s: %w", issue.Issue.Key, err))
				cancel()
				failed.Add(1)
				flushLog(&buf)
				return
			}

			if verdict.IsGold {
				gold.Add(1)
				fmt.Fprintf(&buf, "  GOLD — %s\n", truncate(verdict.Reason, 120))
				if len(verdict.Evidence) > 0 {
					fmt.Fprintf(&buf, "    evidence: %s\n", strings.Join(verdict.Evidence, ", "))
				}
				fmt.Fprintln(&buf)
			} else {
				notGold.Add(1)
				fmt.Fprintf(&buf, "  not gold — %s\n\n", truncate(verdict.Reason, 120))
			}
			flushLog(&buf)
		}(i, issue)
	}

	wg.Wait()

	fmt.Printf("Done. %d gold, %d not gold, %d failed.\n", gold.Load(), notGold.Load(), failed.Load())
	if err, ok := saveErr.Load().(error); ok && err != nil {
		return err
	}
	return showGoldStatus(store)
}

const goldSystemPrompt = `You are reviewing a Jira issue enrichment produced by an AI coding agent that had access to the codebase. Decide whether the agent identified a concrete, actionable bug or fix worth a maintainer's attention.

YES (is_gold=true) if ANY of:
- Agent located the buggy code (specific file path, function name, or line reference)
- Agent diagnosed a root cause that was not stated in the original ticket
- Agent proposed a specific code change with justification

NO (is_gold=false) if:
- Agent merely clarified, reformatted, or restructured the ticket
- Generic architectural advice without specific code references
- "Needs investigation" / "to be determined" / open questions without findings
- Only restated the ticket's own description in different words
- Mentions files only as area-of-interest or context, without identifying a defect or specific change in them

Be skeptical. The presence of file paths in "Related files" alone is not enough — the agent must say what is wrong or what to change in those files.

Reply with JSON only, no prose, no markdown fences:
{"is_gold": <bool>, "reason": "<one sentence>", "evidence": ["path/file.go:42", "..."]}

Keep "evidence" to file/line references the agent cited; empty array if none.`

func classifyGold(issue *storage.StoredIssue) (*storage.GoldVerdict, error) {
	prompt := buildGoldPrompt(issue)
	opts := claude.Options{
		Model:        goldModel,
		MaxTurns:     1,
		SystemPrompt: goldSystemPrompt,
	}
	resp, err := claude.Invoke(prompt, opts)
	if err != nil {
		return nil, err
	}
	return parseGoldResponse(resp.Result)
}

func buildGoldPrompt(issue *storage.StoredIssue) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Issue: %s — %s\n\n", issue.Issue.Key, issue.Issue.Summary())

	if orig := strings.TrimSpace(issue.Issue.Description()); orig != "" {
		fmt.Fprintf(&sb, "Original description:\n%s\n\n", truncate(orig, goldOriginalDescCap))
	}

	enr := issue.Enrichment
	if enr == nil {
		return sb.String()
	}
	if enr.ProposedDescription != "" {
		fmt.Fprintf(&sb, "Agent proposed description:\n%s\n\n", truncate(enr.ProposedDescription, goldProposedDescCap))
	}
	if enr.Notes != "" {
		fmt.Fprintf(&sb, "Agent notes:\n%s\n\n", truncate(enr.Notes, goldNotesCap))
	}
	if len(enr.RelatedFiles) > 0 {
		fmt.Fprintf(&sb, "Related files (per agent): %s\n", strings.Join(enr.RelatedFiles, ", "))
	}

	return sb.String()
}

func parseGoldResponse(text string) (*storage.GoldVerdict, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start == -1 || end <= start {
		return nil, fmt.Errorf("no JSON object in response: %s", truncate(text, 200))
	}

	var raw struct {
		IsGold   bool     `json:"is_gold"`
		Reason   string   `json:"reason"`
		Evidence []string `json:"evidence"`
	}
	jsonStr := text[start : end+1]
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil, fmt.Errorf("parse JSON: %w (snippet: %s)", err, truncate(jsonStr, 200))
	}

	return &storage.GoldVerdict{
		IsGold:     raw.IsGold,
		Reason:     strings.TrimSpace(raw.Reason),
		Evidence:   raw.Evidence,
		Model:      goldModel,
		ReviewedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func showGoldStatus(store *storage.Store) error {
	issues, err := store.GetKeptIssues()
	if err != nil {
		return err
	}

	var eligible, reviewed, gold int
	for _, issue := range issues {
		if issue.Enrichment == nil || issue.Enrichment.Decision != storage.EnrichAccepted {
			continue
		}
		eligible++
		if issue.Gold != nil {
			reviewed++
			if issue.Gold.IsGold {
				gold++
			}
		}
	}

	fmt.Printf("\nGold Review Status:\n")
	fmt.Printf("  Eligible (kept + accepted enrichment): %d\n", eligible)
	fmt.Printf("  Reviewed:   %d\n", reviewed)
	fmt.Printf("  Gold:       %d\n", gold)
	fmt.Printf("  Not gold:   %d\n", reviewed-gold)
	fmt.Printf("  Unreviewed: %d\n", eligible-reviewed)
	return nil
}
