# fuckjira - Jira to GitHub Projects Migration Tool

## Overview

A CLI tool to migrate issues from Jira and JPD (Jira Product Discovery) to GitHub Projects, with intelligent enrichment and interactive filtering.

## Target Structure

| GitHub Project | Source | Jira Project | Count | Purpose |
|---------------|--------|--------------|-------|---------|
| `engineering` | Jira | F0 | ~312 active | Technical implementation tickets |
| `product` | JPD | DISC | ~140 ideas | High-level product initiatives |

Both projects are accessible via standard `acli jira workitem search --jql=...` commands.

## Architecture

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   Ingest    │────▶│   Filter    │────▶│   Enrich    │────▶│    Post     │
│  (Jira/JPD) │     │(Interactive)│     │ (Repo scan) │     │  (GitHub)   │
└─────────────┘     └─────────────┘     └─────────────┘     └─────────────┘
       │                   │                   │                   │
       ▼                   ▼                   ▼                   ▼
   ~/.fuckjira/        Terminal          ~/.fuckjira/         GitHub API
   raw/*.json          prompts           enriched/*.json      Projects
```

## Phase 1: Ingestion

Dump all Jira/JPD data locally for offline processing.

### Data Captured Per Issue
- Key, summary, description
- Status, priority, type
- Labels, components
- Assignee, reporter
- Created/updated timestamps
- Comments (all, with authors)
- Attachments (images, files) - downloaded locally
- Links (blocks, relates to, etc.)
- Sprint/epic associations
- Custom fields (story points, etc.)
- JPD-specific: ideas, insights, roadmap items

### Storage Structure
```
~/.fuckjira/
├── config.yaml           # API credentials, project mappings
├── raw/
│   ├── jira/
│   │   ├── PROJECT-123.json
│   │   └── attachments/
│   │       └── PROJECT-123/
│   │           └── screenshot.png
│   └── jpd/
│       └── ...
├── filtered/             # After interactive triage
├── enriched/             # After repo investigation
└── posted/               # After GitHub creation (with GH issue URLs)
```

### Commands
```bash
fuckjira ingest --source jira --project PROJ    # Single project
fuckjira ingest --source jira --all             # All accessible projects
fuckjira ingest --source jpd --project PROD     # JPD project
fuckjira ingest --status                        # Show ingestion stats
```

## Phase 2: Interactive Filtering

Given ~300 issues with significant rot, interactive triage is essential.

### Triage Workflow
```bash
fuckjira filter --start                         # Begin interactive session
```

For each issue, display:
- Summary + description (truncated)
- Age (created, last updated)
- Status, assignee
- Comment count, attachment count

Prompt for action:
- `[k]eep` - migrate to GitHub
- `[s]kip` - mark as stale/won't migrate
- `[m]erge` - flag as duplicate of another issue
- `[d]efer` - revisit later
- `[v]iew` - show full details + comments
- `[o]pen` - open in browser
- `[q]uit` - save progress and exit

### Bulk Operations
```bash
fuckjira filter --by-status "Won't Do"          # Auto-skip closed-as-wontdo
fuckjira filter --by-age 365                    # Flag issues >1 year untouched
fuckjira filter --by-assignee departed@co.com   # Flag ex-employee issues
fuckjira filter --resume                        # Continue where you left off
```

### Output
- `filtered/keep/*.json` - issues to migrate
- `filtered/skip/*.json` - archived issues (with skip reason)
- `filtered/merge/*.json` - duplicates (with merge target)

## Phase 3a: Issue Cleanup & Classification

Before enrichment, clean up and properly classify the kept issues. Jira's taxonomy doesn't map 1:1 to GitHub — fix that first.

### 3a.1: Label Grouping

Scan all kept issues and recommend labels for grouping them.

- Analyze existing Jira labels, components, and summaries across all kept issues
- Propose a clean set of GitHub labels (consolidating Jira's inconsistent labeling)
- Generate mass-edit recommendations: which issues get which labels
- Interactive review: approve/reject/edit label assignments in bulk

```bash
fuckjira classify labels                        # Analyze and propose label scheme
fuckjira classify labels --apply                # Apply approved label assignments
fuckjira classify labels --dry-run              # Preview assignments
```

### 3a.2: Type Reclassification

Correctly categorize issues as `task`, `bug`, or `idea`:

- Anything with a `DISC` prefix is an **idea** (came from JPD)
- Some F0 tasks are really ideas too — flag these for review
- Bugs stay as bugs
- Interactive review for ambiguous cases

```bash
fuckjira classify types                         # Analyze and propose type changes
fuckjira classify types --apply                 # Apply approved reclassifications
```

### 3a.3: Epic -> Idea Conversion & Duplicate Merging

Epics in Jira were just used to group tasks and relate them to JPD ideas. Convert them to ideas and merge duplicates.

- Identify all epics and their children
- Convert standalone epics to ideas (they stay in the migration set as ideas)
- Detect duplicate epics that match existing DISC ideas (word-overlap similarity)
- Merge duplicates: skip the epic, re-parent its children to the matching DISC idea
- Children get an `epic_parent` link to their parent idea

```bash
fuckjira classify epics                         # Show epics, detect duplicates with DISC
fuckjira classify epics --apply                 # Convert to ideas, merge duplicates
```

### Storage

Classification results stored alongside filtered data:
```
~/.fuckjira/
├── classified/
│   ├── labels.json          # Proposed label scheme + assignments
│   ├── types.json           # Reclassification decisions
│   └── epics.json           # Epic dissolution mapping
```

## Phase 3b: Enrichment (Claude Agent + Local Repos)

A Claude agent processes issues off a queue, scanning **local** repositories to find related code. The agent retains context across issues — by issue #50 it already knows the codebase layout and can find things faster.

### Architecture
```
┌──────────────┐     ┌─────────────────┐     ┌──────────────┐
│  fuckjira    │────>│  claude agent    │────>│  local repos │
│  (queue mgr) │<────│  (retained ctx) │     │  (Glob/Grep/ │
│              │     │                 │     │   Read)       │
└──────────────┘     └─────────────────┘     └──────────────┘
      │                     │
  issue queue          session persists
  (kept issues)        across issues
```

### Configuration
Repo paths live in `~/.fuckjira/config.toml`:
```toml
[enrich]
repos = [
  "~/code/go/src/github.com/fraud-zero/keystone-api/",
]
model = "sonnet"
max_turns = 10
```

### How It Works
1. `fuckjira enrich` starts a Claude session with system context (repo paths)
2. Feeds un-enriched kept issues one at a time via `claude --resume`
3. Agent searches repos using Glob/Grep/Read tools, returns structured JSON
4. `fuckjira` saves enrichment to the issue and moves to the next
5. Session ID is persisted — re-running continues the same agent context

### Commands
```bash
fuckjira enrich                    # Process all un-enriched kept issues
fuckjira enrich --issue F0-123     # Single issue
fuckjira enrich --limit 10         # Process 10 issues
fuckjira enrich --status           # Show enrichment progress
fuckjira enrich --reset            # Re-enrich already enriched issues
```

### Output Per Issue
```json
{
  "enrichment": {
    "summary": "Found billing calc in internal/billing/invoice.go. Rounding error likely from float64 arithmetic on line 142. Test coverage exists but misses edge cases.",
    "related_files": ["internal/billing/invoice.go", "internal/billing/invoice_test.go"],
    "confidence": "high"
  }
}
```

## Phase 4: Posting to GitHub

Create issues in GitHub Projects with full context.

### Issue Mapping
| Jira Field | GitHub Field |
|------------|--------------|
| Summary | Title |
| Description | Body (markdown converted) |
| Type | Label (`bug`, `feature`, `task`) |
| Priority | Label (`p0`, `p1`, `p2`, `p3`) |
| Components | Labels |
| Attachments | Uploaded to issue body as images/links |
| Comments | Appended to body or first comment |
| Links | Mentioned in body |
| Enrichment | Prepended section in body |

### Issue Body Template
```markdown
> **Migrated from Jira:** [PROJECT-123](https://jira.example.com/browse/PROJECT-123)
> **Original Reporter:** @person | **Created:** 2023-01-15

## Enrichment Notes
- **Confidence:** partial
- **Related Code:** `src/billing/invoice.go:142`
- **Related PRs:** #456

---

## Description
[Original description here]

---

<details>
<summary>Original Comments (3)</summary>

**@alice** (2023-01-16):
> Comment text here

</details>
```

### Commands
```bash
fuckjira post --dry-run                         # Preview what would be created
fuckjira post --project engineering             # Post to engineering project
fuckjira post --project product                 # Post to product project
fuckjira post --issue PROJECT-123               # Single issue
fuckjira post --batch 20                        # Post 20 issues
fuckjira post --status                          # Show post progress
```

### Idempotency
- Track posted issues in `posted/` with GitHub URLs
- Skip already-posted issues
- Support `--force` to re-post (creates new issue, updates tracking)

## Configuration

```yaml
# ~/.fuckjira/config.yaml
jira:
  url: https://company.atlassian.net
  email: user@company.com
  token: ${JIRA_API_TOKEN}  # env var reference
  projects:
    - PROJ
    - BACKEND

jpd:
  url: https://company.atlassian.net/jira/polaris
  # Uses same auth as Jira

github:
  token: ${GITHUB_TOKEN}
  org: fraud-zero
  projects:
    engineering:
      id: 123
      repos:
        - main-app
        - api-service
    product:
      id: 456
      repos: []

mappings:
  # Component -> Repository
  components:
    billing: main-app
    auth: api-service
  # Jira status -> Skip reason (auto-filter)
  auto_skip_statuses:
    - "Won't Do"
    - "Duplicate"
  # Label transformations
  labels:
    "tech-debt": "technical-debt"
    "P0": "p0-critical"
```

## Implementation Order

### Milestone 1: Ingestion (MVP)
1. [ ] CLI skeleton with cobra
2. [ ] Config loading (viper)
3. [ ] `acli` wrapper (execute + parse JSON output)
4. [ ] Local storage layer (read/write JSON to ~/.fuckjira)
5. [ ] `ingest` command - shell out to acli for issues, comments, attachments

### Milestone 2: Filtering
1. [ ] Interactive TUI (bubbletea or simple stdin)
2. [ ] Bulk filter operations (by status, age, assignee)
3. [ ] Progress persistence (resume where left off)
4. [ ] `filter` command

### Milestone 3a: Issue Cleanup & Classification
1. [ ] `classify labels` — scan kept issues, propose GitHub label scheme, bulk assign
2. [ ] `classify types` — reclassify as task/bug/idea (DISC = idea, flag ambiguous tasks)
3. [ ] `classify epics` — convert epics to ideas, merge duplicates with DISC

### Milestone 3b: Enrichment (Claude Agent)
1. [ ] `config.toml` with repo paths
2. [ ] Claude CLI wrapper (`internal/claude/client.go`)
3. [ ] Agent queue loop with session persistence
4. [ ] `enrich` command

### Milestone 4: Posting
1. [ ] Issue body templating (markdown conversion)
2. [ ] `gh issue create` + `gh project item-add` wrapper
3. [ ] Idempotency tracking (skip already-posted)
4. [ ] `post` command

### Milestone 5: Polish
1. [ ] JPD-specific handling (may need direct API if acli can't reach it)
2. [ ] Better error handling/retry
3. [ ] Progress bars
4. [ ] Summary reports

## Open Questions

1. **Attachment hosting**: GitHub has size limits. Host large attachments elsewhere?
2. **Comment preservation**: All as one comment, or try to preserve thread structure?
3. **User mapping**: Map Jira users to GitHub users? Or just mention by name?
4. **Project field mapping**: GitHub Projects have custom fields - map Jira fields?
5. **Two-way sync**: Is this one-time migration or ongoing sync needed?
6. **Rate limits**: Jira and GitHub both have limits. Batch sizes? Delays?
7. **JPD Access**: ✅ RESOLVED - JPD (DISC project) is fully accessible via standard JQL.
   - `acli jira workitem search --jql='project = "DISC"'` works
   - Custom fields (scoring, categorization) are included in `--fields '*all'`

## Tech Stack

- **Language**: Go
- **CLI**: cobra
- **Config**: viper
- **TUI**: bubbletea (or simplify with survey)
- **API Layer**: Shell out to `acli` and `gh` (already installed, auth handled)
- **Storage**: JSON files (simple, inspectable)

### Leveraging Existing CLIs

Instead of building Jira/GitHub API clients, we shell out to `acli` and `gh`:

**Jira Ingestion (acli)**
```bash
# List all issues in a project
acli jira workitem search --jql "project = PROJ" --json --paginate

# Get full issue details
acli jira workitem view KEY-123 --fields '*all' --json

# Get comments
acli jira workitem comment list KEY-123 --json

# List attachments
acli jira workitem attachment list KEY-123 --json
```

**GitHub Posting (gh)**
```bash
# Create issue in a repo
gh issue create --repo owner/repo --title "..." --body "..." --label bug

# Add to project
gh project item-add PROJECT_NUM --owner ORG --url ISSUE_URL

# Or create draft directly in project
gh project item-create PROJECT_NUM --owner ORG --title "..." --body "..."
```

This means:
- No API token management in our code
- Auth already configured by user
- Less code to maintain
- Can test commands manually first

## Usage Flow (Happy Path)

```bash
# 1. Setup
fuckjira init                                   # Create config template
vim ~/.fuckjira/config.yaml                     # Fill in credentials

# 2. Ingest everything
fuckjira ingest --source jira --all
fuckjira ingest --source jpd --all
fuckjira ingest --status
# "Ingested 347 issues (312 Jira, 35 JPD)"

# 3. Bulk filter obvious cases
fuckjira filter --by-status "Won't Do"          # Auto-skip 42 issues
fuckjira filter --by-age 730                    # Flag 67 issues >2 years old

# 4. Interactive triage
fuckjira filter --start
# ... work through remaining ~240 issues ...
# "Kept: 180, Skipped: 52, Deferred: 8"

# 5. Classify
fuckjira classify labels                        # Propose label scheme
fuckjira classify labels --apply                # Apply labels
fuckjira classify types                         # Reclassify task/bug/idea
fuckjira classify types --apply                 # Apply types
fuckjira classify epics --apply                 # Convert to ideas, merge duplicates
# "Classified 180 issues: 95 tasks, 40 bugs, 45 ideas. Dissolved 12 epics."

# 6. Enrich (Claude agent scans local repos)
vim ~/.fuckjira/config.toml                     # Add repo paths
fuckjira enrich
# Agent processes issues with retained context, scanning repos
# "Enriched 180 issues: 67 high, 45 medium, 38 low, 30 none"

# 7. Post
fuckjira post --dry-run --project engineering
fuckjira post --project engineering
fuckjira post --project product
# "Posted 156 to engineering, 24 to product"

# 8. Verify
fuckjira post --status
# Shows mapping of Jira -> GitHub issues
```
