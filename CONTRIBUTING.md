# Contributing to fleet

Thanks for helping. This file covers the rules the codebase holds to, how to develop and test, and where things stand. Start with the [README](README.md) for what fleet is.

## Design rules

These are what keep `fleet` safe to run against a real repo and a real server. A PR that breaks one needs a very good reason in its description.

1. **Wrap the orchestrator; never become one.** fleet configures and runs Multica, manages labels, issues and PRs through `gh`, and controls the service through systemd. `fleet sync` decides only what is *eligible* (labels, dependencies, pause, attempts, which profile). It never decides which session runs next, never manages sessions, and never retries agent work (the single exception: runs the server cancelled during a daemon restart). If a change to `internal/fleetsync` would pick *the next thing to run*, it belongs in Multica.
2. **Every command is idempotent.** Running a command again on an already-configured machine changes nothing. Check before acting: `command -v x || install x`, `grep -q || append`, `--force` on label creation.
3. **All side effects go through `internal/shell`.** Commands use `shell.Run`, `shell.Output` or `shell.Interactive`; files are written with `shell.WriteFile`. That is what makes `--dry-run` trustworthy: it prints every command and write and touches nothing. Don't import `os/exec` or call `os.WriteFile` anywhere else.
4. **Quote everything you interpolate.** Any value that reaches a shell string (config values, issue titles and bodies, paths, prompts) goes through `shell.Quote`. Go's `%q` is *not* shell quoting: bash still expands `$`, backticks and `\` inside it.
5. **Project-specific things live in `fleet.yaml`, not in Go.** A new knob is a field on `config.Fleet` with a default in `ApplyDefaults`, a line in `fleet.example.yaml`, and a row in the README configuration table.
6. **Check the live tool before encoding its behaviour.** Agent CLIs and orchestrators change their flags and config keys often. Before writing a flag or config key, run the installed binary's `--help`, `config-help` or schema. Where behaviour hasn't been confirmed yet, the code says `TODO(implementer)`; resolve it or turn it into a documented limitation.
7. **Secrets only in `~/.config/fleet/env` and profile env files (both `0600`).** `fleet.yaml` holds `${VAR}` references, expanded at runtime by `config.ExpandEnv`. Never log a resolved value.
8. **Small dependency surface.** Go stdlib, `cobra`, `yaml.v3`. Adding a dependency needs a reason in the commit message.

## Development

Requires Go 1.23+.

```bash
make build     # ./bin/fleet
make test      # go test ./...
make lint      # go vet ./...
./bin/fleet --dry-run -c fleet.example.yaml bootstrap
```

Layout:

```
cmd/fleet/              main
internal/cli/           one file per command group (cobra); kinds.go holds per-harness CLI knowledge; sync.go observes and applies
internal/config/        fleet.yaml types, defaults, validation, ${VAR} expansion
internal/fleetsync/     the pure planner behind `fleet sync` (rules in docs/orchestrator-decision.md › Closing the gaps)
internal/platform/      Linux (apt, ufw, systemd) and macOS (Homebrew, OrbStack, launchd) bootstrap steps and service commands
internal/shell/         the only place commands run and files are written
internal/templates/     embedded files fleet renders (Multica env + compose override, systemd units, workflows, AGENTS.md, issue template)
docs/                   decisions
fleet.example.yaml      annotated reference config (kept identical to internal/templates/files/fleet.example.yaml by a test)
```

### What runs where

The fleet box is Linux; you'll probably develop on something else.

- **Anywhere:** build, unit tests, every `--dry-run`, template rendering, `init`, `issues sync` against a scratch repo, and `harness add` / `verify` / `update` with the agent CLIs installed locally: Claude Code (`claude`), OpenCode (`opencode`) and Antigravity CLI (`agy`, which replaced Gemini CLI). Read their real `--help` before changing `internal/cli/kinds.go`. `harness verify` also needs Docker if the gate does.
- **Box only (Linux or macOS):** `bootstrap`, `orchestrator init/run`, `up`, `pause --hard`, `resume`, `panic`. Everything OS-specific — package steps and the service manager (systemd user units vs launchd LaunchAgents) — lives in `internal/platform`; the commands call `requireBox` and get a `platform.Platform`. Under `--dry-run`, `FLEET_PLATFORM=linux|darwin` previews the other OS. Adding a platform means implementing that interface and its tests; nothing in `internal/cli` should branch on `runtime.GOOS`.

### Testing Linux commands on a box

Use a disposable VPS you can reinstall. `bootstrap` changes the firewall and SSH config.

```bash
make deploy VPS=user@host     # cross-compile linux/amd64, upload beside ~/.local/bin/fleet and rename over it (safe while the daemon runs)
make install                  # on a Mac that is itself the box
ssh user@host 'cd ~/proj && fleet --dry-run bootstrap && fleet bootstrap'
ssh user@host 'cd ~/proj && fleet bootstrap'   # second run must change nothing
```

Make sure SSH key login works before running `bootstrap` with `machine.firewall: true`; it turns off password authentication.

### Tests

- Tests are table-driven. Config tests load `fleet.example.yaml`, so changing the example means updating `internal/config/config_test.go`.
- Keep parsing and decision logic in pure functions (see `countLabels`, `dependsBody`, `resolveVar`) so tests need no `gh`, network or box.
- Anything that builds shell strings from user text needs a case with `$`, backticks, quotes and newlines.

## Roadmap to v0.1

Each step: implement → `make test` → run it for real (locally or on a box) → update the README where behaviour differs → commit.

1. ~~`config`: `${VAR}` expansion, `ApplyDefaults`, tests, Linux guard~~
2. ~~`bootstrap` on a fresh Ubuntu 24.04 box and on a Mac; the second run is a no-op~~
3. ~~`harness add/login/verify`: every harness runs the gate headless in a worktree; document sign-in over SSH~~
4. ~~orchestrator (per [docs/orchestrator-decision.md](docs/orchestrator-decision.md)): `orchestrator init/run` against a live Multica on the box; `sync` end to end; dashboard reachable over Tailscale only~~ (still to check on Linux: `pause --hard` / `resume` from a fresh SSH session; verified on macOS)
5. `github init`: labels, notify workflow, GitHub App manifest flow; Telegram fires on a test label
6. `issues sync` against a scratch repo with dependencies
7. `kill`, `panic`, `status` (active sessions), `digest`
8. `init` end to end: scaffold an empty repo and run the whole chain from it

## Commits and PRs

- One logical change per commit. Explain *why* in the body, especially for anything touching shell strings, secrets or idempotency.
- `make test` and `make lint` pass.
- If a command's behaviour, flags or config changed, update the README in the same PR. The README and the CLI must agree.
