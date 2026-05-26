# Plan: `screwjira post` — Upload Issues to GitHub

## Goal

Upload all accepted-enrichment kept issues to a single GitHub repo as GitHub Issues,
optionally adding them to a GitHub Project v2 board. Track which issues have been posted
to enable idempotent re-runs.

---

## Config additions (`internal/config/config.go`)

Add a `[post]` section and a `[user_map]` table:

```toml
[post]
# Target GitHub repo (owner/name)
repo = "fraud-zero/keystone"

# Optional: GitHub Project v2 number to add issues to
# project_number = 12
# project_owner  = "fraud-zero"

# Optional: label added to every migrated issue
# migration_label = "from-jira"

[user_map]
# Maps Jira display name (lowercase) to GitHub username
# "stefan koshiw" = "stefan-gh"
# "jane doe"      = "janedoe"
```

New structs:

```go
type PostConfig struct {
    Repo           string `toml:"repo"`            // "owner/repo"
    ProjectNumber  int    `toml:"project_number"`  // 0 = skip
    ProjectOwner   string `toml:"project_owner"`   // required if project_number set
    MigrationLabel string `toml:"migration_label"` // e.g. "from-jira"
}

type Config struct {
    Enrich  EnrichConfig      `toml:"enrich"`
    Post    PostConfig        `toml:"post"`
    UserMap map[string]string `toml:"user_map"` // jira name (lowercase) -> github login
}
```

`Load()` normalises `UserMap` keys to lowercase so lookups are case-insensitive.
`Load()` validates that if `ProjectNumber > 0` then `ProjectOwner != ""`, returning a clear error.

---

## Storage additions (`internal/storage/storage.go`)

Add post state to `StoredIssue`:

```go
type PostedState struct {
    IssueNumber int    `json:"issue_number"`
    IssueURL    string `json:"issue_url"`
    PostedAt    string `json:"posted_at"` // RFC3339
}

type StoredIssue struct {
    // ... existing fields ...
    Posted *PostedState `json:"posted,omitempty"`
}
```

No new storage methods needed. In `cmd/post.go`, call `store.GetKeptIssues()` and filter
inline — same pattern as `enrich.go`. Existing JSON files unmarshal cleanly with `Posted == nil`.

---

## Issue type mapping

GitHub issue types (public preview Jan 2025, REST API support Mar 2025) are set natively
rather than via labels. GitHub's three default org-level types map exactly to our classification:

| Classification.Type   | GitHub issue type |
|-----------------------|-------------------|
| `feature`             | `Feature`         |
| `idea`                | `Feature`         |
| `epic`                | `Feature`         |
| `bug`                 | `Bug`             |
| `task`                | `Task`            |
| (empty/unknown)       | `Task`            |

Issue types are org-level — the default `Bug`, `Feature`, `Task` types are assumed to exist.
If a type name doesn't match (e.g. org renamed them), the create call will fail with a clear
error from the API. No bootstrap step needed for types.

Type labels (`bug`, `task`, `feature`) are **not** added — the native issue type carries this
information. Only enrichment labels and `migration_label` are applied.

Labels are bootstrapped at the start of every `--apply` run only for `migration_label`
(if configured) via `gh label create --force`.

---

## Content selection logic

For each issue, content is selected in priority order:

| Field       | Accepted enrichment                | No enrichment / not accepted        |
|-------------|-------------------------------------|--------------------------------------|
| Title       | `Enrichment.ProposedTitle`          | `Issue.Summary()`                    |
| Body        | `Enrichment.ProposedDescription`    | `Issue.Description()`                |
| Type        | `Classification.Type` (native)      | `Classification.Type` (native)       |
| Labels      | `Enrichment.ProposedLabels`         | `Classification.Labels`              |
| Assignee    | `Issue.Assignee()` via user map     | `Issue.Assignee()` via user map      |

Body footer always appended:

```
---
*Migrated from Jira: [KEY](https://fraud0.atlassian.net/browse/KEY) · Created: 2023-04-12 · Reporter: Stefan Koshiw*
```

`Issue.Created()` returns an RFC3339 string — format as `2006-01-02` for readability.
`Issue.Reporter()` returns the Jira display name — use as-is (not mapped to GitHub username).

If `Classification.EpicParent` is set, also append:

```
*Parent: PARENT-KEY*
```

GitHub does not support setting `created_at` via the API — all issues will be timestamped
at migration time. The Jira creation date is preserved only in the footer.

---

## `cmd/post.go` — Command design

```
screwjira post [flags]
```

Flags:

| Flag                | Default | Description                                              |
|---------------------|---------|----------------------------------------------------------|
| `--apply`           | false   | Actually create issues (default: dry run)                |
| `--issue KEY`       | ""      | Post a single issue (bypasses postable filter)           |
| `--jira-project STR`| ""      | Filter by Jira project key (e.g. F0)                     |
| `--limit N`         | 0       | Max issues to post in this run                           |
| `--all`             | false   | Include kept issues with pending/rejected/deferred enrichments (uses raw Jira content) |
| `--force`           | false   | Re-post already-posted issues (clears Posted state first)|
| `--status`          | false   | Show post progress summary                               |

### Postable filter logic

Default (without `--all`): kept issues where `Posted == nil` AND one of:
- `Enrichment == nil` (never enriched — use raw content)
- `Enrichment.Decision == EnrichAccepted`

Explicitly excluded by default:
- `Enrichment.Decision == EnrichPending` (agent ran, not yet reviewed)
- `Enrichment.Decision == EnrichRejected` (rejected, needs re-enrichment)
- `Enrichment.Decision == EnrichDeferred` (deferred for later review)

With `--all`: include all of the above using raw Jira content regardless of enrichment state.

With `--force`: clear `Posted` on matched issues before filtering, allowing re-post.

### Precondition checks (before any work)

1. `cfg.Post.Repo != ""` — fail with clear message if not configured
2. `gh auth status` succeeds — fail early rather than on first create
3. If `ProjectNumber > 0`: `ProjectOwner != ""` — fail if missing

### Apply mode: per-issue flow

For each issue:

1. Build title, body, type, labels, assignee (content selection logic above)
2. Bootstrap `migration_label` if configured: `gh label create --force --repo REPO --name "from-jira"` (runs once per `--apply` invocation, not per issue)
3. Create issue via REST API (`gh issue create` does not support `--type` yet):
   ```bash
   gh api POST /repos/{owner}/{repo}/issues \
     --field title="..." \
     --field body="..." \
     --field type="Feature" \
     --field labels[]="backend" \
     --field labels[]="from-jira" \
     --field assignees[]="github-user"
   ```
   Response is JSON — parse `html_url` and `number` directly. No stdout URL scraping needed.
4. Save `PostedState{IssueNumber: 42, IssueURL: "...", PostedAt: now}` via `store.SaveStoredIssue(issue)` immediately (atomic write, no partial state).
5. If `ProjectNumber > 0`: immediately run:
   ```bash
   gh project item-add PROJECT_NUMBER --owner PROJECT_OWNER --url ISSUE_URL
   ```
   Then set the project status field via GraphQL (see Status mapping section).
   Log failures without aborting the run.
6. On API failure: print warning, continue to next issue.

Doing the project board add immediately after each create (not in a post-loop phase)
avoids the partial-state problem where some issues are created but not board-added.

### Dry-run output (default)

```
[1/42] F0-123  feature  Add retry logic for webhook delivery
       Title:   Add webhook retry logic with exponential backoff  [enriched]
       Labels:  feature, backend, reliability
       Assignee: stefan-gh

[2/42] DISC-7  feature  Keystone rate limiting
       Title:   Keystone API rate limiting  [enriched]
       Labels:  feature, api
       Assignee: (none)

42 issues would be created in fraud-zero/keystone.
Run with --apply to create them.
```

### Apply output

```
[1/42] F0-123 → https://github.com/fraud-zero/keystone/issues/1
[2/42] DISC-7 → https://github.com/fraud-zero/keystone/issues/2
...
Done. 42 created, 0 failed.
```

---

## Screenshots & attachments

### Problem

The current `extractTextFromADF` in `internal/jira/client.go` silently drops `media` and
`mediaSingle` ADF nodes — inline images in issue descriptions are already being lost during
ingest. Jira also stores standalone file attachments in `fields.attachment` (not yet exposed).

### Jira side: two changes to `internal/jira/client.go`

**1. Add `Attachments()` method**

Jira returns attachment metadata in `fields.attachment`:
```json
[{
  "id": "12345",
  "filename": "screenshot.png",
  "mimeType": "image/png",
  "content": "https://fraud0.atlassian.net/rest/api/2/attachment/content/12345",
  "size": 98304
}]
```

```go
type Attachment struct {
    ID       string
    Filename string
    MimeType string
    Content  string // download URL
}

func (i *Issue) Attachments() []Attachment
```

Only image MIME types are relevant for migration (`image/png`, `image/jpeg`, `image/gif`,
`image/webp`). Non-image attachments are noted in the body footer but not uploaded.

**2. Extend `walkADF` to handle `media` / `mediaSingle` nodes**

Currently these are silently skipped. During post, emit a placeholder that maps to an
attachment ID:

```go
case "media":
    if attrs, ok := node["attrs"].(map[string]interface{}); ok {
        if id, ok := attrs["id"].(string); ok {
            sb.WriteString(fmt.Sprintf("[[JIRA_MEDIA:%s]]", id))
        }
    }
```

The placeholder is later replaced with the GitHub-hosted URL after upload.

Note: ADF media `id` is the attachment ID (matches `Attachment.ID` from `fields.attachment`).

### Downloading from Jira

Attachment URLs require auth. `acli` manages Jira credentials but has no download-attachment
command. We need direct HTTP requests.

Require two env vars (same credentials used by `acli`):
- `ATLASSIAN_EMAIL` — Atlassian account email
- `ATLASSIAN_TOKEN` — Atlassian API token

Download via Go `net/http`:
```go
req, _ := http.NewRequest("GET", attachment.Content, nil)
req.SetBasicAuth(os.Getenv("ATLASSIAN_EMAIL"), os.Getenv("ATLASSIAN_TOKEN"))
resp, err := http.DefaultClient.Do(req)
```

If env vars are missing, skip attachment upload and note it in the dry-run output.

### Uploading to GitHub

Use `gh api` to upload each image as a file in the target repo under a dedicated path:

```bash
gh api repos/{owner}/{repo}/contents/.jira-attachments/{KEY}/{filename} \
  --method PUT \
  --field message="jira-migration: attachments for {KEY}" \
  --field content=@<(base64 -w0 /tmp/attachment.png)
```

The resulting raw URL is:
```
https://raw.githubusercontent.com/{owner}/{repo}/main/.jira-attachments/{KEY}/{filename}
```

This path (`.jira-attachments/`) is committed to the default branch. It is gitignored-by-convention
for review purposes but fully accessible to GitHub rendering.

**Idempotency**: before uploading, check if the file already exists via
`gh api repos/{owner}/{repo}/contents/.jira-attachments/{KEY}/{filename}`. If it returns
a SHA, skip the upload (file already there).

### Post flow with attachments

After building the issue body but before calling `gh issue create`:

1. Call `issue.Attachments()` — get image attachments
2. For each image attachment:
   a. Download from Jira (skip if `ATLASSIAN_*` env vars missing)
   b. Check GitHub for existing file (idempotency)
   c. Upload to GitHub repo via `gh api`
   d. Record `filename → raw GitHub URL` in a map
3. Replace `[[JIRA_MEDIA:{id}]]` placeholders in body with `![filename](raw_github_url)`
4. Append footer for non-image attachments: `📎 Attachments: file.pdf, data.csv`

If any attachment upload fails, log a warning but continue — do not block the issue create.

### Dry-run output addition

```
[1/42] F0-123  feature  Add retry logic ...
       Title:   ...
       Labels:  ...
       Attachments: screenshot.png, diagram.png  (2 images to upload)
```

---

## Status mapping

### Jira statuses present in the data

All terminal statuses (`Done`, `Won't Do`, `Released`, `Abandoned`) are already excluded
during ingest — they never reach the `post` command. The statuses that will appear are:

**F0**: `To Do`, `In Progress`, `In Review`, `Blocked`, `Open`
**DISC**: `Backlog`, `Discovery`, `Prioritized`, `In Progress`

### Mapping strategy

**GitHub Issue state**: always `open`. All migrated issues are active work.

**GitHub Project v2 status field**: if `project_number` is configured, set the "Status" field
on each project item after adding it to the board. This requires GraphQL.

Proposed mapping (configurable in `[post]` if needed):

| Jira status           | GitHub Project status |
|-----------------------|-----------------------|
| `To Do`, `Backlog`, `Open` | `Todo`           |
| `In Progress`, `Discovery`, `Prioritized` | `In Progress` |
| `In Review`           | `In Progress`         |
| `Blocked`             | `In Progress` + label `blocked` |
| (unknown)             | `Todo`                |

### GraphQL implementation for project status

After `gh project item-add`, set the status field value via `gh api graphql`:

**Step 1** (once per run): query the project's "Status" field ID and option IDs:
```graphql
query {
  organization(login: "fraud-zero") {
    projectV2(number: 12) {
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
}
```

Cache `projectId`, `statusFieldId`, and the `name → id` option map for the run.

**Step 2** (per issue): get the item node ID from the `gh project item-add` output
(pass `--format json` to capture it), then:
```graphql
mutation {
  updateProjectV2ItemFieldValue(input: {
    projectId: "PVT_xxx",
    itemId: "PVTI_xxx",
    fieldId: "PVTSSF_xxx",
    value: { singleSelectOptionId: "option_id" }
  }) { projectV2Item { id } }
}
```

**Fallback**: if the "Status" field doesn't exist in the project, or if GraphQL fails,
skip status setting and log a warning. Do not abort the run.

### Config addition for status mapping

Add optional overrides to `[post]` in case the project uses non-standard status names:

```toml
[post.status_map]
# Maps Jira status (lowercase) to GitHub Project status option name
# "blocked"     = "Blocked"
# "in review"   = "In Progress"
```

Default mapping is built-in; `status_map` only needed for overrides.

---

## Implementation order

1. **Config** — add `PostConfig` + `UserMap` + `StatusMap`, update `defaultConfig`, update `Load()` with validation
2. **Storage** — add `PostedState` to `StoredIssue` (no new methods needed)
3. **Jira client** — add `Attachments()` method, extend `walkADF` for `media`/`mediaSingle` nodes
4. **`cmd/post.go`** — dry-run + apply, inline filtering, label bootstrap, user mapping, attachment upload, project board add + status set per-issue

---

## Open questions / decisions

- **Unenriched issues**: included by default (use raw content). `--all` additionally includes pending/rejected/deferred enrichments with raw content.
- **Issue order**: post in Jira key order (F0 before DISC, ascending) so GitHub issue numbers are predictable.
- **Rate limits**: add 300ms sleep between `gh issue create` calls to avoid GitHub secondary rate limits on bulk operations.
- **Assignee failures**: if the mapped GitHub user doesn't exist or isn't a repo collaborator, `gh issue create` will fail. Recommendation: attempt with assignee; on non-zero exit, retry once without assignee and log the warning.
- **`.jira-attachments/` in git**: this is binary content in the main branch. Alternative: use a dedicated orphan branch `jira-attachments` to keep main clean. Tradeoff: orphan branch raw URLs use a different ref path. Decision needed before implementing.
