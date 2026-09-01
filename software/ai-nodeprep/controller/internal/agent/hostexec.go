package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// hostExec runs a command inside the host's mount/UTS/IPC/network namespaces
// via nsenter into PID 1 — the same mechanism as the reboot command — and
// returns its combined output. The DaemonSet runs with hostPID, so PID 1 is
// the host init. Every invocation and its outcome is logged (design §2:
// observable by default), so pod logs audit exactly what ran on the host and
// what the host said back.
//
// Steps do not carry a context in v0.1, so each call enforces its own
// timeout locally; a pod deletion kills the child process regardless.
func (a *Agent) hostExec(env []string, timeout time.Duration, name string, args ...string) (string, error) {
	cmdline := strings.Join(append([]string{name}, args...), " ")
	a.logf("host exec: %s", cmdline)

	cctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	full := append([]string{"-t", "1", "-m", "-u", "-i", "-n", "--", name}, args...)
	cmd := exec.CommandContext(cctx, "nsenter", full...)
	cmd.Env = env
	start := time.Now()
	out, err := cmd.CombinedOutput()
	dur := time.Since(start).Round(time.Millisecond)
	tail := tailString(out, 2000)
	if err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			a.logf("host exec TIMED OUT after %s: %s", timeout, cmdline)
			return string(out), fmt.Errorf("%s: timed out after %s", cmdline, timeout)
		}
		a.logf("host exec failed after %s: %s", dur, tail)
		return string(out), fmt.Errorf("%s: %w: %s", cmdline, err, tail)
	}
	a.logf("host exec ok (%s): %s", dur, tail)
	return string(out), nil
}

// tailString keeps a log line bounded while preserving the end of the
// output, where apt/dpkg put their summaries and errors.
func tailString(b []byte, max int) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "(no output)"
	}
	if len(s) > max {
		return "…" + s[len(s)-max:]
	}
	return s
}

// pkgInstalled asks the host dpkg database whether pkg is installed. Any
// query error (including "no packages found") reads as not installed — the
// install attempt then surfaces the real problem loudly.
func (a *Agent) pkgInstalled(env []string, pkg string) bool {
	out, err := a.hostExec(env, 2*time.Minute, "dpkg-query", "-W", "-f=${Status}", pkg)
	return err == nil && isInstalledStatus(out)
}

// isInstalledStatus parses `dpkg-query -W -f=${Status}` output. Held
// packages count as installed; config-file remnants do not.
func isInstalledStatus(out string) bool {
	f := strings.Fields(out)
	return len(f) == 3 && (f[0] == "install" || f[0] == "hold") && f[1] == "ok" && f[2] == "installed"
}
