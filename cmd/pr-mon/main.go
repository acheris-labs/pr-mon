// Command pr-mon watches GitHub pull requests: the dashboard, and the backend
// that polls, notifies and merges.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/acheris-labs/pr-mon/internal/autostart"
	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/daemon"
	"github.com/acheris-labs/pr-mon/internal/github"
	"github.com/acheris-labs/pr-mon/internal/state"
	"github.com/acheris-labs/pr-mon/internal/tui"
)

// Version is stamped at build time (`-ldflags "-X main.Version=..."`).
var Version = "dev"

const usage = `pr-mon: monitor and merge GitHub pull requests.

Usage: pr-mon [command]

Commands:
  (none)     open the dashboard; starts the backend if needed
  daemon     run the backend in the foreground
  start      start the backend in the background
  stop       stop the background backend
  restart    restart the backend
  status     show whether the backend is running
  autostart  enable | disable | status: run the backend at login (macOS)
  --version  print the version
`

func main() {
	command := ""
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	paths := daemon.Default()
	var err error
	switch command {
	case "", "tui":
		err = runTUI(paths)
	case "daemon":
		err = runDaemon(paths)
	case "start":
		err = startBackend(paths)
	case "stop":
		err = stopBackend(paths)
	case "restart":
		err = restartBackend(paths)
	case "status":
		err = backendStatus(paths)
	case "autostart":
		action := ""
		if len(os.Args) > 2 {
			action = os.Args[2]
		}
		err = autostartCommand(paths, action)
	case "--version", "-v", "version":
		fmt.Printf("pr-mon %s\n", Version)
	case "--help", "-h", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "pr-mon: unknown command %q\n\n%s", command, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pr-mon: %v\n", err)
		os.Exit(1)
	}
}

func runDaemon(paths daemon.Paths) error {
	client, err := github.NewClient(github.GetToken)
	if err != nil {
		return fmt.Errorf("%v. Run `gh auth login`", err)
	}
	stat, _ := os.Stderr.Stat()
	err = daemon.Run(context.Background(), paths, client, daemon.Options{
		ConfigPath: config.DefaultPath(),
		StatePath:  state.DefaultPath(),
		Version:    Version,
		// When spawned, stderr already goes to the log file.
		LogToStderr: stat != nil && stat.Mode()&os.ModeCharDevice != 0,
	})
	var running *daemon.AlreadyRunningError
	if err != nil && asAlreadyRunning(err, &running) {
		// Not a failure: launchd must not keep retrying when a backend is already up.
		fmt.Fprintf(os.Stderr, "pr-mon: %v\n", err)
		return nil
	}
	return err
}

func asAlreadyRunning(err error, target **daemon.AlreadyRunningError) bool {
	running, ok := err.(*daemon.AlreadyRunningError)
	if ok {
		*target = running
	}
	return ok
}

func startBackend(paths daemon.Paths) error {
	pid, err := daemon.Spawn(paths, daemon.StartTimeout)
	if err != nil {
		return err
	}
	fmt.Printf("backend running (pid %d)\n", pid)
	return nil
}

func stopBackend(paths daemon.Paths) error {
	stopped, err := daemon.Stop(paths, daemon.StopTimeout)
	if err != nil {
		return err
	}
	if stopped {
		fmt.Println("backend stopped")
	} else {
		fmt.Println("backend not running")
	}
	return nil
}

func restartBackend(paths daemon.Paths) error {
	pid, err := autostart.Restart(paths, nil)
	if err != nil {
		return err
	}
	fmt.Printf("backend restarted (pid %d)\n", pid)
	return nil
}

func autostartCommand(paths daemon.Paths, action string) error {
	switch action {
	case "enable":
		lines, err := autostart.Enable(paths, nil, nil)
		if err != nil {
			return err
		}
		for _, line := range lines {
			fmt.Println(line)
		}
	case "disable":
		message, err := autostart.Disable(paths, nil)
		if err != nil {
			return err
		}
		fmt.Println(message)
	case "status", "":
		enabled, description, err := autostart.Describe(nil)
		if err != nil {
			return err
		}
		fmt.Println("autostart " + description)
		if !enabled {
			os.Exit(1)
		}
	default:
		return fmt.Errorf("unknown autostart action %q (enable, disable or status)", action)
	}
	return nil
}

func backendStatus(paths daemon.Paths) error {
	if hello := daemon.Info(paths); hello != nil {
		pid := 0
		if hello.PID != nil {
			pid = *hello.PID
		}
		fmt.Printf("backend running (pid %d, version %s)\n", pid, hello.Version)
		return nil
	}
	if pid := daemon.RunningPID(paths); pid >= 0 {
		fmt.Printf("backend not responding (pid %d); see %s\n", pid, paths.Log())
		os.Exit(1)
	}
	fmt.Println("backend stopped")
	os.Exit(1)
	return nil
}

func runTUI(paths daemon.Paths) error {
	return tui.Run(paths, Version)
}
