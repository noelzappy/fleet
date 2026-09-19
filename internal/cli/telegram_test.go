package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noelzappy/fleet/internal/config"
	"github.com/noelzappy/fleet/internal/tgapi"
)

const (
	ownerChat = int64(1001)
	ownerUser = int64(1001)
)

type tgSent struct {
	chat int64
	text string
	kb   tgapi.Keyboard
	id   int64
}

// fakeTG is a Telegram that records what the bot says and feeds it scripted updates.
type fakeTG struct {
	mu      sync.Mutex
	sent    []tgSent
	edits   map[int64]string
	answers []string
	typing  int
	polls   [][]tgapi.Update // one entry per GetUpdates call; an error entry is nil + pollErr
	pollErr []error
	polled  []int64 // offsets requested
	nextID  int64
	meErr   error
}

func (f *fakeTG) GetMe(context.Context) (tgapi.User, error) {
	return tgapi.User{ID: 9, Username: "fleet_bot"}, f.meErr
}
func (f *fakeTG) GetUpdates(ctx context.Context, offset int64, _ time.Duration) ([]tgapi.Update, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polled = append(f.polled, offset)
	if len(f.polls) == 0 {
		return nil, ctx.Err()
	}
	ups, err := f.polls[0], f.pollErr[0]
	f.polls, f.pollErr = f.polls[1:], f.pollErr[1:]
	return ups, err
}
func (f *fakeTG) Send(_ context.Context, chat int64, text string, kb tgapi.Keyboard) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.sent = append(f.sent, tgSent{chat, text, kb, 500 + f.nextID})
	return 500 + f.nextID, nil
}
func (f *fakeTG) Edit(_ context.Context, _, msg int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.edits == nil {
		f.edits = map[int64]string{}
	}
	f.edits[msg] = text
	return nil
}
func (f *fakeTG) AnswerCallback(_ context.Context, _ string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, text)
	return nil
}
func (f *fakeTG) Typing(context.Context, int64) error                { f.typing++; return nil }
func (f *fakeTG) SetCommands(context.Context, []tgapi.Command) error { return nil }

func (f *fakeTG) texts() string {
	var b strings.Builder
	for _, m := range f.sent {
		b.WriteString(m.text + "\n---\n")
	}
	return b.String()
}

func msg(text string) tgapi.Update {
	return tgapi.Update{ID: 1, Message: &tgapi.Message{ID: 1, From: &tgapi.User{ID: ownerUser}, Chat: tgapi.Chat{ID: ownerChat, Type: "private"}, Text: text}}
}

func press(data string, msgID int64) tgapi.Update {
	return tgapi.Update{ID: 2, Callback: &tgapi.Callback{ID: "cb", From: tgapi.User{ID: ownerUser}, Data: data,
		Message: &tgapi.Message{ID: msgID, Chat: tgapi.Chat{ID: ownerChat, Type: "private"}}}}
}

func newBot(t *testing.T) (*tgBot, *fakeTG, *recorder) {
	t.Helper()
	tg, rec := &fakeTG{}, &recorder{answer: "It backgrounded the gate, so nothing was committed."}
	b := newTGBot(tg, ownerChat, 0, rec.ops())
	b.backoff = 5 * time.Millisecond
	return b, tg, rec
}

func TestTelegramAuthorization(t *testing.T) {
	priv := func(id int64) tgapi.Chat { return tgapi.Chat{ID: id, Type: "private"} }
	group := func(id int64) tgapi.Chat { return tgapi.Chat{ID: id, Type: "supergroup"} }
	tests := []struct {
		name       string
		chat, user int64 // the bot's configuration
		got        tgapi.Chat
		from       *tgapi.User
		want       bool
	}{
		{"the owner's private chat", 1001, 0, priv(1001), &tgapi.User{ID: 1001}, true},
		{"a stranger's private chat", 1001, 0, priv(2002), &tgapi.User{ID: 2002}, false},
		{"a group posing as the owner's chat id", 1001, 0, group(1001), &tgapi.User{ID: 7}, false},
		{"a group without TG_USER is never trusted", -500, 0, group(-500), &tgapi.User{ID: 1001}, false},
		{"a group with the owner's user id", -500, 1001, group(-500), &tgapi.User{ID: 1001}, true},
		{"a group, someone else speaking", -500, 1001, group(-500), &tgapi.User{ID: 4242}, false},
		{"a group, no sender (channel post)", -500, 1001, group(-500), nil, false},
		{"another group", -500, 1001, group(-999), &tgapi.User{ID: 1001}, false},
		{"private chat but the wrong user", 1001, 77, priv(1001), &tgapi.User{ID: 1001}, false},
	}
	for _, tt := range tests {
		b := newTGBot(&fakeTG{}, tt.chat, tt.user, watchOps{})
		if got := b.authorized(tt.got, tt.from); got != tt.want {
			t.Errorf("%s: authorized = %v, want %v", tt.name, got, tt.want)
		}
	}
	for _, c := range []struct {
		chat, user int64
		ok         bool
	}{{1001, 0, true}, {-500, 1001, true}, {-500, 0, false}, {0, 0, false}} {
		if err := checkTelegramOwner(c.chat, c.user); (err == nil) != c.ok {
			t.Errorf("checkTelegramOwner(%d, %d) = %v", c.chat, c.user, err)
		}
	}
}

func TestStrangersGetNoReplyAndNoWork(t *testing.T) {
	b, tg, rec := newBot(t)
	stranger := tgapi.Update{Message: &tgapi.Message{ID: 1, From: &tgapi.User{ID: 666}, Chat: tgapi.Chat{ID: 666, Type: "private"}, Text: "/tell 7 delete everything"}}
	for _, text := range []string{"/tell 7 hi", "/ask 7 anything", "/tasks", "/status", "/help", "hello"} {
		stranger.Message.Text = text
		b.handle(context.Background(), stranger)
	}
	// A button press from a stranger, even with a valid pending token, does nothing.
	b.pending["abc"] = pendingTell{n: 7, td: sampleDetail(), text: "x", expires: time.Now().Add(time.Hour)}
	b.handle(context.Background(), tgapi.Update{Callback: &tgapi.Callback{ID: "c", From: tgapi.User{ID: 666}, Data: "t:abc:y",
		Message: &tgapi.Message{ID: 1, Chat: tgapi.Chat{ID: 666, Type: "private"}}}})
	if len(tg.sent) != 0 || len(tg.answers) != 0 || len(tg.edits) != 0 {
		t.Errorf("a stranger got a reply: sent=%v answers=%v edits=%v", tg.sent, tg.answers, tg.edits)
	}
	if rec.fetches+rec.loads+len(rec.asked)+len(rec.sent) != 0 {
		t.Errorf("a stranger triggered work: fetches=%d loads=%d asked=%d sent=%d", rec.fetches, rec.loads, len(rec.asked), len(rec.sent))
	}
	if _, still := b.pending["abc"]; !still {
		t.Error("a stranger's button press consumed the owner's pending request")
	}
}

func TestParsing(t *testing.T) {
	for in, want := range map[string][3]string{
		"/ask 7 why?":           {"ask", "7 why?", "true"},
		"/ask@fleet_bot 7 why?": {"ask", "7 why?", "true"},
		"/TASKS":                {"tasks", "", "true"},
		"  /help  ":             {"help", "", "true"},
		"hello /ask":            {"", "", "false"},
		"/":                     {"", "", "false"},
		"/tell 7 line1\nline2":  {"tell", "7 line1\nline2", "true"},
	} {
		cmd, args, ok := parseCommand(in)
		if cmd != want[0] || args != want[1] || (want[2] == "true") != ok {
			t.Errorf("parseCommand(%q) = %q %q %v", in, cmd, args, ok)
		}
	}
	for in, want := range map[string][3]string{
		"7 why did it stop": {"7", "why did it stop", "true"},
		"#7 why":            {"7", "why", "true"},
		"12":                {"12", "", "true"},
		"why 7":             {"0", "", "false"},
		"0 x":               {"0", "", "false"}, // 0 isn't an issue
		"":                  {"0", "", "false"},
	} {
		n, rest, ok := parseTaskArg(in)
		if strconv.Itoa(n) != want[0] || rest != want[1] || (want[2] == "true") != ok {
			t.Errorf("parseTaskArg(%q) = %d %q %v", in, n, rest, ok)
		}
	}
}

func TestTasksAndTask(t *testing.T) {
	b, tg, _ := newBot(t)
	ctx := context.Background()
	b.handle(ctx, msg("/help"))
	if !strings.Contains(tg.sent[0].text, "/ask 7") || !strings.Contains(tg.sent[0].text, "reply to a fleet notification") {
		t.Errorf("help:\n%s", tg.sent[0].text)
	}

	before := len(tg.sent)
	b.handle(ctx, msg("/tasks"))
	var all []string
	for _, m := range tg.sent[before:] {
		all = append(all, m.text)
	}
	if len(all) < 2 {
		t.Errorf("30 rows with reasons are too long for one Telegram message; got %d", len(all))
	}
	out := strings.Join(all, "\n")
	if !strings.Contains(out, "Open issues (40)") || !strings.Contains(out, "🟠 #1 ready · stalled") || !strings.Contains(out, "↳ no wave label") || !strings.Contains(out, "…and 10 more") {
		t.Errorf("/tasks:\n%s", out)
	}
	if strings.Contains(out, "#31 ") {
		t.Error("the list should be capped at 30")
	}

	b.handle(ctx, msg("/task #7"))
	out = tg.sent[len(tg.sent)-1].text
	for _, want := range []string{"#7 [CONTRACTS] app auth", "idle · no PR · agent impl-g · PR #23", "runs", "codex: 401 unauthorized", "recent comments", "pull request", "branch agent/7-contracts", "uncommitted"} {
		if !strings.Contains(out, want) {
			t.Errorf("/task is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Error("a chat message must not contain terminal colour codes")
	}
	before = len(tg.sent)
	b.handle(ctx, msg("/task"))
	b.handle(ctx, msg("/nonsense"))
	b.handle(ctx, msg("just some words"))
	if got := tg.texts(); len(tg.sent) != before+3 || !strings.Contains(got, "Which task?") || !strings.Contains(got, "I don't know /nonsense") || !strings.Contains(got, "Send /help") {
		t.Errorf("bad input should be answered helpfully:\n%s", got)
	}
}

func TestAsk(t *testing.T) {
	b, tg, rec := newBot(t)
	ctx := context.Background()
	b.handle(ctx, msg("/ask 7 why did it stop?"))
	if len(rec.asked) != 1 || !strings.Contains(rec.asked[0], "QUESTION: why did it stop?") || !strings.Contains(rec.asked[0], "no tools") {
		t.Fatalf("the question should reach the model with the task's context: %v", rec.asked)
	}
	if tg.typing != 1 || tg.sent[len(tg.sent)-1].text != rec.answer {
		t.Errorf("typing=%d reply=%q", tg.typing, tg.sent[len(tg.sent)-1].text)
	}
	if len(rec.sent) != 0 {
		t.Error("asking must never send to an agent")
	}

	// A follow-up sees the conversation about that task, and only that task.
	b.handle(ctx, msg("/ask 7 and then?"))
	if !strings.Contains(rec.asked[1], "Owner asked: why did it stop?") || !strings.Contains(rec.asked[1], "You answered: It backgrounded the gate") {
		t.Errorf("follow-up lost the conversation:\n%s", rec.asked[1])
	}
	b.handle(ctx, msg("/ask 9 what is this?"))
	if strings.Contains(rec.asked[2], "why did it stop?") {
		t.Error("another task's conversation leaked into this prompt")
	}

	// Replying to a notification that names an issue is a question about it.
	// (PR #123 belongs to issue #7 and is named first: the PR number must resolve to its issue.)
	s := syntheticSnapshot()
	s.PRs = append(s.PRs, prRow{Num: 123, Issue: 7, Head: "agent/7-x"})
	b.snap, b.snapAt = s, time.Now()
	reply := msg("is this safe to merge?")
	reply.Message.ReplyTo = &tgapi.Message{Text: "🟢 PR #123 [CONTRACTS] app auth\nhttps://github.com/o/r/pull/123"}
	b.handle(ctx, reply)
	if len(rec.asked) != 4 || !strings.Contains(rec.asked[3], "QUESTION: is this safe to merge?") || !strings.Contains(rec.asked[3], "GitHub issue #7 ") {
		t.Fatalf("a reply to a PR notification should ask about the PR's issue: %d prompts\n%v", len(rec.asked), rec.asked)
	}
	issueNote := msg("what now?")
	issueNote.Message.ReplyTo = &tgapi.Message{Text: "🟠 needs-human: #9 [CONTRACTS] app wallets\nhttps://github.com/o/r/issues/9"}
	b.handle(ctx, issueNote)
	if len(rec.asked) != 5 || !strings.Contains(rec.asked[4], "GitHub issue #9 ") {
		t.Errorf("a reply to an issue notification should ask about that issue: %d prompts", len(rec.asked))
	}
	plain := msg("is this safe?") // a reply to something that names no issue is not a question
	plain.Message.ReplyTo = &tgapi.Message{Text: "good morning"}
	n := len(rec.asked)
	plain.Message.ReplyTo = &tgapi.Message{Text: "see #99999 for details"} // names a number that is nothing we know
	b.handle(ctx, plain)
	if len(rec.asked) != n {
		t.Error("a reply to an unrelated message must not spend a model call")
	}

	// A failing model is reported; missing arguments get usage; a long answer is chunked.
	rec.askErr = errors.New("claude-code couldn't answer: exit 1")
	b.handle(ctx, msg("/ask 7 again"))
	if !strings.Contains(tg.sent[len(tg.sent)-1].text, "Couldn't answer: claude-code couldn't answer") {
		t.Errorf("failed ask:\n%s", tg.sent[len(tg.sent)-1].text)
	}
	b.handle(ctx, msg("/ask 7"))
	if !strings.Contains(tg.sent[len(tg.sent)-1].text, "/ask 7 why did it stop?") {
		t.Error("usage expected")
	}
	rec.askErr, rec.answer = nil, strings.Repeat("a line of answer\n", 600)
	before := len(tg.sent)
	b.handle(ctx, msg("/ask 7 long one"))
	parts := tg.sent[before:]
	if len(parts) < 3 {
		t.Fatalf("a %d-char answer should be several messages, got %d", len(rec.answer), len(parts))
	}
	for i, p := range parts {
		if len([]rune(p.text)) > 4000 {
			t.Errorf("message %d is %d chars; Telegram rejects over 4096", i, len([]rune(p.text)))
		}
	}
}

func TestTellNeedsAButtonPress(t *testing.T) {
	b, tg, rec := newBot(t)
	ctx := context.Background()
	b.handle(ctx, msg("/tell 7 run the gate in the foreground"))
	if len(rec.sent) != 0 {
		t.Fatal("/tell must ask for confirmation, not send")
	}
	ask := tg.sent[len(tg.sent)-1]
	if !strings.Contains(ask.text, "Send this to impl-g on #7?") || !strings.Contains(ask.text, "run the gate in the foreground") || len(ask.kb) != 2 {
		t.Fatalf("confirmation:\n%+v", ask)
	}
	send, cancel := ask.kb[0].Data, ask.kb[1].Data
	if !strings.HasSuffix(send, ":y") || !strings.HasSuffix(cancel, ":n") || len(send) > 64 {
		t.Errorf("callback data %q / %q", send, cancel)
	}

	// Typing more or asking things doesn't send it.
	b.handle(ctx, msg("/ask 7 hmm"))
	if len(rec.sent) != 0 {
		t.Fatal("something sent without a button press")
	}

	// Cancel: nothing is sent and the message says so.
	b.handle(ctx, press(cancel, ask.id))
	if len(rec.sent) != 0 || !strings.Contains(tg.edits[ask.id], "Cancelled") {
		t.Errorf("cancel: sent=%v edit=%q", rec.sent, tg.edits[ask.id])
	}
	// The cancelled request can't be revived by pressing Send afterwards.
	b.handle(ctx, press(send, ask.id))
	if len(rec.sent) != 0 || !strings.Contains(tg.answers[len(tg.answers)-1], "expired") {
		t.Errorf("a used token must not work again: sent=%v answers=%v", rec.sent, tg.answers)
	}

	// Confirm for real; a double tap sends once.
	b.handle(ctx, msg("/tell 7 use the second option"))
	ask = tg.sent[len(tg.sent)-1]
	b.handle(ctx, press(ask.kb[0].Data, ask.id))
	b.handle(ctx, press(ask.kb[0].Data, ask.id))
	if len(rec.sent) != 1 || rec.sent[0] != "use the second option" {
		t.Fatalf("sent = %v", rec.sent)
	}
	if !strings.Contains(tg.edits[ask.id], "✓ Sent to impl-g on #7") {
		t.Errorf("edit = %q", tg.edits[ask.id])
	}
	// What you told the agent is part of the conversation a later /ask sees.
	b.handle(ctx, msg("/ask 7 what did I tell it?"))
	if !strings.Contains(rec.asked[len(rec.asked)-1], "Owner told the agent: use the second option") {
		t.Error("a sent follow-up should be visible to later questions")
	}

	// A failed send says so and doesn't claim success.
	rec.sendEr = errors.New("multica comment: 500")
	b.handle(ctx, msg("/tell 7 again"))
	ask = tg.sent[len(tg.sent)-1]
	b.handle(ctx, press(ask.kb[0].Data, ask.id))
	if !strings.Contains(tg.edits[ask.id], "✗ Not sent: multica comment: 500") {
		t.Errorf("failed send edit = %q", tg.edits[ask.id])
	}

	// A request that waited too long expires.
	rec.sendEr = nil
	b.handle(ctx, msg("/tell 7 late"))
	ask = tg.sent[len(tg.sent)-1]
	b.now = func() time.Time { return time.Now().Add(tgPendingTTL + time.Minute) }
	before := len(rec.sent)
	b.handle(ctx, press(ask.kb[0].Data, ask.id))
	if len(rec.sent) != before || !strings.Contains(tg.answers[len(tg.answers)-1], "expired") {
		t.Errorf("an expired request must not send: %v", tg.answers)
	}

	// Nothing to tell where no agent exists; usage for a bare /tell.
	b.now = time.Now
	b.handle(ctx, msg("/tell 7"))
	if !strings.Contains(tg.sent[len(tg.sent)-1].text, "/tell 7 run the gate") {
		t.Error("usage expected")
	}
}

func TestTellRefusedWithoutAnAgent(t *testing.T) {
	b, tg, rec := newBot(t)
	rec.load = func(td *taskDetail) { td.HasTask = false; td.Task.ID = "" }
	b.handle(context.Background(), msg("/tell 9 hello"))
	last := tg.sent[len(tg.sent)-1]
	if !strings.Contains(last.text, "Can't tell an agent") || len(last.kb) != 0 || len(b.pending) != 0 {
		t.Errorf("undispatched issue: %+v pending=%d", last, len(b.pending))
	}
}

func TestBotSurvivesAPanicInAHandler(t *testing.T) {
	b, tg, rec := newBot(t)
	b.ops.collect = func(context.Context) (*snapshot, error) { panic("boom") }
	b.handle(context.Background(), msg("/tasks"))
	if !strings.Contains(tg.texts(), "Something went wrong") {
		t.Errorf("the owner should hear that it failed:\n%s", tg.texts())
	}
	b.ops.collect = rec.ops().collect
	b.handle(context.Background(), msg("/tasks"))
	if !strings.Contains(tg.texts(), "Open issues") {
		t.Error("the bot should keep working after a panic")
	}
}

func TestRunLoop(t *testing.T) {
	b, tg, rec := newBot(t)
	tg.polls = [][]tgapi.Update{
		{{ID: 40, Message: msg("/tell 7 old backlog").Message}}, // the startup drain (offset -1)
		{{ID: 41, Message: msg("/help").Message}},               // a real update
		nil, // a failed poll: the loop backs off and continues
		{{ID: 42, Message: msg("/ask 7 still alive?").Message}},
	}
	tg.pollErr = []error{nil, nil, errors.New("network is down: token 123456789:AAH-secret_token-value-0123456789abc"), nil}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.run(ctx) }()
	deadline := time.After(3 * time.Second)
	for {
		tg.mu.Lock()
		got := len(rec.asked)
		tg.mu.Unlock()
		if got == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the loop stalled; polled offsets %v", tg.polled)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("shutdown should be clean, got %v", err)
	}
	if len(rec.sent) != 0 {
		t.Error("a message that arrived before the bot started must not run")
	}
	if tg.polled[0] != -1 || tg.polled[1] != 41 || tg.polled[2] != 42 || tg.polled[3] != 42 || tg.polled[4] != 43 {
		t.Errorf("offsets = %v; want -1 to drain the backlog, then each update acknowledged once", tg.polled[:5])
	}
	if !strings.Contains(tg.texts(), "fleet, from your phone") {
		t.Error("the /help that arrived while running should have been answered")
	}
	bad := &fakeTG{meErr: errors.New("401")}
	if err := newTGBot(bad, ownerChat, 0, rec.ops()).run(context.Background()); err == nil {
		t.Error("a rejected token must stop the bot at startup, not loop")
	}
}

func TestBotLogsNeverContainTheToken(t *testing.T) {
	r, w, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = w
	b, tg, _ := newBot(t)
	tg.polls = [][]tgapi.Update{nil, nil}
	tg.pollErr = []error{nil, errors.New("Get https://api.telegram.org/bot123456789:AAH-secret_token-value-0123456789abc/getUpdates: dial tcp: lookup failed")}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = b.run(ctx)
	w.Close()
	os.Stderr = old
	buf := make([]byte, 1<<16)
	n, _ := r.Read(buf)
	log := string(buf[:n])
	if !strings.Contains(log, "retrying") {
		t.Fatalf("the failed poll should have been logged:\n%s", log)
	}
	if strings.Contains(log, "AAH-secret") || strings.Contains(log, "123456789:") {
		t.Errorf("the bot token reached the log:\n%s", log)
	}
}

func TestLoadTGSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	old := cfg
	defer func() { cfg = old }()
	cfg = &config.Fleet{Notify: config.Notify{Telegram: &config.Telegram{TokenSecret: "TG_TOKEN", ChatSecret: "TG_CHAT"}}}
	write := func(body string) {
		os.MkdirAll(filepath.Join(home, ".config/fleet"), 0o755)
		os.WriteFile(filepath.Join(home, ".config/fleet/env"), []byte(body), 0o600)
	}
	t.Setenv("TG_TOKEN", "")
	write("OTHER=1\n")
	if _, err := loadTGSettings(); err == nil || !strings.Contains(err.Error(), "TG_TOKEN isn't set") {
		t.Errorf("missing token: %v", err)
	}
	write("TG_TOKEN=123:abc\nTG_CHAT=1001\n")
	s, err := loadTGSettings()
	if err != nil || s.token != "123:abc" || s.chat != 1001 || s.user != 0 {
		t.Errorf("%+v %v", s, err)
	}
	write("TG_TOKEN=123:abc\nTG_CHAT=-100500\nTG_USER=1001\n")
	if s, _ = loadTGSettings(); s.chat != -100500 || s.user != 1001 {
		t.Errorf("group settings: %+v", s)
	}
	write("TG_TOKEN=123:abc\nTG_CHAT=my-chat\n")
	if _, err := loadTGSettings(); err == nil || !strings.Contains(err.Error(), "isn't a number") {
		t.Errorf("non-numeric chat: %v", err)
	}
	cfg = &config.Fleet{}
	if _, err := loadTGSettings(); err == nil || !strings.Contains(err.Error(), "notify.telegram") {
		t.Errorf("unconfigured: %v", err)
	}
}

// A whole conversation through the real Bot API client and a fake Telegram server, so the
// JSON that goes over the wire (keyboards, callback data, edits) is exercised too.
func TestTelegramEndToEndOverHTTP(t *testing.T) {
	const tok = "123456789:AAH-secret_token-value-0123456789abc"
	var mu sync.Mutex
	var calls []map[string]any
	var methods []string
	queue := [][]map[string]any{
		{}, // the startup drain
		{{"update_id": 1, "message": map[string]any{"message_id": 1, "from": map[string]any{"id": ownerUser}, "chat": map[string]any{"id": ownerChat, "type": "private"}, "text": "/tell 7 run the gate in the foreground"}}},
	}
	var pressed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/bot"+tok+"/")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		methods = append(methods, method)
		calls = append(calls, body)
		switch method {
		case "getMe":
			fmt.Fprint(w, `{"ok":true,"result":{"id":9,"username":"fleet_bot"}}`)
		case "getUpdates":
			if len(queue) > 0 {
				ups := queue[0]
				queue = queue[1:]
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": ups})
				return
			}
			// After the /tell has been answered with a keyboard, press "Send".
			for i, m := range methods {
				if m == "sendMessage" && !pressed {
					kb := calls[i]["reply_markup"].(map[string]any)["inline_keyboard"].([]any)[0].([]any)[0].(map[string]any)
					pressed = true
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []map[string]any{{"update_id": 2, "callback_query": map[string]any{
						"id": "cb1", "from": map[string]any{"id": ownerUser}, "data": kb["callback_data"],
						"message": map[string]any{"message_id": 1, "chat": map[string]any{"id": ownerChat, "type": "private"}}}}}})
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
			fmt.Fprint(w, `{"ok":true,"result":[]}`)
		case "sendMessage":
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1001,"type":"private"}}}`)
		default:
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	}))
	defer srv.Close()

	client := tgapi.New(tok)
	client.Base = srv.URL
	rec := &recorder{}
	b := newTGBot(client, ownerChat, 0, rec.ops())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.run(ctx) }()
	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		sent := len(rec.sent)
		mu.Unlock()
		if sent == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no follow-up was sent; Bot API calls: %v", methods)
		case <-time.After(10 * time.Millisecond):
		}
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if rec.sent[0] != "run the gate in the foreground" {
		t.Errorf("sent = %v", rec.sent)
	}
	var confirm, edit map[string]any
	for i, m := range methods {
		if m == "sendMessage" && confirm == nil {
			confirm = calls[i]
		}
		if m == "editMessageText" {
			edit = calls[i]
		}
	}
	if confirm == nil || !strings.Contains(confirm["text"].(string), "Send this to impl-g on #7?") {
		t.Fatalf("the confirmation message: %v", confirm)
	}
	kb := confirm["reply_markup"].(map[string]any)["inline_keyboard"].([]any)[0].([]any)
	if len(kb) != 2 || !strings.HasPrefix(kb[0].(map[string]any)["callback_data"].(string), "t:") {
		t.Errorf("inline keyboard over the wire: %v", confirm["reply_markup"])
	}
	if edit == nil || !strings.Contains(edit["text"].(string), "✓ Sent to impl-g on #7") {
		t.Errorf("the confirmation should be edited to say it was sent: %v", edit)
	}
	for _, want := range []string{"getMe", "setMyCommands", "answerCallbackQuery"} {
		found := false
		for _, m := range methods {
			found = found || m == want
		}
		if !found {
			t.Errorf("never called %s (calls: %v)", want, methods)
		}
	}
}
