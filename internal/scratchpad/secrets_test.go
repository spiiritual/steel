package scratchpad

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spiiritual/steel/internal/secret"
	"github.com/spiiritual/steel/internal/tool"
)

// secretPad is a pad with one secret, GH_TOKEN, and two tools that return
// what they're given: t.send accepts secrets, t.note doesn't.
func secretPad(t *testing.T, allow bool) (p *Pad, received func() []string, asked func() []tool.Approval) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("GH_TOKEN=ghp_value123\n"), 0o600)
	store, err := secret.Open(path, secret.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	var approvals []tool.Approval
	echo := func(_ context.Context, a json.RawMessage) (any, error) {
		mu.Lock()
		got = append(got, string(a))
		mu.Unlock()
		return "got " + string(a), nil
	}
	reg := tool.NewRegistry(
		tool.Tool{Name: "t.send", Run: echo, AcceptsSecrets: true},
		tool.Tool{Name: "t.note", Run: echo},
		tool.Tool{Name: "t.leak", AcceptsSecrets: true, Run: func(_ context.Context, a json.RawMessage) (any, error) {
			return nil, &json.UnsupportedValueError{Str: "bad token ghp_value123"}
		}},
	)
	p = New(reg, Options{
		Secrets: store,
		Approver: func(_ context.Context, a tool.Approval) (bool, error) {
			mu.Lock()
			approvals = append(approvals, a)
			mu.Unlock()
			return allow, nil
		},
	})
	t.Cleanup(p.Close)
	return p,
		func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) },
		func() []tool.Approval {
			mu.Lock()
			defer mu.Unlock()
			return append([]tool.Approval(nil), approvals...)
		}
}

// A tool that accepts secrets gets the value once the user approves the
// call, shown with the placeholder; the script only ever sees the
// placeholder, even when the tool echoes the value back.
func TestSecretFilledAfterApprovalAndRedacted(t *testing.T) {
	p, received, asked := secretPad(t, true)
	r := run(t, p, `const tok = "{{secret:GH_TOKEN}}"; return await tools.t.send({cmd: "curl -H 'Bearer " + tok + "'"})`)
	if r.Error != "" || strings.Contains(r.String(), "ghp_value123") || !strings.Contains(r.Value, "{{secret:GH_TOKEN}}") {
		t.Errorf("script saw %+v", r)
	}
	if got := received(); len(got) != 1 || got[0] != `{"cmd":"curl -H 'Bearer ghp_value123'"}` {
		t.Errorf("tool received %q", got)
	}
	a := asked()
	if len(a) != 1 || a[0].Action != "curl -H 'Bearer {{secret:GH_TOKEN}}'" || a[0].Reasons[0] != "uses the secret GH_TOKEN" {
		t.Errorf("approvals %+v", a)
	}
}

func TestRefusedSecretCallDoesntRun(t *testing.T) {
	p, received, _ := secretPad(t, false)
	r := run(t, p, `return await tools.t.send({cmd: "{{secret:GH_TOKEN}}"})`)
	if !strings.Contains(r.Error, "refused") || len(received()) != 0 {
		t.Errorf("result %+v, tool received %q", r, received())
	}
}

// Steel's own tools get placeholders as written, so they never hold a
// value, and need no approval.
func TestOtherToolsGetPlaceholders(t *testing.T) {
	p, received, asked := secretPad(t, true)
	run(t, p, `return await tools.t.note({text: "{{secret:GH_TOKEN}}"})`)
	if got := received(); len(got) != 1 || got[0] != `{"text":"{{secret:GH_TOKEN}}"}` || len(asked()) != 0 {
		t.Errorf("tool received %q, approvals %v", got, asked())
	}
}

func TestSecretRedactedFromErrors(t *testing.T) {
	p, _, _ := secretPad(t, true)
	r := run(t, p, `return await tools.t.leak({})`)
	if strings.Contains(r.Error, "ghp_value123") || !strings.Contains(r.Error, "{{secret:GH_TOKEN}}") {
		t.Errorf("error %q", r.Error)
	}
}

func TestUnknownSecretDoesntRun(t *testing.T) {
	p, received, asked := secretPad(t, true)
	r := run(t, p, `return await tools.t.send({cmd: "{{secret:NOPE}}"})`)
	if !strings.Contains(r.Error, "no secret NOPE") || len(received()) != 0 || len(asked()) != 0 {
		t.Errorf("result %+v", r)
	}
}
