package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runBash(t *testing.T, cmd string) (BashResult, error) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"cmd": cmd})
	out, err := Bash(t.TempDir()).Run(context.Background(), raw)
	if err != nil {
		return BashResult{}, err
	}
	return out.(BashResult), nil
}

func TestBashExitCode(t *testing.T) {
	res, err := runBash(t, "echo hi; echo err >&2; exit 3")
	if err != nil || res.ExitCode != 3 || res.Output != "hi\nerr\n" || res.Truncated {
		t.Errorf("got %+v, %v", res, err)
	}
}

func TestBashTimeoutKillsProcessGroup(t *testing.T) {
	defer func(d time.Duration) { bashTimeout = d }(bashTimeout)
	bashTimeout = 300 * time.Millisecond

	for _, cmd := range []string{
		"sleep 6 | cat; echo done", // pipeline child holds stdout
		"sleep 6 & wait",           // background child holds stdout
	} {
		start := time.Now()
		_, err := runBash(t, cmd)
		if err == nil || !strings.Contains(err.Error(), "command stopped") {
			t.Errorf("%q: err = %v", cmd, err)
		}
		if d := time.Since(start); d > bashTimeout+bashWaitDelay+time.Second {
			t.Errorf("%q: returned after %s", cmd, d)
		}
	}
}

func TestBashOrphanHoldingPipeCompletes(t *testing.T) {
	start := time.Now()
	res, err := runBash(t, "sleep 30 & echo $!")
	if err != nil {
		t.Fatal(err)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(res.Output))
	if perr == nil {
		defer syscall.Kill(pid, syscall.SIGKILL)
	}
	if perr != nil || res.ExitCode != 0 {
		t.Errorf("got %+v", res)
	}
	if d := time.Since(start); d > bashWaitDelay+3*time.Second {
		t.Errorf("returned after %s", d)
	}
}

func TestBashTruncatesOutput(t *testing.T) {
	res, err := runBash(t, "printf 'BEGIN'; yes | head -c 1000000; printf 'END'")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.ExitCode != 0 {
		t.Errorf("got truncated=%v exit=%d", res.Truncated, res.ExitCode)
	}
	omitted := 1000000 + len("BEGINEND") - bashOutputHead - bashOutputTail
	marker := "\n[... " + strconv.Itoa(omitted) + " bytes omitted ...]\n"
	if !strings.HasPrefix(res.Output, "BEGINy\ny\n") || !strings.HasSuffix(res.Output, "y\nEND") || !strings.Contains(res.Output, marker) {
		t.Errorf("head/tail/marker missing: %q ... %q", res.Output[:20], res.Output[len(res.Output)-20:])
	}
	if want := bashOutputHead + len(marker) + bashOutputTail; len(res.Output) != want {
		t.Errorf("len = %d, want %d", len(res.Output), want)
	}
}

func TestCappedWriterSmallWrites(t *testing.T) {
	var w cappedWriter
	var all []byte
	for i := range 50000 {
		line := []byte(strconv.Itoa(i) + "\n")
		all = append(all, line...)
		w.Write(line)
	}
	got := w.String()
	omitted := len(all) - bashOutputHead - bashOutputTail
	want := string(all[:bashOutputHead]) + "\n[... " + strconv.Itoa(omitted) + " bytes omitted ...]\n" + string(all[len(all)-bashOutputTail:])
	if got != want {
		t.Errorf("mismatch: len %d want %d", len(got), len(want))
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	tl := ReadFile(dir)
	read := func(path string) (string, error) {
		raw, _ := json.Marshal(map[string]string{"path": path})
		out, err := tl.Run(context.Background(), raw)
		if err != nil {
			return "", err
		}
		return out.(string), nil
	}

	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := read("ok.txt"); err != nil || got != "hello" {
		t.Errorf("ok.txt = %q, %v", got, err)
	}

	if _, err := read(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory: %v", err)
	}

	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := read(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("fifo: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}

	big := filepath.Join(dir, "big.log")
	if err := os.WriteFile(big, make([]byte, readFileMax+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := read(big); err == nil || !strings.Contains(err.Error(), strconv.Itoa(readFileMax+1)+" bytes") || !strings.Contains(err.Error(), "head") {
		t.Errorf("oversized: %v", err)
	}
}
