package platform

import (
	"fmt"
	"strings"

	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/templates"
)

// Linux: Ubuntu 24.04, apt, ufw, systemd user units.
type Linux struct{}

func (Linux) Name() string { return "linux" }

func (Linux) BootstrapSteps(m config.Machine, root string) []Step {
	aptPkgs := "git tmux curl jq build-essential ufw fail2ban"
	steps := []Step{{
		Name:  "apt packages",
		Check: "dpkg -s " + aptPkgs + " >/dev/null 2>&1",
		Apply: "sudo apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq " + aptPkgs,
	}}
	if m.Docker {
		steps = append(steps,
			Step{
				Name:  "docker",
				Check: "command -v docker >/dev/null && systemctl is-active --quiet docker",
				Apply: "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker.io docker-compose-v2 && sudo systemctl enable --now docker",
			},
			Step{
				// Takes effect at the next login; bootstrap tells you to reconnect.
				Name:  "docker group membership",
				Check: `getent group docker | cut -d: -f4 | tr , '\n' | grep -qx "$USER"`,
				Apply: `sudo usermod -aG docker "$USER" && echo "added $USER to docker group — log out and back in before harness verify"`,
			},
		)
	}
	fnm := FnmDir + "/fnm"
	steps = append(steps,
		Step{
			Name:  "fnm",
			Check: "test -x " + fnm,
			Apply: "curl -fsSL https://fnm.vercel.app/install | bash -s -- --install-dir " + FnmDir + " --skip-shell",
		},
		// Ubuntu's ~/.bashrc returns early for non-interactive shells, so PATH for
		// `bash -lc` (every fleet command) and for systemd has to come from ~/.profile.
		profileStep("~/.profile", ""),
	)
	steps = append(steps, nodeSteps(m, fnm)...)
	steps = append(steps,
		Step{
			Name:  "gh",
			Check: "command -v gh >/dev/null",
			Apply: `curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | sudo dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg status=none && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" | sudo tee /etc/apt/sources.list.d/github-cli.list >/dev/null && sudo apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq gh`,
		},
		dirsStep(root, "~/.config/systemd/user", "stat -c %a"),
	)
	if m.Tailscale {
		steps = append(steps,
			Step{Name: "tailscale", Check: "command -v tailscale >/dev/null", Apply: "curl -fsSL https://tailscale.com/install.sh | sh"},
			// Prints a login URL and waits until you open it.
			Step{Name: "tailscale up", Check: "tailscale status >/dev/null 2>&1", Apply: "sudo tailscale up"},
		)
	}
	if m.Firewall {
		rules := []string{"sudo ufw default deny incoming", "sudo ufw default allow outgoing", "sudo ufw allow OpenSSH"}
		check := []string{`s=$(sudo ufw status verbose)`, `grep -q 'Status: active' <<<"$s"`, `grep -q 'deny (incoming)' <<<"$s"`, `grep -q OpenSSH <<<"$s"`}
		if m.Tailscale {
			rules = append(rules, "sudo ufw allow in on tailscale0")
			check = append(check, `grep -q tailscale0 <<<"$s"`)
		}
		rules = append(rules, "sudo ufw --force enable")
		steps = append(steps, Step{Name: "firewall", Check: strings.Join(check, " && "), Apply: strings.Join(rules, " && ")})
		steps = append(steps, sshSteps("sudo systemctl reload ssh")...)
	}
	return append(steps, turboStep(m)...)
}

func (Linux) Notes(config.Machine) []string { return nil }

func (Linux) ServiceFiles(spec Spec) ([]File, error) {
	var files []File
	for _, f := range []struct {
		tmpl, name string
		job        Job
	}{
		{"systemd.service.tmpl", spec.Daemon.Name + ".service", spec.Daemon},
		{"systemd.service.tmpl", spec.Sync.Name + ".service", spec.Sync},
		{"systemd.timer.tmpl", spec.Sync.Name + ".timer", spec.Sync},
	} {
		job := f.job
		job.Exec = strings.Replace(job.Exec, "~/", "%h/", 1)
		b, err := templates.Render(f.tmpl, job)
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: config.ExpandPath("~/.config/systemd/user/" + f.name), Data: b, Mode: 0o644})
	}
	return files, nil
}

func (Linux) ReloadCmd(Spec) string {
	return "systemctl --user daemon-reload && loginctl enable-linger $USER"
}

func (Linux) Jobs(spec Spec) []string { return []string{spec.Daemon.Name, spec.Sync.Name + ".timer"} }

func (Linux) StartCmd(jobs ...string) string {
	return "systemctl --user enable --now " + strings.Join(quoteAll(jobs), " ")
}

func (Linux) StopCmd(jobs ...string) string {
	return "systemctl --user stop " + strings.Join(quoteAll(jobs), " ")
}

func (Linux) IsActiveCmd(job string) string {
	return "systemctl --user is-active " + quote(job) + " || true"
}

func (Linux) ActiveCheck(job string) string {
	return "systemctl --user is-active --quiet " + quote(job)
}

// profileStep puts fnm's default node and ~/.local/bin on PATH for login shells.
// extra is prepended inside the block (macOS adds Homebrew's shellenv).
func profileStep(profile, extra string) Step {
	block := fmt.Sprintf(`\n%s\n%sexport FNM_DIR="%s"\nexport PATH="$FNM_DIR/aliases/default/bin:$HOME/.local/bin:$PATH"\n`, ProfileMarker, extra, FnmDir)
	return Step{
		Name:  "node on PATH for login shells (" + profile + ")",
		Check: "grep -qxF " + quote(ProfileMarker) + " " + profile,
		Apply: "printf " + quote(block) + " >> " + profile,
	}
}

// dirsStep creates the fleet directories and the 0600 secrets file. statMode is the
// platform's "print octal mode" command.
func dirsStep(root, serviceDir, statMode string) Step {
	return Step{
		Name:  "fleet directories and secrets file",
		Check: fmt.Sprintf(`test -d ~/.config/fleet/profiles && test -d %s && test -d %s && test "$(%s %s 2>/dev/null)" = 600`, serviceDir, quote(root), statMode, config.SecretsFile),
		Apply: fmt.Sprintf(`mkdir -p ~/.config/fleet/profiles ~/.local/bin %s %s && touch %s && chmod 600 %s`, serviceDir, quote(root), config.SecretsFile, config.SecretsFile),
	}
}
