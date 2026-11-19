// Package secret keeps the secrets tools use (API tokens, passwords) where
// the model can't see them. Scripts handle a secret only as a placeholder,
// "{{secret:NAME}}"; the scratchpad swaps in the value when a call leaves
// for a tool that accepts secrets, and swaps values back to placeholders in
// everything tools return. Values live in secrets.env beside config.json,
// apart from steel's own tokens and settings in .env.
package secret

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Placeholder is how scripts refer to secret name.
func Placeholder(name string) string { return "{{secret:" + name + "}}" }

var placeholderRE = regexp.MustCompile(`\{\{secret:([A-Za-z_][A-Za-z0-9_]*)\}\}`)

// ValidName reports why name can't name a secret, or nil if it can. Names
// are upper case, like the environment variables they're saved as, so a
// mistyped "steel config" setting isn't saved as a secret.
func ValidName(name string) error {
	if !isVar(name) || name != strings.ToUpper(name) {
		return fmt.Errorf("%q can't name a secret: use upper-case letters, digits and _, like GITHUB_TOKEN", name)
	}
	return nil
}

// Options configure a Store.
type Options struct {
	// Redact maps names to more values to redact that aren't secrets
	// the model may use, such as steel's own tokens.
	Redact map[string]string
}

// Store is the secrets in a dotenv file. It rereads the file whenever it
// changes, so "steel config set" on the host takes effect without a
// restart. It's safe for concurrent use.
type Store struct {
	path  string
	extra map[string]string

	mu       sync.Mutex
	stamp    stamp             // of the file vars was read from
	vars     map[string]string // the file's variables, hidden ones too
	redactor *strings.Replacer
}

// stamp identifies a version of the file.
type stamp struct {
	mod  time.Time
	size int64
	ok   bool // the file existed
}

// Open returns the store for the dotenv file at path, which needn't exist.
func Open(path string, o Options) (*Store, error) {
	s := &Store{path: path, extra: map[string]string{}}
	for n, v := range o.Redact {
		s.extra[n] = v
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// SetRedacted makes name's value one more to redact, replacing the value
// name had, for credentials steel learns after Open, such as refreshed OAuth
// tokens.
func (s *Store) SetRedacted(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.extra[name] == value {
		return
	}
	s.extra[name] = value
	s.redactor = newRedactor(s.vars, s.extra)
}

// loadLocked rereads the file if it changed since the last read. On error
// the previous contents stay, so redaction keeps working.
func (s *Store) loadLocked() error {
	st, err := statFile(s.path)
	if err != nil {
		return err
	}
	if s.vars != nil && st == s.stamp {
		return nil
	}
	d, err := ReadDotenv(s.path)
	if err != nil {
		return err
	}
	s.setLocked(st, d.Vars())
	return nil
}

// setLocked makes vars, read from the file at st, the store's contents.
func (s *Store) setLocked(st stamp, vars map[string]string) {
	s.stamp, s.vars = st, vars
	s.redactor = newRedactor(vars, s.extra)
}

// statFile stamps the file at path; ok is false if it doesn't exist.
func statFile(path string) (stamp, error) {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		return stamp{mod: info.ModTime(), size: info.Size(), ok: true}, nil
	case errors.Is(err, fs.ErrNotExist):
		return stamp{}, nil
	}
	return stamp{}, err
}

// newRedactor replaces each value, raw and as it appears inside a JSON
// string, with its placeholder.
func newRedactor(sets ...map[string]string) *strings.Replacer {
	type pair struct{ old, name string }
	var pairs []pair
	for _, m := range sets {
		for name, v := range m {
			if v == "" {
				continue
			}
			pairs = append(pairs, pair{v, name})
			if j := jsonInner(v); j != v {
				pairs = append(pairs, pair{j, name})
			}
		}
	}
	// The replacer prefers earlier pairs at the same position: longest
	// first, so a value containing another is replaced whole.
	slices.SortFunc(pairs, func(a, b pair) int {
		return cmp.Or(len(b.old)-len(a.old), strings.Compare(a.old, b.old))
	})
	var args []string
	for _, p := range pairs {
		args = append(args, p.old, Placeholder(p.name))
	}
	return strings.NewReplacer(args...)
}

// jsonInner is v as it appears between the quotes of a JSON string.
func jsonInner(v string) string {
	b, _ := json.Marshal(v)
	return string(b[1 : len(b)-1])
}

// Names returns the secrets' names, sorted.
func (s *Store) Names() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	names := []string{}
	for n := range s.vars {
		if ValidName(n) == nil {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names, nil
}

// Has reports whether secret name exists.
func (s *Store) Has(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return false, err
	}
	_, ok := s.vars[name]
	return ok, nil
}

// Value returns secret name's value, read from the file as it is now, for
// steel's own use: expanding ${NAME} in an MCP server's config. It must
// never reach the model.
func (s *Store) Value(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.loadLocked() // on error, use what was read before
	v, ok := s.vars[name]
	return v, ok
}

// CheckName reports why name can't be set, or nil if it can.
func (s *Store) CheckName(name string) error { return ValidName(name) }

// Set saves secret name, replacing any old value.
func (s *Store) Set(name, value string) error {
	if err := s.CheckName(name); err != nil {
		return err
	}
	if value == "" {
		return errors.New("a secret can't be empty")
	}
	return s.edit(func(d *Dotenv) bool { d.Set(name, value); return true })
}

// Remove deletes secret name, reporting whether it existed.
func (s *Store) Remove(name string) (bool, error) {
	if err := s.CheckName(name); err != nil {
		return false, err
	}
	var removed bool
	err := s.edit(func(d *Dotenv) bool { removed = d.Unset(name); return removed })
	return removed, err
}

// edit applies f to the file as it is now, so edits made on the host since
// the last read survive, and saves it if f reports a change.
func (s *Store) edit(f func(*Dotenv) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := ReadDotenv(s.path)
	if err != nil {
		return err
	}
	if !f(d) {
		return nil
	}
	if err := d.Write(s.path); err != nil {
		return err
	}
	st, err := statFile(s.path)
	if err != nil {
		return err
	}
	s.setLocked(st, d.Vars())
	return nil
}

// errNoSecret is the model's error for a secret that doesn't exist.
func errNoSecret(name string) error {
	return fmt.Errorf("no secret %s: tools.secrets.list() shows the secrets there are, and tools.secrets.add() asks the user for a new one", name)
}

// Fill replaces placeholders in args, a tool call's JSON arguments, with
// their secrets' values, returning the names used in order of first use.
// Placeholders sit inside JSON strings, so values are JSON-escaped. A
// placeholder naming no secret is an error: the call must not run with it.
func (s *Store) Fill(args []byte) ([]byte, []string, error) {
	locs := placeholderRE.FindAllSubmatchIndex(args, -1)
	if len(locs) == 0 {
		return args, nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, nil, err
	}
	var out []byte
	var names []string
	last := 0
	for _, l := range locs {
		name := string(args[l[2]:l[3]])
		v, ok := s.vars[name]
		if !ok || v == "" {
			return nil, nil, errNoSecret(name)
		}
		out = append(out, args[last:l[0]]...)
		out = append(out, jsonInner(v)...)
		last = l[1]
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return append(out, args[last:]...), names, nil
}

// Redact replaces every secret value in text with its placeholder: values
// that come back from tools never reach the model. Short values are
// replaced too, wherever they appear: that can mangle unrelated text, but
// never shows a value.
func (s *Store) Redact(text string) string {
	return s.currentRedactor().Replace(text)
}

// RedactJSON is Redact for a JSON document that still parses afterwards.
// Inside strings, values appear JSON-escaped and placeholders are valid.
// Outside them, a literal (a number, true, false, null) containing a
// value becomes a string of its redacted text: {"pin":1234} becomes
// {"pin":"{{secret:PIN}}"}.
func (s *Store) RedactJSON(text string) string {
	r := s.currentRedactor()
	var b strings.Builder
	for {
		start := strings.IndexByte(text, '"')
		if start < 0 {
			b.WriteString(redactLiterals(r, text))
			return b.String()
		}
		end := start + 1
		for end < len(text) && text[end] != '"' {
			if text[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(text) { // malformed: redact it as plain text
			b.WriteString(r.Replace(text))
			return b.String()
		}
		b.WriteString(redactLiterals(r, text[:start]))
		b.WriteString(redactString(r, text[start:end+1]))
		text = text[end+1:]
	}
}

// redactString redacts a JSON string literal, quotes included. It's
// decoded first, so a value can't match part of an escape (a PIN 0041
// inside \u0041) and leave it broken.
func redactString(r *strings.Replacer, lit string) string {
	var v string
	if json.Unmarshal([]byte(lit), &v) != nil {
		return r.Replace(lit)
	}
	red := r.Replace(v)
	if red == v {
		return lit
	}
	return jsonString(red)
}

// jsonString is s as a JSON string literal.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// redactLiterals redacts the JSON literals in text, which holds no
// strings, turning each that contains a value into a string.
func redactLiterals(r *strings.Replacer, text string) string {
	var b strings.Builder
	for len(text) > 0 {
		i := strings.IndexAny(text, "{}[],: \t\r\n")
		if i < 0 {
			i = len(text)
		}
		if i == 0 {
			b.WriteByte(text[0])
			text = text[1:]
			continue
		}
		lit := text[:i]
		if red := r.Replace(lit); red != lit {
			lit = jsonString(red)
		}
		b.WriteString(lit)
		text = text[i:]
	}
	return b.String()
}

func (s *Store) currentRedactor() *strings.Replacer {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.loadLocked() // on error, redact with what was read before
	return s.redactor
}
