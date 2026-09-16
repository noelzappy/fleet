package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/noelzappy/fleet/internal/shell"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// fleet issues sync <file.yaml> — bulk create; resolve depends_on titles → #numbers in a second pass.
//
//   - title: "[ADMIN] wallet freeze/unfreeze"
//     labels: [agent-ready, wave:admin]
//     body: |
//     ...
//     depends_on: ["[CONTRACTS] admin wallets surface"]
type issueSpec struct {
	Title     string   `yaml:"title"`
	Labels    []string `yaml:"labels"`
	Body      string   `yaml:"body"`
	DependsOn []string `yaml:"depends_on"`
}

func issuesCmd() *cobra.Command {
	c := &cobra.Command{Use: "issues", Short: "Bulk-create issues from YAML with dependency resolution"}
	c.AddCommand(&cobra.Command{Use: "sync <file>", Args: cobra.ExactArgs(1), RunE: issuesSync})
	return c
}

func issuesSync(_ *cobra.Command, a []string) error {
	b, err := os.ReadFile(a[0])
	if err != nil {
		return err
	}
	var specs []issueSpec
	if err := yaml.Unmarshal(b, &specs); err != nil {
		return err
	}
	// Check dependencies before creating anything, so a typo doesn't leave half-created issues.
	titles := map[string]bool{}
	for _, s := range specs {
		titles[s.Title] = true
	}
	for _, s := range specs {
		for _, t := range s.DependsOn {
			if !titles[t] {
				return fmt.Errorf("%q depends on unknown %q", s.Title, t)
			}
		}
	}
	ctx := context.Background()
	R := shell.Quote(cfg.Project.Repo)
	numbers := map[string]string{}
	for _, s := range specs {
		out, err := shell.Output(ctx, fmt.Sprintf(`gh issue create -R %s --title %s --label %s --body %s`,
			R, shell.Quote(s.Title), shell.Quote(strings.Join(s.Labels, ",")), shell.Quote(s.Body)))
		if err != nil {
			return fmt.Errorf("create %q: %w", s.Title, err)
		}
		n, err := issueNumber(out)
		if err != nil {
			return fmt.Errorf("create %q: %w", s.Title, err)
		}
		numbers[s.Title] = n
		fmt.Println("created", s.Title, "→ #"+n)
	}
	for _, s := range specs {
		if len(s.DependsOn) == 0 {
			continue
		}
		body := dependsBody(s.Body, s.DependsOn, numbers)
		if _, err := shell.Output(ctx, fmt.Sprintf(`gh issue edit -R %s %s --body %s`, R, numbers[s.Title], shell.Quote(body))); err != nil {
			return err
		}
	}
	return nil
}

// issueNumber extracts N from the issue URL gh prints. Under --dry-run there is no URL,
// so a placeholder keeps the second pass printable.
func issueNumber(ghOut string) (string, error) {
	if shell.DryRun {
		return "N", nil
	}
	n := ghOut[strings.LastIndex(ghOut, "/")+1:]
	if _, err := strconv.Atoi(n); err != nil {
		return "", fmt.Errorf("unexpected gh output %q", ghOut)
	}
	return n, nil
}

func dependsBody(body string, deps []string, numbers map[string]string) string {
	var b strings.Builder
	b.WriteString(body)
	b.WriteString("\n\n## Depends on\n")
	for _, t := range deps {
		b.WriteString("- #" + numbers[t] + "\n")
	}
	return b.String()
}
