# fleet

Set up, run, and supervise a fleet of coding agents from different model vendors against one GitHub repository.

You write issues. Agents pick them up, work in their own git worktrees, run your gate, and open pull requests. An agent from a *different* vendor reviews each PR. You approve and merge. When an agent is unsure, it labels the issue and stops instead of guessing, and you get a Telegram message.

`fleet` is the operator's tool for that loop. It is a single Go binary that turns a `fleet.yaml` into a running, supervised fleet on a Linux or macOS box, and gives you short commands to pause, resume, kill and inspect it from an SSH session on your phone.

> **Status: pre-release (v0.1 in progress).** The command surface is complete and every command supports `--dry-run`, but most commands have not yet been run end to end against real tools. Several agent-CLI flags and the orchestrator config are still unverified. See [Status](#status). Don't point this at a repo with production credentials.

---

## Contents

- [How it works](#how-it-works)
- [What fleet does and doesn't do](#what-fleet-does-and-doesnt-do)
- [Requirements](#requirements)
- [Install](#install)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [Commands](#commands)
- [Operating a fleet](#operating-a-fleet)
- [Security model](#security-model)
- [Status](#status)
- [Contributing](#contributing)
- [License](#license)

---

## How it works

```
 you ──write──▶ GitHub issues (agent-ready, wave:*, Depends on)
                     │
        fleet sync (timer): eligible issues ─▶ Multica issues, one per profile
                     ▼
   ┌──────────── fleet box (Linux, always on) ────────────┐
   │  Multica (web UI + API, Tailscale only)              │
   │  Multica daemon runs the agent CLIs per profile:     │
   │   implementer ─▶ worktree ─▶ gate ─▶ PR              │
   │   fixer       ─▶ lint/typecheck retries              │
   │   reviewer    ─▶ reviews PRs from other vendors      │
   └──────────────────────────────────────────────────────┘
                     │
        fleet sync: blocked ─▶ needs-* label; gate failures ─▶ nudges / agent-stuck
                     ▼
 PRs + escalation labels ──GitHub Actions──▶ Telegram ──▶ you approve / answer
```

**One issue's life:**

1. **Dispatch.** An issue can be picked up when all of these hold:
   - it has `agent-ready`;
   - it has no `needs-*`, `blocked-by` or `agent-stuck` label;
   - every issue under its `## Depends on` heading is closed;
   - no open issue anywhere in the repo is labelled `fleet-paused`.

   Priority follows the order of `waves` in `fleet.yaml`, then issue number.
2. **Implement.** A profile whose `waves` include the issue's `wave:*` label takes it. The agent works in a fresh worktree and stays inside the paths the issue lists.
3. **Gate.** The agent runs `gate.command`, for example `pnpm gate`. A fixer profile may retry lint and typecheck failures. Test failures go back to the implementer. After `max_gate_attempts` failures the issue is labelled `agent-stuck`.
4. **Review.** A reviewer from a different vendor reviews the PR (`cross_vendor_review`), so a model never approves its own vendor's work.
5. **Merge.** Only you merge. The GitHub App that agents use has no permission to approve or change branch protection.

**Escalation.** When an agent hits something it can't settle from the issue and the docs, it comments in a fixed format (what it was doing, what's unclear, the options, its recommendation), applies one label and exits:

| Label | Meaning |
|---|---|
| `needs-human` | a decision only you can make |
| `needs-resource` | missing fixture, credential, design or data |
| `needs-contract` | the API or interface contract is missing or wrong |
| `blocked-by` | waiting on another issue |
| `agent-stuck` | gate failed `max_gate_attempts` times, or you killed the session |

Other labels: `agent-ready` (the agent owns the issue end to end), `agent-assist` (the agent implements it and a human reviews the test assertions), `human-required` (never dispatched), `fleet-paused` (soft pause), and `wave:<name>` for each wave. All of these names can be changed in `fleet.yaml`.

## What fleet does and doesn't do

**fleet wraps an orchestrator; it isn't one.** It never schedules sessions itself. It:

- prepares the machine (`bootstrap`);
- installs and smoke-tests agent CLIs (`harness`);
- generates orchestrator config and a systemd unit from `fleet.yaml` (`orchestrator init`);
- manages labels, issues and PRs through `gh` (`github init`, `issues sync`, `kill`);
- controls the service (`up`, `pause`, `resume`, `panic`);
- summarises state in a few lines (`status`, `digest`).

Scheduling, retries and session management belong to the orchestrator: [Multica](https://github.com/multica-ai/multica), self-hosted on the box. Multica has its own issue board and no notion of GitHub labels, gates or cross-vendor review, so **`fleet sync`** (a systemd timer) bridges the two: it mirrors *eligible* GitHub issues into Multica and Multica's state back as labels, and a generated `pr-contract` check enforces the review rule. fleet decides only what is eligible; Multica decides when and where it runs. The reasoning and the exact rules are in [docs/orchestrator-decision.md](docs/orchestrator-decision.md).

Every command is **idempotent**: running it again on a machine that's already set up changes nothing. Every command supports **`--dry-run`**, which prints each shell command and file write it would make and touches nothing.

**Out of scope:** web UI, metrics store, multiple repos per fleet, macOS or Windows fleet boxes, scheduling, retry logic, and cost tracking beyond what agents put in PR bodies.

## Requirements

**Fleet box** (where agents run), one of:
- **Ubuntu 24.04** VPS, always on. About 8 vCPU / 32 GB runs roughly six concurrent agents. `bootstrap` uses apt, ufw and systemd user units.
- **macOS 14+ on Apple silicon** (a Mac mini that never sleeps). `bootstrap` uses Homebrew, OrbStack for Docker, `pmset` and launchd LaunchAgents. Two things it can't do for you: enable **automatic login** for the fleet user (LaunchAgents and the agent CLIs' keychain tokens need a logged-in session after a reboot), and per-interface firewall rules (macOS has none; fleet keeps the Multica ports on the Tailscale IP instead).

Either way: a non-root user with `sudo` and **SSH key** login (`bootstrap` turns off SSH password authentication when `machine.firewall` is on), and a [Tailscale](https://tailscale.com) account if `machine.tailscale` is on. The Multica UI is only reachable over your tailnet. Docker (or OrbStack) runs Multica and your tests if they use containers.

**Target repository:**
- On GitHub, with branch protection on the default branch that requires your approval.
- A single gate command that a PR must pass (`gate.command`).
- Today, `harness verify` assumes a pnpm workspace (`pnpm install --frozen-lockfile`).
- An `AGENTS.md` that tells agents the rules. `fleet init` scaffolds one.

**Accounts:** GitHub (for a GitHub App), at least one model provider per harness, and optionally a Telegram bot for notifications. Use at least two vendors if you want cross-vendor review.

**Your laptop:** `gh`, and Go 1.23+ only if you build from source.

## Install

### Prepare the box

Most VPS images start with only `root`. `bootstrap` refuses to run as root, so create the user the fleet will run as and copy your SSH key to it:

```bash
# as root on a fresh Ubuntu 24.04 box
adduser --disabled-password --gecos "" fleet
usermod -aG sudo fleet
echo 'fleet ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/fleet && chmod 440 /etc/sudoers.d/fleet
rsync --archive --chown=fleet:fleet ~/.ssh /home/fleet
```

Then check that `ssh fleet@<host>` works from your laptop before going further. With `machine.firewall` on, bootstrap turns off password login (after checking that `~/.ssh/authorized_keys` exists).

### Install the binary

On the fleet box, as the fleet user (pick the asset for your OS: `fleet-linux-amd64` or `fleet-darwin-arm64`):

```bash
mkdir -p ~/.local/bin
curl -fsSL https://github.com/noelzappy/fleet/releases/latest/download/fleet-linux-amd64 -o ~/.local/bin/fleet
chmod +x ~/.local/bin/fleet
fleet version
```

From source (the box commands run on Linux and macOS; elsewhere they only `--dry-run`):

```bash
git clone https://github.com/noelzappy/fleet && cd fleet
make build            # ./bin/fleet
make install          # ~/.local/bin/fleet
```

## Quick start

```bash
# 1. In your project repo, on your laptop: scaffold config and governance files
fleet init --repo your-org/your-repo
#    → fleet.yaml, AGENTS.md, .github/ISSUE_TEMPLATE/agent-task.md  (never overwrites)
#    Edit fleet.yaml, fill every <FILL> in AGENTS.md, commit.

# 2. Preview everything, from anywhere
fleet --dry-run bootstrap
fleet --dry-run orchestrator init

# 3. On the fleet box, as the fleet user, with fleet.yaml present
fleet bootstrap                   # OS deps, docker, node/pnpm, gh, tailscale, firewall, dirs
                                  # prints ✓ for steps already done; a second run changes nothing
                                  # pauses at `tailscale up` until you open the printed link
exec $SHELL -l                    # pick up node on PATH and docker group membership
git config --global user.name "Your Name"          # every agent commit carries this identity
git config --global user.email "you@example.com"
gh auth login && gh auth setup-git                 # see "Signing in over SSH" below
gh repo clone your-org/your-repo ~/fleet/your-repo  # = project.root
$EDITOR ~/.config/fleet/env       # secrets, KEY=VALUE, chmod 600 (bootstrap creates it)

fleet harness add claude-code     # install + min_version + smoke test; repeat per harness
                                  # a failing smoke test usually means: sign the CLI in, then re-run
fleet harness verify              # every harness runs the gate headless in a worktree

fleet github init                 # labels, pr-contract check + Telegram workflow (commit them via PR)
fleet issues sync issues.yaml     # bulk-create GitHub issues, resolving depends_on
fleet orchestrator init           # Multica: compose, headless login (owner_email), workspace, repo, agents, service units
fleet up                          # from then on `fleet sync` runs every sync_interval

fleet status
```

## Configuration

Everything project-specific lives in `fleet.yaml`. The binary itself is project-agnostic. [`fleet.example.yaml`](fleet.example.yaml) is the annotated reference. By default fleet reads `./fleet.yaml`; override with `-c path`.

| Section | What it controls |
|---|---|
| `project` | `name`, `repo` (owner/name), `branch`, `root` (the orchestrator's clone on the box, default `~/fleet/<name>`), `docs` |
| `machine` | node/pnpm versions, and whether bootstrap sets up `docker`, `tailscale`, `firewall`, `always_on` (no sleep, macOS), `turbo_remote` |
| `harnesses` | agent CLIs: `kind` (`claude-code`, `opencode`, `antigravity`), `install`, `login`, `smoke`, `min_version`, `update`, `env`, `env_file` — see [Harnesses](#harnesses) |
| `profiles` | `harness` + `model` + `role` (`implementer`, `reviewer`, `fixer`) + `vendor` + `concurrency` + `waves` |
| `waves` | ordered work streams; each becomes a `wave:<name>` label and sets dispatch priority |
| `routing` | `cross_vendor_review`, `max_gate_attempts` (default 3), `fixer_only_lint` |
| `orchestrator` | `kind` (`multica`), `dir` (its checkout, default `~/fleet/multica`), `dashboard_bind` (IP, your Tailscale address), `service_name` (default `fleet-multica`), `workspace`, `sync_interval` (default `2m`), `owner_email` |
| `github` | GitHub App slug, app-id env var name, private key path, `required_checks` |
| `notify` | Telegram secret *names* (GitHub Actions secrets) and the digest schedule |
| `gate` | `command` (default `pnpm gate`), `timeout` (default `15m`), `workflow` (the Actions workflow that runs it on PRs, default `gate`) |
| `labels` | rename any label; unset ones use the defaults above |

`fleet.yaml` is checked when loaded: every profile must name a known harness and a vendor, and with `cross_vendor_review` on, every implementer vendor needs a reviewer from a different vendor.

### Harnesses

A harness is one agent CLI. Several profiles share a harness with different models. Three harnesses cover every vendor:

- **`claude-code`** for Anthropic models on your Claude plan.
- **`antigravity`** (`agy`) for Google models on your Google AI subscription.
- **`opencode`** for everything else: DeepSeek, GLM, OpenAI and free models through [OpenCode Zen](https://opencode.ai) (`opencode/deepseek-v4-pro`, `opencode/glm-5.3`, the free `opencode/big-pickle`, …), or through your own provider keys. `opencode models` lists what's available.

A profile's `vendor` is the model's maker, not the gateway: `opencode/glm-5.3` is vendor `zai`, so a Claude or Gemini reviewer still counts as cross-vendor. Free models usually come with rate limits and data-use terms; read them before pointing one at private code, and prefer them for fixers or overflow.

These are the kinds fleet supports, with invocations checked against the installed CLIs:

| `kind` | Binary | Install | Sign-in | Headless invocation fleet uses for `harness verify` |
|---|---|---|---|---|
| `claude-code` | `claude` | `npm i -g @anthropic-ai/claude-code` | `claude login` | `claude -p … --output-format text --allowedTools "Bash(<gate>)"` |
| `opencode` | `opencode` | `npm i -g opencode-ai` | `opencode auth login` | `OPENCODE_CONFIG_CONTENT='{"permission":…}' opencode run …` (only the gate is allowed) |
| `antigravity` | `agy` | `curl -fsSL https://antigravity.google/cli/install.sh \| bash` | run `agy` once interactively | `agy -p … --output-format text --dangerously-skip-permissions --print-timeout <gate timeout + 10m>` |

**Antigravity CLI replaces Gemini CLI.** Google moved Gemini CLI users to Antigravity CLI. Since 18 June 2026, Gemini CLI no longer serves requests for Google AI Pro/Ultra subscribers or free individual use. A `kind: gemini-cli` harness is rejected with a pointer to `antigravity`. Notes for a fleet box:
- **Sign-in over SSH:** run `agy` in tmux. On an SSH session it prints an authorization URL; open it on your own machine and paste the code back.
- **API key instead of a subscription:** set `"modelProvider": "gemini"` in `~/.gemini/antigravity-cli/settings.json` and export `GEMINI_API_KEY` (agy 1.1.13+).
- **`--print-timeout` defaults to 5 minutes**, shorter than most gates, so fleet always passes it.
- **No per-command allowlist.** agy only has `--dangerously-skip-permissions`, so an `antigravity` harness gets every tool during `harness verify`, while the other kinds are limited to the gate command.
- `agy models` lists the model IDs to use in profiles (for example `gemini-3.1-pro-high`, `gemini-3.8-flash-medium`).

**Google models through OpenCode need an API key.** OpenCode's Google provider takes a Gemini API key (AI Studio) or Vertex AI credentials; it can't use a Google AI Pro/Ultra subscription. The third-party `opencode-antigravity-auth` plugin that tried is archived, says in its own README that it violates Google's Terms of Service, and users report account bans. To use a subscription, route Google profiles through an `antigravity` harness instead, as `fleet.example.yaml` does.

**Anthropic-compatible endpoints** can also run through Claude Code: a `kind: claude-code` harness with `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN: ${VAR}` in `env` (written to its `env_file`). OpenCode is usually simpler.

### Signing in over SSH

Every sign-in happens once, on the box, as the fleet user, in a normal SSH session (`ssh fleet@<box>`). The browser part happens on your own machine.

| Tool | Command on the box | What you do |
|---|---|---|
| GitHub | `gh auth login` → GitHub.com → HTTPS → web browser, then `gh auth setup-git` | open github.com/login/device and enter the code it prints |
| Claude Code | `claude` (or `claude login`) | open the printed URL, approve, paste the code back |
| Antigravity | `agy` | open the printed URL, approve, paste the authorization code back (it waits 60 s) |
| OpenCode | `opencode auth login` → OpenCode Zen (or another provider) | paste the API key from opencode.ai |
| Tailscale | printed by `fleet bootstrap` | open the link and approve the machine |
| Multica | none with `orchestrator.owner_email` set | `orchestrator init` signs in over the API; without it, create a token in the UI and paste it |

Then `fleet harness add <name>` again: the smoke test should pass.

### Secrets

Secrets never go in `fleet.yaml`. Reference them as `${VAR}`:

```yaml
harnesses:
  glm:
    env:
      ANTHROPIC_AUTH_TOKEN: ${GLM_API_KEY}
```

and put values in `~/.config/fleet/env` on the box, one `KEY=VALUE` per line (mode `0600`). The same file is loaded into the orchestrator service by systemd, so don't use `export` or shell syntax in it.

How `${VAR}` is resolved:
1. `~/.config/fleet/env` **wins** over the process environment. The box then behaves the same over SSH as under systemd, and an old `export` in your shell profile can't override a key you just rotated.
2. An empty value (`KEY=`) counts as unset and falls through to the environment.
3. Any unresolved variable is an error listing every missing name, raised before a command runs. Under `--dry-run` it's a warning instead.

`fleet harness add` writes a harness's resolved `env` to its `env_file` (mode `0600`).

## Commands

Global flags: `-c, --config <path>` (default `fleet.yaml`), `--dry-run`. "box" means Linux or macOS; under `--dry-run`, `FLEET_PLATFORM=linux|darwin` previews the other platform's commands.

| Command | What it does | Runs on |
|---|---|---|
| `fleet init [--repo o/n]` | Scaffold `fleet.yaml`, `AGENTS.md`, issue template. Never overwrites. | anywhere |
| `fleet bootstrap` | Linux: apt packages, docker, fnm + node, pnpm, gh, tailscale, ufw, SSH key-only, fleet dirs. macOS: Homebrew, OrbStack, fnm + node, pnpm, gh, tailscale, no-sleep, application firewall, sshd, SSH key-only. Each step checks first and re-checks after applying | box |
| `fleet harness add <name>` | Run `install`, check `min_version`, write `env_file`, run `smoke` | anywhere |
| `fleet harness login <name>` | Run the interactive `login` (use tmux over SSH) | anywhere |
| `fleet harness verify` | Check `min_version`s, then a throwaway worktree where every harness runs the gate headless; PASS/FAIL table | box |
| `fleet harness update [name]` | Run each CLI's self-update (`claude update`, `opencode upgrade`, `agy update`), then check `min_version`. Run weekly | anywhere |
| `fleet github init` | Create/update all labels; write the `pr-contract` check and the Telegram notify workflow | anywhere |
| `fleet issues sync <file>` | Bulk-create issues from YAML, then write `## Depends on` with real `#numbers` | anywhere |
| `fleet sync` | One reconciliation tick: eligible GitHub issues → Multica; blocked/gate state → labels, nudges, reviews. The timer runs it; safe by hand | anywhere |
| `fleet orchestrator init` | Install the Multica CLI and checkout, generate secrets, compose env/override bound to the Tailscale IP, start the server, log the CLI in (PAT), create workspace, repo and one agent per profile, install daemon + sync units | box |
| `fleet orchestrator run` | Multica daemon in the foreground; the systemd unit calls this | box |
| `fleet up` | Start the Multica server, daemon and sync timer | box |
| `fleet pause` | **Soft:** open a `fleet-paused` issue. `sync` mirrors nothing new; in-flight work finishes | anywhere |
| `fleet pause --hard` | Also stop the daemon and sync timer now (Multica re-queues interrupted runs when the daemon returns) | box |
| `fleet resume` | Close `fleet-paused` issues and start the daemon and timer | box |
| `fleet kill <issue>` | Cancel that issue's Multica runs, label `agent-stuck`, close its PR | anywhere |
| `fleet panic` | Stop daemon and timer, kill agent processes, print the manual rotation checklist | box |
| `fleet status` | Service state, paused, queue and escalation counts, open PRs (≤ 8 lines) | anywhere |
| `fleet digest` | Daily summary (currently the same as `status`) | anywhere |

### `issues sync` format

```yaml
- title: "[CONTRACTS] admin wallets surface"
  labels: [agent-ready, "wave:contracts"]
  body: |
    ## Requirement
    ...
- title: "[ADMIN] wallet freeze/unfreeze"
  labels: [agent-ready, "wave:admin"]
  body: |
    ...
  depends_on: ["[CONTRACTS] admin wallets surface"]   # by title, within this file
```

Every `depends_on` title is checked before any issue is created.

## Operating a fleet

Most of what decides whether a fleet produces mergeable PRs happens outside the tool.

**Issues are the product.** Aim for one issue = one PR, roughly ≤ 400 changed lines and ≤ 90 minutes of agent time. Fill every field of the issue template, list the exact paths the agent may modify, and write acceptance criteria as tests. Thin issues lead to escalations or guessing. Only the first wave needs issues at launch; write the next wave while the current one runs.

**Start small.** Launch with about three workers on one wave. Dry-run one issue by hand first: worktree → install → agent → gate → PR → review. At hour 24, read every PR. If more than one is garbage, stop and fix the template or `AGENTS.md`. Add workers and waves only once acceptance is healthy.

**Daily loop (10–15 minutes, phone):**
1. Answer every `needs-*` issue in its thread and remove the label. **A worker waiting on `needs-human` never resumes until you answer**, so an unattended queue stalls within a day or two.
2. `agent-stuck`: read the last escalation. Usually the issue is too big; split it and close the original.
3. Review PRs: diff, tests, the "Not done" section.
4. Check spend per profile on the provider dashboards.

**Stop conditions.** Soft-pause and fix the root cause (almost always issue quality or a missing rule) when:

| Metric | Healthy | Stop and fix |
|---|---|---|
| PR acceptance (merged / opened) | ≥ 60% | < 40% |
| Gate passes first try | ≥ 50% | < 30% |
| Escalations per merged PR | 0.2–0.5 | > 1.0 (issues too thin) or 0 (agents guessing) |
| Scope violations | 0 | any: tighten the paths in `AGENTS.md` |
| `agent-stuck` backlog | < 5 | > 10 |

More workers won't fix a failing loop.

**Pausing:**
- **Soft pause** (`fleet pause`) stops new dispatches while running sessions finish and open their PRs. `fleet resume` closes the pause issue. Use it for day-to-day stops.
- **Multica mirrors GitHub.** Closing a GitHub issue (merging its PR) marks its Multica task done, and a merged or closed PR marks its review done. A run cancelled by a daemon restart is re-run once. Other things `fleet sync` handles for you: a PR that conflicts after another merge gets a rebase request, a commit with an agent attribution line gets an amend request, and a PR body missing `Closes #N` or `Model:` gets a fix request. The full rule table is in [docs/orchestrator-decision.md](docs/orchestrator-decision.md).
- **Escalations round-trip through labels.** An agent that sets its Multica issue to `blocked` gets a `needs-*` label and its comment on the GitHub issue within one sync interval. Answer on GitHub, remove the label, and the next tick tells the agent to read your answer and continue.
- **Hard pause** (`fleet pause --hard`) stops workers immediately. Use it before changing `AGENTS.md`, the issue template or the docs agents read: merge the change, then resume. Running sessions keep the version they started with.
- **Kill** (`fleet kill <n>`) is for one runaway session. Afterwards, check that session's spend.
- **Panic** (`fleet panic`) is for when you suspect compromise or runaway cost. Then, by hand: suspend the GitHub App installation, rotate every provider key, and inspect open PRs before resuming. That order removes agents' push access before their model access.

**Quota exhaustion.** When one vendor's plan cap hits mid-week, set that implementer's `concurrency` to 0, let other vendors' implementers cover its waves, and switch reviews to a reviewer that is still cross-vendor for those PRs.

## Security model

- **Agents run with permissions bypassed.** Multica starts every harness in its non-interactive, auto-approve mode (`--permission-mode bypassPermissions`, `--dangerously-skip-permissions`); nothing on the box should be something you can't rotate.
- **Commits are the owner's.** Agents commit with the git identity configured on the box (set `git config --global user.name/user.email` to yours; `orchestrator init` checks it), never add `Co-authored-by`/"Generated with" trailers (the `AGENTS.md` scaffold forbids it and `pr-contract` fails PRs that carry one), and `orchestrator init` turns off Multica's own Co-authored-by hook. Attribution lives in the PR body's `Model:` line only.
- **Agents use a GitHub App, not your personal token.** Permissions: contents write, pull requests write, issues write, metadata read, checks read. **No administration and no workflows**, so an agent can't approve, merge past protection, or edit CI to weaken the gate.
- **Secrets** live only in `~/.config/fleet/env` and per-harness env files, both `0600`. Never in `fleet.yaml`, the repo or issues. Put a hard spend cap on every provider key that supports one.
- **Network:** `bootstrap` denies all incoming traffic except SSH and the tailnet, and turns off SSH password auth. Bind the orchestrator dashboard to your Tailscale IP, never `0.0.0.0`.
- **What vendors see.** Every model in the fleet reads your code, issues and specs, not just your data. Choose vendors with that in mind, and keep production credentials and real customer data out of any repo a fleet works on. Run with mocks.
- **Only you write instructions.** Recent vulnerabilities in agent CLIs and their GitHub Actions (Claude Code before 2.1.163; `claude-code-action` before 1.0.74; Gemini CLI before 0.39.1) let untrusted repository or GitHub content reach an agent and leak keys. On a fleet-managed repo:
  - only the repo owner, and the owner's own agent sessions, write issue bodies;
  - no issue templates or forms that external users can trigger. The template `fleet init` writes applies no labels, so `agent-ready` is always applied by hand;
  - no external contributors, and no outside collaborators with triage or write access;
  - no workflows that run an agent on someone else's event. If you use `claude-code-action`, pin it to 1.0.74 or later;
  - harnesses updated weekly with `fleet harness update`.

  `fleet init` writes these rules into `AGENTS.md` as a trust boundary for agents too.
- **Harness versions are pinned from below.** Each harness's `min_version` is enforced by `harness add`, `harness verify` and `harness update`. `fleet.example.yaml` sets floors from each project's security advisories and says which advisory each one comes from.
- **Values are quoted for the shell.** Everything interpolated into a shell command (titles, bodies, labels, paths) is passed as a literal, so issue text can't inject commands on the box.

## Status

v0.1 is done when a fresh box goes from `fleet init` to a running fleet working ten or more issues, and `pause`, `resume`, `kill` and `panic` have each been verified by hand.

| Area | State |
|---|---|
| `fleet.yaml` loading, defaults, validation, `${VAR}` secrets | implemented, unit-tested |
| `--dry-run` for every command | implemented, checked |
| `init` | implemented; not yet exercised end to end |
| `status` | implemented; active sessions and spend per profile not yet shown |
| `bootstrap` | ran for real on Ubuntu 24.04 (Hetzner) and macOS; second run a no-op on both |
| `harness add/login/verify` | ran for real on Linux and macOS: claude-code, opencode, antigravity install, pass smoke, and run the gate headless (PASS) |
| `orchestrator init/run` (Multica) | ran end to end on Linux (systemd) and macOS (launchd): headless login, workspace, repo, agents; second run a no-op; UI/API reachable on the Tailscale IP only |
| `sync` | planner table-tested; ran live on macOS and from the systemd timer on Linux: dispatch, dependency hold, cross-vendor review, nudge, escalate, rebase, owner-only commits, closure mirrored back to Multica |
| `github init` | labels, `pr-contract` check and notify workflow; GitHub App manifest flow not implemented |
| `issues sync` | implemented; not yet run against a live repo |
| `up/pause/resume/kill/panic` | written; not yet verified on a box |
| `digest` | prints `status` only; merged-in-24h, gate pass rate and Telegram delivery to do |
| GitHub App | not implemented: agents currently use the box's `gh` login (your account). Use a scoped App token before a client repo |

Build order for the rest of v0.1: bootstrap → harnesses → orchestrator → GitHub → issues → kill/panic/status/digest → init end to end.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the design rules, the development setup, and how to test the box commands on Linux and macOS.

## License

[MIT](LICENSE)
