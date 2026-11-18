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
	// A failed tool call's error doesn't carry steel's Go stack frames.
	if r := run(t, p, `await tools.t.fail()`); r.Error != "GoError: t.fail: boom" {
		t.Errorf("tool error = %q", r.Error)
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

// A user's reset ends a running script, telling the model it was the user,
// so it doesn't redo what they ended.
func TestResetEndsRunningScript(t *testing.T) {
	p := New(testTools(), Options{Wait: 5 * time.Second})
	defer p.Close()

	go func() {
		time.Sleep(50 * time.Millisecond)
		p.Reset()
	}()
	start := time.Now()
	r := run(t, p, `while (true) {}`)
	if !strings.Contains(r.Error, "the user reset") || !strings.Contains(r.Error, "Don't redo it") || time.Since(start) > time.Second {
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

func TestCatalogAndIndexedDescription(t *testing.T) {
	p := New(testTools(), Options{DeclBudget: 1}) // too small for anything: all indexed
	defer p.Close()

	desc := p.Defs()[0].Description
	if !strings.Contains(desc, "tools.t:\n  t.echo\n  t.fail\n  t.sleep\n  t.stat") || !strings.Contains(desc, "tools.$catalog") {
		t.Errorf("description lacks index or catalog hint:\n%s", desc)
	}
	if r := run(t, p, `tools.$catalog.filter(t => /echo|stat/.test(t.name)).map(t => t.name).join(",")`); r.Value != "t.echo,t.stat" {
		t.Errorf("catalog query = %+v", r)
	}
	// Indexed tools are callable directly; the first call returns their
	// documentation, the second runs them.
	if r := run(t, p, `await tools.t.echo({x: 1})`); !strings.Contains(r.Error, "did not run") {
		t.Errorf("first call = %+v", r)
	}
	if r := run(t, p, `await tools.t.echo({x: 1})`); r.Value != `echo {"x":1}` {
		t.Errorf("second call = %+v", r)
	}
	// With the default budget everything is declared and there's no index.
	small := New(testTools(), Options{})
	defer small.Close()
	if desc := small.Defs()[0].Description; strings.Contains(desc, "More tools,") {
		t.Errorf("small registry was indexed:\n%s", desc)
	}
}

func TestIndexedToolsDiscloseBeforeRunning(t *testing.T) {
	shown := tool.Tool{Name: "shown.echo", Run: func(context.Context, json.RawMessage) (any, error) { return "shown", nil }}
	indexed := tool.Tool{Name: "t.wipe", Description: "Deletes everything.", Signature: "(args: {\n  /** Account to wipe. */\n  id: string;\n}): Promise<string>",
		Run: func(context.Context, json.RawMessage) (any, error) { return "wiped", nil }}
	sibling := tool.Tool{Name: "t.peek", Description: "Looks.", Run: func(context.Context, json.RawMessage) (any, error) { return "peeked", nil }}
	var ran atomic.Int32
	reg := tool.NewRegistry(shown)
	reg.Add("t", "Wipes are permanent. Ask first.", indexed, sibling)
	p := New(reg, Options{
		DeclBudget: len(render([]tool.Tool{shown})), // room for "shown" only
		OnCall: func(e CallEvent) {
			if e.Done && e.Err == nil {
				ran.Add(1)
			}
		},
	})
	defer p.Close()

	// Indexed namespaces show a guidance summary, not the full notes.
	if desc := p.Defs()[0].Description; !strings.Contains(desc, "tools.t: Wipes are permanent\n") || strings.Contains(desc, "Ask first") {
		t.Errorf("description:\n%s", desc)
	}
	// Fully declared tools run on the first call.
	if r := run(t, p, `await tools.shown.echo()`); r.Value != "shown" {
		t.Fatalf("declared tool = %+v", r)
	}
	// Every call to an indexed tool is refused until a result has carried
	// its documentation (and, once per namespace, the provider's full
	// notes), even calls made in parallel.
	r := run(t, p, `await Promise.all([1, 2, 3].map(n => tools.t.wipe({id: "a" + n})))`)
	if !strings.Contains(r.Error, "did not run") || strings.Count(r.Docs, "Deletes everything.") != 1 || !strings.Contains(r.Docs, "Account to wipe.") || !strings.Contains(r.Docs, "> Wipes are permanent. Ask first.") {
		t.Fatalf("first calls = %+v", r)
	}
	if !strings.Contains(r.String(), "[documentation]\n") {
		t.Errorf("rendered result lacks documentation:\n%s", r)
	}
	if ran.Load() != 1 {
		t.Fatalf("indexed tool ran before its documentation was returned: %d runs", ran.Load()-1)
	}
	// Documentation arrives even when the script swallows the refusal;
	// notes aren't repeated.
	r = run(t, p, `try { await tools.t.peek() } catch (e) {} return "ok"`)
	if r.Value != "ok" || !strings.Contains(r.Docs, "Looks.") || strings.Contains(r.Docs, "Ask first") {
		t.Errorf("caught refusal = %+v", r)
	}
	// Once documented, calls run, even after a reset.
	p.Reset()
	if r := run(t, p, `await tools.t.wipe({id: "a"})`); r.Value != "wiped" || r.Docs != "" {
		t.Fatalf("documented call = %+v", r)
	}
}

func TestOneRefusalDocumentsEveryToolTheScriptMentions(t *testing.T) {
	ok := func(context.Context, json.RawMessage) (any, error) { return "ok", nil }
	p := New(tool.NewRegistry(
		tool.Tool{Name: "b.wait", Description: "Wait docs.", Run: ok},
		tool.Tool{Name: "b.title", Description: "Title docs.", Run: ok},
		tool.Tool{Name: "b.snap", Description: "Snap docs.", Run: ok},
		tool.Tool{Name: "b.unused", Description: "Unused docs.", Run: ok},
	), Options{DeclBudget: 1})
	defer p.Close()
	// The script stops at its first call, but the refusal documents the
	// tools it would have called next too, however it spells them, so the
	// rerun runs instead of being refused once per tool.
	script := `await tools.b.wait(); const b = tools["b"]; await b.title(); await b.snap(); return "done"`
	r := run(t, p, script)
	if !strings.Contains(r.Error, "b.wait did not run") {
		t.Fatalf("first run = %+v", r)
	}
	for _, d := range []string{"Wait docs.", "Title docs.", "Snap docs."} {
		if !strings.Contains(r.Docs, d) {
			t.Errorf("docs lack %q:\n%s", d, r.Docs)
		}
	}
	if strings.Contains(r.Docs, "Unused docs.") {
		t.Errorf("docs include a tool the script doesn't mention:\n%s", r.Docs)
	}
	if r := run(t, p, script); r.Value != "done" {
		t.Errorf("rerun = %+v", r)
	}
	// A script that's refused nothing documents nothing, even tools it names.
	if r := run(t, p, `"b.unused"`); r.Docs != "" {
		t.Errorf("unrefused script documented %q", r.Docs)
	}
}

func TestRefusalHiddenByPromiseAllStillDocumented(t *testing.T) {
	ok := func(context.Context, json.RawMessage) (any, error) { return "ok", nil }
	p := New(tool.NewRegistry(tool.Tool{Name: "t.a", Run: ok}, tool.Tool{Name: "t.b", Description: "B docs.", Run: ok}), Options{DeclBudget: 1})
	defer p.Close()
	// Promise.all surfaces only a's rejection; b's documentation still comes back.
	r := run(t, p, `await Promise.all([tools.t.a(), tools.t.b()])`)
	if !strings.Contains(r.Error, "t.a did not run") || !strings.Contains(r.Docs, "B docs.") {
		t.Errorf("result = %+v", r)
	}
	if r := run(t, p, `await tools.t.b()`); r.Value != "ok" {
		t.Errorf("b after documentation = %+v", r)
	}
}

func TestDocsLookupUnlocksWithoutRunning(t *testing.T) {
	var runs atomic.Int32
	ok := func(context.Context, json.RawMessage) (any, error) { runs.Add(1); return "ok", nil }
	p := New(tool.NewRegistry(tool.Tool{Name: "t.a", Description: "A docs.", Run: ok}, tool.Tool{Name: "t.b", Description: "B docs.", Run: ok}), Options{DeclBudget: 1})
	defer p.Close()

	// Docs for several tools come back with the result; nothing runs, even if
	// the script drops $docs's return value.
	r := run(t, p, `tools.$docs("t.a", "tools.t.b"); "looked"`)
	if r.Value != "looked" || !strings.Contains(r.Docs, "A docs.") || !strings.Contains(r.Docs, "B docs.") || runs.Load() != 0 {
		t.Fatalf("lookup = %+v, runs = %d", r, runs.Load())
	}
	// Both are unlocked; looking them up again adds nothing.
	if r := run(t, p, `tools.$docs("t.a"); await Promise.all([tools.t.a(), tools.t.b()])`); r.Error != "" || r.Docs != "" || runs.Load() != 2 {
		t.Errorf("after lookup = %+v, runs = %d", r, runs.Load())
	}
	if r := run(t, p, `tools.$docs("t.nope")`); !strings.Contains(r.Error, `no tool named "t.nope"`) {
		t.Errorf("unknown tool = %+v", r)
	}
}

// Tools added while pads run (an MCP server connected at runtime) keep the
// rule that nothing runs before its documentation reaches the model: the
// script whose call added them gets their docs, with the namespace's
// notes, and can use them next; another pad gets them refused, with docs,
// on first use.
func TestToolsAddedAtRuntime(t *testing.T) {
	var reg *tool.Registry
	hello := tool.Tool{Name: "x.hello", Description: "Say hello.", Signature: "(): Promise<string>",
		Run: func(context.Context, json.RawMessage) (any, error) { return "hello", nil }}
	reg = tool.NewRegistry(tool.Tool{Name: "t.connect", Run: func(ctx context.Context, _ json.RawMessage) (any, error) {
		err := reg.Add("x", "Greets people.", hello)
		tool.Disclose(ctx, "x.hello")
		return "connected", err
	}})
	adder := New(reg, Options{})
	defer adder.Close()
	other := New(reg, Options{})
	defer other.Close()

	r := run(t, adder, `return await tools.t.connect()`)
	if r.Error != "" || !strings.Contains(r.Docs, "hello(): Promise<string>") || !strings.Contains(r.Docs, "Greets people.") {
		t.Errorf("adding script: %+v", r)
	}
	if r := run(t, adder, `return await tools.x.hello()`); r.Value != "hello" {
		t.Errorf("next script: %+v", r)
	}

	r = run(t, other, `return await tools.x.hello()`)
	if !strings.Contains(r.Error, "did not run") || !strings.Contains(r.Docs, "hello(): Promise<string>") {
		t.Errorf("other pad, first use: %+v", r)
	}
	if r := run(t, other, `return await tools.x.hello()`); r.Value != "hello" {
		t.Errorf("other pad, second use: %+v", r)
	}

	reg.Remove("x")
	if r := run(t, adder, `return await tools.x.hello()`); !strings.Contains(r.Error, "unknown tool") {
		t.Errorf("after removal: %+v", r)
	}
}
