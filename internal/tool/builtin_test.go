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
	out, err := Bash(t.TempDir(), nil).Run(context.Background(), raw)
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

func approvalTest(t *testing.T, answer func(Approval) (bool, error)) (ran bool, asked []Approval, err error) {
	t.Helper()
	dir := t.TempDir()
	check := func(cmd string) []string {
		if strings.Contains(cmd, "risky") {
			return []string{"is risky"}
		}
		return nil
	}
	ctx := WithApprover(context.Background(), func(_ context.Context, a Approval) (bool, error) {
		asked = append(asked, a)
		return answer(a)
	})
	raw, _ := json.Marshal(map[string]string{"cmd": "touch ran # risky"})
	_, err = Bash(dir, check).Run(ctx, raw)
	_, statErr := os.Stat(filepath.Join(dir, "ran"))
	return statErr == nil, asked, err
}

func TestBashApproval(t *testing.T) {
	ran, asked, err := approvalTest(t, func(Approval) (bool, error) { return true, nil })
	if !ran || err != nil || len(asked) != 1 || asked[0].Action != "touch ran # risky" || asked[0].Reasons[0] != "is risky" {
		t.Errorf("allowed: ran %v, asked %+v, err %v", ran, asked, err)
	}

	ran, _, err = approvalTest(t, func(Approval) (bool, error) { return false, nil })
	if ran || err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("refused: ran %v, err %v", ran, err)
	}

	ran, _, err = approvalTest(t, func(Approval) (bool, error) { return false, context.DeadlineExceeded })
	if ran || err == nil || !strings.Contains(err.Error(), "didn't answer") {
		t.Errorf("no answer: ran %v, err %v", ran, err)
	}
}

// Without anyone to ask, risky commands don't run; others do.
func TestBashApprovalFailsClosed(t *testing.T) {
	dir := t.TempDir()
	bash := Bash(dir, func(cmd string) []string {
		if strings.Contains(cmd, "risky") {
			return []string{"is risky"}
		}
		return nil
	})
	if _, err := bash.Run(context.Background(), json.RawMessage(`{"cmd":"touch a # risky"}`)); err == nil || !strings.Contains(err.Error(), "can't be asked") {
		t.Errorf("risky: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); err == nil {
		t.Error("risky command ran")
	}
	if _, err := bash.Run(context.Background(), json.RawMessage(`{"cmd":"touch b"}`)); err != nil {
		t.Errorf("safe: %v", err)
	}
}
