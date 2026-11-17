package datafile

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestRoundTripAndVersions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "f.json")
	if err := Write(path, 2, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	var got []string
	if v, err := Read(path, 2, &got); err != nil || v != 2 || !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("Read = %d, %q, %v", v, got, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("leftover temp files: %d entries", len(entries))
	}

	// A reader that only knows version 1 refuses version 2 and leaves v alone.
	keep := []string{"untouched"}
	if v, err := Read(path, 1, &keep); !errors.Is(err, ErrNewer) || v != 2 || !slices.Equal(keep, []string{"untouched"}) {
		t.Errorf("older reader: %d, %q, %v", v, keep, err)
	}

	// Files without a version are rejected rather than guessed at.
	os.WriteFile(path, []byte(`{"data":["x"]}`), 0o600)
	if _, err := Read(path, 2, &got); err == nil {
		t.Error("unversioned file accepted")
	}
}
