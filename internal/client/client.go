// Package client talks to the backend over its Unix socket and mirrors its state.
package client

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"

	"github.com/acheris-labs/pr-mon/internal/config"
	"github.com/acheris-labs/pr-mon/internal/models"
	"github.com/acheris-labs/pr-mon/internal/notify"
	"github.com/acheris-labs/pr-mon/internal/protocol"
	"github.com/acheris-labs/pr-mon/internal/service"
)

const lineLimit = 64 * 1024 * 1024

// ErrUnavailable means nothing is listening on the socket.
var ErrUnavailable = errors.New("no backend is listening")

// ErrStopped means the connection dropped.
var ErrStopped = errors.New("backend stopped")

// MismatchError means the backend speaks another protocol or runs another version.
type MismatchError struct {
	Protocol int
	Version  string
	// A client restarting the backend waits while this is true.
	Merging bool
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("backend is running pr-mon %s (protocol %d)", e.Version, e.Protocol)
}

// Client mirrors the backend's state and sends it commands.
type Client struct {
	socketPath string

	mutex      sync.Mutex
	connection net.Conn
	nextID     int
	pending    map[int]chan reply
	snapshot   protocol.Snapshot
	connected  bool
	listeners  []func(service.Event)
}

type reply struct {
	result json.RawMessage
	err    error
}

func New(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		nextID:     1,
		pending:    map[int]chan reply{},
		snapshot:   protocol.Snapshot{},
	}
}

// AddListener registers a callback for mirrored events; it runs on the reader goroutine.
func (c *Client) AddListener(listener func(service.Event)) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.listeners = append(c.listeners, listener)
}

// Connect loads a snapshot and subscribes. With expectedVersion, it refuses a
// backend running another pr-mon version before reading any of its data.
func (c *Client) Connect(expectedVersion string) error {
	c.Close()
	connection, err := net.Dial("unix", c.socketPath)
	if err != nil {
		// A missing socket or a refused connection both mean "not running".
		return ErrUnavailable
	}
	c.mutex.Lock()
	c.connection = connection
	c.connected = true
	c.mutex.Unlock()
	go c.readLoop(connection)

	var hello protocol.Hello
	if err := c.request("hello", nil, &hello); err != nil {
		c.Close()
		return err
	}
	if hello.Protocol != protocol.Version ||
		(expectedVersion != "" && hello.Version != expectedVersion) {
		c.Close()
		return &MismatchError{Protocol: hello.Protocol, Version: hello.Version, Merging: hello.Merging}
	}
	var snapshot protocol.Snapshot
	if err := c.request("snapshot", nil, &snapshot); err != nil {
		c.Close()
		return err
	}
	c.mutex.Lock()
	c.snapshot = snapshot
	c.snapshot.Status.Connected = true
	c.mutex.Unlock()
	return nil
}

func (c *Client) Close() {
	c.mutex.Lock()
	connection := c.connection
	c.connection = nil
	c.connected = false
	c.mutex.Unlock()
	if connection != nil {
		connection.Close()
	}
}

func (c *Client) Connected() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.connected
}

func (c *Client) readLoop(connection net.Conn) {
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 64*1024), lineLimit)
	for scanner.Scan() {
		line := append([]byte{}, scanner.Bytes()...)
		var envelope struct {
			ID    *int            `json:"id"`
			OK    *bool           `json:"ok"`
			Error string          `json:"error"`
			Event string          `json:"event"`
			Data  json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			continue
		}
		if envelope.Event != "" {
			c.applyEvent(envelope.Event, envelope.Data)
			continue
		}
		if envelope.ID == nil {
			continue
		}
		var result json.RawMessage
		var replyErr error
		if envelope.OK != nil && *envelope.OK {
			var wrapper struct {
				Result json.RawMessage `json:"result"`
			}
			json.Unmarshal(line, &wrapper)
			result = wrapper.Result
		} else {
			message := envelope.Error
			if message == "" {
				message = "request failed"
			}
			replyErr = errors.New(message)
		}
		c.mutex.Lock()
		waiting, found := c.pending[*envelope.ID]
		delete(c.pending, *envelope.ID)
		c.mutex.Unlock()
		if found {
			waiting <- reply{result: result, err: replyErr}
		}
	}
	c.dropConnection(connection)
}

// dropConnection fails everything in flight and tells listeners we're offline.
func (c *Client) dropConnection(connection net.Conn) {
	c.mutex.Lock()
	if c.connection != connection {
		c.mutex.Unlock()
		return
	}
	c.connection = nil
	c.connected = false
	c.snapshot.Status.Connected = false
	waiting := c.pending
	c.pending = map[int]chan reply{}
	listeners := append([]func(service.Event){}, c.listeners...)
	c.mutex.Unlock()
	for _, channel := range waiting {
		channel <- reply{err: ErrStopped}
	}
	for _, listener := range listeners {
		listener(service.Event{Kind: "disconnected"})
	}
}

func (c *Client) request(op string, args any, into any) error {
	c.mutex.Lock()
	connection := c.connection
	if connection == nil {
		c.mutex.Unlock()
		return ErrStopped
	}
	id := c.nextID
	c.nextID++
	waiting := make(chan reply, 1)
	c.pending[id] = waiting
	c.mutex.Unlock()

	if args == nil {
		args = map[string]any{}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return err
	}
	line, err := protocol.Encode(protocol.Request{ID: id, Op: op, Args: encoded})
	if err != nil {
		return err
	}
	if _, err := connection.Write(line); err != nil {
		c.mutex.Lock()
		delete(c.pending, id)
		c.mutex.Unlock()
		return ErrStopped
	}
	answer := <-waiting
	if answer.err != nil {
		return answer.err
	}
	if into != nil && len(answer.result) > 0 {
		if err := json.Unmarshal(answer.result, into); err != nil {
			return fmt.Errorf("the backend sent data this pr-mon doesn't understand (%v); "+
				"run `pr-mon stop` and try again", err)
		}
	}
	return nil
}

func (c *Client) applyEvent(kind string, data json.RawMessage) {
	event := service.Event{Kind: kind}
	c.mutex.Lock()
	switch kind {
	case "repos":
		var snapshot protocol.Snapshot
		if json.Unmarshal(data, &snapshot) == nil {
			c.snapshot = snapshot
			c.snapshot.Status.Connected = true
		}
	case "repo":
		var update protocol.RepoUpdate
		if json.Unmarshal(data, &update) == nil {
			event.Name = update.Name
			if update.Repo == nil {
				delete(c.snapshot.Repos, update.Name)
			} else {
				c.snapshot.Repos[update.Name] = *update.Repo
			}
			if update.Error == nil {
				delete(c.snapshot.Errors, update.Name)
			} else {
				c.snapshot.Errors[update.Name] = *update.Error
			}
			c.snapshot.Unseen[update.Name] = update.Unseen
			c.snapshot.Armed[update.Name] = update.Armed
		}
	case "seen":
		var update protocol.SeenUpdate
		if json.Unmarshal(data, &update) == nil {
			event.Name = update.Name
			c.snapshot.Unseen[update.Name] = update.Unseen
		}
	case "collapsed":
		var update protocol.CollapsedUpdate
		if json.Unmarshal(data, &update) == nil {
			c.snapshot.Collapsed = update.Collapsed
		}
	case "config":
		var update protocol.ConfigUpdate
		if json.Unmarshal(data, &update) == nil {
			c.snapshot.Config = update.Config
		}
	case "status":
		var update protocol.StatusUpdate
		if json.Unmarshal(data, &update) == nil {
			c.snapshot.Status = update.Status
			c.snapshot.Status.Connected = true
		}
	case "toast":
		var toast protocol.Toast
		if json.Unmarshal(data, &toast) == nil {
			event.Message = toast.Message
			event.Severity = toast.Severity
		}
	default:
		c.mutex.Unlock()
		return
	}
	listeners := append([]func(service.Event){}, c.listeners...)
	c.mutex.Unlock()
	for _, listener := range listeners {
		listener(event)
	}
}

// ----- mirrored state -----

func (c *Client) Snapshot() protocol.Snapshot {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.snapshot
}

func (c *Client) Config() config.Config { return c.Snapshot().Config }

func (c *Client) Repos() map[string]models.Repo { return c.Snapshot().Repos }

func (c *Client) Repo(name string) (models.Repo, bool) {
	repo, found := c.Snapshot().Repos[name]
	return repo, found
}

func (c *Client) Errors() map[string]string { return c.Snapshot().Errors }

func (c *Client) Status() service.Status { return c.Snapshot().Status }

func (c *Client) Collapsed() []string { return c.Snapshot().Collapsed }

func (c *Client) Unseen(name string) []int { return c.Snapshot().Unseen[name] }

func (c *Client) Armed(name string) map[int]models.ArmedMerge {
	armed := map[int]models.ArmedMerge{}
	for key, merge := range c.Snapshot().Armed[name] {
		if number, err := strconv.Atoi(key); err == nil {
			armed[number] = merge
		}
	}
	return armed
}

// ----- commands -----

func (c *Client) RefreshAll() error { return c.request("refresh_all", nil, nil) }

func (c *Client) AddRepo(name string) (string, error) {
	var added string
	err := c.request("add_repo", map[string]any{"name": name}, &added)
	return added, err
}

func (c *Client) RemoveRepo(name string) error {
	return c.request("remove_repo", map[string]any{"name": name}, nil)
}

func (c *Client) MarkSeen(name string, number int) error {
	return c.request("mark_seen", map[string]any{"name": name, "number": number}, nil)
}

func (c *Client) SetCollapsed(owner string, collapsed bool) error {
	return c.request("set_collapsed", map[string]any{"owner": owner, "collapsed": collapsed}, nil)
}

func (c *Client) Perform(repo string, number int, action models.Action) error {
	return c.request("perform",
		map[string]any{"repo": repo, "number": number, "action": action}, nil)
}

// AddDependency makes a PR wait for `on` ("owner/repo#12" or a URL) to merge first.
func (c *Client) AddDependency(repo string, number int, on string) error {
	return c.request("add_dependency",
		map[string]any{"repo": repo, "number": number, "on": on}, nil)
}

func (c *Client) RemoveDependency(repo string, number int, on string) error {
	return c.request("remove_dependency",
		map[string]any{"repo": repo, "number": number, "on": on}, nil)
}

func (c *Client) DependencyGraph(repo string, number int) (models.DependencyGraph, error) {
	var graph models.DependencyGraph
	err := c.request("dependency_graph", map[string]any{"repo": repo, "number": number}, &graph)
	return graph, err
}

func (c *Client) SaveNotifications(repo string, settings config.NotifyConfig) error {
	return c.request("save_notifications",
		map[string]any{"repo": repo, "settings": settings}, nil)
}

func (c *Client) SendTest(repo string, settings config.NotifyConfig) error {
	return c.request("send_test", map[string]any{"repo": repo, "settings": settings}, nil)
}

func (c *Client) SetPollInterval(seconds int) error {
	return c.request("set_poll_interval", map[string]any{"seconds": seconds}, nil)
}

func (c *Client) NotificationForm() (notify.Form, error) {
	var form notify.Form
	err := c.request("notification_form", nil, &form)
	return form, err
}

func (c *Client) PreviewNotification(repo, message string) (notify.Preview, error) {
	var preview notify.Preview
	err := c.request("preview_notification",
		map[string]any{"repo": repo, "message": message}, &preview)
	return preview, err
}

func (c *Client) Shutdown() error { return c.request("shutdown", nil, nil) }
