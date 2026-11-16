package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ReadFile returns a tool that reads a text file; relative paths resolve against dir.
func ReadFile(dir string) Tool {
	return Tool{
		Name:        "read_file",
		Description: "Read a file and return its contents. Relative paths are resolved against the working directory " + dir + ".",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"File path"}},"required":["path"]}`),
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				Path string `json:"path"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return "", err
			}
			path := args.Path
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, path)
			}
			b, err := os.ReadFile(path)
			return string(b), err
		},
	}
}

// bashTimeout bounds a single bash invocation until async jobs exist.
const bashTimeout = 2 * time.Minute

// Bash returns a tool that runs a command with `bash -c` in dir.
func Bash(dir string) Tool {
	return Tool{
		Name:        "bash",
		Description: fmt.Sprintf("Run a shell command with bash -c in %s and return combined stdout/stderr. Times out after %s.", dir, bashTimeout),
		Parameters:  json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string","description":"Command to run"}},"required":["cmd"]}`),
		Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				Cmd string `json:"cmd"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return "", err
			}
			ctx, cancel := context.WithTimeout(ctx, bashTimeout)
			defer cancel()
			c := exec.CommandContext(ctx, "bash", "-c", args.Cmd)
			c.Dir = dir
			out, err := c.CombinedOutput()
			// A non-zero exit is a normal result the model should see, not a tool failure.
			if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) && ctx.Err() == nil {
				return fmt.Sprintf("%s[exit status %d]", out, exitErr.ExitCode()), nil
			}
			if ctx.Err() != nil {
				return string(out), fmt.Errorf("command stopped: %w", ctx.Err())
			}
			return string(out), err
		},
	}
}
