package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fraud-zero/screwjira/internal/jira"
)

// Store manages local storage of Jira issues
type Store struct {
	baseDir string
}

// FilterDecision represents the triage decision for an issue
type FilterDecision string

const (
	DecisionPending FilterDecision = "pending"
	DecisionKeep    FilterDecision = "keep"
	DecisionSkip    FilterDecision = "skip"
	DecisionDefer   FilterDecision = "defer"
)

// Classification holds the proposed classification for an issue
type Classification struct {
	Type       string   `json:"type,omitempty"`        // "task", "bug", "idea"
	Labels     []string `json:"labels,omitempty"`       // Proposed GitHub labels
	EpicParent string   `json:"epic_parent,omitempty"`  // Original epic key if under an epic
}

// EnrichDecision represents the review decision for an enrichment
type EnrichDecision string

const (
	EnrichPending  EnrichDecision = "pending"
	EnrichAccepted EnrichDecision = "accepted"
	EnrichRejected EnrichDecision = "rejected"
	EnrichDeferred EnrichDecision = "deferred"
)

// Enrichment holds agent-proposed changes and review state
type Enrichment struct {
	// Proposed changes
	ProposedTitle       string   `json:"proposed_title"`
	ProposedDescription string   `json:"proposed_description"`
	ProposedLabels      []string `json:"proposed_labels"`

	// Agent context
	Notes        string   `json:"notes"`
	RelatedFiles []string `json:"related_files,omitempty"`
	Confidence   string   `json:"confidence"` // high, medium, low, none
	EnrichedBy   string   `json:"enriched_by,omitempty"` // "claude" or "codex"

	// Review state
	Decision        EnrichDecision `json:"decision"`
	RejectionReason string         `json:"rejection_reason,omitempty"`
}

// PostedState records the GitHub issue created for a stored issue.
type PostedState struct {
	IssueNumber int    `json:"issue_number"`
	IssueURL    string `json:"issue_url"`
	PostedAt    string `json:"posted_at"` // RFC3339
}

// GoldVerdict records whether a post-enrichment classifier judged that the
// agent found a real bug or fix during enrichment. Set by the `gold` command.
type GoldVerdict struct {
	IsGold     bool     `json:"is_gold"`
	Reason     string   `json:"reason"`
	Evidence   []string `json:"evidence,omitempty"`
	Model      string   `json:"model,omitempty"`
	ReviewedAt string   `json:"reviewed_at"` // RFC3339
}

// StoredIssue wraps a Jira issue with filter metadata
type StoredIssue struct {
	Issue          *jira.Issue     `json:"issue"`
	Decision       FilterDecision  `json:"decision"`
	Reason         string          `json:"reason,omitempty"`
	Classification *Classification `json:"classification,omitempty"`
	Enrichment     *Enrichment     `json:"enrichment,omitempty"`
	EnrichMode     string          `json:"enrich_mode,omitempty"` // "light" for DISC Ideas
	Gold           *GoldVerdict    `json:"gold,omitempty"`
	Posted         *PostedState    `json:"posted,omitempty"`
}

// writeFileAtomic writes data to path via a temp file + rename so concurrent
// readers never observe a partial write.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// New creates a new Store
func New(baseDir string) (*Store, error) {
	for _, dir := range []string{
		filepath.Join(baseDir, "raw"),
		filepath.Join(baseDir, "filtered"),
		filepath.Join(baseDir, "classified"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}
	return &Store{baseDir: baseDir}, nil
}

// SaveIssue saves an issue to the raw directory. If the issue already exists,
// its Jira fields are updated but Decision, Reason, Classification, and
// Enrichment are preserved.
func (s *Store) SaveIssue(issue *jira.Issue) error {
	project := issue.Project()
	if project == "" {
		return fmt.Errorf("issue %s has no project", issue.Key)
	}

	dir := filepath.Join(s.baseDir, "raw", project)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	stored, err := s.LoadIssue(issue.Key)
	if err != nil {
		stored = &StoredIssue{Decision: DecisionPending}
	}
	stored.Issue = issue

	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal issue: %w", err)
	}

	path := filepath.Join(dir, issue.Key+".json")
	if err := writeFileAtomic(path, data); err != nil {
		return fmt.Errorf("failed to write issue: %w", err)
	}

	return nil
}

// LoadIssue loads an issue from storage
func (s *Store) LoadIssue(key string) (*StoredIssue, error) {
	// Try to find the issue in any project directory
	project := strings.Split(key, "-")[0]
	path := filepath.Join(s.baseDir, "raw", project, key+".json")

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read issue: %w", err)
	}

	var stored StoredIssue
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("failed to parse issue: %w", err)
	}

	return &stored, nil
}

// UpdateDecision updates the filter decision for an issue
func (s *Store) UpdateDecision(key string, decision FilterDecision, reason string) error {
	stored, err := s.LoadIssue(key)
	if err != nil {
		return err
	}

	stored.Decision = decision
	stored.Reason = reason

	project := stored.Issue.Project()
	path := filepath.Join(s.baseDir, "raw", project, key+".json")

	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal issue: %w", err)
	}

	if err := writeFileAtomic(path, data); err != nil {
		return fmt.Errorf("failed to write issue: %w", err)
	}

	return nil
}

// ListIssues lists all issues for a project
func (s *Store) ListIssues(project string) ([]*StoredIssue, error) {
	dir := filepath.Join(s.baseDir, "raw", project)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read directory: %w", err)
	}

	var issues []*StoredIssue
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		key := strings.TrimSuffix(entry.Name(), ".json")
		stored, err := s.LoadIssue(key)
		if err != nil {
			return nil, fmt.Errorf("failed to load issue %s: %w", key, err)
		}
		issues = append(issues, stored)
	}

	return issues, nil
}

// ListAllIssues lists issues from all projects
func (s *Store) ListAllIssues() ([]*StoredIssue, error) {
	rawDir := filepath.Join(s.baseDir, "raw")
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read raw directory: %w", err)
	}

	var allIssues []*StoredIssue
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		issues, err := s.ListIssues(entry.Name())
		if err != nil {
			return nil, err
		}
		allIssues = append(allIssues, issues...)
	}

	return allIssues, nil
}

// Stats returns statistics about stored issues
type Stats struct {
	Total    int
	Pending  int
	Keep     int
	Skip     int
	Defer    int
	ByProject map[string]int
}

func (s *Store) Stats() (*Stats, error) {
	issues, err := s.ListAllIssues()
	if err != nil {
		return nil, err
	}

	stats := &Stats{
		ByProject: make(map[string]int),
	}

	for _, issue := range issues {
		stats.Total++
		stats.ByProject[issue.Issue.Project()]++

		switch issue.Decision {
		case DecisionPending:
			stats.Pending++
		case DecisionKeep:
			stats.Keep++
		case DecisionSkip:
			stats.Skip++
		case DecisionDefer:
			stats.Defer++
		}
	}

	return stats, nil
}

// GetPendingIssues returns issues that haven't been filtered yet
func (s *Store) GetPendingIssues() ([]*StoredIssue, error) {
	issues, err := s.ListAllIssues()
	if err != nil {
		return nil, err
	}

	var pending []*StoredIssue
	for _, issue := range issues {
		if issue.Decision == DecisionPending {
			pending = append(pending, issue)
		}
	}

	return pending, nil
}

// GetIssuesByDecision returns issues matching any of the given decisions.
func (s *Store) GetIssuesByDecision(decisions []FilterDecision) ([]*StoredIssue, error) {
	issues, err := s.ListAllIssues()
	if err != nil {
		return nil, err
	}

	set := make(map[FilterDecision]struct{}, len(decisions))
	for _, d := range decisions {
		set[d] = struct{}{}
	}

	var result []*StoredIssue
	for _, issue := range issues {
		if _, ok := set[issue.Decision]; ok {
			result = append(result, issue)
		}
	}

	return result, nil
}

// GetKeptIssues returns issues marked as keep
func (s *Store) GetKeptIssues() ([]*StoredIssue, error) {
	issues, err := s.ListAllIssues()
	if err != nil {
		return nil, err
	}

	var kept []*StoredIssue
	for _, issue := range issues {
		if issue.Decision == DecisionKeep {
			kept = append(kept, issue)
		}
	}

	return kept, nil
}

// SaveStoredIssue writes a StoredIssue back to disk
func (s *Store) SaveStoredIssue(stored *StoredIssue) error {
	project := stored.Issue.Project()
	if project == "" {
		return fmt.Errorf("issue %s has no project", stored.Issue.Key)
	}

	dir := filepath.Join(s.baseDir, "raw", project)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal issue: %w", err)
	}

	return writeFileAtomic(filepath.Join(dir, stored.Issue.Key+".json"), data)
}

// SaveProposal saves a classification proposal to the classified/ directory
func (s *Store) SaveProposal(name string, data interface{}) error {
	dir := filepath.Join(s.baseDir, "classified")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create classified directory: %w", err)
	}

	bytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal proposal: %w", err)
	}

	return os.WriteFile(filepath.Join(dir, name+".json"), bytes, 0644)
}

// LoadProposal loads a classification proposal from the classified/ directory
func (s *Store) LoadProposal(name string, target interface{}) error {
	path := filepath.Join(s.baseDir, "classified", name+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

// ClassifiedDir returns the path to the classified directory
func (s *Store) ClassifiedDir() string {
	return filepath.Join(s.baseDir, "classified")
}

// GetUpgradeableIssues returns kept issues that were enriched with low or no
// confidence, suitable for a second pass with a more capable model.
func (s *Store) GetUpgradeableIssues() ([]*StoredIssue, error) {
	issues, err := s.GetKeptIssues()
	if err != nil {
		return nil, err
	}

	var result []*StoredIssue
	for _, issue := range issues {
		if issue.Enrichment != nil &&
			(issue.Enrichment.Confidence == "low" || issue.Enrichment.Confidence == "none") {
			result = append(result, issue)
		}
	}

	return result, nil
}

// GetChildIssues returns all stored issues whose parent key matches parentKey.
func (s *Store) GetChildIssues(parentKey string) ([]*StoredIssue, error) {
	all, err := s.ListAllIssues()
	if err != nil {
		return nil, err
	}
	var children []*StoredIssue
	for _, issue := range all {
		if issue.Issue.ParentKey() == parentKey {
			children = append(children, issue)
		}
	}
	return children, nil
}

// GetEnrichableIssues returns kept issues that need enrichment: either not yet
// enriched, or previously rejected (so the agent can retry with feedback).
func (s *Store) GetEnrichableIssues() ([]*StoredIssue, error) {
	issues, err := s.GetKeptIssues()
	if err != nil {
		return nil, err
	}

	var result []*StoredIssue
	for _, issue := range issues {
		if issue.Enrichment == nil || issue.Enrichment.Decision == EnrichRejected {
			result = append(result, issue)
		}
	}

	return result, nil
}

// SaveEnrichSession persists the claude session ID for resumption
func (s *Store) SaveEnrichSession(sessionID string) error {
	return os.WriteFile(filepath.Join(s.baseDir, "enrich_session"), []byte(sessionID), 0644)
}

// LoadEnrichSession loads a previously saved claude session ID
func (s *Store) LoadEnrichSession() string {
	data, err := os.ReadFile(filepath.Join(s.baseDir, "enrich_session"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
