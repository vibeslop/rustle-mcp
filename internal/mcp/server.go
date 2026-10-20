// Package mcp serves tools over the Model Context Protocol, on stdio (JSON-RPC messages,
// one per line) or Streamable HTTP (http.go). Rustle only answers: initialize, ping,
// tools/list and tools/call, with cancellation; it never sends requests of its own.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/vibeslop/rustle-mcp/internal/rustle"
)

const latestProtocol = "2025-11-25"

var supportedProtocols = []string{latestProtocol, "2025-06-18", "2025-03-26", "2024-11-05", "2024-10-07"}

// Tool is one tool: its definition, as tools/list shows it, and its handler.
type Tool struct {
	Name        string
	Description string
	// Schema is the JSON Schema of the arguments; handlers validate leniently themselves.
	Schema *rustle.Obj
	Call   func(ctx context.Context, client string, args map[string]any) Result
}

type Result struct {
	Content []rustle.Block
	IsError bool
}

type Server struct {
	Name         string
	Version      string
	Instructions string
	Tools        []Tool
	// OnError sees protocol errors, which never reach the client.
	OnError func(error)

	client  atomic.Value // the stdio client's name, from initialize
	write   sync.Mutex
	out     *bufio.Writer
	mu      sync.Mutex
	running map[string]context.CancelFunc
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// notification is true for a message without an id, which wants no reply.
func (m *message) notification() bool { return len(m.ID) == 0 || string(m.ID) == "null" }

// ClientName is the stdio client's name, as initialize reported it.
func (s *Server) ClientName() string {
	name, _ := s.client.Load().(string)
	return name
}

// Serve answers messages from in until it ends, writing to out. Calls run concurrently;
// those still running when in ends are cancelled.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = bufio.NewWriterSize(out, 64<<10)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader := bufio.NewReaderSize(in, 64<<10)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			s.handle(ctx, line)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *Server) fail(err error) {
	if s.OnError != nil {
		s.OnError(err)
	} else {
		fmt.Fprintf(os.Stderr, "rustle: %v\n", err)
	}
}

// parse reads one message; ok is false for a blank line, and for an invalid message,
// which is reported to OnError.
func (s *Server) parse(line []byte) (m message, ok bool) {
	line = bytes.TrimRight(line, "\r\n")
	// Windows PowerShell starts what it pipes to a program with a byte order mark.
	line = bytes.TrimPrefix(line, []byte("\ufeff"))
	if len(line) == 0 {
		return m, false
	}
	if err := json.Unmarshal(line, &m); err != nil {
		s.fail(fmt.Errorf("invalid message: %w", err))
		return m, false
	}
	if m.JSONRPC != "2.0" {
		s.fail(fmt.Errorf("invalid message: jsonrpc must be \"2.0\""))
		return m, false
	}
	return m, true
}

func (s *Server) handle(ctx context.Context, line []byte) {
	m, ok := s.parse(line)
	if !ok {
		return
	}
	if m.notification() {
		s.notified("", m)
		return
	}
	switch m.Method {
	case "initialize":
		result, client := s.initialize(m.Params)
		s.client.Store(client)
		s.send(reply(m.ID, result))
	case "tools/call":
		call, done := s.track(ctx, "", m.ID)
		go func() {
			defer done()
			line := s.call(call, s.ClientName(), m)
			// A cancelled call gets no answer: the client has stopped waiting for it.
			if call.Err() == nil {
				s.send(line)
			}
		}()
	default:
		s.send(s.answer(m))
	}
}

// notified acts on a notification from the client in session: only a cancellation needs anything.
func (s *Server) notified(session string, m message) {
	if m.Method != "notifications/cancelled" {
		return
	}
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(m.Params, &p) == nil && len(p.RequestID) > 0 {
		s.mu.Lock()
		if cancel := s.running[session+" "+string(p.RequestID)]; cancel != nil {
			cancel()
		}
		s.mu.Unlock()
	}
}

// track makes the context of call id from session, which a cancellation ends; done
// forgets it. Request ids are only unique within a session.
func (s *Server) track(ctx context.Context, session string, id json.RawMessage) (context.Context, func()) {
	call, cancel := context.WithCancel(ctx)
	key := session + " " + string(id)
	s.mu.Lock()
	if s.running == nil {
		s.running = map[string]context.CancelFunc{}
	}
	s.running[key] = cancel
	s.mu.Unlock()
	return call, func() {
		s.mu.Lock()
		delete(s.running, key)
		s.mu.Unlock()
		cancel()
	}
}

// initialize is the reply to initialize, and the client's name.
func (s *Server) initialize(params json.RawMessage) (*rustle.Obj, string) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name string `json:"name"`
		} `json:"clientInfo"`
	}
	_ = json.Unmarshal(params, &p)
	version := latestProtocol
	for _, v := range supportedProtocols {
		if v == p.ProtocolVersion {
			version = v
		}
	}
	return rustle.NewObj().
		Set("protocolVersion", version).
		Set("capabilities", rustle.NewObj().Set("tools", rustle.NewObj().Set("listChanged", true))).
		Set("serverInfo", rustle.NewObj().Set("name", s.Name).Set("version", s.Version)).
		Set("instructions", s.Instructions), p.ClientInfo.Name
}

// answer is the reply to a request other than initialize and tools/call.
func (s *Server) answer(m message) []byte {
	switch m.Method {
	case "ping":
		return reply(m.ID, rustle.NewObj())
	case "tools/list":
		tools := make([]any, len(s.Tools))
		for i, t := range s.Tools {
			tools[i] = rustle.NewObj().Set("name", t.Name).Set("description", t.Description).Set("inputSchema", t.Schema).
				Set("annotations", rustle.NewObj().Set("readOnlyHint", true))
		}
		return reply(m.ID, rustle.NewObj().Set("tools", tools))
	}
	return errorReply(m.ID, -32601, "Method not found")
}

// call runs a tools/call request for the named client, and is its reply.
func (s *Server) call(ctx context.Context, client string, m message) []byte {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(m.Params) == 0 || m.Params[0] != '{' || json.Unmarshal(m.Params, &p) != nil {
		return errorReply(m.ID, -32602, "Invalid tools/call request: params must be an object")
	}
	var tool *Tool
	for i := range s.Tools {
		if s.Tools[i].Name == p.Name {
			tool = &s.Tools[i]
		}
	}
	if tool == nil {
		return errorReply(m.ID, -32602, fmt.Sprintf("Tool %s not found", p.Name))
	}
	args := map[string]any{}
	if len(p.Arguments) > 0 && p.Arguments[0] == '{' {
		_ = json.Unmarshal(p.Arguments, &args)
	}
	result := tool.Call(ctx, client, args)
	out := rustle.NewObj().Set("content", blocks(result.Content))
	if result.IsError {
		out.Set("isError", true)
	}
	return reply(m.ID, out)
}

// blocks keeps content blocks as they are: an image's base64 is not metadata to round.
type blocks []rustle.Block

func (b blocks) MarshalJSON() ([]byte, error) {
	out := []byte{'['}
	for i, block := range b {
		if i > 0 {
			out = append(out, ',')
		}
		data, _ := block.MarshalJSON()
		out = append(out, data...)
	}
	return append(out, ']'), nil
}

func reply(id json.RawMessage, result *rustle.Obj) []byte {
	line := []byte(`{"result":`)
	line = rustle.AppendCompact(line, result)
	line = append(line, `,"jsonrpc":"2.0","id":`...)
	line = append(line, id...)
	return append(line, '}')
}

func errorReply(id json.RawMessage, code int, message string) []byte {
	line := []byte(`{"jsonrpc":"2.0","id":`)
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	line = append(line, id...)
	line = append(line, fmt.Sprintf(`,"error":{"code":%d,"message":`, code)...)
	line = rustle.AppendString(line, message)
	return append(line, "}}"...)
}

func (s *Server) send(line []byte) {
	s.write.Lock()
	defer s.write.Unlock()
	s.out.Write(line)
	s.out.WriteByte('\n')
	if err := s.out.Flush(); err != nil {
		s.fail(fmt.Errorf("writing a response: %w", err))
	}
}
