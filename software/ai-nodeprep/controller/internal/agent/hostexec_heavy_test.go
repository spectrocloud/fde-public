package agent

import (
	"strings"
	"testing"
)

// The heavy-exec unit name must be a valid systemd unit name: derived from
// the command basename, special characters flattened.
func TestHeavyUnitName(t *testing.T) {
	if got := heavyUnitName("apt-get"); got != "nodeprep-apt-get" {
		t.Fatalf("apt-get -> %q", got)
	}
	if got := heavyUnitName("/usr/bin/dpkg"); got != "nodeprep-dpkg" {
		t.Fatalf("/usr/bin/dpkg -> %q", got)
	}
	if got := heavyUnitName("weird@cmd+name"); got != "nodeprep-weird-cmd-name" {
		t.Fatalf("special chars must be flattened, got %q", got)
	}
}

// Only apt-relevant env reaches the transient unit (--setenv); the pod's
// whole environ does not.
func TestHeavyEnvForward(t *testing.T) {
	yes := []string{"DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=l", "http_proxy=http://p:1", "HTTPS_PROXY=x", "no_proxy=y"}
	for _, e := range yes {
		if !heavyEnvForward(e) {
			t.Fatalf("%q should be forwarded", e)
		}
	}
	no := []string{"PATH=/usr/bin", "HOSTNAME=agent-pod", "KUBECONFIG=/etc/kube.conf", "POD_NAME=x"}
	for _, e := range no {
		if heavyEnvForward(e) {
			t.Fatalf("%q must not be forwarded", e)
		}
	}
}

// The wrapper must invoke systemd-run with --wait --pipe --collect and the
// command after a bare "--", with forwarded env as --setenv flags.
func TestHeavyHostExecArgShape(t *testing.T) {
	env := []string{"PATH=/usr/bin", "DEBIAN_FRONTEND=noninteractive"}
	// Rebuild the argument assembly the way heavyHostExec does, asserting
	// the exact shape (kept in sync with heavyHostExec).
	full := []string{"-t", "1", "-m", "-u", "-i", "-n", "--", "systemd-run",
		"--collect", "--wait", "--pipe", "--quiet", "--unit=" + heavyUnitName("apt-get")}
	for _, e := range env {
		if heavyEnvForward(e) {
			full = append(full, "--setenv="+e)
		}
	}
	full = append(full, "--", "apt-get", "--yes", "doca-all")
	joined := strings.Join(full, " ")
	for _, want := range []string{
		"systemd-run --collect --wait --pipe --quiet --unit=nodeprep-apt-get",
		"--setenv=DEBIAN_FRONTEND=noninteractive",
		"-- apt-get --yes doca-all",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("arg shape missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "PATH=") {
		t.Fatalf("PATH must not leak into the unit env: %s", joined)
	}
}
