package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/platform"
	"github.com/noelzappy/fleet/internal/shell"
	"github.com/noelzappy/fleet/internal/tgapi"
	"github.com/noelzappy/fleet/internal/ui"
	"github.com/spf13/cobra"
)

// The Telegram bot is the phone version of the watch task view: the same status, the
// same task context, the same ask and tell, over a chat. It reuses those functions, so
// the two can't drift apart. It answers exactly one chat and nobody else.

type tgAPI interface {
	GetMe(ctx context.Context) (tgapi.User, error)
	GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]tgapi.Update, error)
	Send(ctx context.Context, chat int64, text string, kb tgapi.Keyboard) (int64, error)
	Edit(ctx context.Context, chat, msg int64, text string) error
	AnswerCallback(ctx context.Context, id, text string) error
	Typing(ctx context.Context, chat int64) error
	SetCommands(ctx context.Context, cmds []tgapi.Command) error
}

const (
	tgMessageLimit = 3900 // under Telegram's 4096, leaving room for a header
	tgPendingTTL   = 10 * time.Minute
	tgSnapshotTTL  = 20 * time.Second
	tgThreadKeep   = 8
)

type pendingTell struct {
	n       int
	td      *taskDetail
	text    string
	msgID   int64
	expires time.Time
}

type tgBot struct {
	api  tgAPI
	chat int64 // the only chat obeyed
	user int64 // 0 = a private chat is enough; otherwise this user, which a group requires
	ops  watchOps
	now  func() time.Time

	backoff time.Duration // first retry delay after a failed poll; doubles to a minute

	snap    *snapshot
	snapAt  time.Time
	threads map[int][]exchange // per task, so "/ask 7 and then?" makes sense
	pending map[string]pendingTell
}

func newTGBot(api tgAPI, chat, user int64, ops watchOps) *tgBot {
	return &tgBot{api: api, chat: chat, user: user, ops: ops, now: time.Now, backoff: time.Second,
		threads: map[int][]exchange{}, pending: map[string]pendingTell{}}
}

// checkTelegramOwner refuses a setup where anyone but the owner could command the fleet:
// a group chat is only safe when the owner's own user id is required too.
func checkTelegramOwner(chat, user int64) error {
	if chat == 0 {
		return fmt.Errorf("TG_CHAT is 0: set it to your chat id (`fleet telegram whoami` prints it)")
	}
	if chat < 0 && user == 0 {
		return fmt.Errorf("TG_CHAT %d is a group: anyone in it could command the fleet. Set TG_USER to your own user id (`fleet telegram whoami`), or use a private chat with the bot", chat)
	}
	return nil
}

// authorized is the one gate every update passes. It fails closed: a different chat, a
// different user, or a group with no user id configured is ignored without a reply.
func (b *tgBot) authorized(chat tgapi.Chat, from *tgapi.User) bool {
	if chat.ID != b.chat {
		return false
	}
	if b.user != 0 {
		return from != nil && from.ID == b.user
	}
	return chat.Type == "private"
}

func (b *tgBot) reply(ctx context.Context, text string) {
	for _, part := range tgapi.Chunk(text, tgMessageLimit) {
		if _, err := b.api.Send(ctx, b.chat, part, nil); err != nil {
			tgLog("telegram: send failed: %v\n", err)
			return
		}
	}
}

func (b *tgBot) snapshot(ctx context.Context) (*snapshot, error) {
	if b.snap != nil && b.now().Sub(b.snapAt) < tgSnapshotTTL {
		return b.snap, nil
	}
	s, err := b.ops.collect(ctx)
	if err != nil {
		return nil, err
	}
	b.snap, b.snapAt = s, b.now()
	return s, nil
}

func (b *tgBot) detail(ctx context.Context, n int) (*taskDetail, *snapshot, error) {
	s, err := b.snapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	td, err := b.ops.load(ctx, s, n)
	return td, s, err
}

var (
	// (?s): a /tell can span several lines, and `.` must match the newlines in it.
	cmdRE     = regexp.MustCompile(`(?s)^/([A-Za-z_]+)(?:@\w+)?(?:\s+(.*))?$`)
	taskArgRE = regexp.MustCompile(`(?s)^#?(\d+)\s*(.*)$`)
	issueRefE = regexp.MustCompile(`#(\d+)`)
)

func parseCommand(text string) (cmd, args string, ok bool) {
	m := cmdRE.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return "", "", false
	}
	return strings.ToLower(m[1]), strings.TrimSpace(m[2]), true
}

func parseTaskArg(args string) (n int, rest string, ok bool) {
	m := taskArgRE.FindStringSubmatch(strings.TrimSpace(args))
	if m == nil {
		return 0, "", false
	}
	n, _ = strconv.Atoi(m[1])
	if n <= 0 {
		return 0, "", false
	}
	return n, strings.TrimSpace(m[2]), true
}

// issueFromText finds the issue a notification is about. A notification names issues and
// pull requests alike ("PR #23 … #7"), and both share GitHub's numbering, so each #N is
// resolved against the snapshot: an open issue is itself, a PR is the issue it closes.
func issueFromText(s *snapshot, text string) int {
	for _, m := range issueRefE.FindAllStringSubmatch(text, -1) {
		n, _ := strconv.Atoi(m[1])
		for _, is := range s.State.GH {
			if is.Number == n && is.State == "OPEN" {
				return n
			}
		}
		for _, pr := range s.PRs {
			if pr.Num == n && pr.Issue != 0 {
				return pr.Issue
			}
		}
	}
	return 0
}

// tgLog writes a log line with anything secret masked: errors from the network layer can
// carry a URL, and a bot token is part of every Telegram URL.
func tgLog(format string, a ...any) {
	ui.Errf("%s", shell.Redact(fmt.Sprintf(format, a...)))
}

const tgHelp = `fleet, from your phone

/status: queue counts, harness health
/tasks: open issues and where each stands
/task 7: one task in detail (runs, comments, PR, worktree)
/ask 7 why did it stop?: ask a model about task 7 (it can't change anything)
/tell 7 run the gate first: send a follow-up to task 7's agent (you confirm first)

You can also reply to a fleet notification that mentions #7 to ask about it.`

func (b *tgBot) handle(ctx context.Context, u tgapi.Update) {
	defer func() {
		if r := recover(); r != nil {
			tgLog("telegram: handler panicked: %v\n", r)
			b.reply(ctx, "Something went wrong handling that. It's in the fleet log.")
		}
	}()
	switch {
	case u.Callback != nil:
		b.handleCallback(ctx, u.Callback)
	case u.Message != nil:
		m := u.Message
		if !b.authorized(m.Chat, m.From) {
			ui.Errf("telegram: ignored a message from chat %d\n", m.Chat.ID) // never its content
			return
		}
		b.handleMessage(ctx, m)
	}
}

func (b *tgBot) handleMessage(ctx context.Context, m *tgapi.Message) {
	cmd, args, isCmd := parseCommand(m.Text)
	if !isCmd {
		// A reply to a message that names an issue is a question about it.
		if m.ReplyTo != nil && strings.TrimSpace(m.Text) != "" && issueRefE.MatchString(m.ReplyTo.Text) {
			if s, err := b.snapshot(ctx); err == nil {
				if n := issueFromText(s, m.ReplyTo.Text); n != 0 {
					cmd, args = "ask", strconv.Itoa(n)+" "+m.Text
					goto dispatch
				}
			}
		}
		b.reply(ctx, "Send /help to see what I can do.")
		return
	}
dispatch:
	ui.Errf("telegram: /%s\n", cmd)
	switch cmd {
	case "start", "help":
		b.reply(ctx, tgHelp)
	case "status":
		b.status(ctx)
	case "tasks":
		b.tasks(ctx)
	case "task":
		b.task(ctx, args)
	case "ask":
		b.ask(ctx, args)
	case "tell":
		b.tell(ctx, args)
	default:
		b.reply(ctx, "I don't know /"+cmd+". Try /help.")
	}
}

func (b *tgBot) status(ctx context.Context) {
	var buf bytes.Buffer
	if err := printStatus(ctx, &buf); err != nil && buf.Len() == 0 {
		b.reply(ctx, "Couldn't read the status: "+err.Error())
		return
	}
	b.reply(ctx, buf.String())
}

var toneGlyph = map[tone]string{toneBad: "🔴", toneWarn: "🟠", toneInfo: "🔵", toneGood: "🟢", toneAccent: "🟣", toneDim: "⚪"}

const maxTasksListed = 30

func (b *tgBot) tasks(ctx context.Context) {
	s, err := b.snapshot(ctx)
	if err != nil {
		b.reply(ctx, "Couldn't read the fleet: "+err.Error())
		return
	}
	if len(s.Issues) == 0 {
		b.reply(ctx, "No open issues.")
		return
	}
	var lines []string
	for i, r := range s.Issues {
		if i == maxTasksListed {
			lines = append(lines, fmt.Sprintf("…and %d more", len(s.Issues)-maxTasksListed))
			break
		}
		line := fmt.Sprintf("%s #%d %s: %s", toneGlyph[r.Tone], r.Num, r.State, clip(r.Title, 48))
		if r.Agent != "" {
			line += " [" + r.Agent + "]"
		}
		if r.PR != 0 {
			line += fmt.Sprintf(" PR #%d %s", r.PR, r.Gate)
		}
		lines = append(lines, strings.TrimSpace(line))
		if r.Why != "" {
			lines = append(lines, "   ↳ "+r.Why)
		}
	}
	b.reply(ctx, fmt.Sprintf("Open issues (%d), what needs you first:\n\n%s", len(s.Issues), strings.Join(lines, "\n")))
}

func (b *tgBot) task(ctx context.Context, args string) {
	n, _, ok := parseTaskArg(args)
	if !ok {
		b.reply(ctx, "Which task? /task 7")
		return
	}
	td, _, err := b.detail(ctx, n)
	if err != nil {
		b.reply(ctx, err.Error())
		return
	}
	head := fmt.Sprintf("#%d %s\n%s", td.Num, td.Title, td.Row.State)
	if td.HasTask {
		head += " · agent " + td.Task.Profile
	}
	if td.PR != nil {
		head += fmt.Sprintf(" · PR #%d", td.PR.Num)
	}
	pal := ui.NewPalette(io.Discard) // plain text: no colour codes in a chat
	b.reply(ctx, head+"\n\n"+strings.Join(contextLines(td, pal, 70, b.now()), "\n"))
}

func (b *tgBot) ask(ctx context.Context, args string) {
	n, q, ok := parseTaskArg(args)
	if !ok || q == "" {
		b.reply(ctx, "Which task, and what do you want to know? /ask 7 why did it stop?")
		return
	}
	td, s, err := b.detail(ctx, n)
	if err != nil {
		b.reply(ctx, err.Error())
		return
	}
	_ = b.api.Typing(ctx, b.chat)
	answer, _, err := b.ops.ask(ctx, s.State.SignedOut, askPrompt(td, b.threads[n], q, b.now()))
	if err != nil {
		b.reply(ctx, "Couldn't answer: "+err.Error())
		return
	}
	th := append(b.threads[n], exchange{Kind: exAsk, Text: q}, exchange{Kind: exAnswer, Text: answer})
	if len(th) > tgThreadKeep {
		th = th[len(th)-tgThreadKeep:]
	}
	b.threads[n] = th
	b.reply(ctx, answer)
}

func (b *tgBot) tell(ctx context.Context, args string) {
	n, text, ok := parseTaskArg(args)
	if !ok || text == "" {
		b.reply(ctx, "Which task, and what should its agent do? /tell 7 run the gate in the foreground")
		return
	}
	td, _, err := b.detail(ctx, n)
	if err != nil {
		b.reply(ctx, err.Error())
		return
	}
	if ok, why := canTell(td); !ok {
		b.reply(ctx, "Can't tell an agent: "+why)
		return
	}
	tok := newToken()
	msgID, err := b.api.Send(ctx, b.chat,
		fmt.Sprintf("Send this to %s on #%d? It wakes the agent.\n\n“%s”", td.Task.Profile, n, clip(text, 1500)),
		tgapi.Keyboard{{Text: "✅ Send", Data: "t:" + tok + ":y"}, {Text: "✖ Cancel", Data: "t:" + tok + ":n"}})
	if err != nil {
		tgLog("telegram: send failed: %v\n", err)
		return
	}
	b.pending[tok] = pendingTell{n: n, td: td, text: text, msgID: msgID, expires: b.now().Add(tgPendingTTL)}
	b.sweepPending()
}

func (b *tgBot) sweepPending() {
	for k, p := range b.pending {
		if b.now().After(p.expires) {
			delete(b.pending, k)
		}
	}
}

func (b *tgBot) handleCallback(ctx context.Context, cb *tgapi.Callback) {
	if cb.Message == nil || !b.authorized(cb.Message.Chat, &cb.From) {
		ui.Errf("telegram: ignored a button press\n")
		return
	}
	parts := strings.Split(cb.Data, ":")
	if len(parts) != 3 || parts[0] != "t" {
		_ = b.api.AnswerCallback(ctx, cb.ID, "")
		return
	}
	// Taken out before it is acted on, so a double tap can't send twice.
	p, found := b.pending[parts[1]]
	delete(b.pending, parts[1])
	if !found || b.now().After(p.expires) {
		_ = b.api.AnswerCallback(ctx, cb.ID, "That request expired. Send /tell again.")
		return
	}
	if parts[2] != "y" {
		_ = b.api.AnswerCallback(ctx, cb.ID, "Cancelled")
		_ = b.api.Edit(ctx, b.chat, p.msgID, "✖ Cancelled. Nothing was sent.")
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := b.ops.send(sctx, p.td, p.text); err != nil {
		_ = b.api.AnswerCallback(ctx, cb.ID, "Not sent")
		_ = b.api.Edit(ctx, b.chat, p.msgID, "✗ Not sent: "+err.Error())
		return
	}
	th := append(b.threads[p.n], exchange{Kind: exTell, Text: p.text})
	if len(th) > tgThreadKeep {
		th = th[len(th)-tgThreadKeep:]
	}
	b.threads[p.n] = th
	_ = b.api.AnswerCallback(ctx, cb.ID, "Sent")
	_ = b.api.Edit(ctx, b.chat, p.msgID, fmt.Sprintf("✓ Sent to %s on #%d: “%s”\nIt wakes now; /task %d shows its new run.", p.td.Task.Profile, p.n, clip(p.text, 1500), p.n))
}

func newToken() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// run long-polls until ctx ends. Updates that piled up while the bot was down are dropped:
// a command from yesterday must not run now.
func (b *tgBot) run(ctx context.Context) error {
	me, err := b.api.GetMe(ctx)
	if err != nil {
		return err
	}
	ui.Errf("✓ telegram: @%s, obeying chat %d\n", me.Username, b.chat)
	_ = b.api.SetCommands(ctx, []tgapi.Command{
		{Command: "status", Description: "queue counts and harness health"},
		{Command: "tasks", Description: "open issues and where each stands"},
		{Command: "task", Description: "one task in detail: /task 7"},
		{Command: "ask", Description: "ask about a task: /ask 7 why did it stop?"},
		{Command: "tell", Description: "tell a task's agent something: /tell 7 …"},
		{Command: "help", Description: "what I can do"},
	})
	var offset int64
	if ups, err := b.api.GetUpdates(ctx, -1, 0); err == nil && len(ups) > 0 {
		offset = ups[len(ups)-1].ID + 1
	}
	backoff := b.backoff
	for ctx.Err() == nil {
		ups, err := b.api.GetUpdates(ctx, offset, 50*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			tgLog("telegram: %v (retrying in %s)\n", err, backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > time.Minute {
				backoff = time.Minute
			}
			continue
		}
		backoff = b.backoff
		for _, u := range ups {
			offset = u.ID + 1
			b.handle(ctx, u)
		}
	}
	return nil
}

// ---- commands -------------------------------------------------------------------------

type tgSettings struct {
	token      string
	chat, user int64
}

func loadTGSettings() (tgSettings, error) {
	var s tgSettings
	tg := cfg.Notify.Telegram
	if tg == nil {
		return s, fmt.Errorf("notify.telegram isn't set in fleet.yaml (token_secret and chat_secret name the variables)")
	}
	lookup, err := config.SecretsLookup()
	if err != nil {
		return s, err
	}
	tok, ok := lookup(tg.TokenSecret)
	if !ok {
		return s, fmt.Errorf("%s isn't set in %s: add the bot token from @BotFather (this is the same value as the %s GitHub secret)", tg.TokenSecret, config.SecretsFile, tg.TokenSecret)
	}
	s.token = tok
	if v, ok := lookup(tg.ChatSecret); ok {
		if s.chat, err = strconv.ParseInt(strings.TrimSpace(v), 10, 64); err != nil {
			return s, fmt.Errorf("%s isn't a number: %q", tg.ChatSecret, v)
		}
	}
	if v, ok := lookup("TG_USER"); ok {
		if s.user, err = strconv.ParseInt(strings.TrimSpace(v), 10, 64); err != nil {
			return s, fmt.Errorf("TG_USER isn't a number: %q", v)
		}
	}
	return s, nil
}

func telegramCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "telegram",
		Short: "reach the fleet from Telegram: status, tasks, ask a model about a task, tell an agent something",
		Long: `A bot that answers one chat, yours. Set the bot token and your chat id in the secrets file
(` + config.SecretsFile + `), the same names notify.telegram uses for the GitHub secrets:

  TG_TOKEN=123456:ABC…      from @BotFather
  TG_CHAT=123456789         your chat id: run 'fleet telegram whoami', then message the bot
  TG_USER=123456789         only needed if TG_CHAT is a group chat

Then 'fleet telegram test', and 'fleet telegram install' to keep it running.`,
	}
	c.AddCommand(
		&cobra.Command{Use: "run", Short: "answer messages in the foreground (what the service runs)", Args: cobra.NoArgs, RunE: tgRun},
		&cobra.Command{Use: "test", Short: "check the token and chat: sends one message", Args: cobra.NoArgs, RunE: tgTest},
		&cobra.Command{Use: "whoami", Short: "wait for you to message the bot, then print your chat and user ids", Args: cobra.NoArgs, RunE: tgWhoami},
		&cobra.Command{Use: "install", Short: "install and start the bot as a background service", Args: cobra.NoArgs, RunE: tgInstall},
		&cobra.Command{Use: "stop", Short: "stop the background service (it starts again at the next login or boot; delete its service file to remove it)", Args: cobra.NoArgs, RunE: tgStop},
	)
	return c
}

func tgRun(cmd *cobra.Command, _ []string) error {
	if shell.DryRun {
		ui.Errln("→ would long-poll Telegram and answer chat", "TG_CHAT")
		return nil
	}
	s, err := loadTGSettings()
	if err != nil {
		return err
	}
	if err := checkTelegramOwner(s.chat, s.user); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shell.Silent = true // a service has no terminal, and children must not write under it
	return newTGBot(tgapi.New(s.token), s.chat, s.user, realOps()).run(ctx)
}

func tgTest(cmd *cobra.Command, _ []string) error {
	s, err := loadTGSettings()
	if err != nil {
		return err
	}
	if err := checkTelegramOwner(s.chat, s.user); err != nil {
		return err
	}
	if shell.DryRun {
		ui.Errln("→ would send a test message to chat", s.chat)
		return nil
	}
	api := tgapi.New(s.token)
	ctx := context.Background()
	me, err := api.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("the token was rejected: %w", err)
	}
	ui.Errf("✓ token works: @%s\n", me.Username)
	if _, err := api.Send(ctx, s.chat, fmt.Sprintf("fleet is connected for %s. Send /help.", cfg.Project.Name), nil); err != nil {
		return fmt.Errorf("couldn't message chat %d (open the bot in Telegram and press Start first): %w", s.chat, err)
	}
	ui.Errf("✓ sent a message to chat %d\n", s.chat)
	return nil
}

func tgWhoami(cmd *cobra.Command, _ []string) error {
	s, err := loadTGSettings()
	if err != nil {
		return err
	}
	if shell.DryRun {
		ui.Errln("→ would wait for a message to the bot and print its chat and user ids")
		return nil
	}
	api := tgapi.New(s.token)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	me, err := api.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("the token was rejected: %w", err)
	}
	ui.Errf("● open @%s in Telegram, press Start and send it any message (waiting up to 3 minutes)\n", me.Username)
	var offset int64
	for ctx.Err() == nil {
		ups, err := api.GetUpdates(ctx, offset, 20*time.Second)
		if err != nil {
			return err
		}
		for _, u := range ups {
			offset = u.ID + 1
			if m := u.Message; m != nil && m.From != nil {
				name := m.From.Username
				if name == "" {
					name = m.From.First
				}
				ui.Errf("✓ %s (%s chat)\n", name, m.Chat.Type)
				fmt.Printf("TG_CHAT=%d\nTG_USER=%d\n", m.Chat.ID, m.From.ID)
				fmt.Fprintln(os.Stderr, "  Put TG_CHAT in "+config.SecretsFile+" (and TG_USER too if the chat is a group), then run: fleet telegram test")
				return nil
			}
		}
	}
	return fmt.Errorf("no message arrived; send one to @%s and run this again", me.Username)
}

// botJob is the always-on service that runs `fleet telegram run`. It is not one of the pair
// `fleet up` and `fleet pause` manage: pausing the agents shouldn't silence the channel you'd
// use to check on them.
func botJob(configPath string) platform.Job {
	return platform.Job{
		Name:        cfg.Orchestrator.ServiceName + "-telegram",
		Description: "Telegram bot for " + cfg.Project.Name + " (fleet)",
		Exec:        shell.Quote(selfPath()) + " -c " + shell.Quote(configPath) + " telegram run",
		WorkingDir:  cfg.Project.Root,
	}
}

func tgInstall(cmd *cobra.Command, _ []string) error {
	p, err := requireBox("telegram install")
	if err != nil {
		return err
	}
	if !shell.DryRun {
		s, err := loadTGSettings()
		if err != nil {
			return err
		}
		if err := checkTelegramOwner(s.chat, s.user); err != nil {
			return err
		}
	}
	abs, _ := filepath.Abs(cfgPath)
	job := botJob(abs)
	files, err := p.JobFiles(job)
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := shell.WriteFile(f.Path, f.Data, f.Mode); err != nil {
			return err
		}
	}
	ctx := context.Background()
	if err := shell.Run(ctx, p.ReloadCmd(serviceSpec(abs)), nil); err != nil {
		return err
	}
	if err := shell.Run(ctx, p.StartCmd(job.Name), nil); err != nil {
		return err
	}
	ui.Errf("✓ %s is running. Check it: fleet telegram test\n", job.Name)
	return nil
}

func tgStop(cmd *cobra.Command, _ []string) error {
	p, err := requireBox("telegram stop")
	if err != nil {
		return err
	}
	abs, _ := filepath.Abs(cfgPath)
	return shell.Run(context.Background(), p.StopCmd(botJob(abs).Name), nil)
}
