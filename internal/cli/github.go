package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/shell"
	"github.com/noelzappy/fleet/internal/templates"
	"github.com/spf13/cobra"
)

// fleet github init — labels, notify workflow, secrets check, App guidance.
func githubCmd() *cobra.Command {
	c := &cobra.Command{Use: "github", Short: "Labels, workflows, and the GitHub App agents authenticate with"}
	c.AddCommand(&cobra.Command{Use: "init", Short: "labels, pr-contract check, notify workflow", RunE: githubInit}, githubAppCmd(), githubTokenCmd())
	return c
}

func githubInit(cmd *cobra.Command, _ []string) error {
	ctx := context.Background()
	// Ordered slice, not a map: dry-run output must be stable across runs.
	L := cfg.Labels
	labels := []struct{ name, color string }{
		{L.Ready, "0E8A16"}, {L.Assist, "FBCA04"}, {L.HumanRequired, "B60205"},
		{L.NeedsHuman, "D93F0B"}, {L.NeedsResource, "D93F0B"}, {L.NeedsContract, "D93F0B"},
		{L.BlockedBy, "5319E7"}, {L.Stuck, "B60205"}, {L.Paused, "000000"},
	}
	for _, w := range cfg.Waves {
		labels = append(labels, struct{ name, color string }{"wave:" + w.Name, "C5DEF5"})
	}
	for _, l := range labels {
		if err := shell.Run(ctx, fmt.Sprintf(`gh label create %s --color %s --force -R %s`, shell.Quote(l.name), l.color, shell.Quote(cfg.Project.Repo)), nil); err != nil {
			return err
		}
	}
	// pr-contract: the required check that enforces the PR body contract and the
	// cross-vendor review rule (docs/orchestrator-decision.md › Closing the gaps).
	vendors := map[string]string{}
	for name, p := range cfg.Profiles {
		vendors[name] = p.Vendor
	}
	vj, _ := json.Marshal(vendors)
	pc, err := templates.Render("pr-contract.yml.tmpl", struct {
		*config.Fleet
		VendorsJSON string
	}{cfg, string(vj)})
	if err != nil {
		return err
	}
	if err := shell.WriteFile(filepath.Join(cfg.Project.Root, ".github/workflows/pr-contract.yml"), pc, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "commit .github/workflows/pr-contract.yml via a PR and add pr-contract to the branch's required checks")
	if cfg.Notify.Telegram != nil {
		b, err := templates.Render("fleet-notify.yml.tmpl", cfg)
		if err != nil {
			return err
		}
		dst := filepath.Join(cfg.Project.Root, ".github/workflows/fleet-notify.yml")
		if err := shell.WriteFile(dst, b, 0o644); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "commit", dst, "via a PR")
		fmt.Fprintf(os.Stderr, "ensure repo secrets exist: %s, %s\n", cfg.Notify.Telegram.TokenSecret, cfg.Notify.Telegram.ChatSecret)
	}
	if cfg.GitHub.Auth != "app" {
		fmt.Fprintln(os.Stderr, "github.auth is gh: agents use this box's gh login. Before a client repo: set github.auth: app, then fleet github app create && fleet github app use")
	}
	return nil
}
