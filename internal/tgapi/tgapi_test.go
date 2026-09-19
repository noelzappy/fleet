package tgapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const token = "123456789:AAH-secret_token-value-0123456789abc"

// fake is a Bot API server that records each call and answers from a table.
type fake struct {
	t     *testing.T
	calls []call
	reply map[string]string // method -> raw response body
}

type call struct {
	Method string
	Body   map[string]any
}

func (f *fake) server() (*Client, func()) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bot"+token+"/") {
			http.Error(w, "wrong token", http.StatusUnauthorized)
			return
		}
		method := strings.TrimPrefix(r.URL.Path, "/bot"+token+"/")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.calls = append(f.calls, call{method, body})
		if raw, ok := f.reply[method]; ok {
			w.Write([]byte(raw))
			return
		}
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	c := New(token)
	c.Base = srv.URL
	return c, srv.Close
}

func TestGetUpdatesAndSend(t *testing.T) {
	f := &fake{t: t, reply: map[string]string{
		"getUpdates": `{"ok":true,"result":[
		  {"update_id":10,"message":{"message_id":1,"from":{"id":42,"username":"em"},"chat":{"id":42,"type":"private"},"text":"/status"}},
		  {"update_id":11,"callback_query":{"id":"cb1","from":{"id":42},"data":"tell:ab12:y","message":{"message_id":9,"chat":{"id":42,"type":"private"}}}},
		  {"update_id":12,"message":{"message_id":2,"chat":{"id":42,"type":"private"},"text":"why?","reply_to_message":{"message_id":1,"chat":{"id":42},"text":"needs-human: #7 title"}}}]}`,
		"sendMessage": `{"ok":true,"result":{"message_id":77,"chat":{"id":42,"type":"private"}}}`,
	}}
	c, done := f.server()
	defer done()
	ctx := context.Background()

	ups, err := c.GetUpdates(ctx, 10, 30*time.Second)
	if err != nil || len(ups) != 3 {
		t.Fatalf("%v %v", ups, err)
	}
	if ups[0].Message.Text != "/status" || ups[0].Message.From.ID != 42 || ups[0].Message.Chat.Type != "private" {
		t.Errorf("message = %+v", ups[0].Message)
	}
	if ups[1].Callback == nil || ups[1].Callback.Data != "tell:ab12:y" || ups[1].Callback.Message.ID != 9 {
		t.Errorf("callback = %+v", ups[1].Callback)
	}
	if ups[2].Message.ReplyTo == nil || !strings.Contains(ups[2].Message.ReplyTo.Text, "#7") {
		t.Errorf("reply = %+v", ups[2].Message)
	}
	if got := f.calls[0].Body; got["offset"] != float64(10) || got["timeout"] != float64(30) {
		t.Errorf("getUpdates params = %v", got)
	}

	id, err := c.Send(ctx, 42, "hello <b>not html</b>", Keyboard{{Text: "Send", Data: "y"}, {Text: "Cancel", Data: "n"}})
	if err != nil || id != 77 {
		t.Fatalf("Send = %d %v", id, err)
	}
	body := f.calls[1].Body
	if _, ok := body["parse_mode"]; ok {
		t.Error("messages must be plain text: no parse_mode")
	}
	kb := body["reply_markup"].(map[string]any)["inline_keyboard"].([]any)[0].([]any)
	if len(kb) != 2 || kb[0].(map[string]any)["callback_data"] != "y" {
		t.Errorf("keyboard = %v", body["reply_markup"])
	}
	if body["text"] != "hello <b>not html</b>" || body["chat_id"] != float64(42) {
		t.Errorf("send body = %v", body)
	}
	c.Send(ctx, 42, "no buttons", nil)
	if _, ok := f.calls[2].Body["reply_markup"]; ok {
		t.Error("no keyboard should mean no reply_markup")
	}
	if err := c.Edit(ctx, 42, 77, "done"); err != nil || f.calls[3].Method != "editMessageText" {
		t.Errorf("edit: %v %v", err, f.calls[3])
	}
	c.AnswerCallback(ctx, "cb1", "ok")
	c.Typing(ctx, 42)
	c.SetCommands(ctx, []Command{{"status", "queue"}})
	var methods []string
	for _, cl := range f.calls[4:] {
		methods = append(methods, cl.Method)
	}
	if strings.Join(methods, ",") != "answerCallbackQuery,sendChatAction,setMyCommands" {
		t.Errorf("methods = %v", methods)
	}
}

func TestErrorsNeverContainTheToken(t *testing.T) {
	f := &fake{t: t, reply: map[string]string{
		"getMe": `{"ok":false,"error_code":401,"description":"Unauthorized: token ` + token + ` is invalid"}`}}
	c, done := f.server()
	defer done()
	_, err := c.GetMe(context.Background())
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != 401 {
		t.Fatalf("want a 401 *Error, got %v", err)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "AAH-secret") {
		t.Errorf("token leaked through an API error: %v", err)
	}

	// A transport error carries the request URL, which contains the token.
	dead := New(token)
	dead.Base = "http://127.0.0.1:1"
	_, err = dead.GetMe(context.Background())
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "AAH-secret") {
		t.Errorf("token leaked through a transport error: %v", err)
	}
	// A cancelled context is reported as such (the run loop treats it as shutdown).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.GetMe(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context = %v", err)
	}
}

func TestChunk(t *testing.T) {
	if got := Chunk("  \n ", 100); got != nil {
		t.Errorf("blank = %v", got)
	}
	if got := Chunk("short", 100); len(got) != 1 || got[0] != "short" {
		t.Errorf("short = %v", got)
	}
	long := strings.Repeat("line of text\n", 100) // 1300 bytes
	parts := Chunk(long, 300)
	if len(parts) < 4 {
		t.Fatalf("expected several parts, got %d", len(parts))
	}
	for i, p := range parts {
		if n := len([]rune(p)); n > 300 || n == 0 {
			t.Errorf("part %d has %d runes", i, n)
		}
		if strings.HasPrefix(p, "\n") || strings.HasSuffix(p, "\n") {
			t.Errorf("part %d isn't trimmed: %q", i, p)
		}
	}
	if strings.Count(strings.Join(parts, "\n"), "line of text") != 100 {
		t.Error("chunking lost or duplicated text")
	}
	// No newline to cut on, and wide runes: still within the limit and lossless.
	wide := strings.Repeat("é", 1000)
	parts = Chunk(wide, 400)
	if strings.Join(parts, "") != wide {
		t.Error("multi-byte text was corrupted when cut")
	}
	for _, p := range parts {
		if len([]rune(p)) > 400 {
			t.Errorf("part of %d runes", len([]rune(p)))
		}
	}
}
