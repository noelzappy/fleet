package cli

import (
	"context"

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
			ctx := context.Background()
			steps := []string{
				`sudo apt-get update -qq && sudo apt-get install -y -qq git tmux curl jq build-essential ufw fail2ban`,
				`command -v docker >/dev/null || (sudo apt-get install -y -qq docker.io && sudo usermod -aG docker $USER)`,
				`command -v fnm >/dev/null || curl -fsSL https://fnm.vercel.app/install | bash`,
				`fnm install ` + shell.Quote(cfg.Machine.NodeVersion) + ` && fnm default ` + shell.Quote(cfg.Machine.NodeVersion),
				`command -v pnpm >/dev/null || npm i -g ` + shell.Quote("pnpm@"+cfg.Machine.Pnpm),
				`command -v gh >/dev/null || (curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | sudo dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" | sudo tee /etc/apt/sources.list.d/github-cli.list && sudo apt-get update -qq && sudo apt-get install -y -qq gh)`,
				`mkdir -p ~/.config/fleet/profiles ~/.local/bin ~/.config/systemd/user ` + shell.Quote(cfg.Project.Root) + ` && touch ~/.config/fleet/env && chmod 600 ~/.config/fleet/env`,
			}
			if cfg.Machine.Tailscale {
				steps = append(steps, `command -v tailscale >/dev/null || (curl -fsSL https://tailscale.com/install.sh | sh)`, `sudo tailscale up || true`)
			}
			if cfg.Machine.Firewall {
				steps = append(steps,
					`sudo ufw default deny incoming && sudo ufw default allow outgoing && sudo ufw allow OpenSSH && sudo ufw allow in on tailscale0 && sudo ufw --force enable`,
					`sudo sed -i 's/^#\?PasswordAuthentication .*/PasswordAuthentication no/' /etc/ssh/sshd_config && sudo systemctl reload ssh`,
				)
			}
			if cfg.Machine.TurboRemote != nil {
				steps = append(steps, `grep -q '^TURBO_TEAM=' ~/.config/fleet/env || echo `+shell.Quote("TURBO_TEAM="+cfg.Machine.TurboRemote.Team)+` >> ~/.config/fleet/env`)
			}
			for _, s := range steps {
				if err := shell.Run(ctx, s, nil); err != nil {
					return err
				}
			}
			cmd.Println("bootstrap complete. Next: fleet harness add <name> for each harness in fleet.yaml")
			return nil
		},
	}
}
