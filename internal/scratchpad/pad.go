// Package scratchpad is the model's persistent JavaScript workspace.
//
// A Pad owns one goja runtime driven by an event loop. Each Run executes a
// script: a snippet of JS run as the body of an async function, so top-level
// await works, and the last line's value is returned. Globals persist across
// scripts until Reset. Registered tools are exposed as async functions on a
// global nested `tools` object (tool "fs.read_file" becomes
// tools.fs.read_file(args)); each call runs its Go implementation on its own
// goroutine, so Promise.all runs tools in parallel.
//
// A script that hasn't settled after Options.Wait becomes a background job:
// Run returns immediately and Options.OnJobDone receives the result later.
//
// Only the event-loop goroutine touches the runtime. Other goroutines reach it
// through RunOnLoop or Runtime.Interrupt, which goja documents as safe.
// Running JS can only be halted by interrupting the whole runtime, so
// stopping anything that is running means replacing the runtime.
package scratchpad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/eventloop"

	"github.com/spiiritual/steel/internal/tool"
)

const (
	// DefaultTimeout bounds a script when Options.Timeout is zero. It must
	// exceed the slowest tool's own limit, or a legitimately slow call resets
	// the pad.
	DefaultTimeout = 5 * time.Minute
	// DefaultWait is how long Run waits before backgrounding a script when
	// Options.Wait is zero.
	DefaultWait = 30 * time.Second
)

// maxResult caps a script's rendered result, in bytes.
const maxResult = 64 << 10

// CallEvent reports a tool call made from inside a script. Done is false
// when the call starts and true when it finishes, with Result and Err set.
type CallEvent struct {
	Name   string
	Args   json.RawMessage
	Done   bool
	Result string // strings verbatim, other results as JSON
	Err    error
}

// Options configures a Pad.
type Options struct {
	// Timeout bounds each script from its start; on expiry the pad is reset.
	// Zero means DefaultTimeout.
	Timeout time.Duration
	// Wait is how long Run blocks before the script continues as a
	// background job. Zero means DefaultWait.
	Wait time.Duration
	// OnCall, if set, observes tool calls made from scripts. It is called
	// from tool goroutines and must be safe for concurrent use.
	OnCall func(CallEvent)
	// OnJobDone, if set, receives the result of each background job, unless
	// Stop, Reset, or Close dropped the job first. Calls are serialized.
	OnJobDone func(id string, r Result)
}

// Result is the outcome of one script.
type Result struct {
	Value string   // returned value: strings verbatim, everything else as JSON; "" if nothing returned
	Logs  []string // console.log output, in order
	Error string   // uncaught exception, syntax error, timeout, or reset; "" on success
	Job   string   // set when the script is still running as a background job
}

// String renders the result as text for the model, within maxResult bytes.
// The value or error always survives; logs get whatever room is left.
func (r Result) String() string {
	if r.Job != "" {
		return fmt.Sprintf("[background]\nJob %s is running in the background. Its result will be sent to you automatically as a new message when it finishes. Do not wait, sleep, or re-run it: end your turn now unless you have other independent work.", r.Job)
	}
	var tail string
	switch {
	case r.Error != "":
		tail = "[error]\n" + r.Error
	case r.Value != "":
		tail = "[result]\n" + r.Value
	case len(r.Logs) == 0:
		tail = "[result]\n(no return value)"
	}
	tail = truncate(strings.TrimRight(tail, "\n"), maxResult/2)
	if len(r.Logs) == 0 {
		return tail
	}
	logs := "[logs]\n" + truncate(strings.TrimRight(strings.Join(r.Logs, "\n"), "\n"), maxResult-len(tail)-64)
	if tail == "" {
		return logs
	}
	return logs + "\n" + tail
}

// truncate shortens s to at most n bytes on a rune boundary, noting the cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := max(n, 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s\n[truncated: %d of %d bytes shown]", s[:cut], cut, len(s))
}

// Pad is a persistent JS runtime with tools mounted. It is safe for
// concurrent use; scripts interleave at await points like Node.js.
type Pad struct {
	reg  *tool.Registry
	opts Options

	// deliverMu orders job delivery against Stop/Reset/Close, so no result
	// is delivered after they return. Lock order: deliverMu, then mu.
	deliverMu sync.Mutex

	mu      sync.Mutex
	rt      *runtime
	open    bool
	jobs    map[string]bool // background jobs not yet delivered or dropped
	nextJob int
}

// runtime is one generation of the pad; resets replace it.
type runtime struct {
	loop   *eventloop.EventLoop
	vm     *goja.Runtime // only for Interrupt; all other use happens on the loop
	dead   chan struct{} // closed when this runtime is stopped
	ctx    context.Context
	cancel context.CancelFunc // cancels tool calls when stopped
	active atomic.Int32       // scripts started and not yet settled

	// Loop goroutine only. Built-ins are captured before any model code runs,
	// so scripts that overwrite JSON or Promise can't break the host.
	scripts                       []*script // running scripts, oldest first
	parse, stringify, promiseThen goja.Callable
}

// script collects one Run's console output.
type script struct {
	mu   sync.Mutex
	logs []string
}

// New returns a Pad exposing reg's tools.
func New(reg *tool.Registry, opts Options) *Pad {
	if opts.Timeout == 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Wait == 0 {
		opts.Wait = DefaultWait
	}
	p := &Pad{reg: reg, opts: opts, open: true, jobs: map[string]bool{}}
	p.rt = p.newRuntime()
	return p
}

func (p *Pad) newRuntime() *runtime {
	rt := &runtime{loop: eventloop.NewEventLoop(), dead: make(chan struct{})}
	rt.ctx, rt.cancel = context.WithCancel(context.Background())
	rt.loop.Start()
	ready := make(chan struct{})
	rt.loop.RunOnLoop(func(vm *goja.Runtime) {
		rt.vm = vm
		jsonObj := vm.Get("JSON").ToObject(vm)
		rt.parse, _ = goja.AssertFunction(jsonObj.Get("parse"))
		rt.stringify, _ = goja.AssertFunction(jsonObj.Get("stringify"))
		rt.promiseThen, _ = goja.AssertFunction(vm.Get("Promise").ToObject(vm).Get("prototype").ToObject(vm).Get("then"))
		_ = vm.Set("tools", p.toolsObject(rt))
		console, logFn := rt.newConsole(nil)
		_ = vm.Set("console", console)
		_ = vm.Set("log", logFn)
		close(ready)
	})
	<-ready
	return rt
}

// onLoop runs fn on the loop unless the runtime has been stopped. A panic is
// recovered so model code can't crash the process; the runtime is then
// reset, since its state may be inconsistent.
func (p *Pad) onLoop(rt *runtime, fn func(vm *goja.Runtime)) bool {
	return rt.loop.RunOnLoop(func(vm *goja.Runtime) {
		select {
		case <-rt.dead:
			return
		default:
		}
		defer func() {
			if recover() != nil {
				go p.timeoutReset(rt, "an internal error")
			}
		}()
		fn(vm)
	})
}

// stop halts the runtime. Pending callbacks see dead and skip JS; goja clears
// an interrupt after each interrupted job, so keep interrupting until the
// loop has terminated.
func (rt *runtime) stop(reason string) {
	close(rt.dead)
	rt.cancel()
	done := make(chan struct{})
	go func() {
		rt.loop.Terminate()
		close(done)
	}()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		rt.vm.Interrupt(reason)
		select {
		case <-done:
			return
		case <-tick.C:
		}
	}
}

// swapLocked replaces the runtime and drops all jobs, returning the old
// runtime for the caller to stop after unlocking. Requires p.mu.
func (p *Pad) swapLocked() *runtime {
	clear(p.jobs)
	old := p.rt
	p.rt = p.newRuntime()
	return old
}

// Stop halts everything running in the pad. If any script or background job
// is running, the runtime is reset (the only way to halt running JS) and
// jobs are dropped without delivery; Stop then reports true, meaning all
// state was lost. With nothing running it does nothing and reports false.
func (p *Pad) Stop() bool {
	p.deliverMu.Lock()
	defer p.deliverMu.Unlock()
	p.mu.Lock()
	if !p.open || (p.rt.active.Load() == 0 && len(p.jobs) == 0) {
		p.mu.Unlock()
		return false
	}
	old := p.swapLocked()
	p.mu.Unlock()
	old.stop("stopped")
	return true
}

// Busy reports whether background jobs are still running or delivering.
func (p *Pad) Busy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.jobs) > 0
}

// Reset discards all state and starts a fresh runtime. Running scripts and
// background jobs end without delivering results.
func (p *Pad) Reset() {
	p.deliverMu.Lock()
	defer p.deliverMu.Unlock()
	p.mu.Lock()
	if !p.open {
		p.mu.Unlock()
		return
	}
	old := p.swapLocked()
	p.mu.Unlock()
	old.stop("scratchpad reset")
}

// Close stops the runtime. Later Runs fail.
func (p *Pad) Close() {
	p.deliverMu.Lock()
	defer p.deliverMu.Unlock()
	p.mu.Lock()
	if !p.open {
		p.mu.Unlock()
		return
	}
	p.open = false
	clear(p.jobs)
	old := p.rt
	p.mu.Unlock()
	old.stop("scratchpad closed")
}

// timeoutReset resets the pad if rt is still current, telling the owners of
// dropped background jobs why they won't get a result.
func (p *Pad) timeoutReset(rt *runtime, cause string) {
	p.deliverMu.Lock()
	defer p.deliverMu.Unlock()
	p.resetDelivering(rt, cause, "")
}

// resetDelivering is timeoutReset for callers holding deliverMu; the job
// named except has already been delivered and is not notified again.
func (p *Pad) resetDelivering(rt *runtime, cause, except string) {
	p.mu.Lock()
	if !p.open || p.rt != rt {
		p.mu.Unlock()
		return
	}
	var dropped []string
	for id := range p.jobs {
		if id != except {
			dropped = append(dropped, id)
		}
	}
	old := p.swapLocked()
	p.mu.Unlock()
	old.stop(cause)
	if p.opts.OnJobDone == nil {
		return
	}
	slices.Sort(dropped)
	for _, id := range dropped {
		p.opts.OnJobDone(id, Result{Error: fmt.Sprintf("stopped: the scratchpad was reset because of %s; all variables were lost", cause)})
	}
}

const errReset = "the scratchpad was reset while this script was running; all variables were lost"

func (p *Pad) timeoutResult() Result {
	return Result{Error: fmt.Sprintf("script timed out after %s; the scratchpad was reset and all variables were lost", p.opts.Timeout)}
}

// Run executes code as one script. If it settles within Options.Wait, its
// result is returned. Otherwise it continues as a background job: the
// returned Result has Job set and Options.OnJobDone gets the final result.
// JS failures, timeouts, and resets are reported in Result.Error; the error
// return is for a closed pad or a cancelled ctx. After ctx is cancelled the
// script keeps running under its timeout.
func (p *Pad) Run(ctx context.Context, code string) (Result, error) {
	src, err := prepare(code)
	if err != nil {
		return Result{Error: err.Error()}, nil
	}
	for attempt := 0; ; attempt++ {
		p.mu.Lock()
		rt, open := p.rt, p.open
		p.mu.Unlock()
		if !open {
			return Result{}, errors.New("scratchpad is closed")
		}
		r, scheduled, err := p.runOn(ctx, rt, src)
		if scheduled {
			return r, err
		}
		// rt was stopped between reading it and scheduling: a reset raced
		// us. Retry once on the new runtime.
		p.mu.Lock()
		replaced := p.open && p.rt != rt
		p.mu.Unlock()
		if !replaced || attempt > 0 {
			return Result{}, errors.New("scratchpad is closed")
		}
	}
}

func (p *Pad) runOn(ctx context.Context, rt *runtime, src string) (Result, bool, error) {
	s := &script{}
	done := make(chan Result, 1)
	rt.active.Add(1)
	var settled atomic.Bool
	finish := func(r Result) {
		if settled.CompareAndSwap(false, true) {
			rt.active.Add(-1)
			done <- r
		}
	}

	scheduled := p.onLoop(rt, func(vm *goja.Runtime) {
		rt.scripts = append(rt.scripts, s)
		end := func(r Result) {
			if i := slices.Index(rt.scripts, s); i >= 0 {
				rt.scripts = slices.Delete(rt.scripts, i, i+1)
			}
			finish(r)
		}
		defer func() {
			if v := recover(); v != nil {
				end(Result{Error: fmt.Sprintf("internal error: %v", v)})
				panic(v) // let onLoop reset the runtime
			}
		}()

		// Scripts are named "script" so error positions read script:line:col
		// and match the model's code. Non-strict, so bare assignments create
		// globals. The script's own console shadows the global one, so its
		// output stays attributed to it while other scripts run.
		fn, err := vm.RunScript("script", src)
		if err != nil {
			end(Result{Error: errorText(err)})
			return
		}
		call, ok := goja.AssertFunction(fn)
		if !ok {
			end(Result{Error: errEscapes.Error()})
			return
		}
		console, logFn := rt.newConsole(s)
		promise, err := call(goja.Undefined(), console, logFn)
		if err != nil {
			end(Result{Error: errorText(err)})
			return
		}
		onOK := func(call goja.FunctionCall) goja.Value {
			end(Result{Value: rt.render(call.Argument(0))})
			return goja.Undefined()
		}
		onErr := func(call goja.FunctionCall) goja.Value {
			end(Result{Error: exceptionText(call.Argument(0))})
			return goja.Undefined()
		}
		if _, err := rt.promiseThen(promise, vm.ToValue(onOK), vm.ToValue(onErr)); err != nil {
			end(Result{Error: errorText(err)})
		}
	})
	if !scheduled {
		rt.active.Add(-1)
		return Result{}, false, nil
	}

	deadline := time.NewTimer(p.opts.Timeout)
	wait := time.NewTimer(p.opts.Wait)
	defer wait.Stop()

	var r Result
	select {
	case r = <-done:
	case <-deadline.C:
		p.timeoutReset(rt, fmt.Sprintf("a script that ran longer than %s", p.opts.Timeout))
		r = p.timeoutResult()
	case <-rt.dead:
		r = Result{Error: errReset}
	case <-ctx.Done():
		go p.watch(rt, done, deadline)
		return Result{}, true, ctx.Err()
	case <-wait.C:
		return p.background(rt, s, done, deadline), true, nil
	}
	deadline.Stop()
	return s.attach(r), true, nil
}

// watch keeps enforcing the timeout on a script whose caller went away.
func (p *Pad) watch(rt *runtime, done <-chan Result, deadline *time.Timer) {
	defer deadline.Stop()
	select {
	case <-done:
	case <-rt.dead:
	case <-deadline.C:
		p.timeoutReset(rt, fmt.Sprintf("a script that ran longer than %s", p.opts.Timeout))
	}
}

// attach adds the script's captured logs to r.
func (s *script) attach(r Result) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Logs = s.logs
	return r
}

// background registers the still-running script as a job and delivers its
// outcome via OnJobDone, unless Stop/Reset/Close drops the job first.
func (p *Pad) background(rt *runtime, s *script, done <-chan Result, deadline *time.Timer) Result {
	p.mu.Lock()
	if !p.open || p.rt != rt {
		p.mu.Unlock()
		deadline.Stop()
		return Result{Error: errReset}
	}
	p.nextJob++
	id := fmt.Sprintf("j%d", p.nextJob)
	p.jobs[id] = true
	p.mu.Unlock()

	go func() {
		defer deadline.Stop()
		var r Result
		timedOut := false
		select {
		case r = <-done:
		case <-deadline.C:
			timedOut = true
			r = p.timeoutResult()
		case <-rt.dead:
			r = Result{Error: errReset}
		}

		p.deliverMu.Lock()
		defer p.deliverMu.Unlock()
		p.mu.Lock()
		live := p.jobs[id]
		p.mu.Unlock()
		if live && p.opts.OnJobDone != nil {
			p.opts.OnJobDone(id, s.attach(r))
		}
		if timedOut {
			p.resetDelivering(rt, fmt.Sprintf("job %s running longer than %s", id, p.opts.Timeout), id)
		}
		// Removed only after delivery, so Busy stays true until the result
		// is queued.
		p.mu.Lock()
		delete(p.jobs, id)
		p.mu.Unlock()
	}()
	return Result{Job: id}
}

// newConsole returns a console object and a bare log function. Output goes
// to owner while it is running; otherwise (helpers defined in finished
// scripts, or the global console) to the newest running script. With no
// script running it is dropped. Loop goroutine only.
func (rt *runtime) newConsole(owner *script) (*goja.Object, goja.Value) {
	vm := rt.vm
	logFn := func(call goja.FunctionCall) goja.Value {
		target := owner
		if target == nil || !slices.Contains(rt.scripts, target) {
			if len(rt.scripts) == 0 {
				return goja.Undefined()
			}
			target = rt.scripts[len(rt.scripts)-1]
		}
		parts := make([]string, len(call.Arguments))
		for i, a := range call.Arguments {
			parts[i] = rt.inspect(a)
		}
		target.mu.Lock()
		target.logs = append(target.logs, strings.Join(parts, " "))
		target.mu.Unlock()
		return goja.Undefined()
	}
	console := vm.NewObject()
	for _, name := range []string{"log", "info", "warn", "error", "debug"} {
		_ = console.Set(name, logFn)
	}
	return console, vm.ToValue(logFn)
}

// toolsObject builds the nested tools object for rt.
func (p *Pad) toolsObject(rt *runtime) *goja.Object {
	vm := rt.vm
	root := vm.NewObject()
	for _, t := range p.reg.All() {
		obj := root
		parts := strings.Split(t.Name, ".")
		for _, ns := range parts[:len(parts)-1] {
			next, ok := obj.Get(ns).(*goja.Object)
			if !ok {
				next = vm.NewObject()
				_ = obj.Set(ns, next)
			}
			obj = next
		}
		name := t.Name
		_ = obj.Set(parts[len(parts)-1], func(call goja.FunctionCall) goja.Value {
			promise, resolve, reject := vm.NewPromise()
			args, err := argsJSON(call.Argument(0))
			if err != nil {
				_ = reject(vm.NewTypeError(fmt.Sprintf("%s: %v", name, err)))
				return vm.ToValue(promise)
			}
			p.observe(CallEvent{Name: name, Args: args})
			go func() {
				out, err := p.reg.Call(rt.ctx, name, args)
				// Non-string results cross into JS as JSON, so scripts see
				// plain objects keyed by the json tags.
				text, isString := out.(string)
				if err == nil && !isString {
					var b []byte
					if b, err = json.Marshal(out); err != nil {
						err = fmt.Errorf("result not JSON-serializable: %w", err)
					}
					text = string(b)
				}
				p.observe(CallEvent{Name: name, Args: args, Done: true, Result: text, Err: err})
				p.onLoop(rt, func(vm *goja.Runtime) {
					switch {
					case err != nil:
						_ = reject(vm.NewGoError(fmt.Errorf("%s: %w", name, err)))
					case isString:
						_ = resolve(text)
					default:
						v, perr := rt.parse(goja.Undefined(), vm.ToValue(text))
						if perr != nil {
							_ = reject(perr)
							return
						}
						_ = resolve(v)
					}
				})
			}()
			return vm.ToValue(promise)
		})
	}
	return root
}

func (p *Pad) observe(e CallEvent) {
	if p.opts.OnCall != nil {
		p.opts.OnCall(e)
	}
}

// argsJSON converts a JS argument to a JSON object; undefined becomes {}.
func argsJSON(v goja.Value) (json.RawMessage, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return json.RawMessage("{}"), nil
	}
	b, err := json.Marshal(v.Export())
	if err != nil {
		return nil, fmt.Errorf("arguments must be JSON-serializable: %w", err)
	}
	if len(b) == 0 || b[0] != '{' {
		return nil, errors.New("arguments must be an object")
	}
	return b, nil
}

// render converts a script's return value to text: strings verbatim,
// undefined as "", everything else as indented JSON.
func (rt *runtime) render(v goja.Value) string {
	if v == nil || goja.IsUndefined(v) {
		return ""
	}
	return rt.inspect(v)
}

// inspect formats a value roughly the way console.log would.
func (rt *runtime) inspect(v goja.Value) string {
	if s, ok := v.Export().(string); ok {
		return s
	}
	if out, err := rt.stringify(goja.Undefined(), v, goja.Null(), rt.vm.ToValue(2)); err == nil && !goja.IsUndefined(out) {
		return out.String()
	}
	return v.String()
}

// exceptionText prefers a JS error's stack, which includes script:line:col.
func exceptionText(e goja.Value) string {
	if o, ok := e.(*goja.Object); ok {
		if st := o.Get("stack"); st != nil && !goja.IsUndefined(st) && st.String() != "" {
			return st.String()
		}
	}
	return e.String()
}

func errorText(err error) string {
	var ex *goja.Exception
	if errors.As(err, &ex) {
		return ex.String()
	}
	return err.Error()
}
