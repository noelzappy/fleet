package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/shell"
	"github.com/noelzappy/fleet/internal/templates"
	"github.com/noelzappy/fleet/internal/ui"
	"github.com/spf13/cobra"
)

// fleet init — scaffold fleet.yaml + governance files into the current repo. Never overwrites.
func initCmd() *cobra.Command {
	var repo string
	c := &cobra.Command{
		Use:   "init",
		Short: "Scaffold fleet.yaml, AGENTS.md skeleton and the issue template into the current repo",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, _ := os.Getwd()
			f := &config.Fleet{Version: 1}
			f.Project.Name = filepath.Base(cwd)
			f.Project.Repo = repo
			f.ApplyDefaults()
			files := map[string]string{
				"AGENTS.md":                            "AGENTS.md.tmpl",
				".github/ISSUE_TEMPLATE/agent-task.md": "agent-task.md.tmpl",
			}
			for dst, name := range files {
				if _, err := os.Stat(dst); err == nil {
					ui.Errf("skip %s (exists)\n", dst)
					continue
				}
				b, err := templates.Render(name, f)
				if err != nil {
					return err
				}
				if err := shell.WriteFile(dst, b, 0o644); err != nil {
					return err
				}
			}
			if _, err := os.Stat("fleet.yaml"); os.IsNotExist(err) {
				ex, _ := templates.FS.ReadFile("files/fleet.example.yaml")
				if err := shell.WriteFile("fleet.yaml", ex, 0o644); err != nil {
					return err
				}
				fmt.Println("edit fleet.yaml, then: fleet bootstrap")
			}
			return nil
		},
	}
	c.Flags().StringVar(&repo, "repo", "", "owner/name")
	return c
}
