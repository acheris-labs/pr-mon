// Taps on two-way messages: each is checked against GitHub as it is now and
// run through the same code as the dashboard's menu, then acknowledged.

package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/state"
	"github.com/acheris-labs/pr-mon/internal/twoway"
)

// Lines a tapped message gains, and why one is retired after a tap.
const (
	whyUntracked   = "No longer tracked"
	whyExpired     = "That button had expired"
	whyChanged     = "The PR changed since this message"
	whyUnavailable = "No longer available"
	whyDismissed   = "Dismissed"
	whyMerged      = "✅ Merged"
	whyRerun       = "🔁 Re-running failed checks"
	whyDone        = "✅ Done"
	whyNoHead      = "⚠️ GitHub hasn't said which commit the PR is at"
)

// Receive errors are logged and retried after a pause that doubles up to a
// minute. Whether two-way works is the channel's to report, through
// availability.
const (
	receiveBackoff    = 5 * time.Second
	maxReceiveBackoff = time.Minute
)

// A Confirm tapped sooner than this after "Force merge #N?" is taken for a
// stray second tap, and the question is asked again.
const confirmGuard = 2 * time.Second

// receiveLoop handles taps until the monitor stops, each in the background so
// a slow merge doesn't hold up the rest.
func (m *Monitor) receiveLoop() {
	defer close(m.receiving)
	m.mutex.Lock()
	channel := m.twoWay
	m.mutex.Unlock()
	pause := time.Duration(0)
	for {
		taps, err := channel.Receive(m.ctx)
		if m.ctx.Err() != nil {
			return
		}
		if err != nil {
			m.mutex.Lock()
			first := m.receiveBackoff
			m.mutex.Unlock()
			pause = min(max(2*pause, first), maxReceiveBackoff)
			m.log.Warn("two-way receive failed", "channel", channel.Name(), "error", err,
				"retry_in", pause)
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(pause):
			}
			continue
		}
		pause = 0
		for _, tap := range taps {
			m.spawn(func() { m.handleTap(m.ctx, tap) })
		}
	}
}

// handleTap acts on one tap and acknowledges it, whatever the outcome.
func (m *Monitor) handleTap(ctx context.Context, tap twoway.Tap) {
	id := tap.MessageID
	m.mutex.Lock()
	channel := m.twoWay
	record, tracked := m.state.Messages[id]
	if tracked && record.Retiring != "" {
		// Its delete waited for the handler, which is back: retry it.
		gone := m.claimLocked(id, record.Retiring)
		m.saveStateLocked()
		m.mutex.Unlock()
		m.retire(channel, gone)
		m.ack(ctx, channel, tap)
		return
	}
	button, known := record.Keys[tap.Key]
	busy := m.tapping[id]
	tooSoon := button.Kind == "confirm_force_merge" && askedWithin(record, m.confirmGuard)
	if tracked && !busy {
		// Every key goes before anything slow, so a second tap finds nothing.
		m.tapping[id] = true
		record.Keys = nil
		m.state.Messages[id] = record
		m.saveStateLocked()
	}
	m.mutex.Unlock()
	defer m.ack(ctx, channel, tap)
	switch {
	case !tracked:
		// Not deleted: another backend may own it.
		// The tap doesn't carry the message's text, so it can't be kept; the
		// title stops Telegram showing an empty bold line instead.
		m.edit(ctx, channel, id, twoway.Message{Title: "pr-mon", Lines: []string{whyUntracked}})
		return
	case busy:
		return // another tap on it is being acted on, and will replace it
	}
	defer func() {
		m.mutex.Lock()
		delete(m.tapping, id)
		m.mutex.Unlock()
	}()
	switch {
	case !known:
		m.rerender(ctx, channel, id, whyExpired)
	case button.Kind == "dismiss":
		m.finish(channel, id, whyDismissed)
	case button.Kind == "force_merge", tooSoon:
		m.askToConfirm(ctx, channel, id, button)
	case button.Kind == "cancel":
		m.rerender(ctx, channel, id, "")
	default:
		m.act(ctx, channel, id, record, button)
	}
}

func (m *Monitor) ack(ctx context.Context, channel twoway.Channel, tap twoway.Tap) {
	if err := channel.Ack(ctx, tap); err != nil {
		m.log.Warn("two-way tap not acknowledged", "channel", channel.Name(), "error", err)
	}
}

// act runs a merge, confirmed force merge or re-run, but only if GitHub, asked
// now, shows the PR at the commit the message was about with the action still
// on offer.
func (m *Monitor) act(ctx context.Context, channel twoway.Channel, id string,
	record state.Message, button state.Button) {
	if record.HeadSha == "" && button.Kind != "rerun_checks" {
		// A merge is always pinned to the commit the message was about.
		m.rerender(ctx, channel, id, whyNoHead)
		return
	}
	if err := m.refreshPRs(ctx, record.Repo, []int{record.Number}); err != nil {
		m.rerender(ctx, channel, id, "⚠️ "+err.Error())
		return
	}
	repo, loaded := m.Repo(record.Repo)
	if !loaded {
		m.rerender(ctx, channel, id, fmt.Sprintf("⚠️ %s hasn't loaded yet", record.Repo))
		return
	}
	pr, open := findPR(repo, record.Number)
	switch {
	case !open && repo.PRTotal > len(repo.PRs):
		// Beyond the newest-N cut-off it may still be open, as in checkArmed.
		m.rerender(ctx, channel, id, whyUnavailable)
		return
	case !open:
		m.finish(channel, id, whyClosed)
		return
	case headOf(pr) != record.HeadSha:
		m.rerender(ctx, channel, id, whyChanged)
		return
	}
	kind := button.Kind
	if kind == "confirm_force_merge" {
		kind = "force_merge"
	}
	if why := unavailable(pr, kind); why != "" {
		m.rerender(ctx, channel, id, why)
		return
	}
	action := models.Action{Kind: kind, DeleteBranch: button.DeleteBranch}
	if button.Method != "" {
		method := models.MergeMethod(button.Method)
		action.Method = &method
	}
	// The recorded head pins a merge to the commit the message was about.
	if err := m.perform(ctx, record.Repo, record.Number, action, record.HeadSha); err != nil {
		m.rerender(ctx, channel, id, "⚠️ "+err.Error())
		return
	}
	switch kind {
	case "merge", "force_merge":
		m.finish(channel, id, whyMerged)
	case "rerun_checks":
		m.finish(channel, id, whyRerun)
	default:
		m.finish(channel, id, whyDone)
	}
}

func findPR(repo models.Repo, number int) (models.PullRequest, bool) {
	for _, pr := range repo.PRs {
		if pr.Number == number {
			return pr, true
		}
	}
	return models.PullRequest{}, false
}

// unavailable is why the PR's option of this kind isn't on offer, or "" if it is.
func unavailable(pr models.PullRequest, kind string) string {
	for _, option := range pr.Actions {
		if option.Kind != kind {
			continue
		}
		if option.Available {
			return ""
		}
		if option.Reason != nil {
			return *option.Reason
		}
		break
	}
	return whyUnavailable
}

// askedWithin reports whether "Force merge #N?" went up less than guard ago.
func askedWithin(record state.Message, guard time.Duration) bool {
	asked, err := time.Parse(time.RFC3339Nano, record.ConfirmAskedAt)
	return err == nil && time.Since(asked) < guard
}

// askToConfirm turns a message into "Force merge #N?" with Cancel, then
// Confirm. Cancel takes the top row, where Re-run or Force merge was, so a
// second tap there doesn't confirm; one that lands on Confirm too soon is
// refused by handleTap, which asks again.
func (m *Monitor) askToConfirm(ctx context.Context, channel twoway.Channel, id string,
	button state.Button) {
	rows, keys := compose([]state.Button{
		{Kind: "cancel"},
		{Kind: "confirm_force_merge", Method: button.Method, DeleteBranch: button.DeleteBranch},
	}, "")
	m.mutex.Lock()
	record, tracked := m.state.Messages[id]
	if !tracked {
		m.mutex.Unlock()
		return // retired meanwhile
	}
	record.Keys = keys
	record.ConfirmAskedAt = nowISO()
	m.state.Messages[id] = record
	m.saveStateLocked()
	m.mutex.Unlock()
	m.edit(ctx, channel, id, twoway.Message{
		Status:  record.Event,
		Title:   record.Title,
		Lines:   append(ownLines(record), fmt.Sprintf("Force merge #%d?", record.Number)),
		Buttons: rows,
	})
}

// rerender redraws a message with the buttons on offer now, fresh keys, and a
// line saying why (none when cancelling). It keeps the event and status the
// message was sent about, so cleanup still judges it by those. Its buttons now
// act on the PR's current head; if that moved, it says so, so no button ever
// acts on a commit the message hasn't announced.
func (m *Monitor) rerender(ctx context.Context, channel twoway.Channel, id, why string) {
	m.mutex.Lock()
	record, tracked := m.state.Messages[id]
	if !tracked {
		m.mutex.Unlock()
		return // retired meanwhile
	}
	repo := m.repos[record.Repo]
	pr, found := findPR(repo, record.Number)
	var message twoway.Message
	lines := []string{}
	if why != "" {
		lines = append(lines, why)
	}
	if found {
		if headOf(pr) != record.HeadSha && why != whyChanged {
			lines = append(lines, whyChanged)
		}
		var fresh state.Message
		message, fresh = prMessage(repo, pr, record.Event, "")
		record.Title, record.Lines, record.Keys, record.HeadSha =
			fresh.Title, fresh.Lines, fresh.Keys, fresh.HeadSha
	} else {
		// A test message, or a PR not loaded: nothing to offer but Dismiss.
		rows, keys := compose([]state.Button{{Kind: "dismiss"}}, "")
		message = twoway.Message{Status: record.Event, Title: record.Title,
			Lines: ownLines(record), Buttons: rows}
		record.Keys = keys
	}
	record.ConfirmAskedAt = ""
	m.state.Messages[id] = record
	m.saveStateLocked()
	m.mutex.Unlock()
	message.Lines = append(append([]string{}, message.Lines...), lines...)
	m.edit(ctx, channel, id, message)
}

// ownLines is a message's first line, the one it was sent with (author and
// branch, or the test text), copied so callers may append.
func ownLines(record state.Message) []string {
	return append([]string{}, record.Lines[:min(1, len(record.Lines))]...)
}

// finish retires a message whose work is done, unless something else retired
// it meanwhile (a merge's own refresh sees the PR close).
func (m *Monitor) finish(channel twoway.Channel, id, why string) {
	m.mutex.Lock()
	if _, tracked := m.state.Messages[id]; !tracked {
		m.mutex.Unlock()
		return
	}
	gone := m.claimLocked(id, why)
	m.saveStateLocked()
	m.mutex.Unlock()
	m.retire(channel, gone)
}

// edit replaces a message; one already deleted (a double-tapped Dismiss) needs nothing.
func (m *Monitor) edit(ctx context.Context, channel twoway.Channel, id string, message twoway.Message) {
	if err := channel.Edit(ctx, id, message); err != nil && !errors.Is(err, twoway.ErrGone) {
		m.twoWayFailed(channel, err)
	}
}
