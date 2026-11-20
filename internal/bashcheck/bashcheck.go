// Package bashcheck decides which shell commands need the user's approval
// before they run. It parses a command the way bash would and looks at every
// program it runs, including those inside pipes, subshells, command
// substitutions, functions and nested `bash -c` scripts.
//
// It asks about two kinds of command: those that would run code nobody can
// see in the command itself (`curl … | bash`, `eval "$x"`), and those with
// effects that are hard to undo or reach outside the machine (recursive
// deletes, git push, publishing, sending data, steel's own files). It is a
// tripwire for mistakes, not a sandbox: a determined script can always hide
// what it does, e.g. by writing a script to a file and running it.
package bashcheck

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Options say where a command runs.
type Options struct {
	Dir  string // working directory, for relative paths
	Home string // what ~ and $HOME expand to
	// Protected are directories whose contents commands may only touch with
	// approval: steel's own settings, secrets, history and memory.
	Protected []string
	// Unprotected are directories inside Protected that commands may touch
	// freely: skills, which the model reads, runs and writes as it likes.
	// A path is cleaned first, so .. can't climb out of one; linking to
	// steel's files from one names them, and so asks.
	Unprotected []string
}

// maxDepth bounds how deeply nested shell scripts (bash -c '…') are checked.
const maxDepth = 4

// Check returns why cmd needs approval, one reason per line in the order
// found; none means it may run.
func Check(cmd string, o Options) []string {
	c := &checker{o: o, dir: o.Dir}
	c.script(cmd, 0)
	return c.reasons
}

type checker struct {
	o       Options
	dir     string // the working directory as of the command being checked
	reasons []string
}

func (c *checker) add(format string, args ...any) {
	r := fmt.Sprintf(format, args...)
	if !slices.Contains(c.reasons, r) {
		c.reasons = append(c.reasons, r)
	}
}

// script checks src, a shell script nested depth levels deep.
func (c *checker) script(src string, depth int) {
	if depth > maxDepth {
		c.add("nests shells too deeply to check")
		return
	}
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err != nil {
		c.add("can't be checked: %v", err)
		return
	}
	piped := map[*syntax.CallExpr]bool{} // commands whose stdin is another command's output
	syntax.Walk(f, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.BinaryCmd:
			if n.Op == syntax.Pipe || n.Op == syntax.PipeAll {
				markFirst(n.Y, piped)
			}
		case *syntax.Stmt:
			var hdoc *syntax.Word
			for _, r := range n.Redirs {
				c.redirect(r)
				if r.Op == syntax.Hdoc || r.Op == syntax.DashHdoc || r.Op == syntax.WordHdoc {
					hdoc = r.Hdoc
					if r.Op == syntax.WordHdoc {
						hdoc = r.Word
					}
				}
			}
			if call, ok := n.Cmd.(*syntax.CallExpr); ok && len(call.Args) > 0 {
				c.call(call.Args, piped[call], hdoc, depth)
			}
		}
		return true
	})
}

// markFirst marks the command that receives stmt's stdin.
func markFirst(stmt *syntax.Stmt, piped map[*syntax.CallExpr]bool) {
	switch cmd := stmt.Cmd.(type) {
	case *syntax.CallExpr:
		piped[cmd] = true
	case *syntax.BinaryCmd:
		markFirst(cmd.X, piped)
	case *syntax.Subshell:
		if len(cmd.Stmts) > 0 {
			markFirst(cmd.Stmts[0], piped)
		}
	case *syntax.Block:
		if len(cmd.Stmts) > 0 {
			markFirst(cmd.Stmts[0], piped)
		}
	}
}

// redirect checks a redirection's target.
func (c *checker) redirect(r *syntax.Redirect) {
	if r.Word == nil || r.Op == syntax.Hdoc || r.Op == syntax.DashHdoc || r.Op == syntax.WordHdoc {
		return
	}
	if target, ok := c.literal(r.Word); ok {
		c.protected(target)
	}
}

// Programs grouped by what checking them involves.
var (
	shells       = []string{"sh", "bash", "zsh", "dash", "ksh", "mksh", "ash", "fish", "csh", "tcsh"}
	interpreters = []string{"python", "python3", "python2", "node", "nodejs", "deno", "bun", "perl", "ruby", "php", "lua", "Rscript"}
	asOtherUser  = []string{"sudo", "doas", "su", "pkexec"}
	destructive  = []string{"dd", "mkfs", "fdisk", "parted", "wipefs", "shred", "shutdown", "reboot", "halt", "poweroff"}
	remote       = []string{"ssh", "scp", "sftp", "mosh"}
)

// call checks one simple command: args are its words, piped says its stdin
// is another command's output, and hdoc is its here-document, if any.
func (c *checker) call(args []*syntax.Word, piped bool, hdoc *syntax.Word, depth int) {
	words := make([]string, len(args))
	known := make([]bool, len(args))
	for i, w := range args {
		words[i], known[i] = c.literal(w)
	}
	if !known[0] {
		c.add("runs a command whose name is only known when it runs")
		return
	}
	for i := 1; i < len(words); i++ {
		if known[i] {
			c.protected(words[i])
		}
	}

	name := filepath.Base(words[0])
	switch {
	case name == "cd" || name == "pushd":
		// Followed in source order, so later relative paths resolve where
		// they will run. Subshells' cds leak out, which only asks more.
		if len(words) > 1 && known[1] {
			c.dir = c.abs(words[1])
		}
	case slices.Contains(asOtherUser, name):
		c.add("runs as another user (%s)", name)
		c.wrapped(args, wrapperArgs(words, map[string]bool{"-u": true, "-g": true, "-C": true}), piped, depth)
	case name == "env":
		i := 1
		for i < len(words) && (strings.HasPrefix(words[i], "-") || strings.Contains(words[i], "=")) {
			if words[i] == "-u" || words[i] == "-C" || words[i] == "-S" {
				i++
			}
			i++
		}
		c.wrapped(args, i, piped, depth)
	case name == "command" || name == "builtin" || name == "exec" || name == "nohup" || name == "time" || name == "stdbuf" || name == "setsid":
		c.wrapped(args, wrapperArgs(words, nil), piped, depth)
	case name == "nice" || name == "ionice":
		c.wrapped(args, wrapperArgs(words, map[string]bool{"-n": true, "-c": true}), piped, depth)
	case name == "timeout":
		i := wrapperArgs(words, map[string]bool{"-s": true, "-k": true, "--signal": true, "--kill-after": true})
		c.wrapped(args, i+1, piped, depth) // skip the duration
	case name == "xargs":
		i := wrapperArgs(words, map[string]bool{"-I": true, "-n": true, "-P": true, "-L": true, "-d": true, "-E": true, "-s": true, "-a": true})
		c.wrapped(args, i, false, depth)
	case name == "eval":
		c.code("eval", words[1:], known[1:], depth)
	case name == "source" || name == ".":
		if len(words) > 1 && !known[1] {
			c.add("runs a script whose name is only known when it runs (%s)", name)
		}
	case slices.Contains(shells, name):
		c.shell(name, words, known, piped, hdoc, depth)
	case slices.Contains(interpreters, name) || strings.HasPrefix(name, "python3."):
		c.interpreter(name, words, known, piped)
	case name == "rm":
		if hasFlag(words[1:], "r", "R", "--recursive") {
			c.add("deletes recursively (rm -r)")
		}
	case name == "find":
		c.find(args, words, depth)
	case name == "git":
		if sub := gitSubcommand(words); sub == "push" {
			c.add("pushes to a git remote")
		}
	case slices.Contains(remote, name):
		c.add("connects to another machine (%s)", name)
	case name == "rsync":
		if slices.ContainsFunc(words[1:], func(w string) bool { return !strings.HasPrefix(w, "-") && strings.Contains(w, ":") }) {
			c.add("connects to another machine (rsync)")
		}
	case (name == "npm" || name == "pnpm" || name == "yarn" || name == "cargo" || name == "gem" || name == "poetry") && slices.Contains(words[1:], "publish"),
		name == "twine" && slices.Contains(words[1:], "upload"):
		c.add("publishes a package (%s)", name)
	case name == "curl":
		if curlSends(words[1:]) {
			c.add("sends data to a server (curl)")
		}
	case name == "wget":
		if slices.ContainsFunc(words[1:], func(w string) bool {
			return strings.HasPrefix(w, "--post-") || strings.HasPrefix(w, "--method") || strings.HasPrefix(w, "--body-")
		}) {
			c.add("sends data to a server (wget)")
		}
	case slices.Contains(destructive, name) || strings.HasPrefix(name, "mkfs."):
		c.add("can damage the system (%s)", name)
	}
}

// wrapperArgs returns the index of the command a wrapper like nice or sudo
// runs: the first word after its options, where withValue lists options
// that take the next word as their value.
func wrapperArgs(words []string, withValue map[string]bool) int {
	i := 1
	for i < len(words) && strings.HasPrefix(words[i], "-") {
		if words[i] == "--" {
			return i + 1
		}
		if withValue[words[i]] {
			i++
		}
		i++
	}
	return i
}

// wrapped checks the command a wrapper runs, starting at words[i].
func (c *checker) wrapped(args []*syntax.Word, i int, piped bool, depth int) {
	if i < len(args) {
		c.call(args[i:], piped, nil, depth)
	}
}

// code checks code given as arguments, as eval takes it.
func (c *checker) code(what string, words []string, known []bool, depth int) {
	if slices.Contains(known, false) {
		c.add("runs code that's only known when it runs (%s)", what)
		return
	}
	c.script(strings.Join(words, " "), depth+1)
}

// shell checks a shell's invocation: code in -c or a here-document is
// checked like the outer command; code from a pipe can't be.
func (c *checker) shell(name string, words []string, known []bool, piped bool, hdoc *syntax.Word, depth int) {
	for i := 1; i < len(words); i++ {
		w := words[i]
		if !strings.HasPrefix(w, "-") || strings.HasPrefix(w, "--") {
			if !known[i] {
				c.add("runs a script whose name is only known when it runs (%s)", name)
			}
			return // a script file: it's on disk, where the user can read it
		}
		if strings.Contains(w[1:], "c") { // -c, -ec, -lc, …
			if i+1 >= len(words) {
				return
			}
			c.code(name+" -c", words[i+1:i+2], known[i+1:i+2], depth)
			return
		}
	}
	switch {
	case hdoc != nil:
		if body, ok := c.literal(hdoc); ok {
			c.script(body, depth+1)
		} else {
			c.add("runs code that's only known when it runs (%s)", name)
		}
	case piped:
		c.add("pipes another command's output into %s, so the code it runs isn't visible", name)
	}
}

// interpreter checks a language interpreter's invocation: inline code is
// visible in the command, but code piped in, or built at run time, isn't.
func (c *checker) interpreter(name string, words []string, known []bool, piped bool) {
	for i := 1; i < len(words); i++ {
		w := words[i]
		if w == "-c" || w == "-e" || w == "--eval" || w == "-p" || w == "--print" || (name == "php" && w == "-r") {
			if i+1 < len(words) && !known[i+1] {
				c.add("runs code that's only known when it runs (%s)", name)
			}
			return
		}
		if !strings.HasPrefix(w, "-") {
			return // a script file, or a module (python -m x)
		}
		if w == "-m" {
			return
		}
	}
	if piped {
		c.add("pipes another command's output into %s, so the code it runs isn't visible", name)
	}
}

// find checks find's actions: -delete, and the commands -exec runs.
func (c *checker) find(args []*syntax.Word, words []string, depth int) {
	for i := 1; i < len(words); i++ {
		switch words[i] {
		case "-delete":
			c.add("deletes files (find -delete)")
		case "-exec", "-execdir", "-ok", "-okdir":
			end := i + 1
			for end < len(words) && words[end] != ";" && words[end] != "+" {
				end++
			}
			if end > i+1 {
				c.call(args[i+1:end], false, nil, depth)
			}
			i = end
		}
	}
}

// hasFlag reports whether words include any of the short flags (alone or
// bundled, as in -rf) or long ones.
func hasFlag(words []string, flags ...string) bool {
	for _, w := range words {
		if w == "--" {
			return false
		}
		for _, f := range flags {
			if strings.HasPrefix(f, "--") {
				if w == f {
					return true
				}
			} else if strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--") && strings.Contains(w[1:], f) {
				return true
			}
		}
	}
	return false
}

// gitSubcommand returns git's subcommand, skipping global options.
func gitSubcommand(words []string) string {
	for i := 1; i < len(words); i++ {
		switch w := words[i]; {
		case w == "-C" || w == "-c" || w == "--git-dir" || w == "--work-tree" || w == "--namespace":
			i++
		case strings.HasPrefix(w, "-"):
		default:
			return w
		}
	}
	return ""
}

// curlSends reports whether curl's arguments send data: a body, a form, an
// upload, or a method other than GET or HEAD.
func curlSends(words []string) bool {
	for i, w := range words {
		switch {
		case strings.HasPrefix(w, "--data") || strings.HasPrefix(w, "--form") || w == "--upload-file" || w == "--json":
			return true
		case w == "-X" || w == "--request":
			if i+1 < len(words) && !isReadMethod(words[i+1]) {
				return true
			}
		case strings.HasPrefix(w, "--request="):
			if !isReadMethod(strings.TrimPrefix(w, "--request=")) {
				return true
			}
		case strings.HasPrefix(w, "-") && !strings.HasPrefix(w, "--"):
			flags := w[1:]
			if j := strings.IndexByte(flags, 'X'); j >= 0 {
				method := flags[j+1:]
				if method == "" && i+1 < len(words) {
					method = words[i+1]
				}
				if !isReadMethod(method) {
					return true
				}
				flags = flags[:j]
			}
			if strings.ContainsAny(flags, "dFT") {
				return true
			}
		}
	}
	return false
}

func isReadMethod(m string) bool {
	m = strings.ToUpper(m)
	return m == "GET" || m == "HEAD" || m == "OPTIONS"
}

// protected asks about a path, or an option's value like --file=path, under
// one of the protected directories.
func (c *checker) protected(word string) {
	if _, v, ok := strings.Cut(word, "="); ok && strings.HasPrefix(word, "-") {
		word = v
	}
	if word == "" {
		return
	}
	path := c.abs(word)
	for _, dir := range c.o.Unprotected {
		if within(path, dir) {
			return
		}
	}
	for _, dir := range c.o.Protected {
		if within(path, dir) {
			c.add("touches steel's own files (%s)", word)
			return
		}
	}
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	dir = filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// abs resolves path against the current working directory.
func (c *checker) abs(path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.dir, path)
	}
	return filepath.Clean(path)
}

// literal returns a word's value when it can be known without running
// anything: quotes are removed, and ~ and $HOME are expanded. Other
// expansions make it unknown.
func (c *checker) literal(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for i, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			v := unescape(p.Value, false)
			if i == 0 && (v == "~" || strings.HasPrefix(v, "~/")) {
				v = c.o.Home + v[1:]
			}
			b.WriteString(v)
		case *syntax.SglQuoted:
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				switch q := q.(type) {
				case *syntax.Lit:
					b.WriteString(unescape(q.Value, true))
				case *syntax.ParamExp:
					if !c.home(q) {
						return "", false
					}
					b.WriteString(c.o.Home)
				default:
					return "", false
				}
			}
		case *syntax.ParamExp:
			if !c.home(p) {
				return "", false
			}
			b.WriteString(c.o.Home)
		default:
			return "", false
		}
	}
	return b.String(), true
}

// home reports whether p is a plain $HOME or ${HOME}.
func (c *checker) home(p *syntax.ParamExp) bool {
	return p.Param != nil && p.Param.Value == "HOME" && p.Index == nil && p.Slice == nil &&
		p.Repl == nil && p.Exp == nil && !p.Length && !p.Excl && c.o.Home != ""
}

// unescape removes the backslashes bash would: before any character outside
// quotes, and only before $ ` " \ and newline inside double quotes.
func unescape(s string, quoted bool) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			next := s[i+1]
			if next == '\n' {
				i++
				continue
			}
			if !quoted || strings.IndexByte("$`\"\\", next) >= 0 {
				b.WriteByte(next)
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
