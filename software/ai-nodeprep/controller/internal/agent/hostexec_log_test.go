package agent

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// captureStdout runs fn with os.Stdout redirected into a pipe and returns
// what logf printed (logf is fmt.Printf, so the exec-level audit lines are
// observable this way).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = old
	w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

// The exec-level logging contract (0.1.74): a successful host exec is
// silent in normal mode — the step bodies aggregate their outcomes into one
// ledger message per pass, and per-exec "host exec:"/"host exec ok:" echoes
// were ~620 of 1577 lines on a routine Finalizing walk. A failure logs once
// with the command; -verbose restores the full trace.
func TestRunLoggedSuccessIsSilent(t *testing.T) {
	ok := func() *exec.Cmd { return exec.Command("true") }
	fail := func() *exec.Cmd { return exec.Command("false") }

	cases := []struct {
		name    string
		verbose bool
		quiet   bool
		cmd     func() *exec.Cmd
		wantSub string // empty = no output expected
	}{
		{"success normal non-quiet", false, false, ok, ""},
		{"success normal quiet", false, true, ok, ""},
		{"success verbose", true, false, ok, "host exec ok"},
		{"failure normal non-quiet", false, false, fail, "host exec failed after"},
		{"failure normal quiet (aggregated by caller)", false, true, fail, ""},
		{"failure verbose", true, false, fail, "host exec failed after"},
	}
	for _, tc := range cases {
		a := &Agent{verbose: tc.verbose}
		cmd := tc.cmd()
		run := func() {
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, _ = a.runLogged(cctx, 10*time.Second, "probe-cmd --flag", cmd, tc.quiet)
			cancel()
		}
		got := captureStdout(t, run)
		if tc.wantSub == "" {
			if strings.TrimSpace(got) != "" {
				t.Fatalf("%s: expected silence, got %q", tc.name, got)
			}
			continue
		}
		if !strings.Contains(got, tc.wantSub) {
			t.Fatalf("%s: output %q missing %q", tc.name, got, tc.wantSub)
		}
		if strings.HasPrefix(tc.wantSub, "host exec failed") && !strings.Contains(got, "probe-cmd --flag") {
			t.Fatalf("%s: failure line must carry the command, got %q", tc.name, got)
		}
	}
}
