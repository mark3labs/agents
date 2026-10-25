# github-triage

A [BONNIE](https://go-bonnie.dev) agent that keeps a GitHub repository's pull
requests triaged. It polls the repo on a schedule. It does not use webhooks or
the GitHub channel.

On every PR it:

- **Labels** the type and status.
- **Closes promotional PRs** (spam, ads, link drops) with a polite comment.
- **Closes PRs with no linked issue** (optional) with a comment explaining why.
- **Tracks who it's waiting on.** A PR waits on its author when it has merge
  conflicts, failing CI, or a maintainer or review-bot request that hasn't been
  answered; the author gets one nudge after a week. Otherwise it waits on the
  maintainers, or is **ready for review** once CI is green and nothing is open.
- **Approves CI for outside contributors** when the change looks safe.
- **Flags likely duplicates** of earlier open or recently merged PRs for a
  maintainer to confirm.

## How it works

```
every 10 min ──► poll: list open PRs, compare with saved state
                   │
                   │  only PRs that are new, have new human activity,
                   │  new CI results or merge conflicts, or a nudge due
                   ▼
                 one agent run per PR
                   1. inspect_pr   fetch the PR, ask the decision model,
                                   apply the rules → a plan
                   2. set_labels   ┐
                   3. close_pr     │ carry out the plan
                   4. approve_ci   │ (each tool refuses anything
                   5. nudge_author │  the plan doesn't allow)
                   6. flag_duplicate ┘
                   7. finish_triage  save "triaged" so the next poll skips it
```

The rules are enforced in code, not left to the chat model. The model can't
close a PR, approve CI or nudge anyone unless the plan says to. Its job is
picking the type and area labels and writing the human-readable messages.

**Decisions.** The yes/no and multiple-choice judgments use BONNIE's
[decision API](https://go-bonnie.dev/guides/decisions), which is cheap, fast
and journalled:

| Question | Used for |
| --- | --- |
| Is this PR promotional? | Close at P ≥ 0.90; label "triage: possible spam" at P ≥ 0.60 |
| What type of change is it? | The type label (`type: bug`, `type: enhancement`, …) |
| Do the open maintainer and review-bot messages wait on the author? | "status: needs submitter response" |
| Could running this in CI be harmful? Is it obfuscated? | Approve CI only if both are below 0.10 |
| Does this PR make the same change as earlier PR #N? | "status: duplicate" at P ≥ 0.80 |

Merge conflicts and CI results come straight from GitHub, with no model
involved.

**State.** A small SQLite file (`.bonnie/triage.db`) remembers what each PR
looked like when it was last triaged: its commit, its comments and reviews, its
CI result and whether it merges cleanly. The bot's own labels and comments don't
count as changes, so it never re-triages itself.

## Quick start

You need Go and the [BONNIE CLI](https://go-bonnie.dev/installation), a GitHub
token for the repository (see [Token](#token)), and an
[OpenRouter](https://openrouter.ai) API key for the default models (or change
the models, see [Configuration](#configuration)).

```sh
git clone https://github.com/mark3labs/agents.git
cd agents/github-triage
```

1. **Set the repo** in `triage.yaml`:

   ```yaml
   repo: your-org/your-repo
   ```

2. **Set credentials:** copy `.env.example` to `.env` and fill in the two
   required values. It also lists the optional overrides.

   ```sh
   cp .env.example .env
   # GITHUB_TOKEN=...        see "Token" below
   # OPENROUTER_API_KEY=...  chat model and decision model
   ```

3. **Build and run:**

   ```sh
   bonnie build
   ./github-triage
   ```

4. **Check what it did** after the first poll:

   ```sh
   ./github-triage runs                       # one run per triaged PR
   ./github-triage runs show <run-id>         # what one run did, step by step
   ```

   `runs` reads the journal directly, so it works whether or not the agent is
   running.

It starts in **dry-run mode**: it works out everything but writes nothing to
GitHub. Check a few results, then set `dry_run: false` (or
`TRIAGE_DRY_RUN=false`). PRs triaged in dry-run mode are triaged again for
real after you switch.

## Token

Use a fine-grained token, ideally from a dedicated bot account, with access to
the one repository:

| Permission | Why |
| --- | --- |
| Pull requests: write | Read PRs, close them |
| Issues: write | Labels and comments |
| Actions: write | Approve workflow runs |
| Metadata: read | Required |

Maintainers' PRs are never auto-closed. If the token is your personal account,
your own PRs are exempt too. With a GitHub App token, set `GITHUB_BOT_LOGIN`
to the app's login.

## Configuration

Everything lives in [`triage.yaml`](triage.yaml), and each setting is
commented. The ones you're most likely to change:

| Setting | Default | What it does |
| --- | --- | --- |
| `model` | `openrouter/deepseek/deepseek-v4.1-flash-1017` | Chat model that carries out the plan and writes comments |
| `decision_model` | `openrouter/typesafe/jev-1.14` | Decision model for the typed questions |
| `poll_cron` | `*/10 * * * *` | How often to poll (UTC) |
| `dry_run` | `true` | Write nothing to GitHub |
| `linked_issue.enabled` | `true` | Close PRs without a linked issue |
| `promotional.close_threshold` | `0.90` | How sure before closing as spam |
| `awaiting_author.nudge_after_days` | `7` | When to nudge; `0` turns nudges off |
| `ci.auto_approve` | `true` | Approve safe CI runs automatically |
| `ci.never_approve_paths` | `.github/**` | Changes here always need a human |
| `review_bots` | none | Bots whose review threads count, such as `coderabbitai[bot]` |
| `duplicates.enabled` | `true` | Flag likely duplicate PRs |
| `duplicates.comment` | a short note | Posted on a flagged PR; empty means label only |
| `exempt_labels` | `triage: keep open` | Add this label to a PR to stop auto-close |
| `labels` | — | The full label set; add your own area labels here |

Environment overrides: `GITHUB_REPO`, `TRIAGE_DRY_RUN`, `TRIAGE_DB` and
`TRIAGE_CONFIG` (the config file path).

### Default labels

The defaults use a `type: …`, `area: …`, `status: …`, `triage: …` naming
scheme. Labels that already exist in the repo are used as they are; missing
ones are created on the first run that isn't a dry run. There are no area
labels by default: add your project's areas in the `area` group, with a
description that says which code each covers, since that's what the agent
goes by.

### Renaming labels

No label name is hard-coded. The labels the agent applies itself are found by
their `role`, so you can rename one, or reuse a label your repo already has,
by editing its entry:

```yaml
labels:
  - {name: "waiting on author", role: awaiting-author, group: status, color: fbca04}
  - {name: "needs review",      role: awaiting-review, group: status, color: 0e8a16}
  - {name: spam?,               role: possible-spam,   group: triage, color: e99695}
```

Each of these roles must be on exactly one label: `awaiting-author`, `awaiting-review`,
`possible-spam`, `ci-needs-review`, `closed-promotional`, `closed-no-issue`.
The agent won't start if one is missing or listed twice, and the error names
the role. Two more roles are optional:

- `ready-for-review`: without it, PRs that are ready get the awaiting-review
  label instead.
- `duplicate`: required while `duplicates.enabled` is true. The agent only
  ever adds this label, so one a maintainer put on stays.

Type and area labels have no role; the `type` and `area` groups mark them.

Two things to watch when adapting the label set to an existing repo:

- **Quote names that contain `": "`**, for example
  `{name: "status: needs submitter response", role: awaiting-author, group: status}`.
  Unquoted, YAML can't read it.
- **Leave out labels you manage by hand** (such as "wontfix"), or put them in a
  group like `manual`. The agent removes labels in the `status` and `triage`
  groups when they no longer apply.

## Rules in detail

**Who is exempt from closing:** bots (such as dependabot), maintainers
(OWNER/MEMBER/COLLABORATOR or anyone listed in `maintainers`), anyone in
`exempt_authors`, and PRs carrying an `exempt_labels` label. Exempt PRs are
still labelled.

**What counts as a linked issue:** an issue GitHub links to the PR ("Fixes
#12" or the sidebar). With `accept_mentions: true`, a plain `#12`, `org/repo#12`
or an issue URL in the description also counts. Draft PRs aren't checked until
they're marked ready.

**Status** is set on every open PR that isn't a draft:

| Status | When |
| --- | --- |
| awaiting-author | Merge conflicts, failing CI, "changes requested", or open messages that the decision model judges are waiting on the author |
| awaiting-review | The author answered after the last maintainer message, or CI hasn't finished (or is waiting for approval) |
| ready-for-review | CI green, merges cleanly, nothing open |

"Open messages" are maintainer comments, reviews and review threads newer than
the author's last commit or comment. They also include unresolved threads from
a `review_bots` account that the author hasn't replied to and that a newer
commit hasn't made outdated. Labels you change by hand stay until the PR
changes again.

**Duplicates.** Each PR is compared with older open PRs and with PRs merged in
the last `merged_lookback_days` days. The ones with the most overlap (shared
issue, changed files, title words), up to `max_candidates`, go to the decision
model. A likely duplicate is labelled and, unless `comment` is empty, gets one
comment naming the earlier PRs. It is never closed.

**CI approval** applies to workflow runs that GitHub holds for a maintainer to
approve, which happens with outside contributors' fork PRs. A run is approved
only when:

- the PR is not being closed,
- no file matches `ci.never_approve_paths`,
- the whole diff fits in `ci.max_diff_bytes`, so the model saw all of it,
- P(harmful) and P(obfuscated) are both below `ci.max_risk`.

Anything else gets "triage: ci needs review" for a human.

## HTTP API

The agent serves no HTTP API by default: it only polls GitHub, so nothing
needs to reach it, and no port is opened. The schedule clock still runs, and
`runs` still works, because it reads the journal directly.

`bonnie dev`, `chat` and `schedules` (including triggering a poll by hand) need
the API. For local work, turn it on with `TRIAGE_HTTP_API=true`:

```sh
TRIAGE_HTTP_API=true ./github-triage --addr 127.0.0.1:8080
./github-triage schedules trigger poll-prs   # poll now; loopback callers only
./github-triage schedules history poll-prs
```

## Run it as a service

[`deploy/github-triage.service`](deploy/github-triage.service) is a sample
systemd unit. It runs the agent as its own user, restarts it on failure, and
stops it cleanly: on SIGTERM BONNIE drains the runs in progress, and anything
cut short resumes from the journal on the next start.

1. **Install the agent** in its own directory, owned by a dedicated user:

   ```sh
   sudo useradd --system --home /opt/github-triage --shell /usr/sbin/nologin triage
   sudo mkdir -p /opt/github-triage
   sudo cp github-triage triage.yaml .env /opt/github-triage/
   sudo chown -R triage:triage /opt/github-triage
   sudo chmod 600 /opt/github-triage/.env
   ```

2. **Install and start the unit:**

   ```sh
   sudo cp deploy/github-triage.service /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now github-triage
   ```

3. **Check on it:**

   ```sh
   systemctl status github-triage
   journalctl -u github-triage -f                       # agent log
   sudo -u triage /opt/github-triage/github-triage runs # triage runs
   ```

   Run `runs` from inside `/opt/github-triage`, or pass `--journal
   /opt/github-triage/.bonnie`, so it finds the journal.

**Updating:** copy the new binary over the old one, then
`sudo systemctl restart github-triage`. Bump `Revision` in `main.go` first if
the rules changed (see Development).

**A different directory or user:** change `User`, `Group`, `WorkingDirectory`,
`ExecStart` and `ReadWritePaths` in the unit. If the directory is under
`/home`, also change `ProtectHome=yes` to `ProtectHome=read-only`, or the
service can't see its own files.

**Without root:** run it as a user service instead. Copy the unit to
`~/.config/systemd/user/`, delete the `User=` and `Group=` lines, set the
paths to your directory, change `WantedBy=` to `default.target`, then:

```sh
systemctl --user daemon-reload
systemctl --user enable --now github-triage
sudo loginctl enable-linger "$USER"   # keep it running after you log out
```

## Project layout

```
main.go               schedule, models, completion check
instructions.md       the agent's system prompt
triage.yaml           configuration and label set
deploy/               sample systemd unit
internal/triage/      the rules, GitHub client, state store
tools/<name>/         the seven tools the agent calls (thin wrappers)
```

## Development

```sh
go test ./...                    # rule tests, using a fake decision model
TRIAGE_HTTP_API=true bonnie dev                   # hot-reload with a terminal UI
TRIAGE_HTTP_API=true bonnie dev --schedule-clock  # same, with polling on
```

`bonnie dev` talks to the agent over its HTTP API, so it needs
`TRIAGE_HTTP_API=true`.

When you change the rules or the instructions in a meaningful way, bump
`Revision` in `main.go`. BONNIE then won't finish an old poll under the new
rules.

## Limits worth knowing

- One repository per agent. Run a second copy for a second repository.
- Each poll starts at most `max_prs_per_poll` (default 10) runs, so a large
  backlog drains over several polls.
- CI results and merge conflicts don't change a PR's "last updated" time, so
  every poll reads them for every open PR: about three API calls per open PR.
  With 100 open PRs and a 10-minute poll, that's roughly 1,800 of the token's
  5,000 calls per hour.
- With the API off (the default) there's no way to trigger a poll by hand;
  the next one runs on the cron. See [HTTP API](#http-api).
- Questions to a human are switched off. These are background runs, and a run
  waiting on an answer would block every later poll.
- Comments and closes are safe to retry: before posting, it checks for its own
  hidden marker comment. GitHub delivery is still at least once, not exactly
  once.
