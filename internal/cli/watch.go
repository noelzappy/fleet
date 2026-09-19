package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
	"github.com/noelzappy/fleet/internal/shell"
	"github.com/noelzappy/fleet/internal/ui"
	"github.com/spf13/cobra"
)

// fleet watch — a live, read-only dashboard. It reuses the `sync` observation and planner,
// so what it shows (including "next sync tick") is what the reconciler sees, not a second
// opinion. It never writes: no labels, no comments, no cooldown file.
func watchCmd() *cobra.Command {
	var interval time.Duration
	var once bool
	c := &cobra.Command{
		Use:   "watch",
		Short: "live terminal dashboard: issues, PRs, agents, harness health and what sync will do next",
		Long: `Refreshes every --interval (default 10s; each refresh makes several gh and multica
calls, so don't go below a few seconds). Read-only.

  tab / shift-tab  switch pane      ↑ ↓ / j k  scroll      z  zoom the pane
  l                sync ⇄ daemon log   v  show command traces   r  refresh   ?  help   q  quit

With --once (or when stdout isn't a terminal) it prints one plain snapshot and exits, which
is what to paste into an issue or pipe to a file.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if interval < 2*time.Second {
				return fmt.Errorf("--interval %s is too short: each refresh makes several gh and multica calls; use 2s or more", interval)
			}
			// A full-screen UI can't have child processes or fleet's own traces writing to
			// stderr underneath it.
			shell.Silent = true
			if once || !ui.IsTerminal(os.Stdout) {
				return watchOnce(cmd.Context(), interval)
			}
			m := newWatchModel(interval)
			_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
			return err
		},
	}
	c.Flags().DurationVar(&interval, "interval", 10*time.Second, "how often to refresh")
	c.Flags().BoolVar(&once, "once", false, "print one plain snapshot and exit")
	return c
}

func watchOnce(ctx context.Context, interval time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s, err := collect(ctx)
	if err != nil {
		return err
	}
	w := 110 // a pipe or file has no width; wide enough for the issue table
	if cols, _, err := term.GetSize(os.Stdout.Fd()); err == nil && cols >= 60 {
		w = cols
	}
	fmt.Println(render(s, viewState{W: w, Interval: interval, Now: time.Now()}, ui.Out, true))
	return nil
}

type (
	tickMsg time.Time
	snapMsg struct {
		s   *snapshot
		err error
	}
)

type watchModel struct {
	viewState
	snap *snapshot
	help bool
}

func newWatchModel(interval time.Duration) *watchModel {
	// 80x24 until the terminal reports its size; some ptys report 0x0 and never improve on it.
	return &watchModel{viewState: viewState{W: 80, H: 24, Interval: interval, Loading: true}}
}

func fetchCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		s, err := collect(ctx)
		return snapMsg{s, err}
	}
}

func (m *watchModel) tickCmd() tea.Cmd {
	return tea.Tick(m.Interval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *watchModel) Init() tea.Cmd { return tea.Batch(fetchCmd(), m.tickCmd()) }

func (m *watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if msg.Width > 0 && msg.Height > 0 {
			m.W, m.H = msg.Width, msg.Height
		}
	case tickMsg:
		if m.Loading { // a slow refresh: skip this tick rather than stack calls
			return m, m.tickCmd()
		}
		m.Loading = true
		return m, tea.Batch(fetchCmd(), m.tickCmd())
	case snapMsg:
		m.Loading = false
		m.Err = msg.err
		if msg.err == nil {
			m.snap = msg.s
		}
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m *watchModel) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	page := max(1, m.H/3)
	switch k.String() {
	case "q", "ctrl+c", "esc":
		if m.help {
			m.help = false
			return m, nil
		}
		return m, tea.Quit
	case "?":
		m.help = !m.help
	case "r":
		if !m.Loading {
			m.Loading = true
			return m, fetchCmd()
		}
	case "tab":
		m.Focus = (m.Focus + 1) % numPanes
	case "shift+tab":
		m.Focus = (m.Focus + numPanes - 1) % numPanes
	case "down", "j":
		m.scroll(1)
	case "up", "k":
		m.scroll(-1)
	case "pgdown", "ctrl+d":
		m.scroll(page)
	case "pgup", "ctrl+u":
		m.scroll(-page)
	case "g", "home":
		if m.Focus == paneLog {
			m.LogBack = 1 << 30 // clamped to the oldest line
		} else {
			m.Off[m.Focus] = 0
		}
	case "G", "end":
		if m.Focus == paneLog {
			m.LogBack = 0 // follow the tail again
		} else {
			m.Off[m.Focus] = 1 << 30 // clamped to the last page
		}
	case "z":
		m.Zoom = !m.Zoom
	case "l":
		m.LogSel = 1 - m.LogSel
		m.LogBack = 0
	case "v":
		m.Verbose = !m.Verbose
		m.LogBack = 0
	}
	return m, nil
}

// scroll moves the focused pane by n lines (positive = down). The log's position is its
// distance from the newest line, so scrolling down toward it shrinks LogBack.
func (m *watchModel) scroll(n int) {
	if m.Focus == paneLog {
		m.LogBack = max(0, m.LogBack-n)
		return
	}
	m.Off[m.Focus] = max(0, m.Off[m.Focus]+n)
}

func (m *watchModel) View() string {
	p := ui.Out
	if m.help {
		return strings.Join(header(m.snap, m.viewState, p), "\n") + "\n" + helpText(p)
	}
	return render(m.snap, m.viewState, p, false) + "\n" + footer(p, m.viewState)
}
