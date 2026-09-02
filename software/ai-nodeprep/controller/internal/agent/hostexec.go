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
// Use only for short, light commands (queries, systemctl toggles): the child
// process lives in the agent pod's cgroup, so its memory counts against the
// pod limit. Anything heavy — apt, dpkg unpacking hundreds of packages —
// must go through heavyHostExec (found in live testing: `apt-get install
// doca-all` as a pod child OOM-killed the agent against its memory limit).
//
// Steps do not carry a context in v0.1, so each call enforces its own
// timeout locally; a pod deletion kills the child process regardless.
func (a *Agent) hostExec(env []string, timeout time.Duration, name string, args ...string) (string, error) {
	return a.hostExecV(env, timeout, name, false, args...)
}

// hostExecQuiet is hostExec without per-invocation logging, for sweeps and
// tool queries whose per-invocation output is steady-state noise: the ACS
// probe reads every PCI function and most expose no capability (~900 lines
// per verify pass on the lab workers), the mlxconfig device queries dump
// their full configuration table, the lspci enumeration lists every device.
// Outcomes still return to the caller, which aggregates them into the step
// message; the ledger stays the audit trail. Only the genuinely unexpected
// — a timeout, or a failed exec when the outcome is not aggregated — still
// logs. With -verbose (troubleshooting) quiet execs log like normal ones.
func (a *Agent) hostExecQuiet(env []string, timeout time.Duration, name string, args ...string) (string, error) {
	return a.hostExecV(env, timeout, name, true, args...)
}

func (a *Agent) hostExecV(env []string, timeout time.Duration, name string, quiet bool, args ...string) (string, error) {
	quiet = quiet && !a.verbose // -verbose (troubleshooting) logs everything
	cmdline := strings.Join(append([]string{name}, args...), " ")
	if !quiet {
		a.logf("host exec: %s", cmdline)
	}

	cctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	full := append([]string{"-t", "1", "-m", "-u", "-i", "-n", "--", name}, args...)
	cmd := exec.CommandContext(cctx, "nsenter", full...)
	cmd.Env = env
	out, err := a.runLogged(cctx, timeout, cmdline, cmd, quiet)
	if cctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("%s: timed out after %s", cmdline, timeout)
	}
	return out, err
}

// heavyHostExec runs a long, memory-hungry host command in a transient
// systemd unit on the host (systemd-run via nsenter) instead of as a child
// of the agent process: the unit is owned by the host's systemd, so its
// memory is accounted to the host, not the pod's cgroup, and the pod cannot
// be OOM-killed by host package management. --wait --pipe keeps output and
// exit status flowing back; --collect garbage-collects the unit afterwards.
//
// The unit name is derived from the command, so a retry while a previous
// attempt is still running fails fast with systemd's "already active"
// instead of two apts racing the dpkg lock. On timeout the transient unit
// keeps running on the host — dpkg state makes a later retry idempotent.
func (a *Agent) heavyHostExec(env []string, timeout time.Duration, name string, args ...string) (string, error) {
	unit := heavyUnitName(name)
	cmdline := strings.Join(append([]string{name}, args...), " ")
	a.logf("host exec (unit %s): %s", unit, cmdline)

	cctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	full := []string{"-t", "1", "-m", "-u", "-i", "-n", "--", "systemd-run",
		"--collect", "--wait", "--pipe", "--quiet", "--unit=" + unit}
	for _, e := range env {
		if heavyEnvForward(e) {
			full = append(full, "--setenv="+e)
		}
	}
	full = append(full, "--", name)
	full = append(full, args...)
	cmd := exec.CommandContext(cctx, "nsenter", full...)
	cmd.Env = env
	out, err := a.runLogged(cctx, timeout, cmdline, cmd, false)
	if cctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("%s: timed out after %s (host unit %s keeps running; dpkg state makes the retry idempotent)", cmdline, timeout, unit)
	}
	return out, err
}

// heavyUnitName derives a stable systemd unit name from a command path.
func heavyUnitName(name string) string {
	base := name
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	var b strings.Builder
	b.WriteString("nodeprep-")
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// heavyEnvForward reports whether an environment variable is worth carrying
// into the transient unit (the unit gets env only via --setenv, not the
// pod's whole environ). Interactive apt guardrails and proxies.
func heavyEnvForward(e string) bool {
	for _, p := range []string{
		"DEBIAN_FRONTEND=", "NEEDRESTART_MODE=", "DEBCONF_NONINTERACTIVE_SEEN=",
		"http_proxy=", "https_proxy=", "no_proxy=", "HTTP_PROXY=", "HTTPS_PROXY=", "NO_PROXY=",
	} {
		if strings.HasPrefix(e, p) {
			return true
		}
	}
	return false
}

// runLogged is the shared execution-and-audit body of hostExec and
// heavyHostExec: run, time, log the outcome with a bounded output tail.
// quiet (hostExecQuiet) suppresses the attempt and outcome lines; a timeout
// still logs there because it is never an expected result. (hostExecV
// already folded -verbose into quiet.)
func (a *Agent) runLogged(cctx context.Context, timeout time.Duration, cmdline string, cmd *exec.Cmd, quiet bool) (string, error) {
	start := time.Now()
	out, err := cmd.CombinedOutput()
	dur := time.Since(start).Round(time.Millisecond)
	tail := tailString(out, 2000)
	if err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			a.logf("host exec TIMED OUT after %s: %s", timeout, cmdline)
		} else if !quiet {
			a.logf("host exec failed after %s: %s", dur, tail)
		}
		return string(out), err
	}
	if !quiet {
		a.logf("host exec ok (%s): %s", dur, tail)
	}
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
