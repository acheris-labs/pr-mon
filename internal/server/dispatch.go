// Which request maps onto which monitor call.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/acheris-labs/pr-mon/internal/protocol"
)

type nameArgs struct {
	Name string `json:"name"`
}

type repoNumberArgs struct {
	Name   string `json:"name"`
	Number int    `json:"number"`
}

type performArgs struct {
	Repo   string          `json:"repo"`
	Number int             `json:"number"`
	Action json.RawMessage `json:"action"`
}

type settingsArgs struct {
	Repo     string          `json:"repo"`
	Settings json.RawMessage `json:"settings"`
}

type previewArgs struct {
	Repo    string `json:"repo"`
	Message string `json:"message"`
}

type secondsArgs struct {
	Seconds int `json:"seconds"`
}

func decodeArgs[T any](raw json.RawMessage, op string) (T, error) {
	var args T
	if len(raw) == 0 {
		return args, fmt.Errorf("bad arguments for %s", op)
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return args, fmt.Errorf("bad arguments for %s", op)
	}
	return args, nil
}

// Ops are every request the backend answers, in the order docs/protocol.md lists them.
var Ops = []string{
	"hello", "snapshot", "refresh_all", "add_repo", "remove_repo", "mark_seen",
	"set_collapsed", "perform", "save_notifications", "send_test", "set_poll_interval",
	"notification_form", "preview_notification", "shutdown",
}

func (s *Server) dispatch(connection net.Conn, request protocol.Request) (any, error) {
	ctx := context.Background()
	switch request.Op {
	case "hello":
		status := s.monitor.Status()
		return protocol.Hello{
			Protocol: protocol.Version,
			Version:  status.Version,
			PID:      status.PID,
			Notifier: status.Notifier,
			Merging:  s.monitor.Merging(),
		}, nil

	case "snapshot":
		// Subscribe before taking the snapshot, then send it under the same lock,
		// so no event is missed or delivered before the snapshot response.
		s.mutex.Lock()
		if !s.closed {
			s.subscribers[connection] = true
		}
		snapshot := protocol.Of(s.monitor)
		s.mutex.Unlock()
		return snapshot, nil

	case "refresh_all":
		s.monitor.RefreshAll()
		return nil, nil

	case "add_repo":
		args, err := decodeArgs[nameArgs](request.Args, request.Op)
		if err != nil {
			return nil, err
		}
		return s.monitor.AddRepo(ctx, args.Name)

	case "remove_repo":
		args, err := decodeArgs[nameArgs](request.Args, request.Op)
		if err != nil {
			return nil, err
		}
		return nil, s.monitor.RemoveRepo(args.Name)

	case "mark_seen":
		args, err := decodeArgs[repoNumberArgs](request.Args, request.Op)
		if err != nil {
			return nil, err
		}
		s.monitor.MarkSeen(args.Name, args.Number)
		return nil, nil

	case "set_collapsed":
		args, err := decodeArgs[struct {
			Owner     string `json:"owner"`
			Collapsed bool   `json:"collapsed"`
		}](request.Args, request.Op)
		if err != nil {
			return nil, err
		}
		s.monitor.SetCollapsed(args.Owner, args.Collapsed)
		return nil, nil

	case "perform":
		args, err := decodeArgs[performArgs](request.Args, request.Op)
		if err != nil {
			return nil, err
		}
		action, err := protocol.ParseAction(args.Action)
		if err != nil {
			return nil, err
		}
		return nil, s.monitor.Perform(ctx, args.Repo, args.Number, action)

	case "save_notifications":
		args, err := decodeArgs[settingsArgs](request.Args, request.Op)
		if err != nil {
			return nil, err
		}
		settings, err := protocol.ParseSettings(args.Settings)
		if err != nil {
			return nil, err
		}
		return nil, s.monitor.SaveNotifications(args.Repo, settings)

	case "send_test":
		args, err := decodeArgs[settingsArgs](request.Args, request.Op)
		if err != nil {
			return nil, err
		}
		settings, err := protocol.ParseSettings(args.Settings)
		if err != nil {
			return nil, err
		}
		s.monitor.SendTest(args.Repo, settings)
		return nil, nil

	case "notification_form":
		return s.monitor.NotificationForm(), nil

	case "preview_notification":
		args, err := decodeArgs[previewArgs](request.Args, request.Op)
		if err != nil {
			return nil, err
		}
		return s.monitor.PreviewNotification(args.Repo, args.Message), nil

	case "set_poll_interval":
		args, err := decodeArgs[secondsArgs](request.Args, request.Op)
		if err != nil {
			return nil, err
		}
		return nil, s.monitor.SetPollInterval(args.Seconds)

	case "shutdown":
		s.monitor.RequestShutdown()
		return nil, nil
	}
	return nil, fmt.Errorf("unknown op %q", request.Op)
}
