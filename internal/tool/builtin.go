package tool

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

var (
	// bashTimeout bounds a single bash invocation; a var so tests can shorten it.
	bashTimeout = 2 * time.Minute
	// bashWaitDelay bounds how long shell.bash waits for output after bash
	// exits (background processes may hold the pipe open) or after the
	// process group is killed.
	bashWaitDelay = 2 * time.Second
)

// bashOutputHead and bashOutputTail bound the output shell.bash keeps: the
// first and last bytes, with the middle replaced by an omission marker.
const (
	bashOutputHead = 32 << 10
	bashOutputTail = 32 << 10
)

// BashResult is what shell.bash returns.
type BashResult struct {
	Output    string `json:"output" desc:"Combined stdout and stderr"`
	ExitCode  int    `json:"exitCode"`
	Truncated bool   `json:"truncated" desc:"Output exceeded the cap; its middle was replaced by a marker stating the omitted byte count"`
}

// Bash returns a tool that runs a command with `bash -c` in dir. Before
// running, check lists why the command needs the user's approval; a command
// with reasons runs only once the approver in the call's context allows it.
// A nil check approves everything.
func Bash(dir string, check func(cmd string) []string) Tool {
	type args struct {
		Cmd string `json:"cmd"`
	}
	return New("shell.bash",
		fmt.Sprintf("Run a command with bash -c in %s; stdin is empty. A non-zero exit is a normal result, not an error. "+
			"Commands that are risky (recursive deletes, piping downloads into a shell, git push, sending data, touching steel's own files, …) wait for the user's approval, up to %s; if the user refuses or doesn't answer, the call throws: don't retry the same command, ask the user instead. "+
			"Output keeps the first %d KB and last %d KB; the middle is replaced by a marker and truncated is true, so filter large output (grep, head, tail) yourself. "+
			"Throws if the command runs longer than %s; its whole process group is killed. "+
			"Background processes that keep stdout/stderr open delay the result by %s; redirect their output to a file.",
			dir, ApprovalTimeout, bashOutputHead>>10, bashOutputTail>>10, bashTimeout, bashWaitDelay),
		func(ctx context.Context, a args) (BashResult, error) {
			if check != nil {
				if reasons := check(a.Cmd); len(reasons) > 0 {
					if err := Approve(ctx, Approval{Tool: "shell.bash", Action: a.Cmd, Reasons: reasons}); err != nil {
						return BashResult{}, err
					}
				}
			}
			ctx, cancel := context.WithTimeout(ctx, bashTimeout)
			defer cancel()
			c := exec.CommandContext(ctx, "bash", "-c", a.Cmd)
			c.Dir = dir
			c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
			c.WaitDelay = bashWaitDelay
			var out cappedWriter
			c.Stdout, c.Stderr = &out, &out
			err := c.Run()
			if ctx.Err() != nil {
				return BashResult{}, fmt.Errorf("command stopped: %w", ctx.Err())
			}
			if errors.Is(err, exec.ErrWaitDelay) {
				err = nil // bash exited; an orphaned background process held the pipe
			}
			res := BashResult{Output: out.String(), Truncated: out.total > bashOutputHead+bashOutputTail}
			if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) {
				res.ExitCode = exitErr.ExitCode()
				return res, nil
			}
			if err != nil {
				return BashResult{}, err
			}
			return res, nil
		})
}

// cappedWriter keeps the first bashOutputHead and last bashOutputTail bytes
// written to it, counting the rest.
type cappedWriter struct {
	head, tail []byte
	total      int64
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.total += int64(n)
	if room := bashOutputHead - len(w.head); room > 0 {
		k := min(room, len(p))
		w.head = append(w.head, p[:k]...)
		p = p[k:]
	}
	if len(p) == 0 {
		return n, nil
	}
	if len(p) >= bashOutputTail {
		w.tail = append(w.tail[:0], p[len(p)-bashOutputTail:]...)
		return n, nil
	}
	w.tail = append(w.tail, p...)
	if len(w.tail) > 2*bashOutputTail { // compact occasionally, not per write
		w.tail = append(w.tail[:0], w.tail[len(w.tail)-bashOutputTail:]...)
	}
	return n, nil
}

func (w *cappedWriter) String() string {
	tail := w.tail
	if len(tail) > bashOutputTail {
		tail = tail[len(tail)-bashOutputTail:]
	}
	omitted := w.total - int64(len(w.head)) - int64(len(tail))
	if omitted == 0 {
		return string(w.head) + string(tail)
	}
	return fmt.Sprintf("%s\n[... %d bytes omitted ...]\n%s", w.head, omitted, tail)
}
