# screwjira

A CLI tool for migrating issues from Jira (and Jira Product Discovery) to GitHub Issues + Projects. It ingests your Jira backlog, lets you triage and clean it up, enriches descriptions using Claude, and posts the results to GitHub.

## Overview

```
ingest → filter → normalize → dedupe → enrich → post
```

Each step is independent and stores state locally in `~/.screwjira/`, so you can pause, review, and re-run any phase without losing work.

## Prerequisites

- [`acli`](https://bobswift.atlassian.net/wiki/spaces/ACLI/overview) — Atlassian CLI, used to fetch Jira issues
- [`gh`](https://cli.github.com/) — GitHub CLI, used to post issues
- An Anthropic API key in `ANTHROPIC_API_KEY` — used by the `enrich` and `gold` commands

## Installation

```sh
go install github.com/panamafrancis/screwjira@latest
```

Or clone and build:

```sh
git clone https://github.com/panamafrancis/screwjira
cd screwjira
go build -o screwjira .
```

## Configuration

screwjira looks for a config file at `~/.screwjira/config.hcl` (HCL) or `~/.screwjira/config.toml` (TOML).

```hcl
project = "MYPROJECT"      # Jira project key
jira_profile = "default"   # acli profile name

[github]
repo  = "myorg/myrepo"
owner = "myorg"

[user_map]
"jira.user@example.com" = "github-handle"

[repos]
my_service = "~/code/my-service"   # local repos for the index command
```

## Commands

### `ingest`

Fetch issues from Jira and store them locally.

```sh
screwjira ingest --project MYPROJECT
screwjira ingest --all              # all projects in config
screwjira ingest --sync             # refresh existing issues, mark stale enrichments
screwjira ingest --status "In Progress,Review"
```

### `filter`

Interactive triage. Work through issues one by one and decide what to migrate.

```sh
screwjira filter
```

Actions: `k` keep · `s` skip · `d` defer · `v` view full description · `o` open in browser · `q` quit

Bulk modes: `--by-status`, `--by-age`, `--by-assignee`

### `review`

Revisit previous triage decisions.

```sh
screwjira review
screwjira review --decision keep    # only re-examine kept issues
screwjira review --project MYPROJECT
```

### `normalize`

Standardise issue types to `bug`, `task`, or `feature` before enrichment.

```sh
screwjira normalize --dry-run
screwjira normalize --apply
```

### `dedupe`

Find and resolve duplicate issues using title/description similarity.

```sh
screwjira dedupe
screwjira dedupe --threshold 0.6    # default 0.5
```

Interactive actions per pair: keep one, skip, or merge (merges child into parent and queues re-enrichment).

### `labels`

Clean up Jira labels before migration.

```sh
screwjira labels          # interactive keep/rename/delete
screwjira labels dedupe   # find and merge near-duplicate labels
```

### `enrich`

Use Claude to improve issue titles and descriptions. The agent has access to your local codebase index and a project glossary for context.

```sh
screwjira enrich
screwjira enrich --issue MYPROJECT-123
screwjira enrich --limit 20
screwjira enrich --review            # review pending enrichments interactively
screwjira enrich --model claude-opus-4-7
screwjira enrich --confidence 7      # skip issues where agent confidence < 7
screwjira enrich --reset             # clear all accepted enrichments
screwjira enrich --upgrade           # re-enrich previously accepted issues
```

Enrichment results go through an accept/reject/defer workflow before being committed.

### `gold`

Flag enriched issues where the agent found a concrete finding (specific code path, root cause, or actionable change). These get an `agent-found-gold` label on post.

```sh
screwjira gold
screwjira gold --limit 50
```

### `index`

Build a codebase index and glossary from your local repos. Used by `enrich` for context.

```sh
screwjira index
```

Scans Go, TypeScript, Terraform, Dataform, and Proto files. Outputs `~/.screwjira/index.md` and `~/.screwjira/glossary.yaml`.

### `post`

Create GitHub issues from your kept and enriched Jira issues.

```sh
screwjira post --dry-run    # preview what would be created
screwjira post --apply      # create the issues
screwjira post --workers 4  # parallel posting
```

- Uses enriched title/description when available, raw Jira content otherwise
- Maps Jira status to GitHub Project fields
- Uploads inline attachments to GitHub
- Skips already-posted issues (tracked in local state)

## Workflow example

```sh
# 1. Pull issues from Jira
screwjira ingest --project MYPROJECT

# 2. Triage — decide what's worth migrating
screwjira filter

# 3. Clean up issue types and labels
screwjira normalize --apply
screwjira labels

# 4. Remove duplicates
screwjira dedupe

# 5. Build codebase context
screwjira index

# 6. Enrich descriptions with Claude
screwjira enrich
screwjira enrich --review

# 7. Flag high-value enrichments
screwjira gold

# 8. Preview, then post to GitHub
screwjira post --dry-run
screwjira post --apply
```

## State

All state lives in `~/.screwjira/`:

| Path | Contents |
|---|---|
| `config.hcl` | Configuration |
| `issues/` | Raw Jira issue JSON |
| `enrichments/` | Claude-generated enrichments |
| `index.md` | Codebase index |
| `glossary.yaml` | Project glossary |
| `posted.json` | Record of posted GitHub issues |

## License

MIT
