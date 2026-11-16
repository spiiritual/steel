package scratchpad

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spiiritual/steel/internal/tool"
)

func testTools() *tool.Registry {
	return tool.NewRegistry(
		tool.Tool{Name: "t.echo", Run: func(_ context.Context, a json.RawMessage) (any, error) { return "echo " + string(a), nil }},
		tool.Tool{Name: "t.fail", Run: func(context.Context, json.RawMessage) (any, error) { return nil, errors.New("boom") }},
		tool.Tool{Name: "t.sleep", Run: func(ctx context.Context, _ json.RawMessage) (any, error) {
			select {
			case <-time.After(200 * time.Millisecond):
				return "slept", nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}},
		tool.Tool{Name: "t.stat", Run: func(context.Context, json.RawMessage) (any, error) {
			return struct {
				Size  int      `json:"size"`
				Lines []string `json:"lines"`
			}{3, []string{"a", "b"}}, nil
		}},
	)
}

func TestStructuredToolResult(t *testing.T) {
	p := New(testTools(), Options{})
	defer p.Close()

	// Go results arrive as plain JS objects keyed by their json tags.
	r := run(t, p, `const s = await tools.t.stat(); return s.size + s.lines.length + (s.lines instanceof Array ? "array" : "?")`)
	if r.Error != "" || r.Value != "5array" {
		t.Errorf("result = %+v", r)
	}
}

func run(t *testing.T, p *Pad, code string) Result {
	t.Helper()
	r, err := p.Run(context.Background(), code)
	if err != nil {
		t.Fatalf("Run(%q): %v", code, err)
	}
	return r
}

func TestStatePersistsAcrossScripts(t *testing.T) {
	p := New(testTools(), Options{})
	defer p.Close()

	if r := run(t, p, `x = 41; globalThis.y = 1; const local = 5; return {a: [1, 2]}`); r.Error != "" || r.Value != "{\n  \"a\": [\n    1,\n    2\n  ]\n}" {
		t.Fatalf("script 1 = %+v", r)
	}
	if r := run(t, p, `return x + y`); r.Value != "42" {
		t.Errorf("globals not kept: %+v", r)
	}
	if r := run(t, p, `return typeof local`); r.Value != "undefined" {
		t.Errorf("const leaked across scripts: %+v", r)
	}
}

func TestHelperFromEarlierScriptUsesToolsAndConsole(t *testing.T) {
	p := New(testTools(), Options{})
	defer p.Close()

	run(t, p, `globalThis.h = async () => { console.log("in h"); return await tools.t.echo({v: 1}) }`)
	r := run(t, p, `return await h()`)
	if r.Error != "" || r.Value != `echo {"v":1}` {
		t.Errorf("result = %+v", r)
	}
	if len(r.Logs) != 1 || r.Logs[0] != "in h" {
		t.Errorf("logs = %q, want [in h] attributed to the calling cell", r.Logs)
	}
}

func TestToolsRunInParallel(t *testing.T) {
	p := New(testTools(), Options{})
	defer p.Close()

	start := time.Now()
	r := run(t, p, `return (await Promise.all([tools.t.sleep(), tools.t.sleep(), tools.t.sleep()])).join(",")`)
	if r.Value != "slept,slept,slept" {
		t.Fatalf("result = %+v", r)
	}
	if d := time.Since(start); d > 450*time.Millisecond {
		t.Errorf("3×200ms calls took %v; not parallel", d)
	}
}

func TestErrors(t *testing.T) {
	p := New(testTools(), Options{})
	defer p.Close()

	if r := run(t, p, `try { await tools.t.fail() } catch (e) { return "caught: " + e.message }`); r.Value != "caught: t.fail: boom" {
		t.Errorf("caught tool error = %+v", r)
	}
	if r := run(t, p, "const a = 1;\nnull.x"); !strings.Contains(r.Error, "script:2") {
		t.Errorf("uncaught error should point at line 2: %+v", r)
	}
	if r := run(t, p, `return (`); r.Error == "" {
		t.Errorf("syntax error not reported: %+v", r)
	}
	if r := run(t, p, `return await tools.t.echo("nope")`); !strings.Contains(r.Error, "must be an object") {
		t.Errorf("bad args not rejected: %+v", r)
	}
	if r := run(t, p, `return 1`); r.Value != "1" {
		t.Errorf("pad unusable after errors: %+v", r)
	}
}

func TestTimeoutResetsPad(t *testing.T) {
	p := New(testTools(), Options{Timeout: 300 * time.Millisecond})
	defer p.Close()

	run(t, p, `globalThis.keep = 1`)
	start := time.Now()
	r := run(t, p, `console.log("spinning"); while (true) {}`)
	if !strings.Contains(r.Error, "timed out") {
		t.Fatalf("result = %+v", r)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("timeout took %v", d)
	}
	if len(r.Logs) != 1 || r.Logs[0] != "spinning" {
		t.Errorf("logs before timeout lost: %q", r.Logs)
	}
	if r := run(t, p, `return typeof keep`); r.Value != "undefined" {
		t.Errorf("state survived reset: %+v", r)
	}
}

type jobResult struct {
	id string
	r  Result
}

func TestSlowScriptBecomesBackgroundJob(t *testing.T) {
	jobs := make(chan jobResult, 1)
	p := New(testTools(), Options{Wait: 50 * time.Millisecond, OnJobDone: func(id string, r Result) { jobs <- jobResult{id, r} }})
	defer p.Close()

	start := time.Now()
	r := run(t, p, `console.log("starting"); return await tools.t.sleep()`)
	if r.Job != "j1" || time.Since(start) > 150*time.Millisecond {
		t.Fatalf("Run = %+v after %v; want job j1 promptly", r, time.Since(start))
	}
	if !p.Busy() {
		t.Error("Busy() = false with a job running")
	}
	select {
	case j := <-jobs:
		if j.id != "j1" || j.r.Value != "slept" || len(j.r.Logs) != 1 || j.r.Logs[0] != "starting" {
			t.Errorf("job = %+v", j)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job result never delivered")
	}
	if p.Busy() {
		t.Error("Busy() = true after the job finished")
	}
}

func TestStopHaltsRunningWorkAndReportsLostState(t *testing.T) {
	jobs := make(chan jobResult, 1)
	var counting atomic.Bool
	var calls atomic.Int32
	p := New(testTools(), Options{
		Wait:      50 * time.Millisecond,
		OnJobDone: func(id string, r Result) { jobs <- jobResult{id, r} },
		OnCall: func(e CallEvent) {
			if counting.Load() && !e.Done {
				calls.Add(1)
			}
		},
	})
	defer p.Close()

	run(t, p, `globalThis.keep = 7`)
	if p.Stop() {
		t.Error("Stop with nothing running reported lost state")
	}
	if r := run(t, p, `keep`); r.Value != "7" {
		t.Errorf("state lost by an idle Stop: %+v", r)
	}

	// A script that swallows errors and keeps calling tools must really stop.
	if r := run(t, p, `for (;;) { try { await tools.t.sleep() } catch {} }`); r.Job == "" {
		t.Fatalf("want a job, got %+v", r)
	}
	counting.Store(true)
	if !p.Stop() {
		t.Error("Stop with a job running didn't report lost state")
	}
	if p.Busy() {
		t.Error("Busy() = true after Stop")
	}
	time.Sleep(500 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Errorf("%d tool calls started after Stop", n)
	}
	select {
	case j := <-jobs:
		t.Errorf("stopped job was delivered: %+v", j)
	default:
	}
	if r := run(t, p, `typeof keep + ":" + await tools.t.echo({})`); r.Value != "undefined:echo {}" {
		t.Errorf("after Stop = %+v", r)
	}
}

func TestResetEndsRunningScript(t *testing.T) {
	p := New(testTools(), Options{Wait: 5 * time.Second})
	defer p.Close()

	go func() {
		time.Sleep(50 * time.Millisecond)
		p.Reset()
	}()
	start := time.Now()
	r := run(t, p, `while (true) {}`)
	if !strings.Contains(r.Error, "reset") || time.Since(start) > time.Second {
		t.Errorf("Run = %+v after %v; want prompt reset error", r, time.Since(start))
	}
}

func TestLogsStayWithTheirScriptWhileOthersRun(t *testing.T) {
	jobs := make(chan jobResult, 2)
	p := New(testTools(), Options{Wait: 50 * time.Millisecond, OnJobDone: func(id string, r Result) { jobs <- jobResult{id, r} }})
	defer p.Close()

	// j1 logs at ~200ms, while the newer j2 is still running.
	run(t, p, `await tools.t.sleep(); console.log("from j1")`)
	run(t, p, `await tools.t.sleep(); await tools.t.sleep(); console.log("from j2")`)
	for range 2 {
		select {
		case j := <-jobs:
			if want := "from " + j.id; len(j.r.Logs) != 1 || j.r.Logs[0] != want {
				t.Errorf("%s logs = %q, want [%s]", j.id, j.r.Logs, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("job never delivered")
		}
	}
}

// Review finding #1: two scripts pending when the pad resets used to hang
// forever, because goja clears an interrupt after the first interrupted job.
func TestResetWithSeveralSpinningScriptsCompletes(t *testing.T) {
	p := New(testTools(), Options{Wait: 20 * time.Millisecond})
	defer p.Close()

	// Two continuations that spin after a tool call resolves.
	run(t, p, `await Promise.all([1, 2].map(async () => { await tools.t.sleep(); for (;;) {} }))`)
	run(t, p, `await tools.t.sleep(); for (;;) {}`)
	time.Sleep(300 * time.Millisecond) // both are now spinning or queued

	done := make(chan struct{})
	go func() { p.Reset(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Reset hung")
	}
	if r := run(t, p, `1 + 1`); r.Value != "2" {
		t.Errorf("pad unusable after reset: %+v", r)
	}
}

// Review finding #2: model code that sabotages built-ins must not crash
// the process.
func TestSabotagedBuiltinsDontCrash(t *testing.T) {
	p := New(testTools(), Options{})
	defer p.Close()

	for _, code := range []string{
		`delete globalThis.JSON; return await tools.t.stat()`,
		`JSON.stringify = null; return {a: 1}`,
		`Promise.prototype.then = null; return 1`,
		`}); 5; ({a: 1`,
	} {
		r := run(t, p, code)
		t.Logf("%s -> %+v", code, r)
	}
	// Still alive and usable; the reset after sabotage is acceptable.
	if r := run(t, p, `globalThis.ok = 1; ok`); r.Value != "1" {
		t.Errorf("pad unusable after sabotage: %+v", r)
	}
}

// Review finding #3: when the caller's ctx is cancelled the script's timeout
// must still apply, and a later innocent script must not take the blame.
func TestCancelledRunStillTimesOut(t *testing.T) {
	p := New(testTools(), Options{Timeout: 300 * time.Millisecond, Wait: 5 * time.Second})
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := p.Run(ctx, `for (;;) {}`); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	time.Sleep(600 * time.Millisecond) // the watcher resets the spinning runtime
	if r := run(t, p, `1`); r.Value != "1" || r.Error != "" {
		t.Errorf("later script = %+v", r)
	}
}

// Review finding #10: huge logs must not push the result out.
func TestResultSurvivesHugeLogs(t *testing.T) {
	r := Result{Logs: []string{strings.Repeat("x", 3*maxResult)}, Value: "the answer"}
	s := r.String()
	if len(s) > maxResult+100 || !strings.HasSuffix(s, "[result]\nthe answer") {
		t.Errorf("len %d, tail %q", len(s), s[max(0, len(s)-40):])
	}
}
