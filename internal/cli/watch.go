package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
	"github.com/noelzappy/fleet/internal/shell"
	"github.com/noelzappy/fleet/internal/ui"
	"github.com/spf13/cobra"
)

// fleet watch — a live dashboard. The board is read-only: it reuses the `sync` observation
// and planner, so what it shows (including "next sync tick") is what the reconciler sees,
// and it never writes. Opening a task adds two things, both explicit: asking a model about
// the task (text in, text out, no tools) and telling the task's agent something (one
// Multica comment, after a confirmation).
func watchCmd() *cobra.Command {
	var interval time.Duration
	var once bool
	c := &cobra.Command{
		Use:   "watch",
		Short: "live terminal dashboard: issues, PRs, agents, harness health; ask about a task or tell its agent something",
		Long: `Refreshes every --interval (default 10s; each refresh makes several gh and multica
calls, so don't go below a few seconds). The board is read-only.

  tab / shift-tab  switch pane      ↑ ↓ / j k  move the cursor / scroll      z  zoom the pane
  enter            open the task under the cursor: its runs, comments, PR and worktree
  l                sync ⇄ daemon log   v  show command traces   r  refresh   ?  help   q  quit

In a task: type a question and press enter to ask (a model answers from what the screen
shows; it has no tools and can't change anything), or press tab to tell the task's agent
something: that posts a comment on its Multica task and wakes it, after you confirm.

With --once (or when stdout isn't a terminal) it prints one plain snapshot and exits.`,
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
			_, err := tea.NewProgram(newWatchModel(interval), tea.WithAltScreen()).Run()
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
	fmt.Println(render(s, &viewState{W: w, Interval: interval, Now: time.Now()}, ui.Out, true))
	return nil
}

type (
	tickMsg time.Time
	snapMsg struct {
		s   *snapshot
		err error
	}
	detailMsg struct {
		td  *taskDetail
		err error
	}
	answerMsg struct {
		text, harness string
		err           error
	}
	sentMsg struct {
		text string
		err  error
	}
	reloadDetailMsg struct{}
)

// watchOps are the calls the task view makes. The model holds them so tests can stand in
// for Multica and the model CLI.
type watchOps struct {
	collect func(ctx context.Context) (*snapshot, error)
	// harness names the CLI a question would go to ("" when none can answer), so the task
	// view can say so before the first question.
	harness func(signedOut map[string]bool) string
	load    func(ctx context.Context, s *snapshot, num int) (*taskDetail, error)
	ask     func(ctx context.Context, signedOut map[string]bool, prompt string) (answer, harness string, err error)
	send    func(ctx context.Context, td *taskDetail, text string) error
}

func realOps() watchOps {
	return watchOps{collect: collect, harness: func(out map[string]bool) string {
		name, _, err := pickAskHarness(out)
		if err != nil {
			return ""
		}
		return name
	}, load: loadTaskDetail, ask: askModel, send: sendFollowUp}
}

type watchMode int

const (
	modeBoard watchMode = iota
	modeTask
)

type watchModel struct {
	viewState
	snap *snapshot
	help bool
	ops  watchOps

	mode         watchMode
	tv           taskView
	input        textinput.Model
	pendingTell  string
	detailLoaded bool // a full detail load has landed for the open task
	loadingTask  bool
}

func newWatchModel(interval time.Duration) *watchModel {
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 4000
	// 80x24 until the terminal reports its size; some ptys report 0x0 and never improve on it.
	return &watchModel{viewState: viewState{W: 80, H: 24, Interval: interval, Loading: true}, ops: realOps(), input: in}
}

func (m *watchModel) fetchCmd() tea.Cmd {
	collect := m.ops.collect
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		s, err := collect(ctx)
		return snapMsg{s, err}
	}
}

func (m *watchModel) loadCmd() tea.Cmd {
	s, num, load := m.snap, m.tv.Detail.Num, m.ops.load
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		td, err := load(ctx, s, num)
		return detailMsg{td, err}
	}
}

func (m *watchModel) askCmd(question string) tea.Cmd {
	td, thread, ask := m.tv.Detail, append([]exchange(nil), m.tv.Thread...), m.ops.ask
	var out map[string]bool
	if m.snap != nil {
		out = m.snap.State.SignedOut
	}
	prompt := askPrompt(td, thread, question, time.Now())
	return func() tea.Msg {
		text, harness, err := ask(context.Background(), out, prompt)
		return answerMsg{text, harness, err}
	}
}

func (m *watchModel) sendCmd(text string) tea.Cmd {
	td, send := m.tv.Detail, m.ops.send
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return sentMsg{text, send(ctx, td, text)}
	}
}

func (m *watchModel) tickCmd() tea.Cmd {
	return tea.Tick(m.Interval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *watchModel) Init() tea.Cmd { return tea.Batch(m.fetchCmd(), m.tickCmd()) }

func (m *watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if msg.Width > 0 && msg.Height > 0 {
			m.W, m.H = msg.Width, msg.Height
		}
	case tickMsg:
		cmds := []tea.Cmd{m.tickCmd()}
		if !m.Loading {
			m.Loading = true
			cmds = append(cmds, m.fetchCmd())
		}
		if m.mode == modeTask && !m.loadingTask && m.snap != nil {
			m.loadingTask = true
			cmds = append(cmds, m.loadCmd())
		}
		return m, tea.Batch(cmds...)
	case snapMsg:
		m.Loading = false
		m.Err = msg.err
		if msg.err == nil {
			m.snap = msg.s
		}
	case detailMsg:
		m.loadingTask = false
		if msg.err != nil {
			m.tv.Err = msg.err
		} else if m.mode == modeTask && m.tv.Detail != nil && msg.td.Num == m.tv.Detail.Num {
			m.tv.Detail, m.tv.Err, m.detailLoaded = msg.td, nil, true
		}
	case reloadDetailMsg:
		if m.mode == modeTask && !m.loadingTask {
			m.loadingTask = true
			return m, m.loadCmd()
		}
	case answerMsg:
		m.tv.Busy = false
		e := exchange{Kind: exAnswer, Text: msg.text, At: time.Now()}
		if msg.err != nil {
			e.Text, e.Bad = msg.err.Error(), true
		}
		if msg.harness != "" {
			m.tv.Harness = msg.harness
		}
		m.tv.Thread, m.tv.ThreadUp = append(m.tv.Thread, e), 0
	case sentMsg:
		if msg.err != nil {
			m.tv.Thread = append(m.tv.Thread, exchange{Kind: exNote, Text: "not sent: " + msg.err.Error(), At: time.Now(), Bad: true})
			m.tv.ThreadUp = 0
			break
		}
		m.tv.Thread = append(m.tv.Thread,
			exchange{Kind: exTell, Text: msg.text, At: time.Now()},
			exchange{Kind: exNote, Text: "Sent. The agent wakes on the comment; its run appears above in a moment.", At: time.Now()})
		m.tv.ThreadUp = 0
		return m, tea.Tick(3*time.Second, func(time.Time) tea.Msg { return reloadDetailMsg{} })
	case tea.KeyMsg:
		if m.mode == modeTask {
			return m.taskKey(msg)
		}
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
			return m, m.fetchCmd()
		}
	case "enter":
		return m.openTask()
	case "tab":
		m.Focus = (m.Focus + 1) % numPanes
	case "shift+tab":
		m.Focus = (m.Focus + numPanes - 1) % numPanes
	case "down", "j":
		m.move(1)
	case "up", "k":
		m.move(-1)
	case "pgdown", "ctrl+d":
		m.move(page)
	case "pgup", "ctrl+u":
		m.move(-page)
	case "g", "home":
		if m.Focus == paneLog {
			m.LogBack = 1 << 30 // clamped to the oldest line
		} else if m.Focus == paneIssues {
			m.selectRow(0)
		} else {
			m.Off[m.Focus] = 0
		}
	case "G", "end":
		if m.Focus == paneLog {
			m.LogBack = 0 // follow the tail again
		} else if m.Focus == paneIssues && m.snap != nil {
			m.selectRow(len(m.snap.Issues) - 1)
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

// move scrolls the focused pane by n lines (positive = down); in the issues pane it moves
// the cursor by n rows instead. The log's position is its distance from the newest line,
// so scrolling down toward it shrinks LogBack.
func (m *watchModel) move(n int) {
	switch m.Focus {
	case paneLog:
		m.LogBack = max(0, m.LogBack-n)
	case paneIssues:
		if m.snap != nil {
			m.selectRow(selectedIssue(m.snap, m.SelNum) + n)
		}
	default:
		m.Off[m.Focus] = max(0, m.Off[m.Focus]+n)
	}
}

func (m *watchModel) selectRow(i int) {
	if m.snap == nil || len(m.snap.Issues) == 0 {
		return
	}
	m.SelNum = m.snap.Issues[clamp(i, 0, len(m.snap.Issues)-1)].Num
}

func (m *watchModel) openTask() (tea.Model, tea.Cmd) {
	if m.snap == nil || len(m.snap.Issues) == 0 {
		return m, nil
	}
	num := m.snap.Issues[selectedIssue(m.snap, m.SelNum)].Num
	td, err := baseDetail(m.snap, num)
	if err != nil {
		m.Err = err
		return m, nil
	}
	m.mode = modeTask
	m.tv = taskView{Detail: td, Loading: true, Harness: m.ops.harness(m.snap.State.SignedOut)}
	m.detailLoaded, m.loadingTask, m.pendingTell = false, true, ""
	m.input.Reset()
	m.input.Placeholder = "ask about this task…"
	m.input.Focus()
	return m, tea.Batch(m.loadCmd(), textinput.Blink)
}

func (m *watchModel) closeTask() {
	m.mode = modeBoard
	m.pendingTell = ""
	m.tv = taskView{}
	m.input.Blur()
	m.input.Reset()
}

func (m *watchModel) setIntent(i intent) {
	m.tv.Intent = i
	m.tv.Err = nil
	if i == intentTell {
		m.input.Placeholder = "tell " + m.tv.Detail.Task.Profile + " something…"
	} else {
		m.input.Placeholder = "ask about this task…"
	}
}

func (m *watchModel) taskKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		if m.pendingTell != "" {
			m.pendingTell = ""
			return m, nil
		}
		m.closeTask()
		return m, nil
	case "tab":
		if m.pendingTell != "" {
			return m, nil
		}
		if m.tv.Intent == intentAsk {
			if ok, why := canTell(m.tv.Detail); !ok {
				m.tv.Err = fmt.Errorf("can't tell an agent: %s", why)
				return m, nil
			}
			m.setIntent(intentTell)
		} else {
			m.setIntent(intentAsk)
		}
		return m, nil
	case "ctrl+r":
		if !m.loadingTask {
			m.loadingTask, m.tv.Loading = true, true
			return m, m.loadCmd()
		}
		return m, nil
	case "pgup":
		m.tv.Off = max(0, m.tv.Off-max(1, m.H/4))
		return m, nil
	case "pgdown":
		m.tv.Off += max(1, m.H/4)
		return m, nil
	case "up":
		m.tv.ThreadUp++
		return m, nil
	case "down":
		m.tv.ThreadUp = max(0, m.tv.ThreadUp-1)
		return m, nil
	case "enter":
		return m.submit()
	}
	if m.pendingTell != "" {
		return m, nil // a confirmation is waiting: typing would change what it confirms
	}
	m.tv.Err = nil
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

func (m *watchModel) submit() (tea.Model, tea.Cmd) {
	if m.pendingTell != "" { // the confirmation
		text := m.pendingTell
		m.pendingTell = ""
		m.input.Reset()
		m.setIntent(intentAsk)
		return m, m.sendCmd(text)
	}
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}
	if m.tv.Intent == intentTell {
		if ok, why := canTell(m.tv.Detail); !ok {
			m.tv.Err = fmt.Errorf("can't tell an agent: %s", why)
			return m, nil
		}
		m.pendingTell = text
		return m, nil
	}
	if m.tv.Busy {
		return m, nil
	}
	m.input.Reset()
	m.tv.Thread = append(m.tv.Thread, exchange{Kind: exAsk, Text: text, At: time.Now()})
	m.tv.Busy, m.tv.ThreadUp = true, 0
	return m, m.askCmd(text)
}

func (m *watchModel) View() string {
	p := ui.Out
	if m.mode == modeTask {
		tv := m.tv
		tv.W, tv.H, tv.Now = m.W, m.H, time.Now()
		tv.Loading = m.loadingTask
		m.input.Width = max(10, m.W-len("tell "+tv.Detail.Task.Profile+" ▸ ")-4)
		tv.Input = m.input.View()
		if m.pendingTell != "" {
			tv.Confirm = fmt.Sprintf("Send to %s now? It wakes the agent. enter = send · esc = cancel   “%s”", tv.Detail.Task.Profile, clip(m.pendingTell, max(20, m.W-60)))
		}
		return renderTask(&tv, p)
	}
	if m.help {
		return strings.Join(header(m.snap, &m.viewState, p), "\n") + "\n" + helpText(p)
	}
	return render(m.snap, &m.viewState, p, false) + "\n" + footer(p, &m.viewState)
}
