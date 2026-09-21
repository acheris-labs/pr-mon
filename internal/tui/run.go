// Starting the dashboard: connect to the backend (starting it if needed), feed
// its events into the model, and reconnect when it goes away.

package tui

import (
	"errors"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/acheris-labs/pr-mon/internal/client"
	"github.com/acheris-labs/pr-mon/internal/daemon"
	"github.com/acheris-labs/pr-mon/internal/service"
)

const (
	reconnectDelay = 5 * time.Second
	// How long to wait for a merge to finish before restarting an outdated backend.
	mergeWait = 3 * time.Second
)

// Run opens the dashboard against the backend at paths, starting it if needed.
func Run(paths daemon.Paths, version string) error {
	remote := client.New(paths.Socket())
	if err := connect(remote, paths, version, true); err != nil {
		return err
	}
	defer remote.Close()

	model := New(remote, version)
	model.loginItem = defaultLoginItem(paths)
	// The backend checks what's on screen more often than the rest.
	remote.SetFocus(model.selectedRepo(), 0)
	events := model.Events()
	remote.AddListener(func(event service.Event) {
		select {
		case events <- event:
		default: // the dashboard is behind; the next snapshot will catch it up
		}
	})
	model.reconnect = func() tea.Cmd {
		return func() tea.Msg {
			for {
				time.Sleep(reconnectDelay)
				if err := connect(remote, paths, version, false); err == nil {
					return eventMsg(service.Event{Kind: "connected"})
				}
			}
		}
	}
	program := tea.NewProgram(model, tea.WithAltScreen())
	_, err := program.Run()
	return err
}

// connect attaches to the backend, starting it when `start` and it isn't
// running, and restarting one that speaks another version.
func connect(remote *client.Client, paths daemon.Paths, version string, start bool) error {
	err := remote.Connect(version)
	if errors.Is(err, client.ErrUnavailable) && start {
		if _, spawnErr := daemon.Spawn(paths, daemon.StartTimeout); spawnErr != nil {
			return fmt.Errorf("could not start the backend: %w", spawnErr)
		}
		err = remote.Connect(version)
	}
	var mismatch *client.MismatchError
	if errors.As(err, &mismatch) && start {
		return restartOutdated(remote, paths, version, mismatch)
	}
	return err
}

// restartOutdated replaces a backend running other code, once it isn't merging.
func restartOutdated(remote *client.Client, paths daemon.Paths, version string,
	mismatch *client.MismatchError) error {
	for mismatch.Merging {
		time.Sleep(mergeWait)
		err := remote.Connect(version)
		if err == nil {
			return nil // someone else restarted it meanwhile
		}
		var again *client.MismatchError
		if !errors.As(err, &again) {
			return err
		}
		mismatch = again
	}
	if _, err := daemon.Stop(paths, daemon.StopTimeout); err != nil {
		return err
	}
	if _, err := daemon.Spawn(paths, daemon.StartTimeout); err != nil {
		return err
	}
	return remote.Connect(version)
}
