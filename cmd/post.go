package cmd

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fraud-zero/screwjira/internal/config"
	"github.com/fraud-zero/screwjira/internal/storage"
	"github.com/spf13/cobra"
)

var (
	postApply       bool
	postIssueKey    string
	postJiraProject string
	postLimit       int
	postAll         bool
	postForce       bool
	postStatus      bool
	postParallel    int
)

var postCmd = &cobra.Command{
	Use:   "post",
	Short: "Upload kept issues to GitHub",
	Long: `Create GitHub Issues from kept issues. Runs as a dry run by default.
Use --apply to actually create issues. Issues with accepted enrichments use the
enriched title/description; others use raw Jira content.

Image attachments only render inline in private repos when uploaded via
GitHub's user-attachments CDN, which needs a browser session cookie. Set
GH_SESSION_TOKEN to your github.com 'user_session' cookie value to enable it;
otherwise images fall back to repo blob links (click-through, not inline).`,
	RunE: runPost,
}

func init() {
	rootCmd.AddCommand(postCmd)
	postCmd.Flags().BoolVar(&postApply, "apply", false, "create GitHub issues (default: dry run)")
	postCmd.Flags().StringVar(&postIssueKey, "issue", "", "post a single issue by key")
	postCmd.Flags().StringVar(&postJiraProject, "jira-project", "", "filter by Jira project key (e.g. F0)")
	postCmd.Flags().IntVar(&postLimit, "limit", 0, "max issues to post in this run (0 = all)")
	postCmd.Flags().BoolVar(&postAll, "all", false, "include issues with pending/rejected/deferred enrichments")
	postCmd.Flags().BoolVar(&postForce, "force", false, "re-post already-posted issues (closes and replaces the existing GitHub issue)")
	postCmd.Flags().BoolVar(&postStatus, "status", false, "show post progress and exit")
	postCmd.Flags().IntVar(&postParallel, "parallel", 1, "concurrent workers when applying")
}

// === Entry point ===

func runPost(cmd *cobra.Command, args []string) error {
	store, err := storage.New(getDataDir())
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}

	if postStatus {
		return showPostStatus(store)
	}

	cfg, err := config.Load(getDataDir())
	if err != nil {
		return err
	}

	if cfg.Post.Repo == "" {
		return fmt.Errorf("post.repo not configured — edit %s/config.toml", getDataDir())
	}

	if postApply {
		if err := exec.Command("gh", "auth", "status").Run(); err != nil {
			return fmt.Errorf("gh auth check failed — run 'gh auth login' first")
		}
	}

	toPost, err := buildPostableList(store)
	if err != nil {
		return err
	}

	if len(toPost) == 0 {
		fmt.Println("No issues to post.")
		return showPostStatus(store)
	}

	if !postApply {
		return showDryRun(toPost, cfg)
	}
	return applyPost(store, cfg, toPost)
}

// === Issue selection ===

// buildPostableList returns the filtered, sorted list of issues to post.
// It has no side effects — state mutations happen in applyPost.
func buildPostableList(store *storage.Store) ([]*storage.StoredIssue, error) {
	issues, err := store.GetKeptIssues()
	if err != nil {
		return nil, err
	}

	var result []*storage.StoredIssue
	for _, issue := range issues {
		// Already posted: skip unless --force
		if issue.Posted != nil && !postForce {
			continue
		}
		if postIssueKey != "" && issue.Issue.Key != postIssueKey {
			continue
		}
		if postJiraProject != "" && !strings.EqualFold(issue.Issue.Project(), postJiraProject) {
			continue
		}
		if !postAll && issue.Enrichment != nil {
			switch issue.Enrichment.Decision {
			case storage.EnrichPending, storage.EnrichRejected, storage.EnrichDeferred:
				continue
			}
		}
		result = append(result, issue)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Issue.Key < result[j].Issue.Key
	})

	if postLimit > 0 && len(result) > postLimit {
		result = result[:postLimit]
	}

	return result, nil
}

// === Content building ===

type issueContent struct {
	Title    string
	Body     string
	Type     string
	Labels   []string
	Assignee string
}

func buildIssueContent(issue *storage.StoredIssue, cfg *config.Config) issueContent {
	accepted := issue.Enrichment != nil && issue.Enrichment.Decision == storage.EnrichAccepted

	title := issue.Issue.Summary()
	if title == "" {
		title = issue.Issue.Key
	}
	if accepted && issue.Enrichment.ProposedTitle != "" {
		title = issue.Enrichment.ProposedTitle
	}

	body := issue.Issue.Description()
	if accepted && issue.Enrichment.ProposedDescription != "" {
		body = issue.Enrichment.ProposedDescription
	}

	body += buildOriginalSection(issue, accepted)
	body += buildFooter(issue, cfg.JiraURL)

	issueType := githubIssueType(issue)

	// Build labels without aliasing the stored slice.
	var srcLabels []string
	if accepted && len(issue.Enrichment.ProposedLabels) > 0 {
		srcLabels = issue.Enrichment.ProposedLabels
	} else if issue.Classification != nil && len(issue.Classification.Labels) > 0 {
		srcLabels = issue.Classification.Labels
	}
	labels := make([]string, len(srcLabels), len(srcLabels)+3)
	copy(labels, srcLabels)
	if cfg.Post.MigrationLabel != "" {
		labels = append(labels, cfg.Post.MigrationLabel)
	}
	if strings.EqualFold(issue.Issue.Status(), "blocked") {
		labels = append(labels, "blocked")
	}
	if issue.Gold != nil && issue.Gold.IsGold {
		labels = append(labels, goldLabel)
	}
	labels = uniqueStrings(labels)

	var assignee string
	if jiraName := issue.Issue.Assignee(); jiraName != "" {
		if mapped := cfg.UserMap[strings.ToLower(jiraName)]; mapped != "n/a" {
			assignee = mapped
		}
	}

	return issueContent{Title: title, Body: body, Type: issueType, Labels: labels, Assignee: assignee}
}

func buildFooter(issue *storage.StoredIssue, jiraURL string) string {
	if jiraURL == "" {
		if site := jiraSiteURL(); site != "" {
			jiraURL = "https://" + site
		} else {
			jiraURL = "https://your-site.atlassian.net"
		}
	}

	created := issue.Issue.Created()
	if len(created) >= 10 {
		created = created[:10]
	}

	footer := fmt.Sprintf("\n\n---\n*Migrated from Jira: [%s](%s/browse/%s)",
		issue.Issue.Key, jiraURL, issue.Issue.Key)
	if created != "" {
		footer += fmt.Sprintf(" · Created: %s", created)
	}
	if reporter := issue.Issue.Reporter(); reporter != "" {
		footer += fmt.Sprintf(" · Reporter: %s", reporter)
	}
	footer += "*"

	if issue.Classification != nil && issue.Classification.EpicParent != "" {
		footer += fmt.Sprintf("\n*Parent: %s*", issue.Classification.EpicParent)
	}

	return footer
}

func buildOriginalSection(issue *storage.StoredIssue, accepted bool) string {
	var parts []string

	if accepted {
		origTitle := issue.Issue.Summary()
		if issue.Enrichment.ProposedTitle != "" && issue.Enrichment.ProposedTitle != origTitle {
			parts = append(parts, fmt.Sprintf("**Original title:** %s", origTitle))
		}

		origDesc := strings.TrimSpace(issue.Issue.Description())
		proposedDesc := strings.TrimSpace(issue.Enrichment.ProposedDescription)
		if proposedDesc != "" && origDesc != "" && origDesc != proposedDesc {
			parts = append(parts, fmt.Sprintf("**Original description:**\n\n%s", origDesc))
		}
	}

	if issue.Enrichment != nil && issue.Enrichment.Notes != "" {
		parts = append(parts, fmt.Sprintf("**Agent notes:** %s", issue.Enrichment.Notes))
	}

	if len(parts) == 0 {
		return ""
	}

	return "\n\n<details>\n<summary>Migration notes</summary>\n\n" +
		strings.Join(parts, "\n\n") +
		"\n\n</details>"
}

func githubIssueType(issue *storage.StoredIssue) string {
	t := ""
	if issue.Classification != nil {
		t = strings.ToLower(issue.Classification.Type)
	}
	switch t {
	case "bug":
		return "Bug"
	case "feature", "idea", "epic":
		return "Feature"
	default:
		return "Task"
	}
}

func uniqueStrings(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// === Dry run ===

func showDryRun(issues []*storage.StoredIssue, cfg *config.Config) error {
	for i, issue := range issues {
		content := buildIssueContent(issue, cfg)

		totalAttachments := len(issue.Issue.Attachments())

		enrichTag := ""
		if issue.Enrichment != nil && issue.Enrichment.Decision == storage.EnrichAccepted {
			enrichTag = "  [enriched]"
		}
		repostTag := ""
		if issue.Posted != nil {
			repostTag = fmt.Sprintf("  [re-post, was #%d]", issue.Posted.IssueNumber)
		}

		fmt.Printf("[%d/%d] %s  %s  %s\n", i+1, len(issues), issue.Issue.Key, strings.ToLower(content.Type), issue.Issue.Summary())
		fmt.Printf("       Title:    %s%s%s\n", content.Title, enrichTag, repostTag)
		fmt.Printf("       Type:     %s\n", content.Type)
		if len(content.Labels) > 0 {
			fmt.Printf("       Labels:   %s\n", strings.Join(content.Labels, ", "))
		}
		if content.Assignee != "" {
			fmt.Printf("       Assignee: %s\n", content.Assignee)
		} else {
			fmt.Printf("       Assignee: (none)\n")
		}
		if totalAttachments > 0 {
			fmt.Printf("       Attachments: %d file(s) to upload\n", totalAttachments)
		}
		fmt.Println()
	}

	fmt.Printf("%d issues would be created in %s.\n", len(issues), cfg.Post.Repo)
	if postForce {
		posted := 0
		for _, issue := range issues {
			if issue.Posted != nil {
				posted++
			}
		}
		if posted > 0 {
			fmt.Printf("  (%d existing GitHub issues would be closed and replaced)\n", posted)
		}
	}
	fmt.Println("Run with --apply to create them.")
	return nil
}

// === Apply ===

func applyPost(store *storage.Store, cfg *config.Config, issues []*storage.StoredIssue) error {
	if cfg.Post.MigrationLabel != "" {
		bootstrapLabel(cfg.Post.Repo, cfg.Post.MigrationLabel)
	}
	bootstrapLabel(cfg.Post.Repo, goldLabel)

	var proj *projectInfo
	if cfg.Post.ProjectNumber > 0 {
		var err error
		proj, err = loadProjectInfo(cfg.Post.ProjectNumber, cfg.Post.ProjectOwner)
		if err != nil {
			fmt.Printf("Warning: failed to load project info: %v — status will not be set\n", err)
		}
	}

	canDownloadAttachments := atlassianAuthHeader() != ""
	hasAttachments := false
	for _, issue := range issues {
		if len(issue.Issue.Attachments()) > 0 {
			hasAttachments = true
			break
		}
	}
	if hasAttachments {
		if !canDownloadAttachments {
			fmt.Printf("Note: ATLASSIAN_EMAIL/ATLASSIAN_TOKEN not set — attachments will not be uploaded.\n\n")
		} else if sessionToken() == "" {
			fmt.Printf("Note: %s not set — images will be uploaded to the repo and linked, not inlined. See `post --help`.\n\n", sessionTokenEnv)
		} else {
			fmt.Printf("Note: %s set — image attachments will be uploaded to user-attachments for inline rendering.\n\n", sessionTokenEnv)
		}
	}

	if err := validateAssignees(issues, cfg); err != nil {
		return err
	}

	parallel := postParallel
	if parallel < 1 {
		parallel = 1
	}
	if parallel > 1 {
		fmt.Printf("Running %d workers in parallel.\n\n", parallel)
	}

	var created, failed, totalUploadFailed atomic.Int64
	var saveErr atomic.Value // stores first save-failure error

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

			content := buildIssueContent(issue, cfg)
			body, uploadFailed := processAttachments(&buf, issue, content.Body, cfg.Post.Repo, canDownloadAttachments)
			content.Body = body
			totalUploadFailed.Add(int64(uploadFailed))

			ghIssue, err := createGitHubIssue(cfg.Post.Repo, content)
			if err != nil {
				fmt.Fprintf(&buf, "  FAILED: %v\n\n", err)
				failed.Add(1)
				flushLog(&buf)
				return
			}

			if issue.Posted != nil {
				comment := fmt.Sprintf("Superseded by #%d (re-migration of %s).", ghIssue.Number, issue.Issue.Key)
				if err := closeGitHubIssue(cfg.Post.Repo, issue.Posted.IssueNumber, comment); err != nil {
					fmt.Fprintf(&buf, "  Warning: failed to close old issue #%d: %v\n", issue.Posted.IssueNumber, err)
				} else {
					fmt.Fprintf(&buf, "  Closed old issue #%d\n", issue.Posted.IssueNumber)
				}
			}

			issue.Posted = &storage.PostedState{
				IssueNumber: ghIssue.Number,
				IssueURL:    ghIssue.HTMLURL,
				PostedAt:    time.Now().UTC().Format(time.RFC3339),
			}
			if err := store.SaveStoredIssue(issue); err != nil {
				fmt.Fprintf(&buf, "  CRITICAL: GitHub issue created at %s but failed to save state: %v\n", ghIssue.HTMLURL, err)
				fmt.Fprintf(&buf, "  Cancelling further workers to prevent duplicates on re-run. Fix the issue and re-run with --force.\n\n")
				saveErr.CompareAndSwap(nil, fmt.Errorf("failed to save posted state for %s: %w", issue.Issue.Key, err))
				cancel()
				flushLog(&buf)
				return
			}

			fmt.Fprintf(&buf, "  → %s\n\n", ghIssue.HTMLURL)
			created.Add(1)

			if proj != nil {
				itemID, err := addToProject(cfg.Post.ProjectNumber, cfg.Post.ProjectOwner, ghIssue.HTMLURL)
				if err != nil {
					fmt.Fprintf(&buf, "  Warning: failed to add to project: %v\n", err)
				} else if mapped := setProjectStatus(&buf, proj, itemID, issue.Issue.Status(), cfg.Post.StatusMap); !mapped {
					fmt.Fprintf(&buf, "  Note: Jira status %q not in status map — defaulted to Todo\n", issue.Issue.Status())
				}
			}

			flushLog(&buf)
		}(i, issue)
	}

	wg.Wait()

	fmt.Printf("Done. %d created, %d failed.\n", created.Load(), failed.Load())
	if n := totalUploadFailed.Load(); n > 0 {
		fmt.Printf("Note: %d attachment(s) failed to upload across all issues.\n", n)
	}
	if err, ok := saveErr.Load().(error); ok && err != nil {
		return err
	}
	return showPostStatus(store)
}

// flushLog writes a worker's buffered output to stdout atomically. logMu is
// shared with enrich.go so concurrent flushes from either command interleave
// cleanly (not that they run at the same time, but they share the package).
func flushLog(buf *bytes.Buffer) {
	logMu.Lock()
	defer logMu.Unlock()
	buf.WriteTo(os.Stdout)
}

// === Validation ===

func validateAssignees(issues []*storage.StoredIssue, cfg *config.Config) error {
	seen := map[string]bool{}
	for _, issue := range issues {
		if jiraName := issue.Issue.Assignee(); jiraName != "" {
			if gh := cfg.UserMap[strings.ToLower(jiraName)]; gh != "" && gh != "n/a" {
				seen[gh] = false
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}

	fmt.Printf("Validating %d mapped assignee(s)...\n", len(seen))
	var bad []string
	for login := range seen {
		var stderr bytes.Buffer
		cmd := exec.Command("gh", "api", "/users/"+login)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			errText := stderr.String()
			if strings.Contains(errText, "404") || strings.Contains(errText, "Not Found") {
				bad = append(bad, login)
			} else {
				return fmt.Errorf("gh api check failed for %q (check auth/network): %s", login, strings.TrimSpace(errText))
			}
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("mapped GitHub user(s) not found: %s\nFix [user_map] in config.toml or remove the mapping", strings.Join(bad, ", "))
	}
	return nil
}

// === GitHub API ===

func bootstrapLabel(repo, name string) {
	var stderr bytes.Buffer
	cmd := exec.Command("gh", "label", "create", "--force", "--repo", repo, name)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("Warning: failed to bootstrap label %q on %s: %s\n", name, repo, strings.TrimSpace(stderr.String()))
	}
}

type ghIssueResponse struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

func createGitHubIssue(repo string, content issueContent) (*ghIssueResponse, error) {
	payload := map[string]interface{}{
		"title": content.Title,
		"body":  content.Body,
		"type":  content.Type,
	}
	if len(content.Labels) > 0 {
		payload["labels"] = content.Labels
	}
	if content.Assignee != "" {
		payload["assignees"] = []string{content.Assignee}
	}

	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command("gh", "api", "--method", "POST", "/repos/"+repo+"/issues", "--input", "-")
	cmd.Stdin = bytes.NewReader(jsonBytes)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}

	var resp ghIssueResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	if resp.Number == 0 {
		return nil, fmt.Errorf("unexpected response (no issue number): %s", truncate(string(out), 200))
	}
	return &resp, nil
}

func closeGitHubIssue(repo string, issueNumber int, comment string) error {
	commentPayload, _ := json.Marshal(map[string]string{"body": comment})
	commentCmd := exec.Command("gh", "api", "--method", "POST",
		fmt.Sprintf("/repos/%s/issues/%d/comments", repo, issueNumber), "--input", "-")
	commentCmd.Stdin = bytes.NewReader(commentPayload)
	commentCmd.Run() // best-effort

	closePayload, _ := json.Marshal(map[string]string{"state": "closed", "state_reason": "not_planned"})
	closeCmd := exec.Command("gh", "api", "--method", "PATCH",
		fmt.Sprintf("/repos/%s/issues/%d", repo, issueNumber), "--input", "-")
	closeCmd.Stdin = bytes.NewReader(closePayload)
	if _, err := closeCmd.Output(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("%s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return err
	}
	return nil
}

// === GitHub Project v2 ===

type projectInfo struct {
	ID            string
	StatusFieldID string
	StatusOptions map[string]string // lowercase option name -> option ID
}

// loadProjectInfo queries project metadata, trying org owner then user owner.
func loadProjectInfo(projectNumber int, owner string) (*projectInfo, error) {
	info, err := queryProjectInfo(projectNumber, owner, "organization")
	if err != nil {
		return nil, err
	}
	if info.ID == "" {
		info, err = queryProjectInfo(projectNumber, owner, "user")
		if err != nil {
			return nil, err
		}
	}
	if info.ID == "" {
		return nil, fmt.Errorf("project %d not found for owner %q (tried org and user)", projectNumber, owner)
	}
	return info, nil
}

func queryProjectInfo(projectNumber int, owner, ownerType string) (*projectInfo, error) {
	query := fmt.Sprintf(`query {
  %s(login: %q) {
    projectV2(number: %d) {
      id
      fields(first: 20) {
        nodes {
          ... on ProjectV2SingleSelectField {
            id
            name
            options { id name }
          }
        }
      }
    }
  }
}`, ownerType, owner, projectNumber)

	payload, _ := json.Marshal(map[string]string{"query": query})
	cmd := exec.Command("gh", "api", "graphql", "--input", "-")
	cmd.Stdin = bytes.NewReader(payload)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("graphql query failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}

	var result struct {
		Data map[string]struct {
			ProjectV2 struct {
				ID     string `json:"id"`
				Fields struct {
					Nodes []struct {
						ID      string `json:"id"`
						Name    string `json:"name"`
						Options []struct {
							ID   string `json:"id"`
							Name string `json:"name"`
						} `json:"options"`
					} `json:"nodes"`
				} `json:"fields"`
			} `json:"projectV2"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("failed to parse project info: %w", err)
	}

	info := &projectInfo{StatusOptions: make(map[string]string)}
	for _, ownerData := range result.Data {
		info.ID = ownerData.ProjectV2.ID
		for _, field := range ownerData.ProjectV2.Fields.Nodes {
			if strings.EqualFold(field.Name, "Status") {
				info.StatusFieldID = field.ID
				for _, opt := range field.Options {
					info.StatusOptions[strings.ToLower(opt.Name)] = opt.ID
				}
				break
			}
		}
		break
	}
	return info, nil
}

func addToProject(projectNumber int, owner, issueURL string) (string, error) {
	cmd := exec.Command("gh", "project", "item-add", strconv.Itoa(projectNumber),
		"--owner", owner, "--url", issueURL, "--format", "json")
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("project item-add failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", err
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return "", fmt.Errorf("failed to parse item-add response: %w", err)
	}
	return result.ID, nil
}

var defaultStatusMap = map[string]string{
	"to do":       "Todo",
	"backlog":     "Todo",
	"open":        "Todo",
	"in progress": "In Progress",
	"discovery":   "In Progress",
	"prioritized": "In Progress",
	"in review":   "In Progress",
	"blocked":     "In Progress",
}

// mapJiraStatus returns the GitHub Project status name and whether it was
// explicitly mapped (false means the default fallback was used).
func mapJiraStatus(jiraStatus string, overrides map[string]string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(jiraStatus))
	if v, ok := overrides[key]; ok {
		return v, true
	}
	if v, ok := defaultStatusMap[key]; ok {
		return v, true
	}
	return "Todo", false
}

// setProjectStatus sets the Status field on a project item via GraphQL.
// Returns true if the status was explicitly mapped, false if it fell back to the default.
func setProjectStatus(log io.Writer, info *projectInfo, itemID, jiraStatus string, overrides map[string]string) bool {
	if info.StatusFieldID == "" || itemID == "" {
		return true // can't set, not an error
	}
	targetName, mapped := mapJiraStatus(jiraStatus, overrides)
	optionID, ok := info.StatusOptions[strings.ToLower(targetName)]
	if !ok {
		return mapped
	}

	mutation := fmt.Sprintf(`mutation {
  updateProjectV2ItemFieldValue(input: {
    projectId: %q
    itemId: %q
    fieldId: %q
    value: { singleSelectOptionId: %q }
  }) { projectV2Item { id } }
}`, info.ID, itemID, info.StatusFieldID, optionID)

	payload, _ := json.Marshal(map[string]string{"query": mutation})
	cmd := exec.Command("gh", "api", "graphql", "--input", "-")
	cmd.Stdin = bytes.NewReader(payload)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(log, "  Warning: failed to set project status for item %s: %s\n", itemID, strings.TrimSpace(stderr.String()))
	}
	return mapped
}

// === Attachments ===

func isImageMime(mimeType string) bool {
	switch mimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// uploadedAttachment tracks where an attachment was hosted and whether the
// resulting URL renders inline in GitHub Markdown. user-attachments URLs from
// the session flow are camo-proxied and render inline; repo blob URLs render
// only as click-through links.
type uploadedAttachment struct {
	url    string
	inline bool
}

// processAttachments uploads attachments and replaces [[JIRA_MEDIA:id]] placeholders.
// When canDownload is false, placeholders are replaced with a fallback note instead.
//
// For images, if GH_SESSION_TOKEN is set, the browser-flow upload is tried first
// so the image renders inline. Everything else (and session-flow failures) goes
// to the Contents API and is linked via the repo blob URL — those don't render
// inline in private repos but do work as authenticated click-through links.
//
// `log` receives per-attachment progress/warning lines; pass a buffer in
// parallel mode to keep each issue's output coherent, or os.Stdout in
// sequential mode.
func processAttachments(log io.Writer, issue *storage.StoredIssue, body, repo string, canDownload bool) (string, int) {
	attachments := issue.Issue.Attachments()
	if len(attachments) == 0 {
		return body, 0
	}

	useSession := sessionToken() != ""
	uploads := make(map[string]uploadedAttachment)
	var nonUploadedNames []string

	for _, att := range attachments {
		if !canDownload {
			nonUploadedNames = append(nonUploadedNames, att.Filename)
			continue
		}

		data, err := downloadAttachment(att.Content)
		if err != nil {
			fmt.Fprintf(log, "  Warning: failed to download %s: %v\n", att.Filename, err)
			nonUploadedNames = append(nonUploadedNames, att.Filename)
			continue
		}

		if useSession && isImageMime(att.MimeType) {
			assetURL, err := uploadAttachmentViaSession(repo, att.Filename, att.MimeType, data)
			if err == nil {
				uploads[att.ID] = uploadedAttachment{url: assetURL, inline: true}
				fmt.Fprintf(log, "  Uploaded %s → %s (inline)\n", att.Filename, assetURL)
				continue
			}
			fmt.Fprintf(log, "  Warning: session upload of %s failed (%v) — falling back to repo blob\n", att.Filename, err)
		}

		blobURL, err := uploadAttachmentToRepo(repo, issue.Issue.Key, att.Filename, data)
		if err != nil {
			fmt.Fprintf(log, "  Warning: failed to upload %s: %v\n", att.Filename, err)
			nonUploadedNames = append(nonUploadedNames, att.Filename)
			continue
		}
		uploads[att.ID] = uploadedAttachment{url: blobURL, inline: false}
		fmt.Fprintf(log, "  Uploaded %s → %s\n", att.Filename, blobURL)
	}

	// Replace [[JIRA_MEDIA:id]] placeholders. The ADF media node's `attrs.id`
	// is a media-platform UUID, while attachment IDs are numeric — they never
	// match directly. Pair by index instead: the Nth placeholder in body
	// order maps to the Nth attachment in attachment-list order. If that
	// attachment failed to download, the placeholder slot gets a "not
	// migrated" marker rather than silently advancing to the next successful
	// upload (which would mis-pair the remaining placeholders).
	referenced := make(map[string]bool)
	placeholderRe := regexp.MustCompile(`\[\[JIRA_MEDIA:[^\]]+\]\]`)

	var attIdx int
	for {
		loc := placeholderRe.FindStringIndex(body)
		if loc == nil || attIdx >= len(attachments) {
			break
		}
		att := attachments[attIdx]
		attIdx++
		referenced[att.ID] = true

		var replacement string
		if up, ok := uploads[att.ID]; ok {
			if up.inline && isImageMime(att.MimeType) {
				replacement = fmt.Sprintf("![%s](%s)", att.Filename, up.url)
			} else {
				replacement = fmt.Sprintf("[%s](%s)", att.Filename, up.url)
			}
		} else {
			replacement = fmt.Sprintf("*(attachment: %s — not migrated)*", att.Filename)
		}
		body = body[:loc[0]] + replacement + body[loc[1]:]
	}

	// Strip any leftover placeholders (more media nodes than attachments).
	body = placeholderRe.ReplaceAllString(body, "")

	// Append unreferenced attachments: inline images get their own section
	// so they actually render; everything else goes in the link footer.
	var inlineExtras, linkExtras []string
	for _, att := range attachments {
		if referenced[att.ID] {
			continue
		}
		up, ok := uploads[att.ID]
		if !ok {
			continue
		}
		if up.inline && isImageMime(att.MimeType) {
			inlineExtras = append(inlineExtras, fmt.Sprintf("![%s](%s)", att.Filename, up.url))
		} else {
			linkExtras = append(linkExtras, fmt.Sprintf("[%s](%s)", att.Filename, up.url))
		}
	}
	if len(inlineExtras) > 0 {
		body += "\n\n**Screenshots from Jira:**\n\n" + strings.Join(inlineExtras, "\n\n")
	}
	if len(linkExtras) > 0 {
		body += fmt.Sprintf("\n\n📎 Attachments: %s", strings.Join(linkExtras, ", "))
	}
	if len(nonUploadedNames) > 0 {
		body += fmt.Sprintf("\n\n⚠️ Attachments not migrated: %s", strings.Join(nonUploadedNames, ", "))
	}

	return body, len(nonUploadedNames)
}

// atlassianAuthHeader returns an Authorization header value for Atlassian API
// requests. Returns ("", false) when no credentials are available.
//
// API-token Basic auth is preferred over acli's OAuth bearer token: acli
// stores a short-lived access_token (typically 1h) that we can't refresh
// from here, so attachment downloads regularly 401 mid-run on long sessions.
// The email+token combo is stable.
func atlassianAuthHeader() string {
	header, _ := atlassianAuth()
	return header
}

// atlassianAuth returns the header value and a flag indicating whether it's
// Basic (true) or Bearer (false). The flag lets the caller skip the
// api.atlassian.com URL rewrite when using Basic auth, which works directly
// against the original site host.
func atlassianAuth() (header string, isBasic bool) {
	email := os.Getenv("ATLASSIAN_EMAIL")
	apiToken := os.Getenv("ATLASSIAN_TOKEN")
	if email != "" && apiToken != "" {
		encoded := base64.StdEncoding.EncodeToString([]byte(email + ":" + apiToken))
		return "Basic " + encoded, true
	}
	if token := acliOAuthToken(); token != "" {
		return "Bearer " + token, false
	}
	return "", false
}

// acliOAuthToken reads the current profile's OAuth access token from the macOS
// Keychain, where acli stores it under service "acli" as a gzip-compressed
// JSON blob prefixed with "go-keyring-base64:".
func acliOAuthToken() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "acli", "global_auth_config.yaml"))
	if err != nil {
		return ""
	}
	var currentProfile string
	for _, line := range strings.Split(string(data), "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "current_profile:") {
			currentProfile = strings.TrimSpace(strings.TrimPrefix(trimmed, "current_profile:"))
			break
		}
	}
	if currentProfile == "" {
		return ""
	}
	accountKey := "jira:" + currentProfile
	cmd := exec.Command("security", "find-generic-password", "-s", "acli", "-a", accountKey, "-w")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	raw := strings.TrimSpace(string(out))

	// acli uses go-keyring which stores values as base64-encoded gzipped JSON.
	encoded := strings.TrimPrefix(raw, "go-keyring-base64:")
	if encoded == raw {
		return raw // plain token, use as-is
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return ""
	}
	reader, err := gzip.NewReader(bytes.NewReader(decoded))
	if err != nil {
		return ""
	}
	defer reader.Close()
	jsonData, err := io.ReadAll(reader)
	if err != nil {
		return ""
	}
	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(jsonData, &tokenResp); err != nil {
		return ""
	}
	return tokenResp.AccessToken
}

func downloadAttachment(rawURL string) ([]byte, error) {
	auth, isBasic := atlassianAuth()
	if auth == "" {
		return nil, fmt.Errorf("no Atlassian credentials available (set ATLASSIAN_EMAIL/ATLASSIAN_TOKEN or ensure acli is authenticated)")
	}

	// OAuth bearer tokens can't hit the original Jira site host (it's an
	// internal Atlassian hostname); they need the api.atlassian.com tunnel.
	// Basic auth works directly against the original host.
	if !isBasic {
		if cloudID := jiraCloudID(); cloudID != "" {
			if parsed, err := url.Parse(rawURL); err == nil {
				parsed.Host = "api.atlassian.com"
				parsed.Path = "/ex/jira/" + cloudID + parsed.Path
				rawURL = parsed.String()
			}
		}
	}

	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", auth)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		bodyText := strings.TrimSpace(string(body))
		hint := ""
		if resp.StatusCode == http.StatusUnauthorized && !isBasic {
			hint = " — acli OAuth token may have expired; try `acli jira auth login` or set ATLASSIAN_EMAIL/ATLASSIAN_TOKEN"
		}
		return nil, fmt.Errorf("HTTP %d %s%s", resp.StatusCode, truncate(bodyText, 200), hint)
	}
	return io.ReadAll(resp.Body)
}

// uploadAttachmentToRepo uploads via the Contents API and returns the blob
// URL (https://github.com/{owner}/{repo}/blob/{branch}/{path}). That URL goes
// through GitHub's normal auth so it works for collaborators on a private
// repo — but it only renders as a click-through link, not inline. For inline
// rendering on private repos, see uploadAttachmentViaSession.
func uploadAttachmentToRepo(repo, issueKey, filename string, data []byte) (string, error) {
	escapedFilename := url.PathEscape(filename)
	apiPath := fmt.Sprintf("/repos/%s/contents/.jira-attachments/%s/%s", repo, issueKey, escapedFilename)

	// Check if file already exists to get its SHA (required for updates).
	var sha string
	var checkStderr bytes.Buffer
	checkCmd := exec.Command("gh", "api", apiPath)
	checkCmd.Stderr = &checkStderr
	checkOut, checkErr := checkCmd.Output()
	if checkErr == nil {
		var existing struct {
			SHA string `json:"sha"`
		}
		if json.Unmarshal(checkOut, &existing) == nil {
			sha = existing.SHA
		}
	} else if !strings.Contains(checkStderr.String(), "404") && !strings.Contains(checkStderr.String(), "Not Found") {
		// Non-404 error (auth failure, 5xx, etc.) — fail rather than proceeding without SHA.
		return "", fmt.Errorf("failed to check existing file: %s", strings.TrimSpace(checkStderr.String()))
	}

	payload := map[string]string{
		"message": fmt.Sprintf("jira-migration: add attachments for %s", issueKey),
		"content": base64.StdEncoding.EncodeToString(data),
	}
	if sha != "" {
		payload["sha"] = sha
	}

	jsonBytes, _ := json.Marshal(payload)
	uploadCmd := exec.Command("gh", "api", "--method", "PUT", apiPath, "--input", "-")
	uploadCmd.Stdin = bytes.NewReader(jsonBytes)
	out, err := uploadCmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("upload failed: %s", truncate(string(out), 200))
	}

	var putResp struct {
		Content struct {
			HTMLURL string `json:"html_url"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &putResp); err == nil && putResp.Content.HTMLURL != "" {
		return putResp.Content.HTMLURL, nil
	}

	// Fallback: construct URL (may be wrong if default branch isn't main).
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid repo format %q — expected owner/name", repo)
	}
	return fmt.Sprintf("https://github.com/%s/%s/blob/main/.jira-attachments/%s/%s",
		parts[0], parts[1], issueKey, escapedFilename), nil
}

// === Status ===

func showPostStatus(store *storage.Store) error {
	issues, err := store.GetKeptIssues()
	if err != nil {
		return err
	}

	posted := 0
	for _, issue := range issues {
		if issue.Posted != nil {
			posted++
		}
	}

	fmt.Printf("\nPost Status:\n")
	fmt.Printf("  Posted:   %d/%d\n", posted, len(issues))
	fmt.Printf("  Unposted: %d\n", len(issues)-posted)
	return nil
}
