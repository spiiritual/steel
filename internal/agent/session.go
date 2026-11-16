package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spiiritual/steel/internal/llm"
)

// Session is one conversation. Messages from the user and from background
// jobs land in an inbox that is drained into history before every model
// call, so input that arrives mid-turn reaches the model at its next step.
// One goroutine runs turns while the inbox is non-empty.
//
// Lock order: a toolbox may call Hooks.Deliver (which takes s.mu) while
// holding its own locks, so s.mu is never held while calling into the
// toolbox, except Busy.
type Session struct {
	id   string
	cfg  *Config
	emit func(Event)

	saveMu sync.Mutex // serializes snapshot+write so the newest state wins

	mu       sync.Mutex
	history  []llm.Message
	inbox    []llm.Message // starts a turn when non-empty
	notes    []llm.Message // steel's notes for the model; sent with the next turn, never start one
	running  bool
	cancel   context.CancelFunc // cancels the running turn
	loopDone chan struct{}      // closed when the running loop exits
	gen      int                // bumped by /new; turns from older generations stop writing
	box      Toolbox
	evicted  bool // box was closed for idleness; tell the model next turn
	closed   bool
	lastUsed time.Time
}

func newSession(id string, cfg *Config, emit func(Event)) *Session {
	s := &Session{id: id, cfg: cfg, emit: emit, lastUsed: time.Now()}
	s.history = s.load()
	return s
}

// Send handles text from the user: a command, or a message for the model.
func (s *Session) Send(text string) {
	switch strings.TrimSpace(text) {
	case "/stop":
		if s.stop() {
			s.note("The user stopped you. Running scripts and background jobs were cancelled and the scratchpad was reset; globals from earlier run_code calls are gone.")
		} else {
			s.note("The user stopped your previous turn.")
		}
		s.emit(Notice{Text: "Stopped."})
	case "/reset":
		if box := s.currentBox(); box != nil {
			box.Reset()
		}
		s.note("The user cleared the scratchpad; globals from earlier run_code calls are gone and background jobs were cancelled.")
		s.emit(Notice{Text: "Scratchpad cleared."})
	case "/new":
		s.stop()
		s.mu.Lock()
		s.gen++
		s.history = s.fresh()
		s.notes, s.evicted = nil, false
		box := s.box
		s.mu.Unlock()
		if box != nil {
			box.Reset()
		}
		s.save()
		s.emit(Notice{Text: "Started a new conversation."})
	default:
		s.enqueue(llm.Message{Role: llm.RoleUser, Content: text})
	}
}

// deliver queues text from a toolbox (e.g. a finished job) for the model.
func (s *Session) deliver(text string) {
	s.enqueue(llm.Message{Role: llm.RoleUser, Content: text})
}

// note queues a message from steel for the model's next turn without
// starting one.
func (s *Session) note(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = append(s.notes, llm.Message{Role: llm.RoleUser, Content: "[steel] " + text})
}

func (s *Session) enqueue(m llm.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.inbox = append(s.inbox, m)
	s.lastUsed = time.Now()
	if !s.running {
		s.running = true
		s.loopDone = make(chan struct{})
		go s.loop(s.loopDone)
	}
}

// stop halts the toolbox's running work first (which waits for in-flight
// job deliveries), then drops queued input and cancels the running turn. It
// reports whether the toolbox lost state.
func (s *Session) stop() bool {
	lost := false
	if box := s.currentBox(); box != nil {
		lost = box.Stop()
	}
	s.mu.Lock()
	s.inbox = nil
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	return lost
}

func (s *Session) currentBox() Toolbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.box
}

func (s *Session) loop(done chan struct{}) {
	defer close(done)
	for {
		s.mu.Lock()
		if len(s.inbox) == 0 {
			s.running, s.cancel = false, nil
			s.lastUsed = time.Now()
			s.mu.Unlock()
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		gen := s.gen
		s.mu.Unlock()

		err := s.turn(ctx, gen)
		if ctx.Err() != nil {
			err = nil // stopped by the user, who already got a Notice
		}
		cancel()
		s.save()
		s.emit(TurnDone{Err: err})
	}
}

// turn calls the model and runs its tool calls until it replies without
// any. History stays valid at every step (each tool call gets a result), so
// nothing is rolled back on error.
func (s *Session) turn(ctx context.Context, gen int) error {
	box := s.toolbox()
	var defs []llm.ToolDef
	if box != nil {
		defs = box.Defs()
	}

	for {
		msgs, ok := s.drain(gen)
		if !ok {
			return nil
		}
		reply, err := s.cfg.Provider.Stream(ctx, llm.Request{Model: s.cfg.Model, Messages: msgs, Tools: defs}, func(d string) error {
			s.emit(TextDelta{Text: d})
			return nil
		})
		// A reply cut off by the token limit is kept: its tool calls still
		// get results (malformed arguments come back as errors), so history
		// stays valid and the model can recover.
		truncated := errors.Is(err, llm.ErrTruncated)
		if err != nil && !truncated {
			return err
		}
		if reply.Content != "" {
			s.emit(Reply{Text: reply.Content})
		}
		if !s.append(gen, reply) {
			return nil
		}
		if len(reply.ToolCalls) == 0 {
			return err
		}

		for _, call := range reply.ToolCalls {
			result := "error: cancelled"
			if ctx.Err() == nil {
				args := json.RawMessage(call.Arguments)
				s.emit(ToolStart{ID: call.ID, Name: call.Name, Args: args})
				out, err := callTool(ctx, box, call.Name, args)
				s.emit(ToolEnd{ID: call.ID, Name: call.Name, Result: out, Err: err})
				result = out
				if err != nil {
					result = "error: " + err.Error()
				}
			}
			if !s.append(gen, llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Content: result}) {
				return nil
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func callTool(ctx context.Context, box Toolbox, name string, args json.RawMessage) (string, error) {
	if box == nil {
		return "", errors.New("no tools available")
	}
	return box.Call(ctx, name, args)
}

// toolbox returns the session's toolbox, creating it if needed. It returns
// nil once the session is closed.
func (s *Session) toolbox() Toolbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.box == nil && s.cfg.NewToolbox != nil && !s.closed {
		s.box = s.cfg.NewToolbox(Hooks{Emit: s.emit, Deliver: s.deliver})
		if s.evicted {
			s.evicted = false
			s.notes = append(s.notes, llm.Message{Role: llm.RoleUser, Content: "[steel] The scratchpad was closed after inactivity; globals from earlier run_code calls are gone."})
		}
	}
	return s.box
}

// drain moves pending notes and the inbox into history and returns a
// snapshot to send. It reports false if /new started a new generation.
func (s *Session) drain(gen int) ([]llm.Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return nil, false
	}
	s.history = append(s.history, s.notes...)
	s.history = append(s.history, s.inbox...)
	s.notes, s.inbox = nil, nil
	return slices.Clone(s.history), true
}

// append adds m to history unless /new started a new generation.
func (s *Session) append(gen int, m llm.Message) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return false
	}
	s.history = append(s.history, m)
	return true
}

// evictIfIdle closes the toolbox if the session has been idle for d.
func (s *Session) evictIfIdle(d time.Duration) {
	s.mu.Lock()
	box := s.box
	idle := !s.running && box != nil && time.Since(s.lastUsed) >= d && !box.Busy()
	if idle {
		s.box, s.evicted = nil, true
	}
	s.mu.Unlock()
	if idle {
		box.Close()
	}
}

// close stops the session, waits for its running turn to finish and save,
// and closes the toolbox. Later input is ignored.
func (s *Session) close() {
	s.stop()
	s.mu.Lock()
	s.closed = true
	done := s.loopDone
	running := s.running
	s.mu.Unlock()
	if running {
		<-done
	}
	s.mu.Lock()
	box := s.box
	s.box = nil
	s.mu.Unlock()
	if box != nil {
		box.Close()
	}
}

// fresh returns the history a new conversation starts with.
func (s *Session) fresh() []llm.Message {
	if s.cfg.System == "" {
		return nil
	}
	return []llm.Message{{Role: llm.RoleSystem, Content: s.cfg.System}}
}

func (s *Session) path() string {
	return filepath.Join(s.cfg.Dir, url.PathEscape(s.id)+".json")
}

// load reads saved history, replacing its system prompt with the current one.
func (s *Session) load() []llm.Message {
	if s.cfg.Dir == "" {
		return s.fresh()
	}
	b, err := os.ReadFile(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return s.fresh()
	}
	var h []llm.Message
	if err == nil {
		err = json.Unmarshal(b, &h)
	}
	if err != nil {
		s.emit(Notice{Text: fmt.Sprintf("Couldn't load saved history (%v); starting fresh.", err)})
		return s.fresh()
	}
	if len(h) > 0 && h[0].Role == llm.RoleSystem {
		h = h[1:]
	}
	return append(s.fresh(), h...)
}

// save writes history atomically. Saves are serialized and each snapshots
// history after acquiring saveMu, so the last save always writes the newest
// state.
func (s *Session) save() {
	if s.cfg.Dir == "" {
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	b, err := json.Marshal(s.history)
	s.mu.Unlock()
	if err == nil {
		err = writeAtomic(s.path(), b)
	}
	if err != nil {
		s.emit(Notice{Text: "Couldn't save history: " + err.Error()})
	}
}

// writeAtomic writes b to path via a unique temp file and rename.
func writeAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		os.Remove(f.Name())
		return err
	}
	return nil
}
