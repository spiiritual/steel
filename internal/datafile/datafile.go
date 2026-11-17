// Package datafile reads and writes steel's saved state as versioned JSON,
// so a newer steel can migrate old files and an older one won't clobber
// files it doesn't understand.
package datafile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNewer means a file was written by a newer steel. Don't overwrite it:
// that would lose whatever the newer format holds.
var ErrNewer = errors.New("written by a newer version of steel")

// envelope is the on-disk shape: {"version": N, "data": ...}.
type envelope struct {
	Version int             `json:"version"`
	Data    json.RawMessage `json:"data"`
}

// Read decodes the file at path into v and returns the format version it
// was saved with, so callers can migrate older ones. A version above
// current is ErrNewer and leaves v untouched. A missing file is an error
// matching fs.ErrNotExist.
func Read(path string, current int, v any) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var env envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return 0, err
	}
	if env.Version > current {
		return env.Version, fmt.Errorf("%s: format version %d %w (this one reads up to %d)", path, env.Version, ErrNewer, current)
	}
	if env.Version < 1 {
		return env.Version, fmt.Errorf("%s: missing format version", path)
	}
	return env.Version, json.Unmarshal(env.Data, v)
}

// Write saves v at path as format version, atomically: a crash mid-write
// leaves the previous file. The directory is created if needed.
func Write(path string, version int, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b, err := json.Marshal(envelope{Version: version, Data: data})
	if err != nil {
		return err
	}
	return WriteFile(path, b)
}

// WriteFile saves b at path atomically, readable only by its owner: a crash
// mid-write leaves the previous file. The directory is created if needed.
// It's for files like config.json that keep their own plain format.
func WriteFile(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp") // mode 0600
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
