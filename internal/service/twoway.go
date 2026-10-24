// Two-way notifications: a message with buttons for each PR that changes
// state, deleted once its work is done, a newer one replaces it, or the PR
// moves on. The chat is a to-do list: what's left in it still needs you.

package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/notify"
	"github.com/acheris-labs/pr-mon/internal/state"
	"github.com/acheris-labs/pr-mon/internal/twoway"
	"github.com/acheris-labs/pr-mon/internal/twoway/handler"
)

// twoWayOff starts the status warning shown while two-way doesn't work.
const twoWayOff = "Two-way notifications are off: "

// Why a message was retired, shown on one too old to delete.
const (
	whyClosed     = "PR closed"
	whyMoved      = "Status changed"
	whySuperseded = "Superseded"
	whyRemoved    = "Repo removed"
)

var mergeLabels = map[models.MergeMethod]string{
	models.MergeSquash: "Squash & merge",
	models.MergeCommit: "Merge commit",
	models.MergeRebase: "Rebase & merge",
}

// defaultTwoWay builds the builder that runs the handler program: the
// runner reports through onChange whether the handler is up, and keeps
// restarting it. openTwoWay has checked the command splits.
func defaultTwoWay(version string, log *slog.Logger) func(context.Context, config.TwoWayConfig,
	func(twoway.Availability)) (twoway.Channel, error) {
	return func(ctx context.Context, table config.TwoWayConfig,
		onChange func(twoway.Availability)) (twoway.Channel, error) {
		argv, err := notify.ScriptArgv(table.Command)
		if err != nil {
			return nil, fmt.Errorf("[two_way] command: %w", err)
		}
		return handler.Start(ctx, handler.Settings{
			Argv: argv, Version: version, Log: log, OnChange: onChange,
		}), nil
	}
}

// openTwoWay builds the channel the [two_way] table asks for, or says why it can't.
func (m *Monitor) openTwoWay(table config.TwoWayConfig) (twoway.Channel, string) {
	if problem := table.Problem(); problem != "" {
		return nil, problem
	}
	if _, err := notify.ScriptArgv(table.Command); err != nil {
		return nil, "[two_way] command: " + err.Error()
	}
	channel, err := m.buildTwoWay(m.ctx, table, m.availability)
	if err != nil {
		return nil, err.Error()
	}
	return channel, ""
}

// availability is the channel's onChange. It shows in the status whether
// two-way works and, once it does, retries the retirements left waiting. The
// channel may call it from its own goroutine, even while Stop closes it, so it
// only takes the lock briefly and never waits on the channel.
func (m *Monitor) availability(now twoway.Availability) {
	m.mutex.Lock()
	warnings := slices.DeleteFunc(slices.Clone(m.status.Warnings), func(w string) bool {
		return strings.HasPrefix(w, twoWayOff)
	})
	var name *string
	if now.Ready {
		name = models.Ptr(now.Name)
	} else {
		warnings = append(warnings, twoWayOff+now.Why)
	}
	changed := !slices.Equal(warnings, m.status.Warnings) ||
		(name == nil) != (m.status.TwoWay == nil) ||
		(name != nil && *name != *m.status.TwoWay)
	m.status.Warnings = warnings
	m.status.TwoWay = name
	if now.Ready {
		m.readyCount++
		m.retireWaitingLocked()
	}
	m.mutex.Unlock()
	if now.Ready {
		m.log.Info("two-way notifications are on", "name", now.Name)
	} else {
		m.log.Warn("two-way notifications are off", "why", now.Why)
	}
	if changed {
		m.emit(Event{Kind: "status"})
	}
}

// retireWaitingLocked retires, in the background, the messages whose
// retirement waited for the channel. Nothing once the monitor is stopping, nor
// before Start has the channel.
func (m *Monitor) retireWaitingLocked() {
	channel := m.twoWay
	if channel == nil || m.ctx.Err() != nil {
		return
	}
	retiring := []retiree{}
	for id, message := range m.state.Messages {
		if message.Retiring != "" {
			retiring = append(retiring, m.claimLocked(id, message.Retiring))
		}
	}
	if len(retiring) == 0 {
		return
	}
	m.saveStateLocked()
	m.spawn(func() {
		for _, gone := range retiring {
			m.retire(channel, gone)
		}
	})
}

// sendsTwoWay reports whether this event goes over the two-way channel too.
// MERGED doesn't: the PR has closed, so its message would be deleted at once.
func sendsTwoWay(settings config.NotifyConfig, event string) bool {
	return settings.TwoWayEnabled && event != "MERGED"
}

// buttonsFor is what a message about this event offers, from the PR's
// available actions: merges when READY, re-run and force merge when FAILING,
// and always Dismiss. The subset is deliberate (see the design).
func buttonsFor(repo models.Repo, pr models.PullRequest, event string) []state.Button {
	available := map[string]models.ActionOption{}
	for _, option := range pr.Actions {
		if option.Available {
			available[option.Kind] = option
		}
	}
	buttons := []state.Button{}
	switch event {
	case "READY":
		if merge, found := available["merge"]; found {
			for _, method := range repo.MergeMethods {
				buttons = append(buttons, state.Button{Kind: "merge", Method: string(method),
					DeleteBranch: merge.OffersDeleteBranch})
			}
		}
	case "FAILING":
		if _, found := available["rerun_checks"]; found {
			buttons = append(buttons, state.Button{Kind: "rerun_checks"})
		}
		if force, found := available["force_merge"]; found && len(repo.MergeMethods) > 0 {
			buttons = append(buttons, state.Button{Kind: "force_merge",
				Method: string(repo.MergeMethods[0]), DeleteBranch: force.OffersDeleteBranch})
		}
	}
	return append(buttons, state.Button{Kind: "dismiss"})
}

func buttonLabel(button state.Button) string {
	switch button.Kind {
	case "merge":
		return mergeLabels[models.MergeMethod(button.Method)]
	case "force_merge":
		return "Force merge"
	case "rerun_checks":
		return "Re-run failed checks"
	case "confirm_force_merge":
		return "Confirm"
	case "cancel":
		return "Cancel"
	}
	return "Dismiss"
}

// newKey is 8 random base-32 characters: short enough for any service's
// button payload, too many to guess.
func newKey() string { return strings.ToLower(rand.Text()[:8]) }

// compose gives each button a fresh key and lays them out: one action per
// row, then Open (when there's a link) and Dismiss together.
func compose(buttons []state.Button, link string) ([][]twoway.Button, map[string]state.Button) {
	keys := map[string]state.Button{}
	rows := [][]twoway.Button{}
	last := []twoway.Button{}
	if link != "" {
		last = append(last, twoway.Button{Label: "Open", URL: link})
	}
	for _, button := range buttons {
		key := newKey()
		for keys[key].Kind != "" { // taken; vanishingly rare
			key = newKey()
		}
		keys[key] = button
		shown := twoway.Button{Label: buttonLabel(button), Key: key}
		if button.Kind == "dismiss" {
			last = append(last, shown)
		} else {
			rows = append(rows, []twoway.Button{shown})
		}
	}
	if len(last) > 0 {
		rows = append(rows, last)
	}
	return rows, keys
}

// prMessage is a message about a PR's event, and its record with fresh keys.
func prMessage(repo models.Repo, pr models.PullRequest, event, reason string) (twoway.Message, state.Message) {
	lines := []string{fmt.Sprintf("%s · %s → %s", pr.Author, pr.HeadRef, pr.BaseRef)}
	if reason != "" {
		lines = append(lines, reason)
	}
	rows, keys := compose(buttonsFor(repo, pr, event), pr.URL)
	message := twoway.Message{
		Status:  event,
		Title:   fmt.Sprintf("%s #%d · %s", repo.Name, pr.Number, pr.Title),
		Lines:   lines,
		Buttons: rows,
	}
	head := ""
	if pr.HeadSha != nil {
		head = *pr.HeadSha
	}
	record := state.Message{
		Repo: repo.Name, Number: pr.Number, Event: event, Status: string(pr.Status),
		HeadSha: head, Title: message.Title, Lines: lines, Keys: keys,
	}
	return message, record
}

// retiree is a message no longer tracked, still to be deleted.
type retiree struct {
	id      string
	message state.Message
	why     string
}

// claimLocked stops tracking a message and returns it for retiring. Claiming
// under the lock means two refreshes never retire the same message.
func (m *Monitor) claimLocked(id, why string) retiree {
	message := m.state.Messages[id]
	delete(m.state.Messages, id)
	return retiree{id: id, message: message, why: why}
}

// syncMessages runs after every refresh of a repo, the first included: it
// retires messages whose PR closed or moved to a status no new message is
// about, then posts the new ones, in the background.
func (m *Monitor) syncMessages(repo models.Repo, posts []notify.Notification) {
	m.mutex.Lock()
	channel := m.twoWay
	if channel == nil {
		m.mutex.Unlock()
		return
	}
	prs := map[int]models.PullRequest{}
	for _, pr := range repo.PRs {
		prs[pr.Number] = pr
	}
	posting := map[int]bool{}
	for _, post := range posts {
		posting[post.PR.Number] = true
	}
	// Beyond the newest-N cut-off a PR may still be open, as in checkArmed.
	truncated := repo.PRTotal > len(repo.PRs)
	retiring := []retiree{}
	for id, message := range m.state.Messages {
		if message.Repo != repo.Name || message.Number == 0 || message.Retiring != "" {
			continue // another repo's, a test message, or one already retiring
		}
		pr, open := prs[message.Number]
		switch {
		case !open && !truncated:
			retiring = append(retiring, m.claimLocked(id, whyClosed))
		case open && string(pr.Status) != message.Status && !posting[message.Number]:
			retiring = append(retiring, m.claimLocked(id, whyMoved))
		}
	}
	if len(retiring) > 0 {
		m.saveStateLocked()
	}
	m.mutex.Unlock()
	if len(retiring) == 0 && len(posts) == 0 {
		return
	}
	m.spawn(func() {
		for _, gone := range retiring {
			m.retire(channel, gone)
		}
		for _, post := range posts {
			message, record := prMessage(repo, post.PR, post.State, "")
			m.post(channel, message, record)
		}
	})
}

// post sends a message first, then retires the PR's previous one and records
// the new one, so a failed post leaves the previous message tracked.
func (m *Monitor) post(channel twoway.Channel, message twoway.Message, record state.Message) error {
	id, err := channel.Post(m.ctx, message)
	if err != nil {
		m.twoWayFailed(channel, err)
		return err
	}
	record.SentAt = nowISO()
	m.mutex.Lock()
	previous := []retiree{}
	for oldID, old := range m.state.Messages {
		if old.Repo == record.Repo && old.Number == record.Number {
			previous = append(previous, m.claimLocked(oldID, whySuperseded))
		}
	}
	m.state.Messages[id] = record
	m.saveStateLocked()
	m.mutex.Unlock()
	for _, gone := range previous {
		m.retire(channel, gone)
	}
	return nil
}

// retire deletes a message already forgotten. One too old to delete loses its
// buttons and says why it's done instead; one already gone needs nothing. One
// the channel can't reach now, or that Stop cut short, is tracked again,
// without keys, until it can.
func (m *Monitor) retire(channel twoway.Channel, gone retiree) {
	m.mutex.Lock()
	readies := m.readyCount
	m.mutex.Unlock()
	err := channel.Delete(m.ctx, gone.id)
	switch {
	case err == nil, errors.Is(err, twoway.ErrGone):
	case errors.Is(err, twoway.ErrTooOld):
		done := twoway.Message{
			Status: gone.message.Event,
			Title:  gone.message.Title,
			Lines:  append(append([]string{}, gone.message.Lines...), gone.why),
		}
		switch err := channel.Edit(m.ctx, gone.id, done); {
		case m.interrupted(err):
			m.keepRetiring(gone, readies)
		case err != nil:
			m.twoWayFailed(channel, err)
		}
	case m.interrupted(err):
		m.keepRetiring(gone, readies)
	default:
		m.twoWayFailed(channel, err)
	}
}

// interrupted says whether a call failed only because the handler is down or
// the monitor is stopping, so it can be tried again later.
func (m *Monitor) interrupted(err error) bool {
	return err != nil && (errors.Is(err, twoway.ErrUnavailable) || m.ctx.Err() != nil)
}

// keepRetiring tracks a message again, without keys, with why it's being
// retired, for availability or a tap on it to retry once the handler is back.
// If the handler reported ready since readies was read, that report found
// nothing to retry, so it retries now.
func (m *Monitor) keepRetiring(gone retiree, readies int) {
	m.log.Info("two-way message kept until the handler is back", "id", gone.id, "why", gone.why)
	record := gone.message
	record.Retiring, record.Keys = gone.why, nil
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.state.Messages[gone.id] = record
	m.saveStateLocked()
	if m.readyCount != readies {
		m.retireWaitingLocked()
	}
}

// twoWayFailed reports a failed call. One failing because the handler is down
// is only logged: the status warning already says so.
func (m *Monitor) twoWayFailed(channel twoway.Channel, err error) {
	m.log.Warn("two-way notification failed", "channel", channel.Name(), "error", err)
	if !errors.Is(err, twoway.ErrUnavailable) {
		m.toast(fmt.Sprintf("%s notification failed: %v", channel.Name(), err), "warning")
	}
}

// postSample is "Send test" over the two-way channel: a sample message with
// only Dismiss, tracked as PR 0 so cleanup leaves it alone.
func (m *Monitor) postSample(channel twoway.Channel, repo string) {
	variables := notify.SampleVariables(repo)
	rows, keys := compose([]state.Button{{Kind: "dismiss"}}, "")
	lines := []string{"A test message from pr-mon"}
	message := twoway.Message{
		Status:  "TEST",
		Title:   fmt.Sprintf("%s #%s · %s", repo, variables["PR_NUM"], variables["PR_TITLE"]),
		Lines:   lines,
		Buttons: rows,
	}
	record := state.Message{Repo: repo, Event: "TEST", Status: "TEST",
		Title: message.Title, Lines: lines, Keys: keys}
	switch err := m.post(channel, message, record); {
	case err == nil:
		m.toast(fmt.Sprintf("Test %s notification sent", channel.Name()))
	case errors.Is(err, twoway.ErrUnavailable):
		m.toast("Test two-way notification not sent: the handler isn't running", "warning")
	}
}
