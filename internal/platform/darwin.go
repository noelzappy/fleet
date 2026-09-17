package platform

import (
	"fmt"
	"strings"
	"time"

	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/templates"
)

// Darwin: macOS 14+ on Apple silicon, Homebrew, OrbStack for Docker, launchd
// LaunchAgents. The fleet user must stay logged in (automatic login) because the
// agent CLIs keep their tokens in the login keychain; see Notes.
type Darwin struct{}

func (Darwin) Name() string { return "darwin" }

// brew resolves Homebrew before it is on PATH (first run) — Apple silicon or Intel.
const brew = `$(test -x /opt/homebrew/bin/brew && echo /opt/homebrew/bin/brew || echo /usr/local/bin/brew)`

func (Darwin) BootstrapSteps(m config.Machine, root string) []Step {
	steps := []Step{
		{
			Name:  "homebrew",
			Check: "test -x /opt/homebrew/bin/brew -o -x /usr/local/bin/brew",
			Apply: `NONINTERACTIVE=1 /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"`,
		},
		// bash login shells on macOS read ~/.bash_profile INSTEAD of ~/.profile when it
		// exists; fleet's commands and the launchd wrappers run `bash -lc`, so that is
		// the file. ~/.zprofile gets the same block for interactive zsh.
		profileStep("~/.bash_profile", `eval "$(`+brew+` shellenv)"\n`),
		profileStep("~/.zprofile", `eval "$(`+brew+` shellenv)"\n`),
		{
			Name:  "brew packages",
			Check: brew + " list --formula git tmux jq gh fnm >/dev/null 2>&1",
			Apply: brew + " install git tmux jq gh fnm",
		},
	}
	if m.Docker {
		steps = append(steps, Step{
			// OrbStack: brew-installable, starts at login, honours per-IP port publishes.
			Name:  "docker (OrbStack)",
			Check: "docker info >/dev/null 2>&1",
			Apply: brew + " install --cask orbstack && open -a OrbStack && for i in $(seq 1 60); do docker info >/dev/null 2>&1 && exit 0; sleep 2; done; echo 'docker did not come up — open OrbStack once to finish its setup' >&2; exit 1",
		})
	}
	steps = append(steps, nodeSteps(m, "$("+brew+" --prefix)/bin/fnm")...)
	steps = append(steps, dirsStep(root, []string{"~/Library/LaunchAgents", "~/Library/Logs/fleet"}, "stat -f %Lp"))
	if m.AlwaysOn {
		steps = append(steps, Step{
			Name:  "never sleep",
			Check: `pmset -g 2>/dev/null | grep -Eq '^\s*sleep\s+0\b'`,
			Apply: "sudo pmset -a sleep 0 disksleep 0",
		})
	}
	if m.Tailscale {
		steps = append(steps,
			Step{Name: "tailscale", Check: "command -v tailscale >/dev/null", Apply: brew + " install tailscale"},
			// The CLI build's tailscaled as a root LaunchDaemon (the App Store app needs a GUI session).
			Step{Name: "tailscaled", Check: "pgrep -x tailscaled >/dev/null", Apply: "sudo " + brew + " services start tailscale && sleep 3"},
			Step{Name: "tailscale up", Check: "tailscale status >/dev/null 2>&1", Apply: "sudo tailscale up"},
		)
	}
	if m.Firewall {
		fw := "/usr/libexec/ApplicationFirewall/socketfilterfw"
		steps = append(steps,
			Step{
				// The application firewall has no per-interface rules like ufw's; it blocks
				// unsolicited inbound to apps that aren't allowed. Docker's published ports
				// are bound to the Tailscale IP by fleet, so nothing listens publicly.
				Name:  "application firewall on, stealth mode",
				Check: "sudo " + fw + " --getglobalstate | grep -q enabled && sudo " + fw + " --getstealthmode | grep -q enabled",
				Apply: "sudo " + fw + " --setglobalstate on && sudo " + fw + " --setstealthmode on",
			},
			Step{
				Name:  "remote login (sshd) on",
				Check: "sudo systemsetup -getremotelogin 2>/dev/null | grep -q On",
				Apply: "sudo systemsetup -setremotelogin on",
			},
		)
		steps = append(steps, sshSteps("sudo launchctl kickstart -k system/com.openssh.sshd")...)
	}
	return append(steps, turboStep(m)...)
}

func (Darwin) Notes(config.Machine) []string {
	return []string{
		"Enable automatic login for this user (System Settings → Users & Groups → Automatically log in as). LaunchAgents and the agent CLIs' keychain tokens need a logged-in session after a reboot.",
		"macOS's firewall has no interface rules; the Multica UI/API are safe because fleet binds them to the Tailscale IP only. Don't publish other ports on 0.0.0.0.",
	}
}

func (Darwin) ServiceFiles(spec Spec) ([]File, error) {
	var files []File
	for _, job := range []Job{spec.Daemon, spec.Sync} {
		data := struct {
			Job
			Seconds int
			Log     string
		}{Job: job, Log: config.ExpandPath("~/Library/Logs/fleet/" + job.Name + ".log")}
		data.Exec = strings.Replace(data.Exec, "~/", "$HOME/", 1)
		if job.Interval != "" {
			d, err := time.ParseDuration(job.Interval)
			if err != nil {
				return nil, fmt.Errorf("%s interval: %w", job.Name, err)
			}
			data.Seconds = int(d.Seconds())
		}
		b, err := templates.Render("launchd.plist.tmpl", data)
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: plistPath(job.Name), Data: b, Mode: 0o644})
	}
	return files, nil
}

func plistPath(label string) string {
	return config.ExpandPath("~/Library/LaunchAgents/" + label + ".plist")
}

// ReloadCmd loads any plist launchd doesn't know yet. launchd never re-reads a
// loaded plist; a changed job needs `fleet pause --hard && fleet up`.
func (d Darwin) ReloadCmd(spec Spec) string {
	var parts []string
	for _, job := range d.Jobs(spec) {
		parts = append(parts, fmt.Sprintf("(launchctl print gui/$(id -u)/%s >/dev/null 2>&1 || launchctl bootstrap gui/$(id -u) %s)", job, quote(plistPath(job))))
	}
	return strings.Join(parts, " && ")
}

func (Darwin) Jobs(spec Spec) []string { return []string{spec.Daemon.Name, spec.Sync.Name} }

func (Darwin) StartCmd(jobs ...string) string {
	var parts []string
	for _, j := range jobs {
		parts = append(parts, fmt.Sprintf("(launchctl print gui/$(id -u)/%s >/dev/null 2>&1 || launchctl bootstrap gui/$(id -u) %s) && launchctl enable gui/$(id -u)/%s && launchctl kickstart gui/$(id -u)/%s",
			j, quote(plistPath(j)), j, j))
	}
	return strings.Join(parts, " && ")
}

// StopCmd unloads the jobs (bootout), which also stops a KeepAlive daemon for good.
func (Darwin) StopCmd(jobs ...string) string {
	var parts []string
	for _, j := range jobs {
		parts = append(parts, fmt.Sprintf("launchctl bootout gui/$(id -u)/%s 2>/dev/null || true", j))
	}
	return strings.Join(parts, "; ")
}

func (Darwin) IsActiveCmd(job string) string {
	return fmt.Sprintf(`if launchctl print gui/$(id -u)/%s 2>/dev/null | grep -q 'state = running'; then echo active; elif launchctl print gui/$(id -u)/%s >/dev/null 2>&1; then echo loaded; else echo inactive; fi`, job, job)
}

func (Darwin) ActiveCheck(job string) string {
	return fmt.Sprintf("launchctl print gui/$(id -u)/%s >/dev/null 2>&1", job)
}
