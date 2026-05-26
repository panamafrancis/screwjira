package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/panamafrancis/screwjira/internal/storage"
	"github.com/spf13/cobra"
)

// Proposal types for JSON serialization

type LabelProposal struct {
	// Normalized label -> list of original Jira values that mapped to it
	LabelMap map[string][]string `json:"label_map"`
	// Issue key -> list of proposed labels
	Assignments map[string][]string `json:"assignments"`
}

type TypeProposal struct {
	Assignments map[string]TypeAssignment `json:"assignments"`
}

type TypeAssignment struct {
	Type     string `json:"type"`     // task, bug, idea
	Summary  string `json:"summary"`  // for human readability in the JSON
	Reason   string `json:"reason"`
	Original string `json:"original"` // original Jira issue type
	Review   bool   `json:"review"`   // flagged for human review
}

type EpicProposal struct {
	// Epics found among kept issues, to be converted to ideas
	Epics map[string]EpicInfo `json:"epics"`
	// Proposed merges: epic-turned-idea that duplicates an existing DISC idea
	Merges []MergeProposal `json:"merges"`
}

type EpicInfo struct {
	Summary  string      `json:"summary"`
	Status   string      `json:"status"`
	Children []EpicChild `json:"children"`
}

type EpicChild struct {
	Key     string `json:"key"`
	Summary string `json:"summary"`
}

type MergeProposal struct {
	EpicKey     string  `json:"epic_key"`
	EpicSummary string  `json:"epic_summary"`
	IdeaKey     string  `json:"idea_key"`
	IdeaSummary string  `json:"idea_summary"`
	Score       float64 `json:"score"`   // 1.0 for link match, 0-1 for similarity
	Source      string  `json:"source"`  // "issue-link" or "similarity"
}

// Command flags

var (
	classifyLabelsApply  bool
	classifyLabelsDryRun bool
	classifyTypesApply   bool
	classifyEpicsApply   bool
)

// Commands

var classifyCmd = &cobra.Command{
	Use:   "classify",
	Short: "Classify and clean up issues before migration",
	Long:  `Analyze kept issues to propose labels, reclassify types, and convert/merge epics.`,
	RunE:  runClassifyStatus,
}

var classifyLabelsCmd = &cobra.Command{
	Use:   "labels",
	Short: "Propose and apply a GitHub label scheme",
	Long:  `Scan all kept issues, consolidate Jira labels and components into a clean GitHub label set.`,
	RunE:  runClassifyLabels,
}

var classifyTypesCmd = &cobra.Command{
	Use:   "types",
	Short: "Reclassify issues as task, bug, or idea",
	Long:  `DISC issues become ideas, bugs stay bugs, tasks/stories stay tasks. Ambiguous ones flagged for review.`,
	RunE:  runClassifyTypes,
}

var classifyEpicsCmd = &cobra.Command{
	Use:   "epics",
	Short: "Convert epics to ideas and merge duplicates",
	Long:  `Epics become ideas. Duplicates with existing DISC ideas are detected and merged.`,
	RunE:  runClassifyEpics,
}

func init() {
	rootCmd.AddCommand(classifyCmd)

	classifyCmd.AddCommand(classifyLabelsCmd)
	classifyLabelsCmd.Flags().BoolVar(&classifyLabelsApply, "apply", false, "apply proposed label assignments to issues")
	classifyLabelsCmd.Flags().BoolVar(&classifyLabelsDryRun, "dry-run", false, "preview assignments without saving proposal")

	classifyCmd.AddCommand(classifyTypesCmd)
	classifyTypesCmd.Flags().BoolVar(&classifyTypesApply, "apply", false, "apply proposed type reclassifications to issues")

	classifyCmd.AddCommand(classifyEpicsCmd)
	classifyEpicsCmd.Flags().BoolVar(&classifyEpicsApply, "apply", false, "convert epics to ideas, merge duplicates, link children")
}

// === Status overview ===

func runClassifyStatus(cmd *cobra.Command, args []string) error {
	store, err := storage.New(getDataDir())
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}

	issues, err := store.GetKeptIssues()
	if err != nil {
		return err
	}

	if len(issues) == 0 {
		fmt.Println("No kept issues to classify. Run 'screwjira filter' first.")
		return nil
	}

	classified := 0
	byType := map[string]int{}
	withLabels := 0
	for _, issue := range issues {
		if issue.Classification != nil {
			if issue.Classification.Type != "" {
				classified++
				byType[issue.Classification.Type]++
			}
			if len(issue.Classification.Labels) > 0 {
				withLabels++
			}
		}
	}

	fmt.Printf("Classification Status (%d kept issues):\n\n", len(issues))
	fmt.Printf("  Types classified: %d/%d\n", classified, len(issues))
	for _, t := range []string{"task", "bug", "idea"} {
		if n, ok := byType[t]; ok {
			fmt.Printf("    %-6s %d\n", t+":", n)
		}
	}
	fmt.Printf("  With labels:      %d/%d\n", withLabels, len(issues))

	var labelsProposal LabelProposal
	if err := store.LoadProposal("labels", &labelsProposal); err == nil {
		fmt.Printf("\n  Pending label proposal: %d assignments\n", len(labelsProposal.Assignments))
	}

	var typesProposal TypeProposal
	if err := store.LoadProposal("types", &typesProposal); err == nil {
		fmt.Printf("  Pending type proposal:  %d assignments\n", len(typesProposal.Assignments))
	}

	fmt.Println("\nSubcommands:")
	fmt.Println("  screwjira classify labels    Propose GitHub label scheme")
	fmt.Println("  screwjira classify types     Reclassify as task/bug/idea")
	fmt.Println("  screwjira classify epics     Convert epics to ideas, merge duplicates")

	return nil
}

// === Labels ===

func runClassifyLabels(cmd *cobra.Command, args []string) error {
	store, err := storage.New(getDataDir())
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}

	if classifyLabelsApply {
		return applyLabelProposal(store)
	}

	issues, err := store.GetKeptIssues()
	if err != nil {
		return err
	}

	if len(issues) == 0 {
		fmt.Println("No kept issues to classify.")
		return nil
	}

	proposal := analyzeLabels(issues)
	displayLabelProposal(proposal, len(issues))

	if classifyLabelsDryRun {
		return nil
	}

	if err := store.SaveProposal("labels", proposal); err != nil {
		return fmt.Errorf("failed to save proposal: %w", err)
	}

	fmt.Printf("\nProposal saved to %s/classified/labels.json\n", getDataDir())
	fmt.Println("Edit the file to adjust, then run 'screwjira classify labels --apply'")
	return nil
}

func analyzeLabels(issues []*storage.StoredIssue) *LabelProposal {
	labelMap := map[string][]string{} // normalized -> originals

	for _, issue := range issues {
		for _, label := range issue.Issue.Labels() {
			norm := normalizeLabel(label)
			if norm == "" {
				continue
			}
			if !containsStr(labelMap[norm], label) {
				labelMap[norm] = append(labelMap[norm], label)
			}
		}
		for _, comp := range issue.Issue.Components() {
			norm := normalizeLabel(comp)
			if norm == "" {
				continue
			}
			tag := comp + " (component)"
			if !containsStr(labelMap[norm], tag) {
				labelMap[norm] = append(labelMap[norm], tag)
			}
		}
	}

	// Build per-issue assignments
	assignments := map[string][]string{}
	for _, issue := range issues {
		seen := map[string]bool{}
		var labels []string

		for _, label := range issue.Issue.Labels() {
			norm := normalizeLabel(label)
			if norm != "" && !seen[norm] {
				labels = append(labels, norm)
				seen[norm] = true
			}
		}
		for _, comp := range issue.Issue.Components() {
			norm := normalizeLabel(comp)
			if norm != "" && !seen[norm] {
				labels = append(labels, norm)
				seen[norm] = true
			}
		}

		if len(labels) > 0 {
			sort.Strings(labels)
			assignments[issue.Issue.Key] = labels
		}
	}

	return &LabelProposal{
		LabelMap:    labelMap,
		Assignments: assignments,
	}
}

func displayLabelProposal(proposal *LabelProposal, totalIssues int) {
	fmt.Printf("Label Analysis (%d kept issues):\n\n", totalIssues)

	if len(proposal.LabelMap) == 0 {
		fmt.Println("  No labels or components found on any issues.")
		return
	}

	// Count issues per label
	labelCounts := map[string]int{}
	for _, labels := range proposal.Assignments {
		for _, l := range labels {
			labelCounts[l]++
		}
	}

	// Sort by frequency
	type labelEntry struct {
		name  string
		count int
	}
	var sorted []labelEntry
	for name := range proposal.LabelMap {
		sorted = append(sorted, labelEntry{name, labelCounts[name]})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].count > sorted[j].count
	})

	fmt.Println("Proposed Labels:")
	for _, entry := range sorted {
		originals := proposal.LabelMap[entry.name]
		fmt.Printf("  %-30s (%3d issues)", entry.name, entry.count)
		// Show originals if there are variants
		if len(originals) > 1 || (len(originals) == 1 && normalizeLabel(originals[0]) != originals[0]) {
			fmt.Printf("  <- %s", strings.Join(originals, ", "))
		}
		fmt.Println()
	}

	assigned := len(proposal.Assignments)
	unassigned := totalIssues - assigned
	fmt.Printf("\n  Assigned:   %d issues\n", assigned)
	fmt.Printf("  Unlabeled:  %d issues\n", unassigned)
}

func applyLabelProposal(store *storage.Store) error {
	var proposal LabelProposal
	if err := store.LoadProposal("labels", &proposal); err != nil {
		return fmt.Errorf("no label proposal found — run 'screwjira classify labels' first: %w", err)
	}

	count := 0
	for key, labels := range proposal.Assignments {
		stored, err := store.LoadIssue(key)
		if err != nil {
			fmt.Printf("  Warning: %s not found, skipping\n", key)
			continue
		}
		if stored.Classification == nil {
			stored.Classification = &storage.Classification{}
		}
		stored.Classification.Labels = labels
		if err := store.SaveStoredIssue(stored); err != nil {
			return fmt.Errorf("failed to save %s: %w", key, err)
		}
		count++
	}

	fmt.Printf("Applied labels to %d issues.\n", count)
	return nil
}

// === Types ===

func runClassifyTypes(cmd *cobra.Command, args []string) error {
	store, err := storage.New(getDataDir())
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}

	if classifyTypesApply {
		return applyTypeProposal(store)
	}

	issues, err := store.GetKeptIssues()
	if err != nil {
		return err
	}

	if len(issues) == 0 {
		fmt.Println("No kept issues to classify.")
		return nil
	}

	proposal := analyzeTypes(issues)
	displayTypeProposal(proposal)

	if err := store.SaveProposal("types", proposal); err != nil {
		return fmt.Errorf("failed to save proposal: %w", err)
	}

	fmt.Printf("\nProposal saved to %s/classified/types.json\n", getDataDir())
	fmt.Println("Edit the file to adjust types, then run 'screwjira classify types --apply'")
	return nil
}

var ideaKeywords = []string{
	"investigate", "explore", "consider", "idea", "proposal",
	"research", "spike", "poc", "prototype", "evaluate",
	"assess", "study", "rethink",
}

func analyzeTypes(issues []*storage.StoredIssue) *TypeProposal {
	proposal := &TypeProposal{
		Assignments: make(map[string]TypeAssignment),
	}

	for _, issue := range issues {
		key := issue.Issue.Key
		jiraType := issue.Issue.IssueType()
		project := issue.Issue.Project()

		a := TypeAssignment{
			Original: jiraType,
			Summary:  issue.Issue.Summary(),
		}

		switch {
		case project == "DISC":
			a.Type = "idea"
			a.Reason = "DISC project (JPD)"

		case strings.EqualFold(jiraType, "Epic"):
			a.Type = "idea"
			a.Reason = "Jira epic -> idea"

		case strings.EqualFold(jiraType, "Bug"):
			a.Type = "bug"
			a.Reason = fmt.Sprintf("Jira type: %s", jiraType)

		default: // Task, Story, Sub-task, etc.
			summary := strings.ToLower(issue.Issue.Summary())
			matchedKeyword := ""
			for _, kw := range ideaKeywords {
				if strings.Contains(summary, kw) {
					matchedKeyword = kw
					break
				}
			}

			if matchedKeyword != "" {
				a.Type = "idea"
				a.Reason = fmt.Sprintf("summary contains '%s' (was %s)", matchedKeyword, jiraType)
				a.Review = true
			} else {
				a.Type = "task"
				a.Reason = fmt.Sprintf("Jira type: %s", jiraType)
			}
		}

		proposal.Assignments[key] = a
	}

	return proposal
}

func displayTypeProposal(proposal *TypeProposal) {
	byType := map[string]int{}
	var needsReview []string

	for key, a := range proposal.Assignments {
		byType[a.Type]++
		if a.Review {
			needsReview = append(needsReview, key)
		}
	}

	fmt.Printf("Type Analysis (%d kept issues):\n\n", len(proposal.Assignments))
	for _, t := range []string{"task", "bug", "idea"} {
		if n, ok := byType[t]; ok {
			fmt.Printf("  %-6s %d\n", t+":", n)
		}
	}

	if len(needsReview) > 0 {
		sort.Strings(needsReview)
		fmt.Printf("\nNeeds Review (%d issues):\n", len(needsReview))
		for _, key := range needsReview {
			a := proposal.Assignments[key]
			fmt.Printf("  %-12s  %-6s  %s\n", key, a.Type, truncate(a.Summary, 50))
			fmt.Printf("  %s  reason: %s\n", strings.Repeat(" ", 12), a.Reason)
		}
	}
}

func applyTypeProposal(store *storage.Store) error {
	var proposal TypeProposal
	if err := store.LoadProposal("types", &proposal); err != nil {
		return fmt.Errorf("no type proposal found — run 'screwjira classify types' first: %w", err)
	}

	count := 0
	for key, a := range proposal.Assignments {
		stored, err := store.LoadIssue(key)
		if err != nil {
			fmt.Printf("  Warning: %s not found, skipping\n", key)
			continue
		}
		if stored.Classification == nil {
			stored.Classification = &storage.Classification{}
		}
		stored.Classification.Type = a.Type
		if err := store.SaveStoredIssue(stored); err != nil {
			return fmt.Errorf("failed to save %s: %w", key, err)
		}
		count++
	}

	fmt.Printf("Applied types to %d issues.\n", count)
	return nil
}

// === Epics ===

func runClassifyEpics(cmd *cobra.Command, args []string) error {
	store, err := storage.New(getDataDir())
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}

	if classifyEpicsApply {
		return applyEpicProposal(store)
	}

	issues, err := store.GetKeptIssues()
	if err != nil {
		return err
	}

	if len(issues) == 0 {
		fmt.Println("No kept issues.")
		return nil
	}

	proposal := analyzeEpics(issues)

	if len(proposal.Epics) == 0 {
		fmt.Println("No epics found among kept issues.")
		return nil
	}

	displayEpicProposal(proposal)

	if err := store.SaveProposal("epics", proposal); err != nil {
		return fmt.Errorf("failed to save proposal: %w", err)
	}

	fmt.Printf("\nProposal saved to %s/classified/epics.json\n", getDataDir())
	fmt.Println("Edit merges in the file, then run 'screwjira classify epics --apply'")
	return nil
}

func analyzeEpics(issues []*storage.StoredIssue) *EpicProposal {
	// Index all issues by key for quick lookup
	byKey := map[string]*storage.StoredIssue{}
	for _, issue := range issues {
		byKey[issue.Issue.Key] = issue
	}

	// Find all epics
	epicSet := map[string]bool{}
	epics := map[string]*EpicInfo{}
	for _, issue := range issues {
		if strings.EqualFold(issue.Issue.IssueType(), "Epic") {
			epicSet[issue.Issue.Key] = true
			epics[issue.Issue.Key] = &EpicInfo{
				Summary: issue.Issue.Summary(),
				Status:  issue.Issue.Status(),
			}
		}
	}

	// Find children of each epic
	for _, issue := range issues {
		parentKey := issue.Issue.ParentKey()
		if parentKey == "" {
			continue
		}
		if epic, ok := epics[parentKey]; ok {
			epic.Children = append(epic.Children, EpicChild{
				Key:     issue.Issue.Key,
				Summary: issue.Issue.Summary(),
			})
		}
	}

	// Collect DISC ideas indexed by key
	discByKey := map[string]*storage.StoredIssue{}
	var discIdeas []*storage.StoredIssue
	for _, issue := range issues {
		if issue.Issue.Project() == "DISC" {
			discByKey[issue.Issue.Key] = issue
			discIdeas = append(discIdeas, issue)
		}
	}

	// Step 1: Match epics to DISC ideas via issue links.
	// In Jira, DISC ideas link to F0 epics (because ideas can't parent tasks directly).
	// Check both directions: DISC idea linking to epic, and epic linking to DISC idea.
	epicToIdea := map[string]string{}        // epic key -> DISC idea key
	epicToIdeaSummary := map[string]string{} // epic key -> DISC idea summary

	// Scan DISC ideas for links to epics
	for _, idea := range discIdeas {
		for _, linkedKey := range idea.Issue.IssueLinks() {
			if epicSet[linkedKey] {
				epicToIdea[linkedKey] = idea.Issue.Key
				epicToIdeaSummary[linkedKey] = idea.Issue.Summary()
			}
		}
	}

	// Scan epics for links to DISC ideas (reverse direction)
	for epicKey := range epics {
		if _, found := epicToIdea[epicKey]; found {
			continue
		}
		if epicIssue, ok := byKey[epicKey]; ok {
			for _, linkedKey := range epicIssue.Issue.IssueLinks() {
				if disc, ok := discByKey[linkedKey]; ok {
					epicToIdea[epicKey] = linkedKey
					epicToIdeaSummary[epicKey] = disc.Issue.Summary()
					break
				}
			}
		}
	}

	// Step 2: For unmatched epics, fall back to word-overlap similarity
	var merges []MergeProposal
	for epicKey, epic := range epics {
		if ideaKey, found := epicToIdea[epicKey]; found {
			merges = append(merges, MergeProposal{
				EpicKey:     epicKey,
				EpicSummary: epic.Summary,
				IdeaKey:     ideaKey,
				IdeaSummary: epicToIdeaSummary[epicKey],
				Score:       1.0,
				Source:      "issue-link",
			})
			continue
		}

		bestScore := 0.0
		bestIdea := ""
		bestSummary := ""
		for _, idea := range discIdeas {
			score := wordOverlapScore(epic.Summary, idea.Issue.Summary())
			if score > bestScore {
				bestScore = score
				bestIdea = idea.Issue.Key
				bestSummary = idea.Issue.Summary()
			}
		}
		if bestScore >= 0.4 {
			merges = append(merges, MergeProposal{
				EpicKey:     epicKey,
				EpicSummary: epic.Summary,
				IdeaKey:     bestIdea,
				IdeaSummary: bestSummary,
				Score:       bestScore,
				Source:      "similarity",
			})
		}
	}

	// Sort: links first, then by score
	sort.Slice(merges, func(i, j int) bool {
		if merges[i].Source != merges[j].Source {
			return merges[i].Source == "issue-link"
		}
		return merges[i].Score > merges[j].Score
	})

	// Convert to value map for serialization
	result := make(map[string]EpicInfo, len(epics))
	for k, v := range epics {
		result[k] = *v
	}

	return &EpicProposal{Epics: result, Merges: merges}
}

func displayEpicProposal(proposal *EpicProposal) {
	var keys []string
	for k := range proposal.Epics {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Build set of epics that have a merge
	mergedEpics := map[string]MergeProposal{}
	for _, m := range proposal.Merges {
		mergedEpics[m.EpicKey] = m
	}

	fmt.Printf("Epics -> Ideas (%d epics):\n\n", len(keys))

	for _, key := range keys {
		epic := proposal.Epics[key]
		fmt.Printf("  %s  %s  [%s]  (%d children)\n", key, truncate(epic.Summary, 45), epic.Status, len(epic.Children))
		for _, child := range epic.Children {
			fmt.Printf("    -> %s  %s\n", child.Key, truncate(child.Summary, 50))
		}
		if m, ok := mergedEpics[key]; ok {
			if m.Source == "issue-link" {
				fmt.Printf("    ** MERGE into %s  %s  (linked)\n", m.IdeaKey, truncate(m.IdeaSummary, 40))
			} else {
				fmt.Printf("    ** MERGE into %s  %s  (similarity: %.0f%%)\n", m.IdeaKey, truncate(m.IdeaSummary, 40), m.Score*100)
			}
		}
		fmt.Println()
	}

	linked := 0
	similar := 0
	for _, m := range proposal.Merges {
		if m.Source == "issue-link" {
			linked++
		} else {
			similar++
		}
	}
	standalone := len(keys) - len(proposal.Merges)
	fmt.Printf("  Standalone (become new ideas):  %d\n", standalone)
	fmt.Printf("  Merge via issue links:          %d\n", linked)
	if similar > 0 {
		fmt.Printf("  Merge via similarity (review!): %d\n", similar)
	}
}

func applyEpicProposal(store *storage.Store) error {
	var proposal EpicProposal
	if err := store.LoadProposal("epics", &proposal); err != nil {
		return fmt.Errorf("no epic proposal found — run 'screwjira classify epics' first: %w", err)
	}

	// Build merge lookup: epic key -> DISC idea key
	mergeTarget := map[string]string{}
	for _, m := range proposal.Merges {
		mergeTarget[m.EpicKey] = m.IdeaKey
	}

	converted := 0
	merged := 0
	childrenLinked := 0

	for epicKey, epic := range proposal.Epics {
		ideaKey, shouldMerge := mergeTarget[epicKey]

		if shouldMerge {
			// Merge: skip the epic, children point to the DISC idea
			if err := store.UpdateDecision(epicKey, storage.DecisionSkip, fmt.Sprintf("merged into %s", ideaKey)); err != nil {
				return fmt.Errorf("failed to skip epic %s: %w", epicKey, err)
			}
			merged++

			// Link children to the DISC idea
			for _, child := range epic.Children {
				stored, err := store.LoadIssue(child.Key)
				if err != nil {
					fmt.Printf("  Warning: child %s not found, skipping\n", child.Key)
					continue
				}
				if stored.Classification == nil {
					stored.Classification = &storage.Classification{}
				}
				stored.Classification.EpicParent = ideaKey
				if err := store.SaveStoredIssue(stored); err != nil {
					return fmt.Errorf("failed to save %s: %w", child.Key, err)
				}
				childrenLinked++
			}
		} else {
			// Standalone: convert epic to idea
			stored, err := store.LoadIssue(epicKey)
			if err != nil {
				fmt.Printf("  Warning: epic %s not found, skipping\n", epicKey)
				continue
			}
			if stored.Classification == nil {
				stored.Classification = &storage.Classification{}
			}
			stored.Classification.Type = "idea"
			if err := store.SaveStoredIssue(stored); err != nil {
				return fmt.Errorf("failed to save %s: %w", epicKey, err)
			}
			converted++

			// Link children to the epic-turned-idea
			for _, child := range epic.Children {
				cs, err := store.LoadIssue(child.Key)
				if err != nil {
					fmt.Printf("  Warning: child %s not found, skipping\n", child.Key)
					continue
				}
				if cs.Classification == nil {
					cs.Classification = &storage.Classification{}
				}
				cs.Classification.EpicParent = epicKey
				if err := store.SaveStoredIssue(cs); err != nil {
					return fmt.Errorf("failed to save %s: %w", child.Key, err)
				}
				childrenLinked++
			}
		}
	}

	fmt.Printf("Converted %d epics to ideas.\n", converted)
	fmt.Printf("Merged %d epics into existing DISC ideas (marked as skip).\n", merged)
	fmt.Printf("Linked %d children to their parent idea.\n", childrenLinked)
	return nil
}

// wordOverlapScore computes similarity between two strings based on word overlap.
// Returns a value between 0 and 1.
func wordOverlapScore(a, b string) float64 {
	wordsA := tokenize(a)
	wordsB := tokenize(b)

	if len(wordsA) == 0 || len(wordsB) == 0 {
		return 0
	}

	setB := make(map[string]bool, len(wordsB))
	for _, w := range wordsB {
		setB[w] = true
	}

	overlap := 0
	for _, w := range wordsA {
		if setB[w] {
			overlap++
		}
	}

	maxLen := len(wordsA)
	if len(wordsB) > maxLen {
		maxLen = len(wordsB)
	}

	return float64(overlap) / float64(maxLen)
}

var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true,
	"to": true, "in": true, "for": true, "of": true, "with": true,
	"is": true, "it": true, "as": true, "on": true, "at": true,
	"by": true, "be": true, "we": true, "our": true, "this": true,
	"that": true, "from": true, "are": true, "was": true, "has": true,
	"have": true, "will": true, "can": true, "do": true, "not": true,
}

func tokenize(s string) []string {
	s = strings.ToLower(s)
	words := strings.FieldsFunc(s, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	var result []string
	for _, w := range words {
		if len(w) > 1 && !stopWords[w] {
			result = append(result, w)
		}
	}
	return result
}

// === Helpers ===

func normalizeLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "_", "-")
	return s
}

func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
