// Package twoway is the boundary between pr-mon and a chat service that posts
// a message with buttons and reports which button was tapped. Nothing here is
// specific to one service; each implementation decides its own markup.
package twoway

import (
	"context"
	"errors"
)

var (
	// ErrTooOld means the service refuses to delete a message this old.
	ErrTooOld = errors.New("message is too old to delete")
	// ErrGone means the message no longer exists.
	ErrGone = errors.New("message no longer exists")
	// ErrUnavailable means the channel can't be used right now, e.g. its
	// handler isn't running.
	ErrUnavailable = errors.New("the two-way handler isn't running")
)

// Availability says whether a channel is ready. Name is set when Ready; Why is
// a sentence saying what is wrong when not.
type Availability struct {
	Ready     bool
	Name, Why string
}

// Button is a key button, whose Key comes back in a Tap, or a link button when
// URL is set, which sends no tap.
type Button struct {
	Label, Key, URL string
}

// Message is structured content: a status (e.g. "READY"), a title, any further
// lines, and rows of buttons.
type Message struct {
	Status, Title string
	Lines         []string
	Buttons       [][]Button
}

// Tap is one button press: the message it was on and the button's Key.
// Receipt identifies the delivery, for Ack.
type Tap struct {
	MessageID, Key, Receipt string
}

// Channel is a two-way chat service.
type Channel interface {
	// Name is the service's name as clients show it, e.g. "Telegram".
	Name() string
	// Post sends a message and returns its ID.
	Post(ctx context.Context, message Message) (string, error)
	// Edit replaces a message; a message without buttons loses its buttons.
	Edit(ctx context.Context, id string, message Message) error
	// Delete removes a message: ErrTooOld if the service refuses, ErrGone if
	// it is already gone.
	Delete(ctx context.Context, id string) error
	// Receive waits a while for taps and returns those that arrived, possibly
	// none.
	Receive(ctx context.Context) ([]Tap, error)
	// Ack marks a tap handled. One not acknowledged is delivered again, so a
	// crash mid-handling doesn't lose it.
	Ack(ctx context.Context, tap Tap) error
}
