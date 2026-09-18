// Running the backend in the foreground: logging, signals, shutdown.

package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/acheris-labs/pr-mon/internal/github"
	"github.com/acheris-labs/pr-mon/internal/notify"
	"github.com/acheris-labs/pr-mon/internal/server"
	"github.com/acheris-labs/pr-mon/internal/service"
)

const (
	logBytes   = 1_000_000
	logBackups = 3
)

// rotatingFile writes to a log file and rolls it over at logBytes.
type rotatingFile struct {
	path  string
	mutex sync.Mutex
	file  *os.File
	size  int64
}

func openLog(path string) (*rotatingFile, error) {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	return &rotatingFile{path: path, file: file, size: info.Size()}, nil
}

func (r *rotatingFile) Write(data []byte) (int, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.size+int64(len(data)) > logBytes {
		r.rotate()
	}
	count, err := r.file.Write(data)
	r.size += int64(count)
	return count, err
}

func (r *rotatingFile) rotate() {
	r.file.Close()
	// daemon.log.2 becomes .3, .1 becomes .2, and the current file becomes .1.
	os.Remove(fmt.Sprintf("%s.%d", r.path, logBackups))
	for index := logBackups - 1; index >= 1; index-- {
		os.Rename(fmt.Sprintf("%s.%d", r.path, index), fmt.Sprintf("%s.%d", r.path, index+1))
	}
	os.Rename(r.path, r.path+".1")
	file, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	r.file = file
	r.size = 0
}

func (r *rotatingFile) Close() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.file.Close()
}

// Options are what the backend needs to run.
type Options struct {
	ConfigPath  string
	StatePath   string
	Version     string
	LogToStderr bool
}

// Run is the backend: it holds the lock, serves the socket, and polls until asked
// to stop.
func Run(ctx context.Context, paths Paths, client *github.Client, options Options) error {
	lock, err := AcquireLock(paths)
	if err != nil {
		return err
	}
	defer lock.Close()
	// Running again, whoever started it: no longer deliberately stopped.
	os.Remove(paths.Stopped())
	if err := paths.EnsureSocketDir(); err != nil {
		return err
	}

	logFile, err := openLog(paths.Log())
	if err != nil {
		return err
	}
	defer logFile.Close()
	var writer io.Writer = logFile
	if options.LogToStderr {
		// Only when attached to a terminal, or every line would be logged twice.
		writer = io.MultiWriter(logFile, os.Stderr)
	}
	logger := slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo}))

	monitor := service.New(client, options.ConfigPath, options.StatePath, service.Options{
		Notifier: notify.DetectDesktopNotifier(),
		Version:  options.Version,
		Logger:   logger,
	})
	socket := server.New(monitor, paths.Socket(), logger)
	if err := socket.Start(); err != nil {
		return err
	}
	monitor.Start(ctx)
	logger.Info("pr-mon backend started", "version", options.Version, "pid", os.Getpid())

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	select {
	case <-ctx.Done():
	case <-monitor.Shutdown:
		logger.Info("shutdown requested")
		if err := os.WriteFile(paths.Stopped(), nil, 0o644); err != nil {
			logger.Warn("could not record the requested stop", "error", err)
		}
	case received := <-signals:
		logger.Info("stopping", "signal", received.String())
	}
	socket.Close()
	monitor.Stop()
	logger.Info("pr-mon backend stopped")
	return nil
}
