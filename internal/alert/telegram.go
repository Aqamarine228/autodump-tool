// Package alert sends operator notifications to a Telegram chat.
package alert

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxMessageRunes keeps messages under Telegram's 4096-character limit.
const maxMessageRunes = 4000

// Sender delivers a message and can pin it.
type Sender interface {
	Send(text string) (messageID int64, err error)
	Pin(messageID int64) error
}

// Telegram talks to the Telegram Bot API.
type Telegram struct {
	BaseURL string
	Client  *http.Client
	Backoff time.Duration // first retry delay, doubled on each attempt

	token    string
	chatID   string
	attempts int
}

// NewTelegram creates a Bot API sender for one chat.
func NewTelegram(token, chatID string, attempts int) *Telegram {
	if attempts < 1 {
		attempts = 1
	}
	return &Telegram{
		BaseURL:  "https://api.telegram.org",
		Client:   &http.Client{Timeout: 30 * time.Second},
		Backoff:  2 * time.Second,
		token:    token,
		chatID:   chatID,
		attempts: attempts,
	}
}

// Send posts a message, retrying failed attempts with exponential backoff.
func (t *Telegram) Send(text string) (int64, error) {
	if r := []rune(text); len(r) > maxMessageRunes {
		text = string(r[:maxMessageRunes-1]) + "…"
	}
	form := url.Values{"chat_id": {t.chatID}, "text": {text}}

	var lastErr error
	delay := t.Backoff
	for i := 0; i < t.attempts; i++ {
		if i > 0 && delay > 0 {
			time.Sleep(delay)
			delay *= 2
		}
		var res struct {
			MessageID int64 `json:"message_id"`
		}
		if lastErr = t.call("sendMessage", form, &res); lastErr == nil {
			return res.MessageID, nil
		}
	}
	return 0, lastErr
}

// Pin pins a message in the chat without notifying members again.
func (t *Telegram) Pin(messageID int64) error {
	form := url.Values{
		"chat_id":              {t.chatID},
		"message_id":           {strconv.FormatInt(messageID, 10)},
		"disable_notification": {"true"},
	}
	return t.call("pinChatMessage", form, nil)
}

func (t *Telegram) call(method string, form url.Values, result any) error {
	resp, err := t.Client.PostForm(t.BaseURL+"/bot"+t.token+"/"+method, form)
	if err != nil {
		return fmt.Errorf("telegram %s: %s", method, t.redact(err.Error()))
	}
	defer resp.Body.Close()

	var body struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("telegram %s: http %d: bad response", method, resp.StatusCode)
	}
	if !body.OK {
		return fmt.Errorf("telegram %s: http %d: %s", method, resp.StatusCode, body.Description)
	}
	if result != nil {
		if err := json.Unmarshal(body.Result, result); err != nil {
			return fmt.Errorf("telegram %s: decode result: %w", method, err)
		}
	}
	return nil
}

func (t *Telegram) redact(s string) string {
	if t.token == "" {
		return s
	}
	return strings.ReplaceAll(s, t.token, "***")
}
