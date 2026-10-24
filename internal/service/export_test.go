package service

import (
	"context"
	"time"

	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/state"
)

// PerformPinned lets the external tests call perform.
func (m *Monitor) PerformPinned(ctx context.Context, repoName string, number int,
	action models.Action, expectedHead string) error {
	return m.perform(ctx, repoName, number, action, expectedHead)
}

// ButtonsFor lets the external tests call buttonsFor.
func ButtonsFor(repo models.Repo, pr models.PullRequest, event string) []state.Button {
	return buttonsFor(repo, pr, event)
}

// DefaultTwoWay is the builder New uses when Options.TwoWay is nil.
var DefaultTwoWay = defaultTwoWay

// SetConfirmGuard changes how soon after "Force merge #N?" Confirm is taken.
func (m *Monitor) SetConfirmGuard(guard time.Duration) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.confirmGuard = guard
}

// SetReceiveBackoff changes the first pause after a failed receive.
func (m *Monitor) SetReceiveBackoff(pause time.Duration) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.receiveBackoff = pause
}
