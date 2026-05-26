package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/fraud-zero/screwjira/internal/storage"
	"github.com/spf13/cobra"
)

var (
	filterStart      bool
	filterByStatus   string
	filterByAge      int
	filterByAssignee string
	filterResume     bool
)

var filterCmd = &cobra.Command{
	Use:   "filter",
	Short: "Filter issues interactively",
	Long:  `Triage issues with keep/skip/defer decisions.`,
	RunE:  runFilter,
}

func init() {
	rootCmd.AddCommand(filterCmd)
	filterCmd.Flags().BoolVar(&filterStart, "start", false, "start interactive filtering")
	filterCmd.Flags().StringVar(&filterByStatus, "by-status", "", "auto-skip issues with this status")
	filterCmd.Flags().IntVar(&filterByAge, "by-age", 0, "flag issues older than N days without updates")
	filterCmd.Flags().StringVar(&filterByAssignee, "by-assignee", "", "flag issues assigned to this person")
	filterCmd.Flags().BoolVar(&filterResume, "resume", false, "resume interactive filtering")
}

func runFilter(cmd *cobra.Command, args []string) error {
	store, err := storage.New(getDataDir())
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}

	switch {
	case filterByStatus != "":
		return filterByStatusFn(store, filterByStatus)
	case filterByAge > 0:
		return filterByAgeFn(store, filterByAge)
	case filterByAssignee != "":
		return filterByAssigneeFn(store, filterByAssignee)
	case filterStart || filterResume:
		return interactiveFilter(store)
	default:
		return showFilterStatus(store)
	}
}

func filterByStatusFn(store *storage.Store, status string) error {
	issues, err := store.GetPendingIssues()
	if err != nil {
		return err
	}

	count := 0
	for _, issue := range issues {
		if strings.EqualFold(issue.Issue.Status(), status) {
			if err := store.UpdateDecision(issue.Issue.Key, storage.DecisionSkip, fmt.Sprintf("auto-skip: status=%s", status)); err != nil {
				return err
			}
			count++
		}
	}

	fmt.Printf("Skipped %d issues with status '%s'\n", count, status)
	return nil
}

func filterByAgeFn(store *storage.Store, days int) error {
	issues, err := store.GetPendingIssues()
	if err != nil {
		return err
	}

	cutoff := time.Now().AddDate(0, 0, -days)
	count := 0

	for _, issue := range issues {
		updated := issue.Issue.Updated()
		if updated == "" {
			continue
		}

		t, err := time.Parse("2006-01-02T15:04:05.000-0700", updated)
		if err != nil {
			continue
		}

		if t.Before(cutoff) {
			if err := store.UpdateDecision(issue.Issue.Key, storage.DecisionSkip, fmt.Sprintf("auto-skip: no updates in %d days", days)); err != nil {
				return err
			}
			count++
		}
	}

	fmt.Printf("Skipped %d issues with no updates in %d days\n", count, days)
	return nil
}

func filterByAssigneeFn(store *storage.Store, assignee string) error {
	issues, err := store.GetPendingIssues()
	if err != nil {
		return err
	}

	count := 0
	for _, issue := range issues {
		if strings.Contains(strings.ToLower(issue.Issue.Assignee()), strings.ToLower(assignee)) {
			if err := store.UpdateDecision(issue.Issue.Key, storage.DecisionSkip, fmt.Sprintf("auto-skip: assignee=%s", assignee)); err != nil {
				return err
			}
			count++
		}
	}

	fmt.Printf("Skipped %d issues assigned to '%s'\n", count, assignee)
	return nil
}

func showFilterStatus(store *storage.Store) error {
	stats, err := store.Stats()
	if err != nil {
		return err
	}

	fmt.Println("Filter Status:")
	fmt.Printf("  Total:   %d\n", stats.Total)
	fmt.Printf("  Pending: %d\n", stats.Pending)
	fmt.Printf("  Keep:    %d\n", stats.Keep)
	fmt.Printf("  Skip:    %d\n", stats.Skip)
	fmt.Printf("  Defer:   %d\n", stats.Defer)

	if stats.Pending > 0 {
		fmt.Printf("\nRun 'screwjira filter --start' to begin interactive filtering\n")
	}

	return nil
}

func interactiveFilter(store *storage.Store) error {
	issues, err := store.GetPendingIssues()
	if err != nil {
		return err
	}

	if len(issues) == 0 {
		fmt.Println("No pending issues to filter.")
		return nil
	}

	// Sort by project, then by key
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Issue.Project() != issues[j].Issue.Project() {
			return issues[i].Issue.Project() < issues[j].Issue.Project()
		}
		return issues[i].Issue.Key < issues[j].Issue.Key
	})

	fmt.Printf("Found %d pending issues. Starting interactive filter...\n", len(issues))
	fmt.Println("Commands: [k]eep [s]kip [d]efer [v]iew [o]pen [q]uit [?]help")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	kept, skipped, deferred := 0, 0, 0

	for i := 0; i < len(issues); {
		issue := issues[i]
		displayIssueSummary(issue, i+1, len(issues))

		fmt.Print("\nAction: ")
		input, err := reader.ReadString('\n')
		if err != nil {
			return err
		}

		input = strings.TrimSpace(strings.ToLower(input))

		switch input {
		case "k", "keep":
			if err := store.UpdateDecision(issue.Issue.Key, storage.DecisionKeep, ""); err != nil {
				return err
			}
			kept++
			i++
			fmt.Println("✓ Kept")

		case "s", "skip":
			fmt.Print("Reason (optional): ")
			reason, _ := reader.ReadString('\n')
			reason = strings.TrimSpace(reason)
			if err := store.UpdateDecision(issue.Issue.Key, storage.DecisionSkip, reason); err != nil {
				return err
			}
			skipped++
			i++
			fmt.Println("✗ Skipped")

		case "d", "defer":
			if err := store.UpdateDecision(issue.Issue.Key, storage.DecisionDefer, ""); err != nil {
				return err
			}
			deferred++
			i++
			fmt.Println("⏸ Deferred")

		case "v", "view":
			displayIssueFull(issue)

		case "o", "open":
			openInBrowser(issue.Issue.Key)

		case "q", "quit":
			fmt.Printf("\nSession ended. Kept: %d, Skipped: %d, Deferred: %d\n", kept, skipped, deferred)
			return nil

		case "?", "help":
			printHelp()

		default:
			fmt.Println("Unknown command. Type ? for help.")
		}

		fmt.Println()
	}

	fmt.Printf("\nAll issues processed! Kept: %d, Skipped: %d, Deferred: %d\n", kept, skipped, deferred)
	return nil
}

func displayIssueSummary(issue *storage.StoredIssue, current, total int) {
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("[%d/%d] %s\n", current, total, issue.Issue.Key)
	fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
	fmt.Printf("Summary:  %s\n", issue.Issue.Summary())
	fmt.Printf("Type:     %s | Status: %s | Priority: %s\n",
		issue.Issue.IssueType(), issue.Issue.Status(), issue.Issue.Priority())
	fmt.Printf("Reporter: %s | Assignee: %s\n",
		nvl(issue.Issue.Reporter(), "-"), nvl(issue.Issue.Assignee(), "-"))
	fmt.Printf("Created:  %s\n", formatTime(issue.Issue.Created()))

	if labels := issue.Issue.Labels(); len(labels) > 0 {
		fmt.Printf("Labels:   %s\n", strings.Join(labels, ", "))
	}

	desc := issue.Issue.Description()
	if desc != "" {
		fmt.Printf("\nDescription:\n%s\n", truncateLines(desc, 5))
	}
}

func displayIssueFull(issue *storage.StoredIssue) {
	fmt.Println("\n=== Full Issue Details ===")
	fmt.Printf("Key:         %s\n", issue.Issue.Key)
	fmt.Printf("Summary:     %s\n", issue.Issue.Summary())
	fmt.Printf("Type:        %s\n", issue.Issue.IssueType())
	fmt.Printf("Status:      %s\n", issue.Issue.Status())
	fmt.Printf("Priority:    %s\n", issue.Issue.Priority())
	fmt.Printf("Reporter:    %s\n", nvl(issue.Issue.Reporter(), "-"))
	fmt.Printf("Assignee:    %s\n", nvl(issue.Issue.Assignee(), "-"))
	fmt.Printf("Created:     %s\n", issue.Issue.Created())
	fmt.Printf("Updated:     %s\n", issue.Issue.Updated())

	if labels := issue.Issue.Labels(); len(labels) > 0 {
		fmt.Printf("Labels:      %s\n", strings.Join(labels, ", "))
	}

	fmt.Println("\nDescription:")
	fmt.Println(issue.Issue.Description())

	// Show comments if present
	if comments, ok := issue.Issue.Fields["comment"].(map[string]interface{}); ok {
		if commentList, ok := comments["comments"].([]interface{}); ok && len(commentList) > 0 {
			fmt.Printf("\nComments (%d):\n", len(commentList))
			for _, c := range commentList {
				if cm, ok := c.(map[string]interface{}); ok {
					author := "-"
					if a, ok := cm["author"].(map[string]interface{}); ok {
						if name, ok := a["displayName"].(string); ok {
							author = name
						}
					}
					created := ""
					if c, ok := cm["created"].(string); ok {
						created = formatTime(c)
					}
					body := ""
					if b, ok := cm["body"].(map[string]interface{}); ok {
						body = extractTextFromADF(b)
					}
					fmt.Printf("\n  [%s] %s:\n  %s\n", created, author, indent(body, "  "))
				}
			}
		}
	}
}

func extractTextFromADF(doc map[string]interface{}) string {
	content, ok := doc["content"].([]interface{})
	if !ok {
		return ""
	}

	var result string
	for _, block := range content {
		blockMap, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		innerContent, ok := blockMap["content"].([]interface{})
		if !ok {
			continue
		}
		for _, item := range innerContent {
			itemMap, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if text, ok := itemMap["text"].(string); ok {
				result += text
			}
		}
		result += "\n"
	}
	return strings.TrimSpace(result)
}

func openInBrowser(key string) {
	site := jiraSiteURL()
	if site == "" {
		fmt.Printf("Open in browser: https://your-site.atlassian.net/browse/%s\n", key)
		return
	}
	url := "https://" + site + "/browse/" + key

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		fmt.Printf("Open in browser: %s\n", url)
		return
	}
	cmd.Run()
}

// jiraSiteURL reads the acli jira config and returns the site hostname for the
// current profile (e.g. "fraud0.atlassian.net").
// parseJiraConfig parses the current cloud_id and site from acli's jira_config.yaml.
func parseJiraConfig() (cloudID, site string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "acli", "jira_config.yaml"))
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")

	// Extract cloud_id from current_profile: <cloud_id>:<rest>
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "current_profile:") {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "current_profile:"))
			cloudID = strings.SplitN(val, ":", 2)[0]
			break
		}
	}

	// Walk profiles to find the matching site.
	// YAML list items are prefixed with "- ", so strip that before matching.
	var lastSite string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.TrimPrefix(trimmed, "- ")
		if strings.HasPrefix(trimmed, "site:") {
			lastSite = strings.TrimSpace(strings.TrimPrefix(trimmed, "site:"))
		} else if strings.HasPrefix(trimmed, "cloud_id:") {
			cid := strings.TrimSpace(strings.TrimPrefix(trimmed, "cloud_id:"))
			if cloudID == "" || cid == cloudID {
				site = lastSite
				return
			}
		}
	}
	site = lastSite
	return
}

func jiraSiteURL() string {
	_, site := parseJiraConfig()
	return site
}

func jiraCloudID() string {
	cloudID, _ := parseJiraConfig()
	return cloudID
}

func printHelp() {
	fmt.Println(`
Commands:
  k, keep   - Keep this issue for migration
  s, skip   - Skip this issue (won't migrate)
  d, defer  - Defer decision for later
  v, view   - Show full issue details
  o, open   - Open issue in browser
  q, quit   - Save and quit
  ?, help   - Show this help
`)
}

func nvl(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func formatTime(s string) string {
	t, err := time.Parse("2006-01-02T15:04:05.000-0700", s)
	if err != nil {
		return s
	}
	return t.Format("2006-01-02")
}

func truncateLines(s string, maxLines int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n") + "\n  ..."
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}
