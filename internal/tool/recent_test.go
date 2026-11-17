package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/spiiritual/steel/internal/datafile"
)

func TestRegistryRemembersRecentToolsAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steel", "recent.json")
	ok := func(context.Context, json.RawMessage) (any, error) { return "ok", nil }
	tools := []Tool{
		{Name: "a.x", Run: ok},
		{Name: "b.y", Run: ok},
		{Name: "c.fail", Run: func(context.Context, json.RawMessage) (any, error) { return nil, errors.New("no") }},
	}

	rec, err := LoadRecent(path, func(err error) { t.Error(err) })
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(tools...)
	reg.TrackRecent(rec)
	for _, name := range []string{"a.x", "b.y", "a.x", "c.fail"} {
		reg.Call(context.Background(), name, nil)
	}
	// Most recent first, each once; failed calls don't count.
	if got := reg.Recent(); !slices.Equal(got, []string{"a.x", "b.y"}) {
		t.Errorf("recent = %q", got)
	}

	// A restart reads the same order back.
	rec2, err := LoadRecent(path, nil)
	if err != nil || !slices.Equal(rec2.Names(), []string{"a.x", "b.y"}) {
		t.Errorf("after restart: %q, %v", rec2.Names(), err)
	}
	// Two processes sharing the file keep each other's updates.
	rec2.Use("c.z")
	rec.Use("b.y")
	if got := rec.Names(); !slices.Equal(got, []string{"b.y", "c.z", "a.x"}) {
		t.Errorf("after another process's update: %q", got)
	}

	// A corrupt file is an empty list plus an error, not a failed start.
	os.WriteFile(path, []byte("{"), 0o600)
	if rec3, err := LoadRecent(path, nil); err == nil || len(rec3.Names()) != 0 {
		t.Errorf("corrupt file: %q, %v", rec3.Names(), err)
	}
}

func TestRecentLeavesNewerFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recent.json")
	newer := `{"version":99,"data":{"tools":["x"]}}`
	os.WriteFile(path, []byte(newer), 0o600)
	rec, err := LoadRecent(path, func(err error) { t.Error(err) })
	if !errors.Is(err, datafile.ErrNewer) {
		t.Fatalf("LoadRecent err = %v", err)
	}
	rec.Use("a.x") // remembered in memory only
	if b, _ := os.ReadFile(path); string(b) != newer || !slices.Equal(rec.Names(), []string{"a.x"}) {
		t.Errorf("file = %s, names = %q", b, rec.Names())
	}
}
