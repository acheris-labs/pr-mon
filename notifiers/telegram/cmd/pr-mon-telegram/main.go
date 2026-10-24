// Command pr-mon-telegram is pr-mon's two-way handler for Telegram. pr-mon
// runs it as a child process and speaks JSON lines to it on stdin/stdout.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/acheris-labs/pr-mon/notifiers/telegram/internal/aws"
	"github.com/acheris-labs/pr-mon/notifiers/telegram/internal/config"
	"github.com/acheris-labs/pr-mon/notifiers/telegram/internal/protocol"
	"github.com/acheris-labs/pr-mon/notifiers/telegram/internal/telegram"
)

// endpoints override AWS's own service URLs; only tests set them.
type endpoints struct{ Lambda, SQS string }

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, endpoints{}))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, ep endpoints) int {
	const usage = "usage: pr-mon-telegram --config <path>"
	flags := flag.NewFlagSet("pr-mon-telegram", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {}
	configPath := flags.String("config", "", "path to the settings file (required)")
	if err := flags.Parse(args); err != nil || *configPath == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "pr-mon-telegram: %v\n", err)
		return 1
	}

	log := newLogger(stderr)
	client := &aws.Client{
		Credentials:    aws.Credentials{AccessKeyID: cfg.AWS.AccessKeyID, SecretAccessKey: cfg.AWS.SecretAccessKey},
		Region:         cfg.Region,
		LambdaEndpoint: ep.Lambda,
		SQSEndpoint:    ep.SQS,
	}
	backend := telegram.New(telegram.Settings{
		QueueURL:       cfg.QueueURL,
		SenderFunction: cfg.SenderFunction,
		ChatID:         cfg.ChatID,
	}, client, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := protocol.Run(ctx, stdin, stdout, backend, protocol.Options{Log: log}); err != nil {
		log.Error("session ended", "err", err)
		return 1
	}
	return 0
}

// newLogger logs to w without a timestamp: pr-mon timestamps what it keeps of
// stderr, and shows only the last 200 characters of the last line.
func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}
