// Package skill keeps steel's skills: how to do things, written down by the
// user or by the model. A skill is a folder holding SKILL.md, the Agent
// Skills format Claude Code and others use, so skills move between agents
// unchanged: frontmatter with a name and a description of when to use it,
// then instructions, beside any scripts or files they refer to.
package skill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/spiiritual/steel/internal/datafile"
	"github.com/spiiritual/steel/internal/llm"
)

const (
	// File is a skill's instructions, in its folder.
	File = "SKILL.md"
	// MaxName and MaxDescription are the Agent Skills format's limits.
	MaxName        = 64
	MaxDescription = 1024
	// indexDescription caps a description in the list each conversation
	// starts with; skills.read shows it whole.
	indexDescription = 300
	// indexBytes caps that list; skills.list shows everything.
	indexBytes = 12000
	// maxFiles caps the files skills.read lists.
	maxFiles = 100
)

// validName is the Agent Skills format's name: lowercase letters, digits
// and single hyphens between them.
var validName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Skill is one skill as listed.
type Skill struct {
	Name        string // its folder's name, which is how it's found
	Description string
}

// Doc is a skill opened for use.
type Doc struct {
	Skill
	Dir          string   // the skill's folder, absolute
	Instructions string   // SKILL.md after the frontmatter
	Files        []string // the other files in Dir, relative to it
}

// Store is a directory of skills. The directory is the truth: skills the
// user adds or edits there are seen at once.
type Store struct {
	dir string
	mu  sync.Mutex // one change at a time
}

// Open opens the skills in dir, creating it if needed.
func Open(dir string) (*Store, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir is where the skills are.
func (s *Store) Dir() string { return s.dir }

// List returns every skill, by name, and what's wrong with each folder
// that isn't a usable skill.
func (s *Store) List() ([]Skill, []string) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, []string{err.Error()}
	}
	var skills []Skill
	var problems []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		// Stat, not e.IsDir: a skill may be a link to a folder elsewhere.
		if fi, err := os.Stat(filepath.Join(s.dir, name)); err != nil || !fi.IsDir() {
			continue
		}
		front, _, err := s.load(name)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		skills = append(skills, Skill{Name: name, Description: front.description})
	}
	return skills, problems
}

// Read opens skill name.
func (s *Store) Read(name string) (Doc, error) {
	if err := lookupName(name); err != nil {
		return Doc{}, err
	}
	front, body, err := s.load(name)
	if err != nil {
		return Doc{}, err
	}
	dir := filepath.Join(s.dir, name)
	d := Doc{Skill: Skill{Name: name, Description: front.description}, Dir: dir, Instructions: strings.TrimSpace(body), Files: []string{}}
	// WalkDir doesn't follow links, so a linked skill's own folder is
	// opened by its real path.
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return d, nil
	}
	errFull := errors.New("full")
	filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil || path == root {
			return nil
		}
		if strings.HasPrefix(e.Name(), ".") {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if e.IsDir() || rel == File {
			return nil
		}
		if len(d.Files) == maxFiles {
			d.Files = append(d.Files, "… and more")
			return errFull
		}
		d.Files = append(d.Files, filepath.ToSlash(rel))
		return nil
	})
	return d, nil
}

// Write saves skill name's description and instructions, making it if
// it's new. Anything else in its folder stays, as does any other
// frontmatter in SKILL.md. It reports whether the skill is new.
func (s *Store) Write(name, description, instructions string) (bool, error) {
	if err := newName(name); err != nil {
		return false, err
	}
	description = strings.Join(strings.Fields(description), " ")
	switch {
	case description == "":
		return false, errors.New("give the skill a description: what it does and when to use it")
	case utf8.RuneCountInString(description) > MaxDescription:
		return false, fmt.Errorf("the description is over %d characters; shorten it and move details into the instructions", MaxDescription)
	case strings.TrimSpace(instructions) == "":
		return false, errors.New("give the skill instructions")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.dir, name)
	var extra []string
	old, err := os.ReadFile(filepath.Join(dir, File))
	created := errors.Is(err, fs.ErrNotExist)
	switch {
	case created:
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	default:
		if front, _, err := parse(old); err == nil {
			extra = front.extra
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "---\nname: %s\ndescription: %s\n", name, quote(description))
	for _, line := range extra {
		b.WriteString(line + "\n")
	}
	b.WriteString("---\n\n" + strings.TrimSpace(instructions) + "\n")
	return created, datafile.WriteFile(filepath.Join(dir, File), []byte(b.String()))
}

// Delete removes skill name and everything in its folder; a linked skill
// loses only the link.
func (s *Store) Delete(name string) error {
	if err := lookupName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, name)
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("there's no skill %q", name)
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return os.Remove(path)
	}
	return os.RemoveAll(path)
}

// Wake lists the skills for a new conversation, so the model knows what
// it knows how to do. It's a memory.Memory: the list is saved with the
// conversation and read from the prompt cache until /new. With no skills
// it says nothing; the system prompt already says how to make one.
func (s *Store) Wake(context.Context, string) ([]llm.Message, error) {
	skills, _ := s.List()
	if len(skills) == 0 {
		return nil, nil
	}
	var b strings.Builder
	b.WriteString("[steel] Your skills. Before a task one of these describes, read it with tools.skills.read and follow it.\n<skills>\n")
	for i, sk := range skills {
		line := fmt.Sprintf("%s: %s\n", sk.Name, cut(sk.Description, indexDescription))
		if b.Len()+len(line) > indexBytes {
			fmt.Fprintf(&b, "… and %d more: tools.skills.list() shows them all.\n", len(skills)-i)
			break
		}
		b.WriteString(line)
	}
	b.WriteString("</skills>")
	return []llm.Message{{Role: llm.RoleUser, Content: b.String()}}, nil
}

// load reads skill name's frontmatter and body.
func (s *Store) load(name string) (frontmatter, string, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, name, File))
	if errors.Is(err, fs.ErrNotExist) {
		if _, serr := os.Stat(filepath.Join(s.dir, name)); serr != nil {
			return frontmatter{}, "", fmt.Errorf("there's no skill %q", name)
		}
		return frontmatter{}, "", fmt.Errorf("no %s", File)
	}
	if err != nil {
		return frontmatter{}, "", err
	}
	front, body, err := parse(data)
	if err != nil {
		return frontmatter{}, "", fmt.Errorf("%s: %w", File, err)
	}
	if front.description == "" {
		return frontmatter{}, "", fmt.Errorf("%s has no description", File)
	}
	return front, body, nil
}

// lookupName checks a name that finds a skill. Folders the user made may
// break the format's naming rules, so it only keeps the lookup inside
// the skills directory.
func lookupName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return fmt.Errorf("%q isn't a skill's name", name)
	}
	return nil
}

// newName checks the name of a skill being written against the format.
func newName(name string) error {
	if len(name) > MaxName || !validName.MatchString(name) {
		return fmt.Errorf("%q isn't a valid skill name: use lowercase letters, digits and single hyphens, at most %d characters, e.g. \"deploy-blog\"", name, MaxName)
	}
	return nil
}

// frontmatter is what steel reads of SKILL.md's frontmatter. extra is
// everything else, line for line, kept when the skill is rewritten.
type frontmatter struct {
	name, description string
	extra             []string
}

// parse splits SKILL.md into frontmatter and body. It reads the YAML that
// skills use, not all of YAML: top-level "key: value" lines, quoted or
// plain, and block (| or >) or continued values for name and description.
func parse(data []byte) (frontmatter, string, error) {
	text := strings.ReplaceAll(string(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return frontmatter{}, "", errors.New("no frontmatter: it must start with a line of ---")
	}
	head, body, ok := strings.Cut("\n"+rest, "\n---")
	if !ok {
		return frontmatter{}, "", errors.New("the frontmatter never ends: close it with a line of ---")
	}
	head = strings.TrimPrefix(head, "\n")
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:]
	} else {
		body = ""
	}
	var f frontmatter
	lines := strings.Split(head, "\n")
	for i := 0; i < len(lines); {
		line := lines[i]
		j := i + 1
		for j < len(lines) && (lines[j] == "" || lines[j][0] == ' ' || lines[j][0] == '\t') {
			j++
		}
		key, value, isKey := strings.Cut(line, ":")
		switch {
		case isKey && key == "name":
			f.name = scalar(value, lines[i+1:j])
		case isKey && key == "description":
			f.description = scalar(value, lines[i+1:j])
		default:
			f.extra = append(f.extra, lines[i:j]...)
		}
		i = j
	}
	for len(f.extra) > 0 && strings.TrimSpace(f.extra[len(f.extra)-1]) == "" {
		f.extra = f.extra[:len(f.extra)-1]
	}
	return f, body, nil
}

// scalar reads a value, given the rest of its line and the indented lines
// after it, as one line of text.
func scalar(first string, more []string) string {
	first = strings.TrimSpace(first)
	var parts []string
	if first != "" && !strings.HasPrefix(first, "|") && !strings.HasPrefix(first, ">") {
		parts = append(parts, first)
	}
	for _, l := range more {
		parts = append(parts, strings.TrimSpace(l))
	}
	v := strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
	if n := len(v); n >= 2 {
		switch {
		case v[0] == '"' && v[n-1] == '"':
			v = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(v[1 : n-1])
		case v[0] == '\'' && v[n-1] == '\'':
			v = strings.ReplaceAll(v[1:n-1], "''", "'")
		}
	}
	return v
}

// quote writes a description as a YAML double-quoted string, which any
// text survives.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// cut shortens s to at most n characters.
func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
