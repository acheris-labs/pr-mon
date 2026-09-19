// Package server exposes the monitor on a Unix socket: it maps client requests
// onto the monitor and fans out its events.
package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/acheris-labs/pr-mon/internal/protocol"
	"github.com/acheris-labs/pr-mon/internal/service"
)

// LineLimit is generous: a snapshot of busy repos is several MB.
const LineLimit = 64 * 1024 * 1024

// mergeRecheck is how soon an idle backend looks again when a merge held it up.
const mergeRecheck = 10 * time.Second

type Server struct {
	monitor    *service.Monitor
	socketPath string
	log        *slog.Logger

	listener net.Listener
	mutex    sync.Mutex
	// Connections that asked for a snapshot and so receive events.
	subscribers map[net.Conn]bool
	connections sync.WaitGroup
	closed      bool

	// IdleExit, when set, closes Idle once no front end (a connection that sent
	// snapshot) has been connected for that long; zero means never.
	IdleExit time.Duration
	Idle     chan struct{}
	idleOnce sync.Once
	// When the last front end left, or the server started.
	vacantSince time.Time
}

func New(monitor *service.Monitor, socketPath string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		monitor:     monitor,
		socketPath:  socketPath,
		log:         logger,
		subscribers: map[net.Conn]bool{},
		Idle:        make(chan struct{}),
	}
}

// Start listens on the socket and serves clients until Close.
func (s *Server) Start() error {
	// A socket left behind by a crash would refuse connections.
	if err := os.Remove(s.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return err
	}
	s.listener = listener
	s.monitor.AddListener(s.broadcast)
	go s.accept()
	// Started but never used counts as idle too.
	s.mutex.Lock()
	s.vacantSince = time.Now()
	s.mutex.Unlock()
	s.checkIdleIn(s.IdleExit)
	return nil
}

// checkIdleIn looks, after `delay`, whether the backend has had no front end
// for IdleExit. Each time the last one leaves schedules a check, so a front
// end that comes and goes during the grace period just pushes it back.
func (s *Server) checkIdleIn(delay time.Duration) {
	if s.IdleExit <= 0 {
		return
	}
	time.AfterFunc(delay, func() {
		s.mutex.Lock()
		vacant := len(s.subscribers) == 0 && !s.closed
		waited := time.Since(s.vacantSince)
		s.mutex.Unlock()
		switch {
		case !vacant || waited < s.IdleExit:
		case s.monitor.Merging():
			s.checkIdleIn(mergeRecheck) // let the merge finish its follow-up steps
		default:
			s.log.Info("stopping: no app or dashboard for a while, and not started at login",
				"idle", waited.Round(time.Second).String())
			s.idleOnce.Do(func() { close(s.Idle) })
		}
	})
}

func (s *Server) accept() {
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			return // the listener closed
		}
		s.connections.Add(1)
		go func() {
			defer s.connections.Done()
			s.serve(connection)
		}()
	}
}

// Close stops listening and drops every connection.
func (s *Server) Close() {
	s.mutex.Lock()
	if s.closed {
		s.mutex.Unlock()
		return
	}
	s.closed = true
	subscribers := make([]net.Conn, 0, len(s.subscribers))
	for connection := range s.subscribers {
		subscribers = append(subscribers, connection)
	}
	s.subscribers = map[net.Conn]bool{}
	s.mutex.Unlock()

	if s.listener != nil {
		s.listener.Close()
	}
	for _, connection := range subscribers {
		connection.Close()
	}
	s.connections.Wait()
	os.Remove(s.socketPath)
}

func (s *Server) serve(connection net.Conn) {
	defer func() {
		s.mutex.Lock()
		followed := s.subscribers[connection]
		delete(s.subscribers, connection)
		vacated := followed && len(s.subscribers) == 0
		if vacated {
			s.vacantSince = time.Now()
		}
		s.mutex.Unlock()
		connection.Close()
		if vacated {
			s.checkIdleIn(s.IdleExit)
		}
	}()
	reader := bufio.NewReaderSize(connection, 64*1024)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), LineLimit)
	for scanner.Scan() {
		var request protocol.Request
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			s.log.Warn("dropping client: malformed message", "error", err)
			return
		}
		// Requests are answered concurrently, so a slow poll doesn't block the rest.
		go s.answer(connection, request)
	}
}

func (s *Server) answer(connection net.Conn, request protocol.Request) {
	result, err := s.dispatch(connection, request)
	response := protocol.Response{ID: request.ID, OK: err == nil, Result: result}
	if err != nil {
		response.Error = err.Error()
	}
	s.write(connection, response)
}

func (s *Server) write(connection net.Conn, message any) {
	line, err := protocol.Encode(message)
	if err != nil {
		s.log.Error("could not encode a message", "error", err)
		return
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	// A failed write is left to the connection's reader, which notices the
	// same broken connection and drops it (counting it as a front end leaving).
	connection.Write(line)
}

func (s *Server) broadcast(event service.Event) {
	s.mutex.Lock()
	subscribers := make([]net.Conn, 0, len(s.subscribers))
	for connection := range s.subscribers {
		subscribers = append(subscribers, connection)
	}
	s.mutex.Unlock()
	if len(subscribers) == 0 {
		return
	}
	message, err := protocol.EventFor(s.monitor, event)
	if err != nil {
		s.log.Warn("dropping event", "kind", event.Kind, "error", err)
		return
	}
	for _, connection := range subscribers {
		s.write(connection, message)
	}
}
