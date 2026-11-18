package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spiiritual/steel/internal/datafile"
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
	// saved is the history last written to (or read from) the file, as
	// its generation and length: history only grows within a generation,
	// so a match means the file is current. Guarded by mu.
	savedGen, savedLen int
	// keepFile is set when the saved history came from a newer steel: it
	// isn't loaded, and isn't overwritten. Set before the session is used.
	keepFile bool

	mu       sync.Mutex
	history  []llm.Message
	inbox    []queued      // starts a turn when non-empty
	notes    []llm.Message // steel's notes for the model; sent with the next turn, never start one
	running  bool
	cancel   context.CancelFunc // cancels the running turn
	loopDone chan struct{}      // closed when the running loop exits
	gen      int                // bumped by /new; turns from older generations stop writing
	box      Toolbox
	evicted  bool // the model's earlier scratchpad is gone; tell it when the next one opens
	closed   bool
	lastUsed time.Time
	// approvals are the requests waiting for the user, by ID; each channel
	// takes one answer.
	approvals map[string]chan bool
	// reading are the callers waiting for the running turn, which read
	// their input; reply is its last reply so far.
	reading []chan<- TurnResult
	reply   string
}

// queued is input in the inbox, and where to send the result of the turn
// that reads it, if someone waits for it.
type queued struct {
	msg  llm.Message
	done chan<- TurnResult
}

// TurnResult is how the turn that read some input ended: its last reply,
// and its error.
type TurnResult struct {
	Reply string
	Err   error
}

// ErrBusy means a session has work, so a new conversation can't start.
var ErrBusy = errors.New("session busy")

// errDropped is the result of input that never reached the model.
var errDropped = errors.New("dropped before the model read it: stopped, or steel is shutting down")

// errStopped is the result of a turn stopped before it finished.
var errStopped = errors.New("stopped before it finished: by the user, or steel is shutting down")

// steelNote is a message from steel, not the user, for the model.
func steelNote(text string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: "[steel] " + text}
}

// waiters returns the channels waiting for qs.
func waiters(qs []queued) []chan<- TurnResult {
	var chs []chan<- TurnResult
	for _, q := range qs {
		if q.done != nil {
			chs = append(chs, q.done)
		}
	}
	return chs
}

// busyLocked reports whether the session has work: a turn, input, a
// question for the user or background jobs. Callers hold s.mu.
func (s *Session) busyLocked() bool {
	// Busy is safe under s.mu (see the lock order).
	return s.running || len(s.inbox) > 0 || len(s.approvals) > 0 || (s.box != nil && s.box.Busy())
}

func newSession(id string, cfg *Config, emit func(Event)) *Session {
	s := &Session{id: id, cfg: cfg, emit: emit, lastUsed: time.Now()}
	s.history = s.load()
	s.savedLen = len(s.history)
	// A saved conversation's scratchpad didn't survive: steel restarted, or
	// the session was retired for idleness.
	s.evicted = usedScratchpad(s.history)
	return s
}

// scratchpadClosed is the note telling the model its scratchpad is gone.
const scratchpadClosed = "[steel] The scratchpad was closed after inactivity or a restart; globals from earlier run_code calls are gone."

// usedScratchpad reports whether the model has called tools since it was
// last told its scratchpad was closed: only then can it expect state that
// is gone.
func usedScratchpad(history []llm.Message) bool {
	for _, m := range slices.Backward(history) {
		switch {
		case len(m.ToolCalls) > 0:
			return true
		case m.Role == llm.RoleUser && m.Content == scratchpadClosed:
			return false
		}
	}
	return false
}

// started reports whether a conversation has begun: it holds more than the
// system prompt.
func started(history []llm.Message) bool {
	return slices.ContainsFunc(history, func(m llm.Message) bool { return m.Role != llm.RoleSystem })
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
		box := s.renew()
		s.mu.Unlock()
		if box != nil {
			box.Close()
		}
		s.save()
		s.emit(Notice{Text: "Started a new conversation."})
	default:
		s.enqueue(llm.Message{Role: llm.RoleUser, Content: text})
	}
}

// renew starts a new conversation, returning the old toolbox for the
// caller to close once s.mu is released. Callers hold s.mu.
func (s *Session) renew() Toolbox {
	s.gen++
	s.history = s.fresh()
	s.notes, s.evicted = nil, false
	// A new toolbox, not a reset one: what a toolbox remembers about the
	// conversation (e.g. which tools' docs the model has seen) must go
	// with it.
	box := s.box
	s.box = nil
	return box
}

// prompt queues text for the model like a user message, returning where
// the result of the turn that reads it arrives.
func (s *Session) prompt(text string) (<-chan TurnResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enqueueLocked(llm.Message{Role: llm.RoleUser, Content: text}, true)
}

// start begins a new conversation with notes and text, unless the session
// has work (ErrBusy).
func (s *Session) start(text string, notes []string) (<-chan TurnResult, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errDropped
	}
	if s.busyLocked() {
		s.mu.Unlock()
		return nil, ErrBusy
	}
	box := s.renew()
	for _, n := range notes {
		s.notes = append(s.notes, steelNote(n))
	}
	ch, err := s.enqueueLocked(llm.Message{Role: llm.RoleUser, Content: text}, true)
	s.mu.Unlock()
	if box != nil {
		box.Close()
	}
	return ch, err
}

// approve asks the user to approve a tool's action and waits for the
// answer or for ctx to end.
func (s *Session) approve(ctx context.Context, tool, action string, reasons []string) (bool, error) {
	var b [8]byte
	rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	answer := make(chan bool, 1)
	s.mu.Lock()
	if s.approvals == nil {
		s.approvals = map[string]chan bool{}
	}
	s.approvals[id] = answer
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.approvals, id)
		s.mu.Unlock()
	}()

	s.emit(ApprovalRequest{ID: id, Tool: tool, Action: action, Reasons: reasons})
	select {
	case ok := <-answer:
		outcome := Refused
		if ok {
			outcome = Allowed
		}
		s.emit(ApprovalDone{ID: id, Outcome: outcome})
		return ok, nil
	case <-ctx.Done():
		outcome := Cancelled
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			outcome = Expired
		}
		s.emit(ApprovalDone{ID: id, Outcome: outcome})
		return false, ctx.Err()
	}
}

// answer passes the user's answer to approval request id, reporting
// whether it was still waiting.
func (s *Session) answer(id string, allow bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.approvals[id]
	if ok {
		delete(s.approvals, id)
		ch <- allow
	}
	return ok
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
	s.notes = append(s.notes, steelNote(text))
}

func (s *Session) enqueue(m llm.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enqueueLocked(m, false)
}

// enqueueLocked adds m to the inbox, starting a turn if none is running.
// With wait, it returns a channel for the result of the turn that reads
// m. Callers hold s.mu.
func (s *Session) enqueueLocked(m llm.Message, wait bool) (<-chan TurnResult, error) {
	if s.closed {
		return nil, errDropped
	}
	var ch chan TurnResult
	q := queued{msg: m}
	if wait {
		ch = make(chan TurnResult, 1)
		q.done = ch
	}
	s.inbox = append(s.inbox, q)
	s.lastUsed = time.Now()
	if !s.running {
		s.running = true
		s.loopDone = make(chan struct{})
		go s.loop(s.loopDone)
	}
	return ch, nil
}

// resolve sends r to every waiter in ws.
func resolve(ws []chan<- TurnResult, r TurnResult) {
	for _, w := range ws {
		w <- r
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
	dropped := waiters(s.inbox)
	s.inbox = nil
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	resolve(dropped, TurnResult{Err: errDropped})
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
		s.reply = ""
		gen := s.gen
		s.mu.Unlock()

		err := s.turn(ctx, gen)
		result := err
		if ctx.Err() != nil {
			err = nil // stopped by the user, who already got a Notice
			result = errStopped
		}
		cancel()
		s.save()
		s.mu.Lock()
		read, reply := s.reading, s.reply
		s.reading = nil
		s.mu.Unlock()
		s.emit(TurnDone{Err: err})
		resolve(read, TurnResult{Reply: reply, Err: result})
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
	if err := s.wake(ctx, gen); err != nil {
		return err
	}

	for {
		msgs, ok := s.drain(gen)
		if !ok {
			return nil
		}
		// Save before every model call: history is valid here (each tool
		// call has its result), and a kill that can't be caught, like a
		// supervisor's after its stop timeout, then loses at most one step.
		s.save()
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
			s.mu.Lock()
			s.reply = reply.Content
			s.mu.Unlock()
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
		s.box = s.cfg.NewToolbox(Hooks{Session: s.id, Emit: s.emit, Deliver: s.deliver, Approve: s.approve})
		if s.evicted {
			s.evicted = false
			s.notes = append(s.notes, llm.Message{Role: llm.RoleUser, Content: scratchpadClosed})
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
	for _, q := range s.inbox {
		s.history = append(s.history, q.msg)
	}
	s.reading = append(s.reading, waiters(s.inbox)...)
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
		s.box = nil
		s.evicted = s.evicted || usedScratchpad(s.history)
	}
	s.mu.Unlock()
	if idle {
		box.Close()
	}
}

// settled reports whether the session has been idle for d with nothing
// that lives only in memory, except notes, which retire saves: no
// scratchpad, input, or approvals, and history that can be saved.
// Dropping it then loses nothing; it is loaded again from its file.
func (s *Session) settled(d time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Notes can be saved only in a conversation that has begun; in a new
	// one they'd count as its start, and it would never wake.
	return s.cfg.Dir != "" && !s.keepFile && !s.closed && !s.running && s.box == nil &&
		len(s.inbox) == 0 && len(s.approvals) == 0 && (len(s.notes) == 0 || started(s.history)) &&
		time.Since(s.lastUsed) >= d
}

// retire marks a settled session closed once its history is saved,
// reporting whether it was. Pending notes join the history first, as the
// next turn would add them. A closed session ignores input, so nothing
// reaches a session that is no longer in the map. A failed save is left
// for the next try rather than reported: the session's own saves report
// failures, and this runs on every janitor tick.
func (s *Session) retire() bool {
	s.mu.Lock()
	s.history = append(s.history, s.notes...)
	s.notes = nil
	current := s.gen == s.savedGen && len(s.history) == s.savedLen
	s.mu.Unlock()
	if !current && s.write() != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return true
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

// historyVersion is the format version of saved history files; bump it,
// and migrate in load, when the format changes.
const historyVersion = 1

// load reads saved history, replacing its system prompt with the current one.
func (s *Session) load() []llm.Message {
	if s.cfg.Dir == "" {
		return s.fresh()
	}
	var h []llm.Message
	_, err := datafile.Read(s.path(), historyVersion, &h)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return s.fresh()
	case errors.Is(err, datafile.ErrNewer):
		s.keepFile = true
		s.emit(Notice{Text: fmt.Sprintf("Saved history was written by a newer steel (%v); starting fresh without saving, so it isn't overwritten.", err)})
		return s.fresh()
	case err != nil:
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
	if err := s.write(); err != nil {
		s.emit(Notice{Text: "Couldn't save history: " + err.Error()})
	}
}

// write is save without telling the user about failures.
func (s *Session) write() error {
	if s.cfg.Dir == "" || s.keepFile {
		return nil
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	h, gen := slices.Clone(s.history), s.gen
	s.mu.Unlock()
	if err := datafile.Write(s.path(), historyVersion, h); err != nil {
		return err
	}
	s.mu.Lock()
	s.savedGen, s.savedLen = gen, len(h)
	s.mu.Unlock()
	return nil
}

// wake starts a new conversation with what the memory, if any, recalls.
// The messages join the history, so they're saved with it and stay the
// same, read from the prompt cache, until /new. A conversation is new
// while its history has nothing but the system prompt; one whose wake
// failed tries again next turn.
func (s *Session) wake(ctx context.Context, gen int) error {
	mem := s.cfg.Memory
	if mem == nil {
		return nil
	}
	s.mu.Lock()
	begun := started(s.history)
	s.mu.Unlock()
	if begun {
		return nil
	}
	msgs, err := mem.Wake(ctx, s.id)
	if err != nil {
		// The input waits for the next turn, which tries again; left in
		// the inbox, it would start one now, and fail again, forever.
		// Input someone waits for is dropped instead: they're told it
		// failed, so it mustn't reach the model later.
		s.mu.Lock()
		for _, q := range s.inbox {
			if q.done == nil {
				s.notes = append(s.notes, q.msg)
			}
		}
		s.reading = append(s.reading, waiters(s.inbox)...)
		s.inbox = nil
		s.mu.Unlock()
		return fmt.Errorf("memory: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen == s.gen {
		s.history = append(s.history, msgs...)
	}
	return nil
}
