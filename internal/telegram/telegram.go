// Package telegram is a minimal Telegram Bot API client: long polling,
// sending messages, and registering the command menu. Three methods do not
// justify a dependency, and a hand-written client is trivially faked in tests.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.telegram.org"

// Client talks to one bot.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a client for the bot with the given token.
func New(token string) *Client {
	return NewWithBaseURL(defaultBaseURL, token)
}

// NewWithBaseURL points the client somewhere else; tests use it.
func NewWithBaseURL(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		// Must outlast the long-poll timeout below, or every idle poll would be
		// reported as a failure.
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// User is a Telegram account.
type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// Chat is where a message was sent.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// ChatPrivate is a one-to-one conversation with the bot.
const ChatPrivate = "private"

// Message is an incoming message.
type Message struct {
	MessageID int64  `json:"message_id"`
	From      *User  `json:"from"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
}

// Update is one event from getUpdates.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// PollTimeout is how long a single getUpdates call waits for new events.
// Long polling is what lets the bot run without a public URL of its own.
const PollTimeout = 30 * time.Second

// GetUpdates long-polls for events after offset.
func (c *Client) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	var updates []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         int(PollTimeout.Seconds()),
		"allowed_updates": []string{"message"},
	}, &updates)
	return updates, err
}

// Button is an inline keyboard button that opens a URL.
type Button struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

// SendMessage sends HTML-formatted text, optionally with one row of URL buttons.
func (c *Client) SendMessage(ctx context.Context, chatID int64, html string, buttons ...Button) error {
	params := map[string]any{
		"chat_id":    chatID,
		"text":       html,
		"parse_mode": "HTML",
		// A preview of the docs link would bury the message it belongs to.
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	if len(buttons) > 0 {
		params["reply_markup"] = map[string]any{"inline_keyboard": [][]Button{buttons}}
	}
	return c.call(ctx, "sendMessage", params, nil)
}

// Command is one entry of the menu Telegram shows under the message box.
type Command struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// SetCommands registers the command menu.
func (c *Client) SetCommands(ctx context.Context, commands []Command) error {
	return c.call(ctx, "setMyCommands", map[string]any{"commands": commands}, nil)
}

type envelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
}

func (c *Client) call(ctx context.Context, method string, params map[string]any, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("telegram: encode %s: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/bot"+c.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: build %s request: %w", method, redact(err))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: %s: %w", method, redact(err))
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("telegram: read %s response: %w", method, err)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("telegram: %s: unexpected response (HTTP %d)", method, resp.StatusCode)
	}
	if !env.OK {
		return fmt.Errorf("telegram: %s failed (%d): %s", method, env.ErrorCode, env.Description)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("telegram: decode %s result: %w", method, err)
	}
	return nil
}

// redact strips the request URL from a transport error.
//
// The bot token is part of every Bot API URL, and net/http formats errors as
// `Post "https://api.telegram.org/bot<token>/getUpdates": dial tcp: ...`. Logged
// as is, the first network blip would write the token -- full control of the
// bot -- into the logs.
func redact(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s request failed: %w", urlErr.Op, urlErr.Err)
	}
	return err
}
