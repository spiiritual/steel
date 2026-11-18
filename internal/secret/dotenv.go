package secret

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/spiiritual/steel/internal/datafile"
)

// Dotenv is the .env file next to config.json that holds secrets: KEY=value
// lines, with optional "export ", quotes and # comments. steel looks its
// values up itself and never exports them to its own environment, so tools
// (shell.bash) can't read them.
type Dotenv struct {
	lines []string // kept verbatim, so comments and order survive edits
}

// envName is what a .env key (and so a secret's name) must look like.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// isVar reports whether s can be a .env key.
func isVar(s string) bool { return envName.MatchString(s) }

// ReadDotenv reads path; a missing file is an empty one.
func ReadDotenv(path string) (*Dotenv, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Dotenv{}, nil
	}
	if err != nil {
		return nil, err
	}
	d := &Dotenv{lines: strings.Split(strings.TrimRight(string(b), "\n"), "\n")}
	for i, line := range d.lines {
		if _, _, ok, err := parseDotenvLine(line); ok && err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, i+1, err)
		}
	}
	return d, nil
}

// parseDotenvLine returns the variable a line sets; ok is false for blank
// lines and comments.
func parseDotenvLine(line string) (key, value string, ok bool, err error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false, nil
	}
	line = strings.TrimPrefix(line, "export ")
	key, value, found := strings.Cut(line, "=")
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	if !found || !envName.MatchString(key) {
		return "", "", true, errors.New("want KEY=value")
	}
	switch {
	case strings.HasPrefix(value, `"`):
		value, err = strconv.Unquote(value)
	case strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") && len(value) >= 2:
		value = value[1 : len(value)-1]
	}
	return key, value, true, err
}

// Vars returns the variables the file sets; a later line wins.
func (d *Dotenv) Vars() map[string]string {
	m := map[string]string{}
	for _, line := range d.lines {
		if k, v, ok, err := parseDotenvLine(line); ok && err == nil {
			m[k] = v
		}
	}
	return m
}

// Set replaces key's first line, dropping any later ones, or appends one.
func (d *Dotenv) Set(key, value string) {
	line := key + "=" + value
	if strings.ContainsAny(value, " \t\r\n#\"'\\") || value == "" {
		line = key + "=" + strconv.Quote(value)
	}
	for i, old := range d.lines {
		if k, _, ok, _ := parseDotenvLine(old); ok && k == key {
			// No line before i sets key, so i is still its place after unset.
			d.Unset(key)
			d.lines = slices.Insert(d.lines, i, line)
			return
		}
	}
	d.lines = append(d.lines, line)
}

// Unset removes every line setting key, reporting whether there was one.
func (d *Dotenv) Unset(key string) bool {
	kept := d.lines[:0]
	for _, line := range d.lines {
		if k, _, ok, _ := parseDotenvLine(line); ok && k == key {
			continue
		}
		kept = append(kept, line)
	}
	removed := len(kept) < len(d.lines)
	d.lines = kept
	return removed
}

// Write saves the file at path, readable only by its owner.
func (d *Dotenv) Write(path string) error {
	return datafile.WriteFile(path, []byte(strings.Join(d.lines, "\n")+"\n"))
}
