# Feature Implementation Plan

## Feature 1: Smart re-enrichment on field changes

**Goal:** When `screwjira ingest --sync` refreshes an issue, detect if any important fields
changed and mark the enrichment stale so the issue gets re-enriched.

### Fields to watch

Track: `summary`, `description`, `labels`, `components`, `issuetype`, `priority`
Ignore: `status` (handled separately for terminal states), `assignee`, `reporter`, timestamps

### Behaviour on change

- **Accepted enrichment** + field changed → set `Enrichment.Decision = EnrichRejected`,
  reason = `"sync: fields changed: summary, description"`. Keeps the old enrichment visible
  in review so the agent can use it as a starting point.
- **Pending/deferred enrichment** + field changed → clear `Enrichment = nil`. No value in
  keeping an unreviewed draft that's now stale.
- **Rejected enrichment** + field changed → clear `Enrichment = nil`. Already queued for redo.
- **No enrichment** → nothing to do.

### Code changes

**`internal/jira/client.go`** — add:
```go
func compareIssueFields(old, fresh *Issue) []string
```
Returns names of changed fields. Compares labels/components as sorted slices.

**`cmd/ingest.go`** — in `syncIssues()`:
- Replace the existing `len(newDesc) > len(oldDesc)+100` description-growth check with
  `compareIssueFields(stored.Issue, fresh)`.
- Apply the decision logic above.
- Count in `reenriched` counter (already exists).
- Log: `%s: fields changed (%s), enrichment reset`.

---

## Feature 2: Hierarchical enrichment for DISC ideas

**Goal:** DISC Ideas sit atop a hierarchy: `Idea → Epics → Bugs/Tasks`. Because Ideas are
wide strategic items (not deep technical ones), both enrichment paths use **light mode** —
no source-code scan, just a cursory look at the docs repo and a summary of child tickets.

### The two paths

| Condition | Context provided to agent | Agent instructions |
|---|---|---|
| DISC Idea **with** child epics | Idea description + child epics + their child tasks/bugs | Summarise hierarchy into a feature description; check docs repo only |
| DISC Idea **without** child epics | Idea description only | Light improvements; check docs repo only |

Both paths: no `AddDirs` for source repos, no MCP servers, `AllowedTools = ["Read"]` on
docs repo only (or no tools if no docs repo is configured).

### Data model changes

**`internal/storage/storage.go`** — add to `StoredIssue`:
```go
EnrichMode string `json:"enrich_mode,omitempty"` // "light" or ""
```
Stored so `enrich --status` can show it and future runs don't upgrade light issues to full.

Add:
```go
func (s *Store) GetChildIssues(parentKey string) ([]*StoredIssue, error)
```
Scans all stored issues, returns those where `Issue.ParentKey() == parentKey`.

### Config changes

Add `docs_repo` to `[enrich]` in `config.toml`:
```toml
docs_repo = "~/code/github.com/panamafrancis/docs"
```
Used to restrict the agent to docs files only in light mode. Optional; if absent, light mode
uses no tools (agent answers from provided context only).

### Code changes

**`cmd/enrich.go`** — add `buildLightEnrichSystemPrompt(docsRepo string) string`:
- Same repo/glossary context as the regular prompt.
- Replaces search instructions with: "Do NOT scan source code. If a docs repo is provided,
  you may Read files from it. Summarise the provided child tickets into a coherent feature
  description. Focus on what the feature is and why it matters."
- No `indexSection`.

**`cmd/enrich.go`** — add `buildHierarchicalIssuePrompt(issue *StoredIssue, store *Store) string`:
- Calls `buildIssuePrompt(issue)` for the Idea.
- Calls `store.GetChildIssues(issue.Issue.Key)` for child epics.
- For each child epic: appends `\nChild epic: KEY — SUMMARY` and recursively calls
  `store.GetChildIssues(epicKey)` to list grandchild tasks/bugs as `  - KEY: SUMMARY`.
- Total prompt size capped at ~4k chars.

**`cmd/enrich.go`** — in `runEnrichAgent()`:
```go
isLight := issue.Issue.Project() == "DISC" && issue.Issue.IssueType() == "Idea"
```
If true:
- Use `buildLightEnrichSystemPrompt(cfg.Enrich.DocsRepo)` as system prompt.
- Use `buildHierarchicalIssuePrompt(issue, store)` as issue prompt.
- Set `opts.AddDirs` to only the docs repo (or empty).
- Set `opts.MCPServers = nil`.
- After saving: `issue.EnrichMode = "light"`.

---

## Feature 3: Issue type normalisation

**Goal:** Map all Jira issue types to three GitHub output types: `bug`, `task`, `feature`.
Child issues of epics are **preserved** as separate bug/task issues. The collapse only happens
at the feature level (Idea/Epic → feature label).

### Mapping table

| Project | Jira type | Output type |
|---|---|---|
| DISC | Idea | feature |
| DISC | Epic | feature |
| DISC | Bug | bug |
| DISC | Task | task |
| F0 | Epic | feature |
| F0 | Bug | bug |
| F0 | Story | task |
| F0 | Task | task |
| F0 | Sub-task | task |
| F0 | Improvement | task |
| F0 | New Feature | feature |
| Any | (unknown) | task |

Milestones: ignored for now (no Jira type maps to milestone).

For DISC Ideas: copy `issue.Issue.Labels()` → `Classification.Labels` during normalisation
so the feature inherits the idea's labels for enrichment.

### New command: `screwjira normalize`

```
screwjira normalize           # dry run — show counts
screwjira normalize --apply   # set Classification.Type for all kept issues
```

**`cmd/normalize.go`** — new file:
- `normalizeIssueType(issue *jira.Issue) string` applies the mapping table.
- `runNormalize(store, apply bool) error` iterates kept issues, sets/reports types.
- Prints: `feature: N  bug: N  task: N  (already set: N)`.

**`cmd/ingest.go`** — call `runNormalize(store, true)` at the end of `runIngest` and
`syncIssues` so types are always current after ingestion.

---

## Feature 4: Glossary YAML

**Goal:** Build a project glossary during `screwjira index`, include it in enrichment system
prompts. Replaces the hardcoded codebase-history block with a maintainable, user-editable file.

### Format (`~/.screwjira/glossary.yaml`)

```yaml
terms:
  - term: Keystone API
    aliases: [keystone-api, admin-api]
    definition: "Primary internal API. Replaced admin-api."
    source: manual       # "manual" entries survive index rebuilds
  - term: BigQuery
    aliases: [BQ, Elasticsearch, ES]
    definition: "Data warehouse. Replaced Elasticsearch."
    source: manual
  - term: RFM Model
    definition: "Recency-Frequency-Monetary scoring used for merchant risk."
    source: auto         # extracted from docs; overwritten on next index
```

### Extraction strategy

1. **Docs repo markdown**: scan H2/H3 headings ≤ 5 words that look like noun phrases;
   extract the first non-empty paragraph as the definition.
2. **Go packages**: extract `// Package foo ...` first sentence from each package's godoc.
3. **Seed entries**: the hardcoded history notes become `source: manual` entries in the
   initial file so they're always present.

On rebuild: `auto` entries are replaced; `manual` entries are preserved.

### Code changes

**`internal/index/glossary.go`** — new file:
```go
type GlossaryTerm struct {
    Term       string   `yaml:"term"`
    Aliases    []string `yaml:"aliases,omitempty"`
    Definition string   `yaml:"definition"`
    Source     string   `yaml:"source"` // "auto" or "manual"
}
type GlossaryFile struct {
    Terms []GlossaryTerm `yaml:"terms"`
}
func ExtractGlossary(repos []string) []GlossaryTerm
func LoadGlossary(path string) (*GlossaryFile, error)
func SaveGlossary(path string, g *GlossaryFile) error
func MergeGlossary(existing *GlossaryFile, fresh []GlossaryTerm) *GlossaryFile
func RenderGlossaryCompact(g *GlossaryFile) string  // "term (aliases): def\n", ~2k max
```

Requires `gopkg.in/yaml.v3` — check `go.mod` before adding.

**`cmd/index.go`** — after building `index.md`:
- Call `ExtractGlossary(cfg.Enrich.Repos)`.
- Load existing `~/.screwjira/glossary.yaml` (or empty if missing).
- Merge and save.
- Print `Glossary: N terms (M auto, K manual)`.
- On first run: seed with the two hardcoded entries.

**`cmd/enrich.go`** — add `loadGlossary(dataDir string) string`:
- Reads `~/.screwjira/glossary.yaml`, calls `RenderGlossaryCompact`.
- Returns `""` if missing.
- Warns if older than 24h (same as index warning).

In `buildEnrichSystemPrompt`: replace the hardcoded codebase history block with:
```go
if glossary != "" {
    glossarySection = "## Codebase Glossary\n" + glossary + "\n"
}
```

---

## Implementation order

1. **Feature 1** — self-contained, no new storage fields, low risk. Start here.
2. **Feature 3** — self-contained too; unblocks Feature 2 (light mode logic reads
   `Classification.Type` which normalize sets).
3. **Feature 2** — depends on Feature 3 for type detection; requires `GetChildIssues`.
4. **Feature 4** — last, improves enrichment quality but blocks nothing.

## Open questions

- **Feature 2**: Confirm `docs_repo` is the right config key name, or auto-detect by
  directory name containing `"docs"` as fallback.
- **Feature 4**: Confirm `gopkg.in/yaml.v3` is in `go.mod` before implementing glossary.
