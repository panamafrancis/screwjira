package jira

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Issue represents a Jira issue with all fields
type Issue struct {
	ID     string                 `json:"id"`
	Key    string                 `json:"key"`
	Self   string                 `json:"self"`
	Fields map[string]interface{} `json:"fields"`
}

// SearchResult is the result from acli search (array of issues)
type SearchResult []Issue

// Search executes a JQL query and returns matching issues
func Search(jql string, limit int) ([]Issue, error) {
	args := []string{"jira", "workitem", "search", "--jql", jql, "--json"}
	if limit > 0 {
		args = append(args, "--limit", fmt.Sprintf("%d", limit))
	} else {
		args = append(args, "--paginate")
	}

	cmd := exec.Command("acli", args...)
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("acli search failed: %s", string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("acli search failed: %w", err)
	}

	var issues []Issue
	if err := json.Unmarshal(output, &issues); err != nil {
		return nil, fmt.Errorf("failed to parse search results: %w", err)
	}

	return issues, nil
}

// GetIssue fetches a single issue with all fields
func GetIssue(key string) (*Issue, error) {
	cmd := exec.Command("acli", "jira", "workitem", "view", key, "--fields", "*all", "--json")
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("acli view failed: %s", string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("acli view failed: %w", err)
	}

	var issue Issue
	if err := json.Unmarshal(output, &issue); err != nil {
		return nil, fmt.Errorf("failed to parse issue: %w", err)
	}

	return &issue, nil
}

// Count returns the number of issues matching a JQL query
func Count(jql string) (int, error) {
	cmd := exec.Command("acli", "jira", "workitem", "search", "--jql", jql, "--count")
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return 0, fmt.Errorf("acli count failed: %s", string(exitErr.Stderr))
		}
		return 0, fmt.Errorf("acli count failed: %w", err)
	}

	var count int
	// Output format: "✓ Number of work items in the search: 312"
	_, err = fmt.Sscanf(string(output), "✓ Number of work items in the search: %d", &count)
	if err != nil {
		return 0, fmt.Errorf("failed to parse count: %w", err)
	}

	return count, nil
}

// Helper functions to extract typed fields from issue

func (i *Issue) Summary() string {
	if s, ok := i.Fields["summary"].(string); ok {
		return s
	}
	return ""
}

func (i *Issue) Status() string {
	if status, ok := i.Fields["status"].(map[string]interface{}); ok {
		if name, ok := status["name"].(string); ok {
			return name
		}
	}
	return ""
}

func (i *Issue) IssueType() string {
	if it, ok := i.Fields["issuetype"].(map[string]interface{}); ok {
		if name, ok := it["name"].(string); ok {
			return name
		}
	}
	return ""
}

func (i *Issue) Priority() string {
	if p, ok := i.Fields["priority"].(map[string]interface{}); ok {
		if name, ok := p["name"].(string); ok {
			return name
		}
	}
	return ""
}

func (i *Issue) Assignee() string {
	if a, ok := i.Fields["assignee"].(map[string]interface{}); ok {
		if name, ok := a["displayName"].(string); ok {
			return name
		}
	}
	return ""
}

func (i *Issue) Reporter() string {
	if r, ok := i.Fields["reporter"].(map[string]interface{}); ok {
		if name, ok := r["displayName"].(string); ok {
			return name
		}
	}
	return ""
}

func (i *Issue) Created() string {
	if c, ok := i.Fields["created"].(string); ok {
		return c
	}
	return ""
}

func (i *Issue) Updated() string {
	if u, ok := i.Fields["updated"].(string); ok {
		return u
	}
	return ""
}

func (i *Issue) Labels() []string {
	if labels, ok := i.Fields["labels"].([]interface{}); ok {
		result := make([]string, 0, len(labels))
		for _, l := range labels {
			if s, ok := l.(string); ok {
				result = append(result, s)
			}
		}
		return result
	}
	return nil
}

func (i *Issue) Project() string {
	if p, ok := i.Fields["project"].(map[string]interface{}); ok {
		if key, ok := p["key"].(string); ok {
			return key
		}
	}
	return ""
}

func (i *Issue) Components() []string {
	if comps, ok := i.Fields["components"].([]interface{}); ok {
		result := make([]string, 0, len(comps))
		for _, c := range comps {
			if cm, ok := c.(map[string]interface{}); ok {
				if name, ok := cm["name"].(string); ok {
					result = append(result, name)
				}
			}
		}
		return result
	}
	return nil
}

func (i *Issue) ParentKey() string {
	if parent, ok := i.Fields["parent"].(map[string]interface{}); ok {
		if key, ok := parent["key"].(string); ok {
			return key
		}
	}
	// Try common epic link custom fields
	for _, field := range []string{"customfield_10014", "customfield_10008"} {
		if val, ok := i.Fields[field].(string); ok && val != "" {
			return val
		}
	}
	return ""
}

func (i *Issue) IssueLinks() []string {
	links, ok := i.Fields["issuelinks"].([]interface{})
	if !ok {
		return nil
	}
	var keys []string
	for _, link := range links {
		lm, ok := link.(map[string]interface{})
		if !ok {
			continue
		}
		for _, dir := range []string{"inwardIssue", "outwardIssue"} {
			if issue, ok := lm[dir].(map[string]interface{}); ok {
				if key, ok := issue["key"].(string); ok {
					keys = append(keys, key)
				}
			}
		}
	}
	return keys
}

func (i *Issue) Description() string {
	// Description is in ADF format, we'll convert it later
	if desc, ok := i.Fields["description"].(map[string]interface{}); ok {
		return extractTextFromADF(desc)
	}
	return ""
}

// CompareIssueFields returns the names of important fields that differ between old and fresh.
// Status, assignee, reporter, and timestamps are intentionally excluded.
func CompareIssueFields(old, fresh *Issue) []string {
	var changed []string
	if old.Summary() != fresh.Summary() {
		changed = append(changed, "summary")
	}
	if old.Description() != fresh.Description() {
		changed = append(changed, "description")
	}
	if !equalStringSlices(sortedStrings(old.Labels()), sortedStrings(fresh.Labels())) {
		changed = append(changed, "labels")
	}
	if !equalStringSlices(sortedStrings(old.Components()), sortedStrings(fresh.Components())) {
		changed = append(changed, "components")
	}
	if old.IssueType() != fresh.IssueType() {
		changed = append(changed, "issuetype")
	}
	if old.Priority() != fresh.Priority() {
		changed = append(changed, "priority")
	}
	return changed
}

func sortedStrings(s []string) []string {
	c := make([]string, len(s))
	copy(c, s)
	sort.Strings(c)
	return c
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Attachment represents a file attached to a Jira issue.
type Attachment struct {
	ID       string
	Filename string
	MimeType string
	Content  string // authenticated download URL
}

// Attachments returns file attachments on this issue.
func (i *Issue) Attachments() []Attachment {
	list, ok := i.Fields["attachment"].([]interface{})
	if !ok {
		return nil
	}
	var result []Attachment
	for _, item := range list {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		var a Attachment
		if v, ok := m["id"].(string); ok {
			a.ID = v
		}
		if v, ok := m["filename"].(string); ok {
			a.Filename = v
		}
		if v, ok := m["mimeType"].(string); ok {
			a.MimeType = v
		}
		if v, ok := m["content"].(string); ok {
			a.Content = v
		}
		if a.ID != "" && a.Filename != "" {
			result = append(result, a)
		}
	}
	return result
}

// extractTextFromADF extracts plain text from Atlassian Document Format.
// ADF is a tree: doc → blocks (paragraph, bulletList, …) → inlines (text, hardBreak, …).
// Lists add an extra level: bulletList → listItem → paragraph → text.
func extractTextFromADF(doc map[string]interface{}) string {
	var sb strings.Builder
	walkADF(doc, &sb)
	return strings.TrimSpace(sb.String())
}

func walkADF(node map[string]interface{}, sb *strings.Builder) {
	nodeType, _ := node["type"].(string)

	switch nodeType {
	case "text":
		if text, ok := node["text"].(string); ok {
			sb.WriteString(text)
		}
		return
	case "hardBreak":
		sb.WriteString("\n")
		return
	case "mention":
		if attrs, ok := node["attrs"].(map[string]interface{}); ok {
			if text, ok := attrs["text"].(string); ok {
				sb.WriteString(text)
			}
		}
		return
	case "media":
		// Emit a placeholder replaced with a GitHub-hosted URL during post.
		if attrs, ok := node["attrs"].(map[string]interface{}); ok {
			if id, ok := attrs["id"].(string); ok {
				sb.WriteString(fmt.Sprintf("[[JIRA_MEDIA:%s]]", id))
			}
		}
		return
	}

	content, ok := node["content"].([]interface{})
	if !ok {
		return
	}

	switch nodeType {
	case "listItem":
		sb.WriteString("- ")
		for _, child := range content {
			if m, ok := child.(map[string]interface{}); ok {
				walkADF(m, sb)
			}
		}
		return
	default:
		for _, child := range content {
			if m, ok := child.(map[string]interface{}); ok {
				walkADF(m, sb)
			}
		}
	}

	// Block-level elements get a trailing newline, but suppress the extra
	// newline from a paragraph that's already inside a list item.
	switch nodeType {
	case "paragraph", "heading", "codeBlock", "blockquote", "bulletList", "orderedList", "rule", "mediaSingle":
		sb.WriteString("\n")
	}
}
