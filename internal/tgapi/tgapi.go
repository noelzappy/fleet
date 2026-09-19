// Package tgapi is the small part of the Telegram Bot API fleet uses: long-polling for
// messages and button presses, and replying. The bot token is part of every URL, so every
// error this package returns has it scrubbed; nothing here may print or wrap a raw URL.
package tgapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	Base  string // https://api.telegram.org
	Token string
	HTTP  *http.Client
}

func New(token string) *Client {
	return &Client{Base: "https://api.telegram.org", Token: token, HTTP: &http.Client{Timeout: 90 * time.Second}}
}

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	First    string `json:"first_name"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // private | group | supergroup | channel
}

type Message struct {
	ID      int64    `json:"message_id"`
	From    *User    `json:"from"`
	Chat    Chat     `json:"chat"`
	Text    string   `json:"text"`
	ReplyTo *Message `json:"reply_to_message"`
}

type Callback struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

type Update struct {
	ID       int64     `json:"update_id"`
	Message  *Message  `json:"message"`
	Callback *Callback `json:"callback_query"`
}

type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

// Keyboard is one row of inline buttons under a message.
type Keyboard []Button

type response struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	Code        int             `json:"error_code"`
}

// scrub removes the token from anything that might be shown or logged.
func (c *Client) scrub(s string) string {
	if c.Token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.Token, "***")
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/bot"+c.Token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram %s: %s", method, c.scrub(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("telegram %s: %s", method, c.scrub(err.Error()))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d, not a Bot API response", method, resp.StatusCode)
	}
	if !r.OK {
		return &Error{Method: method, Code: r.Code, Description: c.scrub(r.Description)}
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// Error is a failure the Bot API reported (a bad token is 401, a blocked bot 403, ...).
type Error struct {
	Method      string
	Code        int
	Description string
}

func (e *Error) Error() string {
	return fmt.Sprintf("telegram %s: %s (%d)", e.Method, e.Description, e.Code)
}

func (c *Client) GetMe(ctx context.Context) (User, error) {
	var u User
	err := c.call(ctx, "getMe", struct{}{}, &u)
	return u, err
}

// GetUpdates long-polls: it returns as soon as there is an update, or empty after timeout.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	var ups []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": int(timeout.Seconds()), "allowed_updates": []string{"message", "callback_query"},
	}, &ups)
	return ups, err
}

func markup(kb Keyboard) any {
	if len(kb) == 0 {
		return nil
	}
	return map[string]any{"inline_keyboard": []Keyboard{kb}}
}

// Send posts plain text (no parse mode, so nothing in an agent's output can break the
// message or be read as markup) and returns the new message's id.
func (c *Client) Send(ctx context.Context, chat int64, text string, kb Keyboard) (int64, error) {
	p := map[string]any{"chat_id": chat, "text": text, "disable_web_page_preview": true}
	if m := markup(kb); m != nil {
		p["reply_markup"] = m
	}
	var m Message
	err := c.call(ctx, "sendMessage", p, &m)
	return m.ID, err
}

// Edit replaces a message's text and drops its buttons.
func (c *Client) Edit(ctx context.Context, chat, msg int64, text string) error {
	return c.call(ctx, "editMessageText", map[string]any{"chat_id": chat, "message_id": msg, "text": text, "disable_web_page_preview": true}, nil)
}

func (c *Client) AnswerCallback(ctx context.Context, id, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}

func (c *Client) Typing(ctx context.Context, chat int64) error {
	return c.call(ctx, "sendChatAction", map[string]any{"chat_id": chat, "action": "typing"}, nil)
}

type Command struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

func (c *Client) SetCommands(ctx context.Context, cmds []Command) error {
	return c.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}

// Chunk splits text into messages under Telegram's 4096-character limit, on line breaks
// where it can, so a long answer arrives as a few readable messages instead of an error.
func Chunk(text string, max int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var out []string
	for len([]rune(text)) > max {
		r := []rune(text)
		cut := max
		if i := strings.LastIndex(string(r[:max]), "\n"); i > max/2 {
			cut = len([]rune(string(r[:max])[:i]))
		}
		out = append(out, strings.TrimSpace(string(r[:cut])))
		text = strings.TrimSpace(string(r[cut:]))
	}
	return append(out, text)
}
