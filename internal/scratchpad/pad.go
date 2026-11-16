// Package scratchpad is the model's persistent JavaScript workspace.
//
// A Pad owns one goja runtime driven by an event loop. Each Run executes a
// cell: a snippet of JS run as the body of an async function, so top-level
// await and return work. Globals persist across cells until Reset. Registered
// tools are exposed as async functions on a global nested `tools` object
// (tool "fs.read_file" becomes tools.fs.read_file(args)); each call runs its
// Go implementation on its own goroutine, so Promise.all runs tools in
// parallel.
//
// Only the event-loop goroutine touches the runtime. Other goroutines reach it
// through RunOnLoop or Runtime.Interrupt, which goja documents as safe.
package scratchpad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/eventloop"

	"github.com/spiiritual/steel/internal/tool"
)

// DefaultTimeout bounds a cell when Options.Timeout is zero. It must exceed
// the slowest tool's own limit, or a legitimately slow call resets the pad.
const DefaultTimeout = 5 * time.Minute

// maxResult caps a cell's rendered result, in bytes.
const maxResult = 64 << 10

// CallEvent reports a tool call made from inside a cell. Done is false when
// the call starts and true when it finishes, with Result and Err set.
type CallEvent struct {
	Name   string
	Args   json.RawMessage
	Done   bool
	Result string // strings verbatim, other results as JSON
	Err    error
}

// Options configures a Pad.
type Options struct {
	// Timeout bounds each cell; on expiry the cell is interrupted and the pad
	// is reset. Zero means DefaultTimeout.
	Timeout time.Duration
	// OnCall, if set, observes tool calls made from cells. It is called from
	// tool goroutines and must be safe for concurrent use.
	OnCall func(CallEvent)
}

// Result is the outcome of one cell.
type Result struct {
	Value string   // returned value: strings verbatim, everything else as JSON; "" if nothing returned
	Logs  []string // console.log output, in order
	Error string   // uncaught exception, syntax error, or timeout; "" on success
}

// String renders the result as text for the model.
func (r Result) String() string {
	var b strings.Builder
	if len(r.Logs) > 0 {
		b.WriteString("[logs]\n")
		b.WriteString(strings.Join(r.Logs, "\n"))
		b.WriteString("\n")
	}
	switch {
	case r.Error != "":
		b.WriteString("[error]\n" + r.Error)
	case r.Value != "":
		b.WriteString("[result]\n" + r.Value)
	case len(r.Logs) == 0:
		b.WriteString("[result]\n(no return value)")
	}
	s := strings.TrimRight(b.String(), "\n")
	if len(s) > maxResult {
		s = fmt.Sprintf("%s\n[truncated: %d of %d bytes shown]", s[:maxResult], maxResult, len(s))
	}
	return s
}

// Pad is a persistent JS runtime with tools mounted. It is safe for
// concurrent use; cells interleave at await points like Node.js.
type Pad struct {
	reg  *tool.Registry
	opts Options

	mu   sync.Mutex
	rt   *runtime
	open bool
}

// runtime is one generation of the pad; Reset replaces it.
type runtime struct {
	loop   *eventloop.EventLoop
	vm     *goja.Runtime      // only for Interrupt; all other use happens on the loop
	ctx    context.Context    // bounds tool calls; cancelled on reset/close
	cancel context.CancelFunc //
	cells  []*cell            // running cells, oldest first; loop goroutine only
}

// cell collects one Run's console output.
type cell struct {
	mu   sync.Mutex
	logs []string
}

// New returns a Pad exposing reg's tools.
func New(reg *tool.Registry, opts Options) *Pad {
	if opts.Timeout == 0 {
		opts.Timeout = DefaultTimeout
	}
	p := &Pad{reg: reg, opts: opts, open: true}
	p.rt = p.newRuntime()
	return p
}

func (p *Pad) newRuntime() *runtime {
	ctx, cancel := context.WithCancel(context.Background())
	rt := &runtime{loop: eventloop.NewEventLoop(), ctx: ctx, cancel: cancel}
	rt.loop.Start()
	ready := make(chan struct{})
	rt.loop.RunOnLoop(func(vm *goja.Runtime) {
		rt.vm = vm
		_ = vm.Set("tools", p.toolsObject(rt))
		console := vm.NewObject()
		logFn := func(call goja.FunctionCall) goja.Value {
			if len(rt.cells) == 0 {
				return goja.Undefined() // no cell running: nowhere to report
			}
			parts := make([]string, len(call.Arguments))
			for i, a := range call.Arguments {
				parts[i] = inspect(vm, a)
			}
			c := rt.cells[len(rt.cells)-1] // newest running cell
			c.mu.Lock()
			c.logs = append(c.logs, strings.Join(parts, " "))
			c.mu.Unlock()
			return goja.Undefined()
		}
		for _, name := range []string{"log", "info", "warn", "error", "debug"} {
			_ = console.Set(name, logFn)
		}
		_ = vm.Set("console", console)
		_ = vm.Set("log", logFn)
		close(ready)
	})
	<-ready
	return rt
}

func (rt *runtime) stop(reason string) {
	rt.cancel()
	rt.vm.Interrupt(reason)
	rt.loop.Terminate()
}

// Reset discards all state and starts a fresh runtime. Running cells fail.
func (p *Pad) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.open {
		return
	}
	p.rt.stop("scratchpad reset")
	p.rt = p.newRuntime()
}

// resetIfCurrent resets only if rt is still the live runtime.
func (p *Pad) resetIfCurrent(rt *runtime) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open && p.rt == rt {
		p.rt.stop("cell timed out")
		p.rt = p.newRuntime()
	}
}

// Close stops the runtime. Later Runs fail.
func (p *Pad) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open {
		p.open = false
		p.rt.stop("scratchpad closed")
	}
}

// Run executes code as one cell and waits for it to settle. JS failures and
// timeouts are reported in Result.Error; the error return is for a closed pad
// or a cancelled ctx.
func (p *Pad) Run(ctx context.Context, code string) (Result, error) {
	p.mu.Lock()
	rt, open := p.rt, p.open
	p.mu.Unlock()
	if !open {
		return Result{}, errors.New("scratchpad is closed")
	}

	c := &cell{}
	done := make(chan Result, 1)
	finish := func(r Result) {
		select {
		case done <- r:
		default:
		}
	}

	scheduled := rt.loop.RunOnLoop(func(vm *goja.Runtime) {
		rt.cells = append(rt.cells, c)
		end := func(r Result) {
			for i, x := range rt.cells {
				if x == c {
					rt.cells = append(rt.cells[:i], rt.cells[i+1:]...)
					break
				}
			}
			finish(r)
		}

		// The cell body starts on line 1 of "cell" so error positions match
		// the model's code. Non-strict, so bare assignments create globals.
		fn, err := vm.RunScript("cell", "(async () => {"+code+"\n})")
		if err != nil {
			end(Result{Error: errorText(err)})
			return
		}
		call, _ := goja.AssertFunction(fn)
		promise, err := call(goja.Undefined())
		if err != nil {
			end(Result{Error: errorText(err)})
			return
		}
		then, _ := goja.AssertFunction(promise.ToObject(vm).Get("then"))
		onOK := func(call goja.FunctionCall) goja.Value {
			end(Result{Value: render(vm, call.Argument(0))})
			return goja.Undefined()
		}
		onErr := func(call goja.FunctionCall) goja.Value {
			end(Result{Error: exceptionText(call.Argument(0))})
			return goja.Undefined()
		}
		if _, err := then(promise, vm.ToValue(onOK), vm.ToValue(onErr)); err != nil {
			end(Result{Error: errorText(err)})
		}
	})
	if !scheduled {
		return Result{}, errors.New("scratchpad is closed")
	}

	timer := time.NewTimer(p.opts.Timeout)
	defer timer.Stop()
	var r Result
	select {
	case r = <-done:
	case <-timer.C:
		p.resetIfCurrent(rt)
		r = Result{Error: fmt.Sprintf("cell timed out after %s; the scratchpad was reset and all variables were lost", p.opts.Timeout)}
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	c.mu.Lock()
	r.Logs = c.logs
	c.mu.Unlock()
	return r, nil
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
				rt.loop.RunOnLoop(func(vm *goja.Runtime) {
					switch {
					case err != nil:
						_ = reject(vm.NewGoError(fmt.Errorf("%s: %w", name, err)))
					case isString:
						_ = resolve(text)
					default:
						parse, _ := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("parse"))
						v, perr := parse(goja.Undefined(), vm.ToValue(text))
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

// render converts a cell's return value to text: strings verbatim, undefined
// as "", everything else as indented JSON.
func render(vm *goja.Runtime, v goja.Value) string {
	if v == nil || goja.IsUndefined(v) {
		return ""
	}
	return inspect(vm, v)
}

// inspect formats a value roughly the way console.log would.
func inspect(vm *goja.Runtime, v goja.Value) string {
	if s, ok := v.Export().(string); ok {
		return s
	}
	stringify, _ := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("stringify"))
	if out, err := stringify(goja.Undefined(), v, goja.Null(), vm.ToValue(2)); err == nil && !goja.IsUndefined(out) {
		return out.String()
	}
	return v.String()
}

// exceptionText prefers a JS error's stack, which includes cell:line:col.
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
