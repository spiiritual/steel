package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ReadFile returns a tool that reads a text file; relative paths resolve against dir.
func ReadFile(dir string) Tool {
	type args struct {
		Path string `json:"path" desc:"Absolute, or relative to the working directory"`
	}
	return New("fs.read_file", "Read a text file. Working directory: "+dir,
		func(_ context.Context, a args) (string, error) {
			path := a.Path
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, path)
			}
			b, err := os.ReadFile(path)
			return string(b), err
		})
}

// bashTimeout bounds a single bash invocation.
const bashTimeout = 2 * time.Minute

// BashResult is what shell.bash returns.
type BashResult struct {
	Output   string `json:"output" desc:"Combined stdout and stderr"`
	ExitCode int    `json:"exitCode"`
}

// Bash returns a tool that runs a command with `bash -c` in dir.
func Bash(dir string) Tool {
	type args struct {
		Cmd string `json:"cmd"`
	}
	return New("shell.bash",
		fmt.Sprintf("Run a command with bash -c in %s. A non-zero exit is a normal result, not an error. Throws if the command runs longer than %s.", dir, bashTimeout),
		func(ctx context.Context, a args) (BashResult, error) {
			ctx, cancel := context.WithTimeout(ctx, bashTimeout)
			defer cancel()
			c := exec.CommandContext(ctx, "bash", "-c", a.Cmd)
			c.Dir = dir
			out, err := c.CombinedOutput()
			if ctx.Err() != nil {
				return BashResult{}, fmt.Errorf("command stopped: %w", ctx.Err())
			}
			if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) {
				return BashResult{Output: string(out), ExitCode: exitErr.ExitCode()}, nil
			}
			if err != nil {
				return BashResult{}, err
			}
			return BashResult{Output: string(out)}, nil
		})
}
