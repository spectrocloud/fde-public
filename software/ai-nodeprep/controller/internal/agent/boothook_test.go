package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

func TestRenderBootHookAlways(t *testing.T) {
	unit, script := renderBootHook("node-7", v1alpha1.HostBootSpec{KubeletStateReset: "always"})
	if !strings.Contains(unit, "Before=kubelet.service") || !strings.Contains(unit, "ExecStart=/usr/local/sbin/nodeprep-boot-hook") {
		t.Fatalf("unit missing the kubelet ordering or ExecStart:\n%s", unit)
	}
	if !strings.Contains(unit, "WantedBy=multi-user.target") {
		t.Fatalf("unit missing [Install] target:\n%s", unit)
	}
	for _, want := range []string{
		"reset_kubelet_state",
		"systemctl is-active --quiet kubelet",
		"rm -f /var/lib/kubelet/cpu_manager_state /var/lib/kubelet/memory_manager_state",
		`case "always" in`,
		"exit 0",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	// The guarded reset must never be a plain rm under a running kubelet.
	if !strings.Contains(script, "systemctl stop kubelet") || !strings.Contains(script, "systemctl start kubelet") {
		t.Fatalf("guarded reset missing the stop/restart sequence:\n%s", script)
	}
}

func TestRenderBootHookReadyCheck(t *testing.T) {
	_, script := renderBootHook("node-7", v1alpha1.HostBootSpec{KubeletStateReset: "readyCheck"})
	for _, want := range []string{`case "readyCheck" in`, "readycheck || reset_kubelet_state", "nodepreps/node-7", "kubelet.conf"} {
		if !strings.Contains(script, want) {
			t.Fatalf("readyCheck script missing %q:\n%s", want, script)
		}
	}
}

func TestRenderBootHookMlnxGate(t *testing.T) {
	_, wait := renderBootHook("n", v1alpha1.HostBootSpec{KubeletStateReset: "off", MlnxInterfaceMgr: "wait"})
	// The wait is two-condition gated: manager script present AND Mellanox
	// hardware enumerated. Without that gate a node without the hardware
	// burns the full timeout at every boot.
	for _, want := range []string{
		"[ -f /usr/bin/mlnx_interface_mgr.sh ]",
		"grep -qs 0x15b3 /sys/bus/pci/devices/*/vendor",
		"netplan apply",
		"-ge 600",
	} {
		if !strings.Contains(wait, want) {
			t.Fatalf("wait hook missing %q:\n%s", want, wait)
		}
	}
	_, ignore := renderBootHook("n", v1alpha1.HostBootSpec{KubeletStateReset: "off", MlnxInterfaceMgr: "ignore"})
	if strings.Contains(ignore, "netplan apply") {
		t.Fatalf("ignore mode must not wait or netplan:\n%s", ignore)
	}
	// The content-hash placeholder must be substituted everywhere.
	if strings.Contains(wait, "@@HASH@@") {
		t.Fatalf("content-hash placeholder left unsubstituted")
	}
}

// Every rendering variant must parse as valid POSIX shell. Found in live
// testing: a stray quote in the curl line made dash reject the script at
// boot, the kubelet state reset silently never ran, and the node
// crashlooped after reboot. sh -n here is the same parser the host uses.
func TestRenderBootHookShellSyntax(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}
	for _, hb := range []v1alpha1.HostBootSpec{
		{KubeletStateReset: "always", MlnxInterfaceMgr: "wait"},
		{KubeletStateReset: "always", MlnxInterfaceMgr: "disable"},
		{KubeletStateReset: "always", MlnxInterfaceMgr: "ignore"},
		{KubeletStateReset: "readyCheck", MlnxInterfaceMgr: "wait"},
		{KubeletStateReset: "readyCheck", MlnxInterfaceMgr: "ignore"},
		{KubeletStateReset: "off", MlnxInterfaceMgr: "wait"},
		{}, // all defaults
	} {
		for _, nodeName := range []string{"bm-a-rtx6k-7", "node-with-dashes", "n"} {
			_, script := renderBootHook(nodeName, hb)
			f := filepath.Join(t.TempDir(), "hook")
			if err := os.WriteFile(f, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := exec.Command("sh", "-n", f).Run(); err != nil {
				t.Fatalf("rendered hook does not parse (reset=%q mlnx=%q node=%q): %v\n%s",
					hb.KubeletStateReset, hb.MlnxInterfaceMgr, nodeName, err, script)
			}
		}
	}
}

func TestBootHookOn(t *testing.T) {
	if !(v1alpha1.HostBootSpec{}).BootHookOn() {
		t.Fatalf("unset bootHook must default to on")
	}
	yes := true
	if !(v1alpha1.HostBootSpec{BootHook: &yes}).BootHookOn() {
		t.Fatalf("explicit true lost")
	}
	no := false
	if (v1alpha1.HostBootSpec{BootHook: &no}).BootHookOn() {
		t.Fatalf("explicit false lost")
	}
}

// stepKubeletState decision gates: off skips, detect-only blocks on stale
// files, and a stale-file reset reports the hook plus what was removed.
func TestStepKubeletStateGates(t *testing.T) {
	np := &v1alpha1.NodePrep{}

	state, msg := stepKubeletState(&Agent{}, np, &v1alpha1.NodePrepProfile{})
	if state != v1alpha1.StepDone || !strings.Contains(msg, "skipped") {
		t.Fatalf("mode off: got %s %q, want Done/skipped", state, msg)
	}

	a := &Agent{hostKubeletDir: t.TempDir()} // detect-only by zero value
	state, msg = stepKubeletState(a, np, &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		HostBoot: v1alpha1.HostBootSpec{KubeletStateReset: "always"},
	}})
	if state != v1alpha1.StepDone || !strings.Contains(msg, "detect-only") {
		t.Fatalf("detect-only, no stale files: got %s %q, want Done", state, msg)
	}

	if err := os.WriteFile(filepath.Join(a.hostKubeletDir, "cpu_manager_state"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	state, msg = stepKubeletState(a, np, &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		HostBoot: v1alpha1.HostBootSpec{KubeletStateReset: "always"},
	}})
	if state != v1alpha1.StepBlocked || !strings.Contains(msg, "-host-mutations") {
		t.Fatalf("detect-only, stale files: got %s %q, want Blocked", state, msg)
	}
}

func TestStaleKubeletStateFiles(t *testing.T) {
	dir := t.TempDir()
	if got := staleKubeletStateFiles(dir); len(got) != 0 {
		t.Fatalf("empty dir: got %v", got)
	}
	for _, f := range []string{"cpu_manager_state", "memory_manager_state"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := staleKubeletStateFiles(dir)
	if len(got) != 2 {
		t.Fatalf("want both stale files, got %v", got)
	}
}

// The ledger accumulates: entries from completed stages survive the stage
// advance, missing current-stage definitions are added once, and definitions
// first seen by a newer agent on a mid-prep node are backfilled as Done
// rather than looming as Pending forever.
func TestEnsureStepsAccumulates(t *testing.T) {
	existing := []v1alpha1.StepStatus{
		{Name: "kubeletState", Stage: v1alpha1.PhaseFinalizing, State: v1alpha1.StepBlocked, Message: "stub"},
		{Name: "sriovNumVFs", Stage: v1alpha1.PhaseFinalizing, State: v1alpha1.StepDone},
	}
	phase := v1alpha1.PhaseFinalizing
	out := ensureSteps(existing, stepsForStage(phase), phase)
	if len(out) != len(stepsForStage(phase)) {
		t.Fatalf("all current-stage defs must be present: %+v", out)
	}
	byName := map[string]v1alpha1.StepStatus{}
	for _, s := range out {
		byName[s.Name] = s
	}
	if byName["kubeletState"].State != v1alpha1.StepBlocked || byName["kubeletState"].Message != "stub" {
		t.Fatalf("existing ledger entry must be preserved: %+v", byName["kubeletState"])
	}

	// Older-stage definitions appear exactly once, marked Done.
	prov := ensureSteps(out, stepsForStage(v1alpha1.PhaseProvisioning), phase)
	byName = map[string]v1alpha1.StepStatus{}
	for _, s := range prov {
		byName[s.Name] = s
	}
	dl, ok := byName["downloads"]
	if !ok {
		t.Fatalf("downloads missing from accumulated ledger: %+v", prov)
	}
	if dl.State != v1alpha1.StepDone || dl.Message == "" {
		t.Fatalf("backfilled step must be Done with an explanation: %+v", dl)
	}

	// Idempotent: a second merge adds nothing.
	again := ensureSteps(prov, stepsForStage(v1alpha1.PhaseProvisioning), phase)
	if len(again) != len(prov) {
		t.Fatalf("ensureSteps must be idempotent: %d -> %d", len(prov), len(again))
	}
}
