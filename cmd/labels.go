package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode"

	"github.com/fraud-zero/screwjira/internal/storage"
	"github.com/spf13/cobra"
)

var labelsCmd = &cobra.Command{
	Use:   "labels",
	Short: "Interactively consolidate labels across all kept issues",
	RunE:  runLabels,
}

var labelsDedupCmd = &cobra.Command{
	Use:   "dedupe",
	Short: "Find and merge near-duplicate labels",
	RunE:  runLabelsDedupe,
}

func init() {
	rootCmd.AddCommand(labelsCmd)
	labelsCmd.AddCommand(labelsDedupCmd)
}

type labelEntry struct {
	Name  string
	Count int
	Keys  []string
}

type labelAction int

const (
	labelKeep   labelAction = iota
	labelRename             // newName must be non-empty
	labelDelete
)

type labelDecision struct {
	action  labelAction
	newName string
}

// activeLabels returns the label slice that post will use for this issue.
func activeLabels(issue *storage.StoredIssue) []string {
	if issue.Enrichment != nil && issue.Enrichment.Decision == storage.EnrichAccepted {
		return issue.Enrichment.ProposedLabels
	}
	if issue.Classification != nil {
		return issue.Classification.Labels
	}
	return issue.Issue.Labels()
}

// setActiveLabels writes updated labels back to the appropriate field.
func setActiveLabels(issue *storage.StoredIssue, labels []string) {
	if issue.Enrichment != nil && issue.Enrichment.Decision == storage.EnrichAccepted {
		issue.Enrichment.ProposedLabels = labels
	} else if issue.Classification != nil {
		issue.Classification.Labels = labels
	} else {
		issue.Classification = &storage.Classification{Labels: labels}
	}
}

// resolveRename follows rename chains to the final label name, returning
// ("", false) if the chain resolves to a deleted label.
func resolveRename(name string, decisions map[string]labelDecision) (string, bool) {
	visited := map[string]bool{}
	for {
		if visited[name] {
			fmt.Printf("Warning: rename cycle detected involving label %q — keeping as-is\n", name)
			return name, true
		}
		visited[name] = true
		d := decisions[name]
		switch d.action {
		case labelDelete:
			return "", false
		case labelRename:
			name = d.newName
		default:
			return name, true
		}
	}
}

// readLine reads one line from r, returning ("", errEOF) on EOF/Ctrl-D.
var errEOF = errors.New("input closed")

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			fmt.Println()
			return "", errEOF
		}
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func runLabels(cmd *cobra.Command, args []string) error {
	store, err := storage.New(getDataDir())
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}

	issues, err := store.GetKeptIssues()
	if err != nil {
		return err
	}

	// Build label → issue keys map.
	labelIssues := map[string][]string{}
	for _, issue := range issues {
		for _, label := range activeLabels(issue) {
			labelIssues[label] = append(labelIssues[label], issue.Issue.Key)
		}
	}

	if len(labelIssues) == 0 {
		fmt.Println("No labels found across kept issues.")
		return nil
	}

	entries := make([]labelEntry, 0, len(labelIssues))
	for name, keys := range labelIssues {
		entries = append(entries, labelEntry{Name: name, Count: len(keys), Keys: keys})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Count != entries[j].Count {
			return entries[i].Count > entries[j].Count
		}
		return entries[i].Name < entries[j].Name
	})

	fmt.Printf("Labels across %d kept issues (%d distinct):\n\n", len(issues), len(entries))

	decisions := make(map[string]labelDecision, len(entries))
	reader := bufio.NewReader(os.Stdin)

	for i, entry := range entries {
		keyPreview := strings.Join(entry.Keys, ", ")
		if len(entry.Keys) > 3 {
			keyPreview = strings.Join(entry.Keys[:3], ", ") + fmt.Sprintf(", +%d more", len(entry.Keys)-3)
		}
		fmt.Printf("[%d/%d] %q — %d issue(s): %s\n", i+1, len(entries), entry.Name, entry.Count, keyPreview)
		fmt.Print("  > [k]eep  [r]ename  [d]elete  [q]uit: ")

		line, err := readLine(reader)
		if err != nil {
			fmt.Println("Input closed. No changes applied.")
			return nil
		}

		switch line {
		case "", "k":
			decisions[entry.Name] = labelDecision{action: labelKeep}
		case "r":
			fmt.Print("  → ")
			newName, err := readLine(reader)
			if err != nil {
				fmt.Println("Input closed. No changes applied.")
				return nil
			}
			if newName == "" || newName == entry.Name {
				decisions[entry.Name] = labelDecision{action: labelKeep}
				fmt.Println("  No change.")
			} else {
				decisions[entry.Name] = labelDecision{action: labelRename, newName: newName}
				fmt.Printf("  Will rename → %q\n", newName)
			}
		case "d":
			decisions[entry.Name] = labelDecision{action: labelDelete}
			fmt.Printf("  Will delete from %d issue(s)\n", entry.Count)
		case "q":
			fmt.Println("Quit (no changes applied).")
			return nil
		default:
			decisions[entry.Name] = labelDecision{action: labelKeep}
		}
		fmt.Println()
	}

	renames, deletes := 0, 0
	for _, d := range decisions {
		switch d.action {
		case labelRename:
			renames++
		case labelDelete:
			deletes++
		}
	}

	if renames == 0 && deletes == 0 {
		fmt.Println("No changes to apply.")
		return nil
	}

	fmt.Printf("Summary: %d rename(s), %d delete(s)\n", renames, deletes)
	fmt.Print("Apply? [y/n]: ")
	confirm, err := readLine(reader)
	if err != nil || strings.ToLower(confirm) != "y" {
		fmt.Println("Aborted.")
		return nil
	}

	updated, skipped := 0, 0
	for _, orig := range issues {
		fresh, err := store.LoadIssue(orig.Issue.Key)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Printf("Warning: %s no longer on disk — skipping\n", orig.Issue.Key)
			} else {
				fmt.Printf("Error: failed to reload %s: %v\n", orig.Issue.Key, err)
			}
			skipped++
			continue
		}

		current := activeLabels(fresh)
		if len(current) == 0 {
			continue
		}

		changed := false
		seen := make(map[string]bool, len(current))
		newLabels := make([]string, 0, len(current))
		for _, label := range current {
			resolved, keep := resolveRename(label, decisions)
			if !keep {
				changed = true
				continue
			}
			if resolved != label {
				changed = true
			}
			if !seen[resolved] {
				newLabels = append(newLabels, resolved)
				seen[resolved] = true
			}
		}

		if !changed {
			continue
		}

		setActiveLabels(fresh, newLabels)
		if err := store.SaveStoredIssue(fresh); err != nil {
			fmt.Printf("Warning: failed to save %s: %v\n", fresh.Issue.Key, err)
		} else {
			updated++
		}
	}

	fmt.Printf("Done. %d issue(s) updated.\n", updated)
	if skipped > 0 {
		fmt.Printf("Warning: %d issue(s) skipped due to load errors — label changes may be incomplete.\n", skipped)
	}
	return nil
}

// === labels dedupe ===

// normalizeLabelForDedup strips all separators and lowercases so "api-gateway",
// "api_gateway", and "API Gateway" all compare equal.
func normalizeLabelForDedup(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// labelEditDistance returns the Levenshtein distance between a and b.
func labelEditDistance(a, b string) int {
	if a == b {
		return 0
	}
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	row := make([]int, lb+1)
	for j := range row {
		row[j] = j
	}
	for i := 1; i <= la; i++ {
		prev := i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur := row[j-1] + cost
			if row[j]+1 < cur {
				cur = row[j] + 1
			}
			if prev+1 < cur {
				cur = prev + 1
			}
			row[j-1] = prev
			prev = cur
		}
		row[lb] = prev
	}
	return row[lb]
}

// labelPair represents two candidate-duplicate labels. A < B lexicographically
// (enforced by the construction loop over a sorted slice).
type labelPair struct {
	A, B           string
	CountA, CountB int
	exactNorm      bool // normalizeLabelForDedup(A) == normalizeLabelForDedup(B)
}

func runLabelsDedupe(cmd *cobra.Command, args []string) error {
	store, err := storage.New(getDataDir())
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}

	issues, err := store.GetKeptIssues()
	if err != nil {
		return err
	}

	labelCount := map[string]int{}
	for _, issue := range issues {
		for _, l := range activeLabels(issue) {
			labelCount[l]++
		}
	}
	if len(labelCount) == 0 {
		fmt.Println("No labels found across kept issues.")
		return nil
	}

	labels := make([]string, 0, len(labelCount))
	for l := range labelCount {
		labels = append(labels, l)
	}
	sort.Strings(labels)

	var pairs []labelPair
	for i := 0; i < len(labels); i++ {
		for j := i + 1; j < len(labels); j++ {
			a, b := labels[i], labels[j]
			na, nb := normalizeLabelForDedup(a), normalizeLabelForDedup(b)
			exactNorm := na == nb
			if !exactNorm {
				maxLen := len(na)
				if len(nb) > maxLen {
					maxLen = len(nb)
				}
				if maxLen == 0 {
					continue
				}
				if float64(labelEditDistance(na, nb))/float64(maxLen) >= 0.3 {
					continue
				}
			}
			pairs = append(pairs, labelPair{
				A: a, B: b,
				CountA: labelCount[a], CountB: labelCount[b],
				exactNorm: exactNorm,
			})
		}
	}

	if len(pairs) == 0 {
		fmt.Println("No near-duplicate labels found.")
		return nil
	}

	// Exact-norm matches first, then by combined count descending.
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].exactNorm != pairs[j].exactNorm {
			return pairs[i].exactNorm
		}
		return pairs[i].CountA+pairs[i].CountB > pairs[j].CountA+pairs[j].CountB
	})

	fmt.Printf("Found %d near-duplicate label pair(s).\n", len(pairs))
	fmt.Println("Commands: [1] keep first  [2] keep second  [b]oth  [n]ext  [q]uit")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	merges := map[string]string{} // loser → winner
	resolved := 0

	for i := 0; i < len(pairs); {
		p := pairs[i]

		// Skip if both sides already resolve to the same winner.
		effectiveA := resolveChain(p.A, merges)
		effectiveB := resolveChain(p.B, merges)
		if effectiveA == effectiveB {
			i++
			continue
		}

		tag := ""
		if p.exactNorm {
			tag = "  [same when normalised]"
		}
		// Show effective names so the user sees what they're actually merging.
		fmt.Printf("[%d/%d] %q (%d)  vs  %q (%d)%s\n",
			i+1, len(pairs), effectiveA, labelCount[p.A], effectiveB, labelCount[p.B], tag)
		fmt.Print("  > [1]  [2]  [b]oth  [n]ext  [q]uit: ")

		line, err := readLine(reader)
		if err != nil {
			fmt.Println("Input closed. No changes applied.")
			return nil
		}
		fmt.Println()

		switch line {
		case "1":
			merges[p.B] = effectiveA
			resolved++
			fmt.Printf("  Will rename %q → %q\n\n", effectiveB, effectiveA)
			i++
		case "2":
			merges[p.A] = effectiveB
			resolved++
			fmt.Printf("  Will rename %q → %q\n\n", effectiveA, effectiveB)
			i++
		case "b", "both", "n", "next":
			i++
		case "q", "quit":
			fmt.Println("Quit (no changes applied).")
			return nil
		default:
			i++
		}
	}

	if resolved == 0 {
		fmt.Println("No merges to apply.")
		return nil
	}

	fmt.Printf("Summary: %d merge(s)\n", resolved)
	fmt.Print("Apply? [y/n]: ")
	confirm, err := readLine(reader)
	if err != nil || strings.ToLower(confirm) != "y" {
		fmt.Println("Aborted.")
		return nil
	}

	// Convert merges into labelDecisions and reuse the existing apply loop.
	decisions := make(map[string]labelDecision, len(merges))
	for loser, winner := range merges {
		decisions[loser] = labelDecision{action: labelRename, newName: winner}
	}

	updated, skipped := 0, 0
	for _, orig := range issues {
		fresh, err := store.LoadIssue(orig.Issue.Key)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Printf("Warning: %s no longer on disk — skipping\n", orig.Issue.Key)
			} else {
				fmt.Printf("Error: failed to reload %s: %v\n", orig.Issue.Key, err)
			}
			skipped++
			continue
		}
		current := activeLabels(fresh)
		if len(current) == 0 {
			continue
		}
		changed := false
		seen := make(map[string]bool, len(current))
		newLabels := make([]string, 0, len(current))
		for _, label := range current {
			resolved, keep := resolveRename(label, decisions)
			if !keep {
				changed = true
				continue
			}
			if resolved != label {
				changed = true
			}
			if !seen[resolved] {
				newLabels = append(newLabels, resolved)
				seen[resolved] = true
			}
		}
		if !changed {
			continue
		}
		setActiveLabels(fresh, newLabels)
		if err := store.SaveStoredIssue(fresh); err != nil {
			fmt.Printf("Warning: failed to save %s: %v\n", fresh.Issue.Key, err)
		} else {
			updated++
		}
	}

	fmt.Printf("Done. %d issue(s) updated.\n", updated)
	if skipped > 0 {
		fmt.Printf("Warning: %d issue(s) skipped due to load errors — label changes may be incomplete.\n", skipped)
	}
	return nil
}

// resolveChain follows a merge chain to its final winner.
func resolveChain(name string, merges map[string]string) string {
	visited := map[string]bool{}
	for {
		if visited[name] {
			return name
		}
		visited[name] = true
		next, ok := merges[name]
		if !ok {
			return name
		}
		name = next
	}
}
