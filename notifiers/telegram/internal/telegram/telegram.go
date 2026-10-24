// Package telegram is a two-way channel over Telegram, reached through an AWS
// relay: the sender Lambda sends, edits and deletes messages, and
// button taps arrive on an SQS queue as raw Telegram updates.
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/acheris-labs/pr-mon/notifiers/telegram/internal/aws"
)

const (
	// Telegram's limits: message text after entities are parsed, and a
	// button's callback_data.
	maxText         = 4096
	maxCallbackData = 64

	receiveWait = 20 * time.Second
	receiveMax  = 10
)

// Route prefixes every button's callback_data, so taps meant for something
// else are told apart.
const Route = "pr-mon"

var (
	// ErrTooOld means Telegram refuses to delete a message this old.
	ErrTooOld = errors.New("message is too old to delete")
	// ErrGone means the message no longer exists.
	ErrGone = errors.New("message no longer exists")
)

// Message is what the protocol sends to be posted: a status, a title, detail
// lines and rows of buttons.
type Message struct {
	Status  string     `json:"status"`
	Title   string     `json:"title"`
	Lines   []string   `json:"lines"`
	Buttons [][]Button `json:"buttons"`
}

// Button is a callback button (Key) or a link (URL).
type Button struct {
	Label string `json:"label"`
	Key   string `json:"key,omitempty"`
	URL   string `json:"url,omitempty"`
}

// Tap is a button press: the message it was on, the button's key, and the
// queue receipt to acknowledge it with.
type Tap struct {
	MessageID, Key, Receipt string
}

// Settings say where the relay is. ChatID, when set, overrides the sender's
// default destination.
type Settings struct {
	QueueURL, SenderFunction, ChatID string
}

// Client posts, edits and deletes Telegram messages and receives button taps.
type Client struct {
	settings Settings
	client   *aws.Client
	log      *slog.Logger
}

func New(settings Settings, client *aws.Client, log *slog.Logger) *Client {
	return &Client{settings: settings, client: client, log: log}
}

// senderRequest is the sender Lambda's input.
type senderRequest struct {
	Message     string    `json:"message,omitempty"`
	ParseMode   string    `json:"parse_mode,omitempty"`
	ReplyMarkup *keyboard `json:"reply_markup,omitempty"`
	Destination string    `json:"destination,omitempty"`
	MessageID   int       `json:"message_id,omitempty"`
	Delete      bool      `json:"delete,omitempty"`
}

type keyboard struct {
	InlineKeyboard [][]button `json:"inline_keyboard"`
}

type button struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}

func (c *Client) Post(ctx context.Context, message Message) (string, error) {
	request, err := c.render(message)
	if err != nil {
		return "", err
	}
	reply, err := c.invoke(ctx, request)
	if err != nil {
		return "", err
	}
	var sent struct {
		MessageID int `json:"message_id"`
	}
	if err := json.Unmarshal(reply, &sent); err != nil || sent.MessageID == 0 {
		return "", fmt.Errorf("the sender returned no message_id: %s", reply)
	}
	return strconv.Itoa(sent.MessageID), nil
}

func (c *Client) Edit(ctx context.Context, id string, message Message) error {
	messageID, err := strconv.Atoi(id)
	if err != nil || messageID <= 0 {
		return fmt.Errorf("%w: message id %q isn't one this handler sent", ErrGone, id)
	}
	request, err := c.render(message)
	if err != nil {
		return err
	}
	request.MessageID = messageID
	_, err = c.invoke(ctx, request)
	switch {
	case refused(err, "message is not modified"):
		return nil
	case refused(err, "message to edit not found"):
		return fmt.Errorf("%w: %v", ErrGone, err)
	}
	return err
}

func (c *Client) Delete(ctx context.Context, id string) error {
	messageID, err := strconv.Atoi(id)
	if err != nil || messageID <= 0 {
		return fmt.Errorf("%w: message id %q isn't one this handler sent", ErrGone, id)
	}
	_, err = c.invoke(ctx, senderRequest{Delete: true, MessageID: messageID, Destination: c.settings.ChatID})
	switch {
	case refused(err, "message can't be deleted"):
		return fmt.Errorf("%w: %v", ErrTooOld, err)
	case refused(err, "message to delete not found"):
		return fmt.Errorf("%w: %v", ErrGone, err)
	}
	return err
}

// refused reports whether err is the sender passing on a Telegram error whose
// description contains `description`.
func refused(err error, description string) bool {
	var functionError *aws.FunctionError
	return errors.As(err, &functionError) && strings.Contains(functionError.Message, description)
}

func (c *Client) invoke(ctx context.Context, request senderRequest) (json.RawMessage, error) {
	return c.client.Invoke(ctx, c.settings.SenderFunction, request)
}

// update is the part of a Telegram Update a tap needs.
type update struct {
	UpdateID      int64 `json:"update_id"`
	CallbackQuery *struct {
		Data    string `json:"data"`
		Message *struct {
			MessageID int `json:"message_id"`
		} `json:"message"`
	} `json:"callback_query"`
}

// Receive long-polls the queue. Bodies that aren't one of our taps are logged
// and removed from the queue, so they don't come back.
func (c *Client) Receive(ctx context.Context) ([]Tap, error) {
	messages, err := c.client.Receive(ctx, c.settings.QueueURL, receiveWait, receiveMax)
	if err != nil {
		return nil, err
	}
	prefix := Route + ":"
	var taps []Tap
	for _, message := range messages {
		var body update
		err := json.Unmarshal([]byte(message.Body), &body)
		query := body.CallbackQuery
		reason := ""
		switch {
		case err != nil:
			reason = "unreadable"
		case query == nil || query.Message == nil:
			reason = "not a button tap"
		case !strings.HasPrefix(query.Data, prefix):
			reason = "another route"
		}
		if reason == "" {
			taps = append(taps, Tap{
				MessageID: strconv.Itoa(query.Message.MessageID),
				Key:       strings.TrimPrefix(query.Data, prefix),
				Receipt:   message.ReceiptHandle,
			})
			continue
		}
		c.log.Warn("skipping a queued Telegram update", "update_id", body.UpdateID, "reason", reason)
		if err := c.client.DeleteMessage(ctx, c.settings.QueueURL, message.ReceiptHandle); err != nil && !aws.ReceiptGone(err) {
			c.log.Warn("could not remove a skipped Telegram update", "update_id", body.UpdateID, "err", err)
		}
	}
	return taps, nil
}

// Ack removes a handled tap from the queue. A receipt SQS no longer knows
// (already deleted, or expired and redelivered) counts as done.
func (c *Client) Ack(ctx context.Context, receipt string) error {
	err := c.client.DeleteMessage(ctx, c.settings.QueueURL, receipt)
	if aws.ReceiptGone(err) {
		return nil
	}
	return err
}

var statusEmoji = map[string]string{
	"READY":        "✅",
	"FAILING":      "❌",
	"EARLY_FAIL":   "❌",
	"CONFLICT":     "⚠️",
	"BLOCKED":      "⛔",
	"BEHIND":       "⏪",
	"PENDING":      "⏳",
	"CHECKING":     "⏳",
	"WAITING":      "⏸️",
	"DRAFT":        "📝",
	"NEW":          "🆕",
	"MERGED":       "🎉",
	"MERGE_FAILED": "❗",
}

// render builds a send request: the status line and title in bold, then each
// line, with the plain text cut to Telegram's limit before it is escaped so no
// tag or entity is ever split.
func (c *Client) render(message Message) (senderRequest, error) {
	status := message.Status
	if emoji := statusEmoji[status]; emoji != "" {
		status = emoji + " " + status
	}
	budget := maxText - units(status)
	var text strings.Builder
	text.WriteString("<b>" + html.EscapeString(status) + "</b>")
	for i, line := range append([]string{message.Title}, message.Lines...) {
		budget-- // the newline
		if budget <= 0 {
			break
		}
		line = shorten(line, budget)
		budget -= units(line)
		text.WriteString("\n")
		if i == 0 && line != "" {
			text.WriteString("<b>" + html.EscapeString(line) + "</b>")
		} else {
			text.WriteString(html.EscapeString(line))
		}
	}

	request := senderRequest{Message: text.String(), ParseMode: "HTML", Destination: c.settings.ChatID}
	if len(message.Buttons) == 0 {
		return request, nil
	}
	request.ReplyMarkup = &keyboard{InlineKeyboard: make([][]button, len(message.Buttons))}
	for i, row := range message.Buttons {
		for _, b := range row {
			rendered := button{Text: b.Label, URL: b.URL}
			if b.URL == "" {
				rendered.CallbackData = Route + ":" + b.Key
				if len(rendered.CallbackData) > maxCallbackData {
					return senderRequest{}, fmt.Errorf("telegram callback_data %q is over %d bytes", rendered.CallbackData, maxCallbackData)
				}
			}
			request.ReplyMarkup.InlineKeyboard[i] = append(request.ReplyMarkup.InlineKeyboard[i], rendered)
		}
	}
	return request, nil
}

// units is a string's length as Telegram counts it, in UTF-16 code units.
func units(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// shorten cuts s to at most `budget` UTF-16 units, ending with an ellipsis
// when it cuts.
func shorten(s string, budget int) string {
	if units(s) <= budget {
		return s
	}
	var out strings.Builder
	used := 1 // the ellipsis
	for _, r := range s {
		if used+utf16.RuneLen(r) > budget {
			break
		}
		used += utf16.RuneLen(r)
		out.WriteRune(r)
	}
	return out.String() + "…"
}
