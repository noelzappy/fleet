package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/shell"
	"github.com/spf13/cobra"
)

// fleet bootstrap — prepare an Ubuntu box. Idempotent: every step checks before acting.
func bootstrapCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bootstrap",
		Short: "Install OS deps, docker, node/pnpm, gh, tailscale; lock down the box; create dirs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireLinux("bootstrap"); err != nil {
				return err
			}
			if os.Geteuid() == 0 && !shell.DryRun {
				return fmt.Errorf("run bootstrap as the non-root user the fleet will run as, not root — see README › Requirements")
			}
			changed, err := runSteps(context.Background(), bootstrapSteps(cfg.Machine, cfg.Project.Root))
			if err != nil {
				return err
			}
			if changed == 0 && !shell.DryRun {
				cmd.Println("bootstrap: nothing to do")
			}
			cmd.Println("Next: fleet harness add <name> for each harness in fleet.yaml")
			return nil
		},
	}
}

// step is one idempotent unit of setup. check exits 0 when the step is already done;
// apply makes it so. check runs again after apply, so a step that "succeeds" without
// taking effect (a binary that isn't on PATH, a config that's overridden) fails loudly.
type step struct {
	name  string
	check string
	apply string
}

// runSteps applies each step whose check fails and returns how many it changed.
func runSteps(ctx context.Context, steps []step) (changed int, err error) {
	for _, s := range steps {
		if shell.Check(ctx, s.check) {
			fmt.Fprintf(os.Stderr, "✓ %s\n", s.name)
			continue
		}
		fmt.Fprintf(os.Stderr, "● %s\n", s.name)
		if err := shell.Run(ctx, s.apply, nil); err != nil {
			return changed, fmt.Errorf("%s: %w", s.name, err)
		}
		changed++
		if !shell.DryRun && !shell.Check(ctx, s.check) {
			return changed, fmt.Errorf("%s: applied, but the check still fails: %s", s.name, s.check)
		}
	}
	return changed, nil
}

const (
	fnmDir = "$HOME/.local/share/fnm"
	// Ubuntu's ~/.bashrc returns early for non-interactive shells, so PATH for `bash -lc`
	// (every fleet command) and for systemd has to come from ~/.profile.
	profileMarker = "# fleet: node via fnm"
)

func bootstrapSteps(m config.Machine, root string) []step {
	aptPkgs := "git tmux curl jq build-essential ufw fail2ban"
	steps := []step{
		{
			name:  "apt packages",
			check: "dpkg -s " + aptPkgs + " >/dev/null 2>&1",
			apply: "sudo apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq " + aptPkgs,
		},
	}
	if m.Docker {
		steps = append(steps,
			step{
				name:  "docker",
				check: "command -v docker >/dev/null && systemctl is-active --quiet docker",
				apply: "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker.io && sudo systemctl enable --now docker",
			},
			step{
				// Takes effect at the next login; bootstrap tells you to reconnect.
				name:  "docker group membership",
				check: `getent group docker | cut -d: -f4 | tr , '\n' | grep -qx "$USER"`,
				apply: `sudo usermod -aG docker "$USER" && echo "added $USER to docker group — log out and back in before harness verify"`,
			},
		)
	}
	steps = append(steps,
		step{
			name:  "fnm",
			check: "test -x " + fnmDir + "/fnm",
			apply: "curl -fsSL https://fnm.vercel.app/install | bash -s -- --install-dir " + fnmDir + " --skip-shell",
		},
		step{
			name:  "node on PATH for login shells",
			check: "grep -qxF " + shell.Quote(profileMarker) + " ~/.profile",
			apply: fmt.Sprintf(`printf '\n%s\nexport PATH="%s/aliases/default/bin:$HOME/.local/bin:$PATH"\n' >> ~/.profile`, profileMarker, fnmDir),
		},
		step{
			name:  "node " + m.NodeVersion,
			check: "node --version 2>/dev/null | grep -q " + shell.Quote("^v"+m.NodeVersion+`\.`),
			apply: fnmDir + "/fnm install " + shell.Quote(m.NodeVersion) + " && " + fnmDir + "/fnm default " + shell.Quote(m.NodeVersion),
		},
		step{
			name:  "pnpm " + m.Pnpm,
			check: "pnpm --version 2>/dev/null | grep -q " + shell.Quote("^"+m.Pnpm+`\.`),
			apply: "npm i -g " + shell.Quote("pnpm@"+m.Pnpm),
		},
		step{
			name:  "gh",
			check: "command -v gh >/dev/null",
			apply: `curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | sudo dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg status=none && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" | sudo tee /etc/apt/sources.list.d/github-cli.list >/dev/null && sudo apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq gh`,
		},
		step{
			name:  "fleet directories and secrets file",
			check: `test -d ~/.config/fleet/profiles && test -d ~/.config/systemd/user && test -d ` + shell.Quote(root) + ` && test "$(stat -c %a ~/.config/fleet/env 2>/dev/null)" = 600`,
			apply: `mkdir -p ~/.config/fleet/profiles ~/.local/bin ~/.config/systemd/user ` + shell.Quote(root) + ` && touch ~/.config/fleet/env && chmod 600 ~/.config/fleet/env`,
		},
	)
	if m.Tailscale {
		steps = append(steps,
			step{
				name:  "tailscale",
				check: "command -v tailscale >/dev/null",
				apply: "curl -fsSL https://tailscale.com/install.sh | sh",
			},
			step{
				// Prints a login URL and waits until you open it.
				name:  "tailscale up",
				check: "tailscale status >/dev/null 2>&1",
				apply: "sudo tailscale up",
			},
		)
	}
	if m.Firewall {
		ufwRules := []string{"sudo ufw default deny incoming", "sudo ufw default allow outgoing", "sudo ufw allow OpenSSH"}
		ufwCheck := []string{`s=$(sudo ufw status verbose)`, `grep -q 'Status: active' <<<"$s"`, `grep -q 'deny (incoming)' <<<"$s"`, `grep -q OpenSSH <<<"$s"`}
		if m.Tailscale {
			ufwRules = append(ufwRules, "sudo ufw allow in on tailscale0")
			ufwCheck = append(ufwCheck, `grep -q tailscale0 <<<"$s"`)
		}
		ufwRules = append(ufwRules, "sudo ufw --force enable")
		steps = append(steps,
			step{
				name:  "firewall",
				check: strings.Join(ufwCheck, " && "),
				apply: strings.Join(ufwRules, " && "),
			},
			step{
				// Guard for the next step: never disable passwords on a box you can't key into.
				name:  "SSH key installed for this user",
				check: "test -s ~/.ssh/authorized_keys",
				apply: `echo "no ~/.ssh/authorized_keys for $USER — install your key (ssh-copy-id) before bootstrap disables password login" >&2; exit 1`,
			},
			step{
				// A drop-in, not an sshd_config edit: sshd keeps the first value it reads and
				// includes sshd_config.d/*.conf first, so cloud-init's 50-cloud-init.conf would
				// override an edit to the main file. 00- sorts before it.
				name:  "SSH password login disabled",
				check: `sudo sshd -T 2>/dev/null | grep -qx 'passwordauthentication no'`,
				apply: `printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\n' | sudo tee /etc/ssh/sshd_config.d/00-fleet.conf >/dev/null && sudo sshd -t && sudo systemctl reload ssh`,
			},
		)
	}
	if m.TurboRemote != nil {
		steps = append(steps, step{
			name:  "TURBO_TEAM in secrets file",
			check: "grep -q '^TURBO_TEAM=' ~/.config/fleet/env",
			apply: "echo " + shell.Quote("TURBO_TEAM="+m.TurboRemote.Team) + " >> ~/.config/fleet/env",
		})
	}
	return steps
}
