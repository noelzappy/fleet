# Orchestrator decision: Multica vs Agent Orchestrator (AO)

**Status:** proposed, awaiting owner review · **Date:** 2026-09-16 · **Blocks:** `fleet orchestrator init`

## Question

`fleet` wraps an orchestrator and never schedules sessions itself. Which orchestrator should it wrap: **Multica** or **Agent Orchestrator (AO)**, the backend the skeleton assumed?

Both were judged on six requirements. The evidence comes from each project's source, not its README. Citations are `path:line` in:
- **Multica:** `multica-ai/multica` at `7e4758a` (2026-09-16), paths under `server/`
- **AO:** `Untrivial-ai/agent-orchestrator` at tag `desktop-v0.10.3` / `c89414d` (2026-07-12), paths under `backend/`. This is the version npm `@aoagents/ao@0.10.3` installs; the repo is ahead at v0.13 nightlies.

Two research passes read the source. I then re-read every citation marked ✔ below myself.

## Summary

| Requirement | Multica | AO |
|---|---|---|
| 1. Dispatch from GitHub Issues by label | **No.** Its own issue board; GitHub issue events are ignored | **Partial.** Polls GitHub Issues, but filters by assignee, not label |
| 2. Multiple harnesses, routed per task | **Yes.** Claude Code, OpenCode, agy and more; routed by assignee agent, with model per agent | **Partial.** Many harnesses, but intake always uses the single project `worker`; agy and OpenCode ignore the model setting |
| 3. Reviewer from a different vendor | **Partial.** Any agent can be asked to review; nothing enforces a different vendor | **Partial.** A `reviewers[0]` role (claude-code, codex, opencode only), triggered only by API call; defaults to the worker's own vendor |
| 4. Headless on Linux, web UI bound to one interface | **Yes, by editing compose.** Postgres plus server plus web UI; binds 127.0.0.1 by default; has login | **No.** The daemon is headless, but the dashboard is an Electron desktop app; the API is fixed to 127.0.0.1 with no auth |
| 5. Pause by external signal | **Partial.** `multica daemon stop` stops claiming work; cancel endpoints for single tasks; no global pause | **No.** No pause. `ao stop` leaves sessions running; disable intake through config |
| 6. Gate failures and retries | **No gate.** Retries infrastructure failures only; agents set `blocked` / `in_review` | **No gate.** Nudges the agent with CI failures an unlimited number of times; no final-failure state |
| Antigravity CLI (`agy`) | First-class: model, print-timeout, stream-json | Worker only; model ignored; can't review |
| Issue dependencies | Table exists but is **never read** | None |
| Concurrency per agent | Yes (`max_concurrent_tasks`) | None |
| License | Apache-2.0 **plus conditions** (no hosting it as a service for third parties, keep the branding) | Apache-2.0 |
| Footprint | Docker, Postgres 17, server, frontend, daemon | One static binary, tmux |

**Neither one meets requirement 1 or 6.** Whichever we choose, `fleet`'s model changes; see [What changes in fleet](#what-changes-in-fleet).

## Evidence

### 1. Dispatch from GitHub Issues by label

**Multica: No.**
- ✔ The GitHub webhook handles only `ping`, `installation`, `pull_request`, and `check_suite`/`check_run`/`status`. Everything else is acknowledged and dropped: `internal/handler/github.go:1075-1091` (*"ignore types we don't model"*). GitHub data comes in for PRs and CI only.
- Runs start when an issue on Multica's own board is assigned to an agent or leaves backlog: `internal/service/issue_trigger.go:15-21` (`RunSourceAssign`, `RunSourceStatus`).
- The closest route is a generic Autopilot webhook (`cmd/server/router.go:1418`). It can map `github.issues.labeled`, but it filters on event name and action, never the label name (`internal/handler/autopilot_webhook.go:552,637-640,766`). It creates a new Multica issue and writes nothing back to GitHub (`internal/service/autopilot.go:696-743`).

**AO: Partial.**
- ✔ Intake lists open issues filtered by assignee only: `internal/observe/trackerintake/observer.go:183-186`. It matches on `""`, `"*"`, `"none"` or a login: `:221-233`. The config has no label field (`internal/domain/tracker.go:84-94`).
- Each issue gets exactly one worker (`observer.go:203`). There's no concurrency cap, and dependency or blocker labels aren't understood (searched for `depends on|blocked by|blocker`; the only hits are PR merge blockers).

### 2. Multiple harnesses with per-task routing

**Multica: Yes. Routing is by assignee.**
- Backends include `claude`, `opencode`, `antigravity`, `codex`, `cursor` and others: `pkg/agent/agent.go:418-479`. There's no Gemini CLI backend.
- Claude Code runs with `-p --output-format stream-json … --permission-mode bypassPermissions [--model]` (`pkg/agent/claude.go:728-750`). OpenCode runs with `run --format json --dangerously-skip-permissions [--model]` (`pkg/agent/opencode.go:87-116`).
- An agent is a runtime plus a provider plus a model. Work goes to the issue's assignee agent or squad leader (`internal/service/issue_trigger.go:60-66`), so per-task routing means choosing the assignee (`multica issue assign`, `cmd/multica/cmd_issue.go:214`). Labels play no part.
- ✔ Per-agent concurrency is a column that the CLI validates (`migrations/023_agent_concurrency_default.up.sql`, `cmd/multica/cmd_agent_validation.go:9`). There's also a daemon-wide cap, `MULTICA_DAEMON_MAX_CONCURRENT_TASKS` (`cmd/multica/cmd_daemon.go:1041`).

**AO: Partial.**
- There are 23 worker harnesses, including claude-code, opencode and agy (`internal/domain/harness.go`). None of them is Gemini CLI.
- Intake always spawns the project's single `worker.agent` (`observer.go:206-211`). Per-task harness choice exists only if something calls `ao spawn --harness` for each issue, and that something would be the dispatcher.
- The model is set per role (`internal/domain/projectconfig.go:19-48`). The OpenCode adapter never passes a model (`internal/adapters/agent/opencode/opencode.go:100-102`), and neither does the agy adapter (below).

### 3. Reviewer from a different vendor

**Multica: Partial.**
- There's no reviewer role in code; the squad `role` is free text (`migrations/084_squad.up.sql:22`).
- Agents are told to move finished work to `in_review` and to leave `done` to a human (`internal/daemon/execenv/runtime_config_sections.go:730`).
- Another agent, on any provider, can be delegated or @mentioned to review. Repeat review requests are deduplicated per PR head SHA (`internal/service/task.go:1208-1231`).
- Nothing enforces vendor ≠ implementer vendor.

**AO: Partial.**
- ✔ The reviewer harnesses are `claude-code`, `codex` and `opencode` only (`internal/domain/reviewerharness.go:13-15`), so agy can't review.
- Only `Reviewers[0]` is used. With no reviewer configured it falls back to the worker's harness, i.e. the **same vendor** (`projectconfig.go:65-72`).
- A review is started only by `POST /api/v1/sessions/{id}/reviews/trigger` (`internal/httpd/controllers/reviews.go:72,99`); nothing in the codebase triggers it automatically.

### 4. Headless on Linux, web UI bound to one interface

**Multica: Yes, with a compose edit.**
- Self-hosting runs Postgres (pgvector pg17), backend and frontend, with Redis optional: `docker-compose.selfhost.yml:36-37,55,69,207`.
- ✔ Host ports are pinned to loopback: `"127.0.0.1:${BACKEND_PORT…}:8080"` (`:61`) and `"127.0.0.1:${FRONTEND_PORT:-3000}:3000"` (`:212`). There's no host-IP variable, so binding to a Tailscale IP means `fleet` generates a compose override for `ports:` (or puts a proxy in front).
- **Auth:** email-code login (`router.go:1403-1404`), `JWT_SECRET` required, `ALLOWED_EMAILS` available. With no mail backend the codes are logged (`cmd/server/main.go:326-327`), which is workable for a single operator.
- Agents run in the daemon (`multica daemon`), which can live on the same box. It keeps a bare clone per repo plus a worktree per task (`internal/daemon/repocache/cache.go:599`).

**AO: No.**
- The daemon is a static Go binary started with the hidden `ao daemon`. It needs tmux.
- ✔ The bind address is hard-coded: `LoopbackHost = "127.0.0.1"`, with *"There is deliberately no AO_HOST env var: the daemon has no auth/CORS/TLS"* (`internal/config/config.go:18-23`). Only `AO_PORT` can change.
- The dashboard is an Electron desktop app that `ao start` downloads and opens (`internal/cli/start.go:186-213,349`). A VPS has no web UI to reach over Tailscale.

### 5. Pause by an external signal

**Multica: Partial.**
- No global pause: searching for "paus" finds only an internal claim pause during daemon self-update (`internal/daemon/daemon.go:5057-5072`).
- `multica daemon stop` / SIGTERM stops claiming new work and waits up to 30 s for running tasks (`daemon.go:5344-5357`). Work queued while the runtime is offline waits instead of failing (`internal/service/agent_ready.go:27-29`). Whether running tasks are killed after the 30 s grace wasn't traced; **unverified**.
- Cancel: per task (`POST /api/tasks/{id}/cancel`, `multica issue cancel-task`) and per agent (`POST …/cancel-tasks`, `internal/service/task.go:2647`).

**AO: No.**
- There's no pause state.
- ✔ `ao stop` deliberately leaves sessions running (`internal/daemon/daemon.go:220-223`).
- Dispatch stops only when `trackerIntake` is disabled through `ao project set-config`, which is re-read every tick (`internal/daemon/tracker_intake_wiring.go:20-22`).
- Kill works per session only (`ao session kill <id>`).
- **Skeleton commands that don't exist:** `ao session stop --issue N` and `ao start --config … --bind …`.

### 6. Gate failures and retries

**Multica: no gate step; retries infrastructure failures only.**
- Nothing runs a check command after an agent finishes. Searched for `post_task`, `verify_command`, `check_command`, `test_command`, `gate_command`, `after_task`, `hook_command` and `PostRun`; none exist.
- ✔ Retries cover `runtime_offline`, `runtime_recovery`, `timeout`, `provider_network` and similar, and agent errors such as failing builds are *"intentionally excluded"* (`internal/service/task.go:5108-5132`). Default `max_attempts` is 2 (`:5153-5163`).
- On final failure it posts a system comment and leaves the issue status alone (`task.go:5070-5071`).
- Agents report problems through statuses: *"cannot proceed → `blocked`, and post a comment"*, and finished work goes to `in_review` (`runtime_config_sections.go:730-732`).

**AO: no gate step; unlimited CI nudges.**
- `postCreate` runs before the work starts, not after (`projectconfig.go:30-31`).
- ✔ When GitHub CI fails, the failing log is pasted into the agent with `maxAttempts: 0`, i.e. unlimited (`internal/lifecycle/reactions.go:178-191`). Review-feedback nudges are capped at 3 (`:18,205`).
- There's no final-failure state and no hand-off to a human.
- Agents raise blockers with `ao send` to an orchestrator session. They never label the issue; intake is *"read-only toward the tracker in v1"* (`projectconfig.go:44-46`).

### Antigravity CLI (`agy`)

- **Multica:** ✔ `agy -p <prompt> --dangerously-skip-permissions --output-format stream-json [--model] --print-timeout <t> --log-file … [--conversation] [--add-dir]` (`pkg/agent/antigravity.go:659-684`). It always passes `--print-timeout`, because agy's 5-minute default killed long builds (MUL-3570), and it has about 44 KB of adapter tests.
- **AO:** ✔ `agy --add-dir <ws> [--dangerously-skip-permissions] [--prompt-interactive <prompt>]` (`internal/adapters/agent/agy/agy.go:64-85`). It is interactive in tmux, never passes a model, and agy isn't a reviewer harness.

## Recommendation: Multica

On the requirements that shape a multi-vendor fleet, Multica is the stronger execution layer:
- **Routing by vendor:** one agent is a harness plus a model with its own concurrency limit, which maps directly onto `fleet.yaml` profiles. AO has a single worker harness per project and ignores the model for agy and OpenCode.
- **Antigravity:** agy is a first-class backend with a model and a long print timeout. In AO it's an interactive worker that can't review.
- **Operable headless:** a web UI with login that binds to loopback by default. AO has no web UI on a server, and its API has no auth.
- **Escalation that survives a restart:** `blocked` and `in_review` statuses plus comments, where AO has in-memory messages to an orchestrator session.
- **Maturity:** about 50k stars, an active adapter surface, and bugs already worked around (agy's print-timeout, a stream regression). AO's npm release lags its repo by three minor versions, and the module path moved organisations.

AO's one real advantage is that it reads GitHub Issues directly, but only by assignee. Every other requirement would push `fleet` into calling `ao spawn` for each issue with its own label, dependency, routing and pause logic. That makes `fleet` the dispatcher, which is the one thing it must not become.

## What changes in fleet

Multica doesn't use GitHub Issues as its backlog. Adopting it changes `fleet`'s model:

1. **The backlog moves into Multica.** GitHub keeps code, PRs, CI and branch protection.
   - `fleet issues sync` creates Multica issues (`multica issue create`) instead of GitHub issues.
   - **Routing happens once, when an issue is created:** `fleet` assigns each issue to the Multica agent for its wave (a deterministic lookup from `fleet.yaml`). After that, Multica decides when it runs, within each agent's concurrency limit.
2. **Escalation labels become Multica statuses and comments.** `needs-human`, `needs-resource` and `needs-contract` map to `blocked` plus a structured comment (the shape `AGENTS.md` already defines); `agent-stuck` maps to `blocked`; hand-back maps to `in_review`. Telegram notifications would come from Multica events rather than the GitHub label workflow; **how is unverified.**
3. **The gate becomes CI plus instructions.** Required GitHub checks enforce the gate on merge. `max_gate_attempts` becomes an `AGENTS.md` rule ("after N failed gate runs, set `blocked`"), not something the orchestrator enforces.
4. **Pause.**
   - `fleet pause --hard` = `multica daemon stop`.
   - `fleet kill <issue>` = cancel that issue's task.
   - A soft pause (finish running work, start nothing new) has no direct equivalent. Candidate: reassign waiting issues to backlog, or accept the daemon stop's 30 s grace. **Needs a decision.**
5. **Cross-vendor review is configured, not enforced.** `fleet` would set up review delegation (a squad whose leader delegates reviews to a different-vendor agent, or a reviewer agent per implementer vendor) and check the pairing in `config.validate`, as it does today.
6. **Config changes.** `orchestrator.kind: multica`. `orchestrator init` writes a compose override (ports bound to the Tailscale IP, `JWT_SECRET` and `ALLOWED_EMAILS` from the secrets file) and creates the runtimes and agents from `profiles`. `agent-orchestrator.yaml.tmpl` is deleted.

## Open questions for the owner

1. **Dependencies.** Multica has an `issue_dependency` table (`blocks`, `blocked_by`, `related`, `migrations/001_init.up.sql:89-93`) that no query reads (the only reference is workspace delete, `pkg/db/queries/workspace_delete.sql:354`). "Don't start B until A is done" isn't enforced. Options:
   - (a) `issues sync` creates dependent issues in `backlog`, and you move them forward. Manual, and keeps fleet out of scheduling.
   - (b) Sync one wave at a time.
   - (c) `fleet` promotes issues when their blockers close. That is scheduling, which the brief rules out.

   Recommended: (b), with (a) inside a wave.
2. **Backlog outside GitHub.** Planning and escalations move to Multica's board, and GitHub issues stop being the source of truth. Acceptable?
3. **License.** Multica's extra conditions allow internal use within one organisation, but forbid offering it as a hosted service to third parties and removing its branding. `fleet` only automates your own self-hosted instance and doesn't redistribute Multica, so this looks fine for an MIT `fleet`. Confirm you're comfortable with it.
4. **Footprint.** Docker, Postgres, server, frontend and daemon on the same box as about six agents. Probably fine on 8 vCPU / 32 GB; to be measured in step 4.
5. **Soft pause:** see change 4 above.

If you'd rather keep GitHub Issues as the backlog even though it means `fleet` doing dispatch through `ao spawn`, AO is the only option of the two. This document recommends against that.
