# fleet

Set up, run, and supervise a fleet of coding agents from different model vendors against one GitHub repository.

You write issues. Agents pick them up, work in their own git worktrees, run your gate, and open pull requests. An agent from a *different* vendor reviews each PR. You approve and merge. When an agent is unsure, it labels the issue and stops instead of guessing, and you get a Telegram message.

`fleet` is the operator's tool for that loop. It is a single Go binary that turns a `fleet.yaml` into a running, supervised fleet on a Linux box, and gives you short commands to pause, resume, kill and inspect it from an SSH session on your phone.

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
                     ▼
   ┌──────────── fleet box (Linux, always on) ────────────┐
   │  systemd ─▶ fleet orchestrator run ─▶ orchestrator   │
   │                                        (AO)          │
   │        dispatches ready issues to profiles:          │
   │   implementer ─▶ worktree ─▶ gate ─▶ PR              │
   │   fixer       ─▶ lint/typecheck retries              │
   │   reviewer    ─▶ reviews PRs from other vendors      │
   └──────────────────────────────────────────────────────┘
                     │
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

Scheduling, retries and session management belong to the orchestrator. Today that is [Agent Orchestrator](https://www.npmjs.com/package/@aoagents/ao) (`ao`). `vibe-kanban` is planned.

Every command is **idempotent**: running it again on a machine that's already set up changes nothing. Every command supports **`--dry-run`**, which prints each shell command and file write it would make and touches nothing.

**Out of scope:** web UI, metrics store, multiple repos per fleet, macOS or Windows fleet boxes, scheduling, retry logic, and cost tracking beyond what agents put in PR bodies.

## Requirements

**Fleet box** (where agents run):
- Ubuntu 24.04, always on. A VPS with about 8 vCPU and 32 GB RAM runs roughly six concurrent agents. Docker is needed if your tests use containers.
- A non-root user with `sudo` and **SSH key** login. `bootstrap` turns off SSH password authentication when `machine.firewall` is on.
- A [Tailscale](https://tailscale.com) account if `machine.tailscale` is on. The orchestrator dashboard is only reachable over your tailnet.

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

On the fleet box, as the fleet user:

```bash
mkdir -p ~/.local/bin
curl -fsSL https://github.com/noelzappy/fleet/releases/latest/download/fleet-linux-amd64 -o ~/.local/bin/fleet
chmod +x ~/.local/bin/fleet
fleet version
```

From source (any OS; the Linux-only commands refuse to run elsewhere except with `--dry-run`):

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

# 3. On the fleet box, with fleet.yaml present
fleet bootstrap                   # OS deps, docker, node/pnpm, gh, tailscale, firewall, dirs
                                  # prints ✓ for steps already done; a second run changes nothing
exec $SHELL -l                    # pick up node on PATH and docker group membership
gh auth login
git clone https://github.com/your-org/your-repo ~/fleet/your-repo   # = project.root
$EDITOR ~/.config/fleet/env       # secrets, KEY=VALUE, chmod 600 (bootstrap creates it)

fleet harness add claude-code     # install + smoke test; repeat per harness
fleet harness login claude-code   # if the smoke test needs auth; run inside tmux
fleet harness verify              # every harness runs the gate headless in a worktree

fleet github init                 # labels + Telegram notify workflow (commit it via PR)
fleet issues sync issues.yaml     # bulk-create issues, resolving depends_on
fleet orchestrator init           # orchestrator config + systemd user unit
fleet up

fleet status
```

## Configuration

Everything project-specific lives in `fleet.yaml`. The binary itself is project-agnostic. [`fleet.example.yaml`](fleet.example.yaml) is the annotated reference. By default fleet reads `./fleet.yaml`; override with `-c path`.

| Section | What it controls |
|---|---|
| `project` | `name`, `repo` (owner/name), `branch`, `root` (the orchestrator's clone on the box, default `~/fleet/<name>`), `docs` |
| `machine` | node/pnpm versions, and whether bootstrap sets up `docker`, `tailscale`, `firewall`, `turbo_remote` |
| `harnesses` | agent CLIs: `kind` (`claude-code`, `opencode`, `antigravity`), `install`, `login`, `smoke`, `env`, `env_file` — see [Harnesses](#harnesses) |
| `profiles` | `harness` + `model` + `role` (`implementer`, `reviewer`, `fixer`) + `vendor` + `concurrency` + `waves` |
| `waves` | ordered work streams; each becomes a `wave:<name>` label and sets dispatch priority |
| `routing` | `cross_vendor_review`, `max_gate_attempts` (default 3), `fixer_only_lint` |
| `orchestrator` | `kind` (default `ao`), `config_path`, `dashboard_bind`, `service_name` (default `fleet-<kind>`) |
| `github` | GitHub App slug, app-id env var name, private key path, `required_checks` |
| `notify` | Telegram secret *names* (GitHub Actions secrets) and the digest schedule |
| `gate` | `command` (default `pnpm gate`) and `timeout` (default `15m`) |
| `labels` | rename any label; unset ones use the defaults above |

`fleet.yaml` is checked when loaded: every profile must name a known harness and a vendor, and with `cross_vendor_review` on, every implementer vendor needs a reviewer from a different vendor.

### Harnesses

A harness is one agent CLI. Several profiles can share a harness with different models. These are the kinds fleet supports, with invocations checked against the installed CLIs:

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

**Anthropic-compatible endpoints** (GLM and others) use `kind: claude-code` with `ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN` in `env`; see the `glm` harness in the example.

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

Global flags: `-c, --config <path>` (default `fleet.yaml`), `--dry-run`.

| Command | What it does | Runs on |
|---|---|---|
| `fleet init [--repo o/n]` | Scaffold `fleet.yaml`, `AGENTS.md`, issue template. Never overwrites. | anywhere |
| `fleet bootstrap` | apt packages, docker, fnm + node, pnpm, gh, tailscale, ufw, SSH key-only, fleet dirs. Each step checks first and re-checks after applying | box |
| `fleet harness add <name>` | Run `install`, write `env_file`, run `smoke` | anywhere |
| `fleet harness login <name>` | Run the interactive `login` (use tmux over SSH) | anywhere |
| `fleet harness verify` | Throwaway worktree; every harness runs the gate headless; PASS/FAIL table | box |
| `fleet github init` | Create/update all labels; write the Telegram notify workflow | anywhere |
| `fleet issues sync <file>` | Bulk-create issues from YAML, then write `## Depends on` with real `#numbers` | anywhere |
| `fleet orchestrator init` | Install the orchestrator, render its config, install the systemd user unit, enable linger | box |
| `fleet orchestrator run` | Foreground orchestrator; the systemd unit calls this | box |
| `fleet up` | Enable and start the service | box |
| `fleet pause` | **Soft:** open a `fleet-paused` issue. No new dispatches; in-flight work finishes | anywhere |
| `fleet pause --hard` | Stop the service now. Worktrees and branches persist | box |
| `fleet resume` | Close `fleet-paused` issues and start the service | box |
| `fleet kill <issue>` | Stop that session, label `agent-stuck`, remove its worktree, close its PR | box |
| `fleet panic` | Stop the service, kill agent processes, print the manual rotation checklist | box |
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
- **Hard pause** (`fleet pause --hard`) stops workers immediately. Use it before changing `AGENTS.md`, the issue template or the docs agents read: merge the change, then resume. Running sessions keep the version they started with.
- **Kill** (`fleet kill <n>`) is for one runaway session. Afterwards, check that session's spend.
- **Panic** (`fleet panic`) is for when you suspect compromise or runaway cost. Then, by hand: suspend the GitHub App installation, rotate every provider key, and inspect open PRs before resuming. That order removes agents' push access before their model access.

**Quota exhaustion.** When one vendor's plan cap hits mid-week, set that implementer's `concurrency` to 0, let other vendors' implementers cover its waves, and switch reviews to a reviewer that is still cross-vendor for those PRs.

## Security model

- **Agents use a GitHub App, not your personal token.** Permissions: contents write, pull requests write, issues write, metadata read, checks read. **No administration and no workflows**, so an agent can't approve, merge past protection, or edit CI to weaken the gate.
- **Secrets** live only in `~/.config/fleet/env` and per-harness env files, both `0600`. Never in `fleet.yaml`, the repo or issues. Put a hard spend cap on every provider key that supports one.
- **Network:** `bootstrap` denies all incoming traffic except SSH and the tailnet, and turns off SSH password auth. Bind the orchestrator dashboard to your Tailscale IP, never `0.0.0.0`.
- **What vendors see.** Every model in the fleet reads your code, issues and specs, not just your data. Choose vendors with that in mind, and keep production credentials and real customer data out of any repo a fleet works on. Run with mocks.
- **Values are quoted for the shell.** Everything interpolated into a shell command (titles, bodies, labels, paths) is passed as a literal, so issue text can't inject commands on the box.

## Status

v0.1 is done when a fresh box goes from `fleet init` to a running fleet working ten or more issues, and `pause`, `resume`, `kill` and `panic` have each been verified by hand.

| Area | State |
|---|---|
| `fleet.yaml` loading, defaults, validation, `${VAR}` secrets | implemented, unit-tested |
| `--dry-run` for every command | implemented, checked |
| `init` | implemented; not yet exercised end to end |
| `status` | implemented; active sessions and spend per profile not yet shown |
| `bootstrap` | written; not yet run on a real box |
| `harness add/login/verify` | written; headless flags for each CLI unverified; OAuth-over-SSH steps not yet documented |
| `orchestrator init/run` | written; **AO config template is a best guess**, not checked against `ao config-help` |
| `github init` | labels and notify workflow; GitHub App manifest flow not implemented |
| `issues sync` | implemented; not yet run against a live repo |
| `up/pause/resume/kill/panic` | written; not yet verified on a box. Soft pause depends on the orchestrator honouring the `fleet-paused` rule |
| `digest` | prints `status` only; merged-in-24h, gate pass rate and Telegram delivery to do |

Build order for the rest of v0.1: bootstrap → harnesses → orchestrator → GitHub → issues → kill/panic/status/digest → init end to end.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the design rules, the development setup, and how to test the Linux-only commands.

## License

[MIT](LICENSE)
