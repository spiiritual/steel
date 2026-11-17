package tool

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// maxRecent caps how many tools Recent remembers.
const maxRecent = 100

// Recent is a most-recently-used list of tool names, saved to a JSON file so
// it survives restarts. It is safe for concurrent use, and several steel
// processes can share the file: each update starts from what's saved.
//
// It's a plain slice: moving a name to the front costs at most maxRecent
// comparisons, once per tool call, and listing in order is free.
type Recent struct {
	path      string
	onSaveErr func(error)
	mu        sync.Mutex
	names     []string // most recent first
	reported  bool     // a save error went to onSaveErr already
}

// LoadRecent reads the list saved at path. A missing file is an empty list;
// an unreadable or corrupt one is also empty, with the error, so callers can
// warn and carry on. onSaveErr, if set, receives the first error saving the
// list; later ones are dropped, since they'd likely repeat it.
func LoadRecent(path string, onSaveErr func(error)) (*Recent, error) {
	r := &Recent{path: path, onSaveErr: onSaveErr}
	names, err := readNames(path)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	r.names = names
	return r, nil
}

func readNames(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(b, &names); err != nil {
		return nil, err
	}
	return names, nil
}

// Names returns the remembered tool names, most recently used first.
func (r *Recent) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.names)
}

// Use records name as the most recently used tool and saves the list.
func (r *Recent) Use(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Start from the saved list, which another process may have updated.
	if saved, err := readNames(r.path); err == nil {
		r.names = saved
	}
	if len(r.names) > 0 && r.names[0] == name {
		return // already first: nothing to save
	}
	r.names = slices.DeleteFunc(r.names, func(n string) bool { return n == name })
	r.names = slices.Insert(r.names, 0, name)
	if len(r.names) > maxRecent {
		r.names = r.names[:maxRecent]
	}
	if err := r.save(); err != nil && r.onSaveErr != nil && !r.reported {
		r.reported = true
		r.onSaveErr(err)
	}
}

// save writes the list atomically: a crash mid-write leaves the old file.
func (r *Recent) save() error {
	b, err := json.Marshal(r.names)
	if err != nil {
		return err
	}
	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".recent-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), r.path)
}
