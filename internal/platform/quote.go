package platform

import "github.com/noelzappy/fleet/internal/shell"

func quote(s string) string { return shell.Quote(s) }

func quoteAll(list []string) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = shell.Quote(s)
	}
	return out
}
