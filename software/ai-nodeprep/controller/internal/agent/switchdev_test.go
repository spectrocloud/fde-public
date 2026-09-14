package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

// The switchdev wiring (0.1.68): fn_set_vfs's eswitch flip gates, the
// devlink mode parser, and the two OVS steps' skip gates. The OVS exec
// bodies are validated live on DSX Air — the unit environment has no host
// tooling — so these tests pin the gates and the parsing, the parts that
// silently mis-gate.

func TestParseEswitchMode(t *testing.T) {
	cases := []struct {
		out, want string
	}{
		{"pci/0000:05:00.0: mode legacy\n", "legacy"},
		{"pci/0000:05:00.0: mode switchdev\n", "switchdev"},
		// Newer iproute2 appends the other eswitch parameters.
		{"pci/0000:05:00.0: mode legacy inline-mode none encap-mode none\n", "legacy"},
		{"pci/0000:05:00.0: mode switchdev inline-mode none encap-mode basic\n", "switchdev"},
		{"", ""},
		{"pci/0000:05:00.0: (device gone)\n", ""},
	}
	for _, c := range cases {
		if got := parseEswitchMode(c.out); got != c.want {
			t.Fatalf("parseEswitchMode(%q) = %q, want %q", c.out, got, c.want)
		}
	}
}

// The bash gate (fn_set_vfs L625): ConnectX/SuperNIC class + east-west VF
// demand + switchdev mode. The controller's VF demand is rail-mapped, so
// unmapped functions and DPUs never flip here — a DPU's demand comes from
// the north-south side and bash treats that side as legacy.
func TestSwitchdevFlipGate(t *testing.T) {
	profile := &v1alpha1.NodePrepProfile{}
	profile.Spec.EastWest.EswitchMode = "switchdev"
	profile.Spec.EastWest.NumVFs = 1
	rail := pciDevice{pci: "0000:05:00.0", devType: "ConnectX7", rail: "r0"}
	if !switchdevFlip(profile, rail) {
		t.Fatal("a rail-mapped ConnectX-7 with a VF demand under switchdev must flip")
	}
	dpu := pciDevice{pci: "0000:0d:00.0", devType: "ConnectX7", rail: "dpu"}
	if switchdevFlip(profile, dpu) {
		t.Fatal("a DPU's north-south VFs must not flip (bash treats the NS side as legacy)")
	}
	unmapped := pciDevice{pci: "0000:0e:00.0", devType: "ConnectX7"}
	if switchdevFlip(profile, unmapped) {
		t.Fatal("an unmapped function has no VF demand and must not flip")
	}
	legacyProfile := &v1alpha1.NodePrepProfile{}
	legacyProfile.Spec.EastWest.EswitchMode = "legacy"
	legacyProfile.Spec.EastWest.NumVFs = 1
	if switchdevFlip(legacyProfile, rail) {
		t.Fatal("a legacy profile must not flip")
	}
	noVFs := &v1alpha1.NodePrepProfile{}
	noVFs.Spec.EastWest.EswitchMode = "switchdev"
	if switchdevFlip(noVFs, rail) {
		t.Fatal("no VF demand must not flip")
	}
}

func TestOvsValueTrim(t *testing.T) {
	cases := []struct{ in, want string }{
		{"true\n", "true"},
		{"\"300000\"\n", "300000"},
		{"  secure  \n", "secure"},
		{"\"netdev\"\n", "netdev"},
		{"\n", ""},
	}
	for _, c := range cases {
		if got := ovsValue(c.in); got != c.want {
			t.Fatalf("ovsValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The convergence contract: ovsSetupConverged returns "" only when
// converged, and stepOvsSetup must treat a divergence as work to do — never
// Done. Found live on DSX Air (0.1.68): the call sites read the inverted
// contract, so a divergent host ("OVS is not answering ovs-vsctl",
// "other_config:doca-init = no key, want true") reported StepDone and the
// apply path never ran; ovsBridges then failed forever against bridges
// ovsSetup was supposed to create.
func TestOvsSetupDivergenceMustNotReadDone(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "usr", "bin", "ovs-vsctl"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldRoot := hostToolRoot
	hostToolRoot = dir
	defer func() { hostToolRoot = oldRoot }()

	a := &Agent{mellanoxFns: []pciDevice{{pci: "0000:05:00.0", devType: "ConnectX7", rail: "r0", netdev: "eth_r0"}}}
	np := &v1alpha1.NodePrep{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	profile := &v1alpha1.NodePrepProfile{}
	profile.Spec.EastWest.EswitchMode = "switchdev"
	profile.Spec.EastWest.NumVFs = 1

	// A bare agent cannot exec the host: OVS is not answering — divergent.
	// The step must fall through to the mutations gate (Blocked on a bare
	// agent), never return Done with the divergence as its message.
	st, msg := stepOvsSetup(a, np, profile)
	if st == v1alpha1.StepDone {
		t.Fatalf("divergent OVS state must not read Done, got Done %q", msg)
	}
	if st != v1alpha1.StepBlocked || !strings.Contains(msg, "-host-mutations") {
		t.Fatalf("divergent detect with mutations off must Block, got %s %q", st, msg)
	}

	// The same divergence behind the mutations gate: with mutations on the
	// step must attempt the apply (which fails hostExec on a bare agent —
	// Failed, still never Done).
	a.hostMutations = true
	yes := true
	profile.Spec.Policy.HostMutations = &yes
	if st, msg := stepOvsSetup(a, np, profile); st == v1alpha1.StepDone {
		t.Fatalf("a failed apply must not read Done, got Done %q", msg)
	}
}

// The 0.1.85/0.1.86 boot-recovery fix: applyOvsSetup must land the five
// other_config settings on the OVS database BEFORE openvswitch-switch is
// started, and those pre-start sets must carry --no-wait. Live on
// dsx-nwo-267: ovs-vswitchd's first start raced its SR-IOV operator control
// hook against a freshly created conf.db — the hook timed out (signal 14)
// and took the unit down, ovsSetup Failed, and the node reboot-looped
// (0.1.85's fix starts ovsdb-server alone first); then 0.1.85's own pre-start
// sets hung to their 30s timeouts because a waiting ovs-vsctl write blocks
// until ovs-vswitchd processes the change and vswitchd is down in that
// window by construction (measured: the transaction still committed, reads
// stayed instant, fsync 2ms — only the wait was broken). --no-wait is the
// mode ovs-ctl uses for its own startup config. And the start itself is
// expected to fail once (0.1.87): vswitchd's cold start loses the operator
// hook's 5s race and its Restart=on-failure second start succeeds — the
// step waits that recovery out, and only a vswitchd that never activates
// is a real failure. Also pinned: the settings are on the database even
// when the start never recovers (the next boot must find them), and a
// silent ovsdb-server stops the start attempt entirely.
func TestApplyOvsSetupOtherConfigBeforeStart(t *testing.T) {
	oldPoll, oldWait, oldVsw := ovsdbPoll, ovsdbWait, vswitchWait
	ovsdbPoll, ovsdbWait, vswitchWait = time.Millisecond, 5*time.Millisecond, 5*time.Millisecond
	defer func() { ovsdbPoll, ovsdbWait, vswitchWait = oldPoll, oldWait, oldVsw }()

	rails := []pciDevice{{pci: "0000:05:00.0", devType: "ConnectX7", rail: "r0", netdev: "eth_r0"}}
	profile := &v1alpha1.NodePrepProfile{}

	var wantSets []string
	for _, kv := range [][2]string{
		{"doca-init", "true"}, {"hw-offload", "true"}, {"hw-offload-ct-size", "0"},
		{"max-idle", "300000"}, {"doca-eswitch-max", "1"}, // one rail
	} {
		wantSets = append(wantSets, "ovs-vsctl other_config:"+kv[0]+"="+kv[1])
	}

	// run drives applyOvsSetup against a scripted host: every command is
	// recorded in order; the ovs-vsctl poll answers per `answer`,
	// openvswitch-switch starts fail per `startFails`, and
	// `systemctl is-active ovs-vswitchd` reports "active" per `vsActive`
	// ("" — the empty string — means the unit never activates).
	run := func(t *testing.T, startFails bool, vsActive string, answer func() (string, error)) ([]string, error) {
		t.Helper()
		var calls []string
		a := &Agent{
			mellanoxFns: rails,
			execFn: func(_ []string, _ time.Duration, name string, _ bool, args []string) (string, error) {
				switch name {
				case "systemctl":
					calls = append(calls, "systemctl "+strings.Join(args, " "))
					switch {
					case (args[0] == "start" || args[0] == "restart") && args[1] == "openvswitch-switch" && startFails:
						return "", fmt.Errorf("A dependency job for openvswitch-switch.service failed")
					case args[0] == "is-active" && args[1] == "ovs-vswitchd":
						if vsActive == "active" {
							return "active\n", nil
						}
						return "", fmt.Errorf("failed")
					}
					return "", nil
				case "ovs-vsctl":
					switch {
					case args[0] == "-t": // waitOvsdb's readiness probe
						return answer()
					case args[0] == "set" && strings.HasPrefix(args[3], "other_config:"):
						calls = append(calls, "ovs-vsctl "+args[3])
						return "", nil
					case args[0] == "--no-wait" && args[1] == "set" && strings.HasPrefix(args[4], "other_config:"):
						calls = append(calls, "ovs-vsctl --no-wait "+args[4])
						return "", nil
					case args[0] == "--may-exist":
						calls = append(calls, "ovs-vsctl add-br "+args[2])
						return "", nil
					}
					return "", fmt.Errorf("unexpected ovs-vsctl: %v", args)
				case "ip":
					return "", nil
				}
				return "", fmt.Errorf("unexpected exec: %s %v", name, args)
			},
		}
		return calls, a.applyOvsSetup(profile, rails)
	}

	// preStartSets checks the sets recorded before the start attempt: all
	// five, in order, each carrying --no-wait (a waiting ovs-vsctl write
	// blocks until vswitchd processes the change — vswitchd is down in this
	// window, measured live on dsx-nwo-267 in 0.1.85).
	preStartSets := func(t *testing.T, calls []string, start int) int {
		t.Helper()
		n := 0
		for _, c := range calls[:start] {
			if strings.HasPrefix(c, "ovs-vsctl ") && strings.Contains(c, "other_config:") {
				if !strings.Contains(c, "--no-wait") {
					t.Fatalf("pre-start set %q must carry --no-wait (vswitchd is down in this window)", c)
				}
				if c != "ovs-vsctl --no-wait "+strings.TrimPrefix(wantSets[n], "ovs-vsctl ") {
					t.Fatalf("pre-start set %d = %q, want %q", n, c, wantSets[n])
				}
				n++
			}
		}
		return n
	}
	startIndex := func(t *testing.T, calls []string) int {
		t.Helper()
		for i, c := range calls {
			if c == "systemctl start openvswitch-switch" {
				return i
			}
		}
		t.Fatalf("openvswitch-switch was never started: %v", calls)
		return -1
	}

	t.Run("settings land before the start", func(t *testing.T) {
		calls, err := run(t, false, "active", func() (string, error) { return "uuid\n", nil })
		if err != nil {
			t.Fatalf("applyOvsSetup: %v", err)
		}
		firstSet := -1
		for i, c := range calls {
			if strings.HasPrefix(c, "ovs-vsctl ") && strings.Contains(c, "other_config:") {
				firstSet = i
				break
			}
		}
		start := startIndex(t, calls)
		if firstSet < 0 || firstSet > start {
			t.Fatalf("the five other_config settings must precede start openvswitch-switch: %v", calls)
		}
		ovsdbStart := -1
		for i, c := range calls {
			if c == "systemctl start ovsdb-server" {
				ovsdbStart = i
			}
		}
		if ovsdbStart < 0 || ovsdbStart > firstSet {
			t.Fatalf("ovsdb-server must start before the sets: %v", calls)
		}
		if n := preStartSets(t, calls, start); n != len(wantSets) {
			t.Fatalf("want all %d settings pre-start, got %d: %v", len(wantSets), n, calls)
		}
		// Post-start re-assertion runs waiting-mode: vswitchd is up, and the
		// sets double as proof it processed the config.
		for _, c := range calls[start:] {
			if strings.Contains(c, "other_config:") && strings.Contains(c, "--no-wait") {
				t.Fatalf("post-start set %q must not carry --no-wait", c)
			}
		}
	})

	t.Run("start failure recovers when vswitchd restarts", func(t *testing.T) {
		// The measured shape (dsx-nwo-267, 0.1.86): the first start fails at
		// the operator hook's 5s alarm, vswitchd's Restart=on-failure second
		// start succeeds — the step must ride that recovery to Done, not
		// fail.
		calls, err := run(t, true, "active", func() (string, error) { return "uuid\n", nil })
		if err != nil {
			t.Fatalf("a vswitchd auto-restart must recover the failed start, got %v", err)
		}
		addBr := -1
		for i, c := range calls {
			if strings.HasPrefix(c, "ovs-vsctl add-br") {
				addBr = i
			}
		}
		if addBr < 0 {
			t.Fatalf("the flow must continue past the recovered start: %v", calls)
		}
	})

	t.Run("settings survive an unrecovered start", func(t *testing.T) {
		calls, err := run(t, true, "", func() (string, error) { return "uuid\n", nil })
		if err == nil || !strings.Contains(err.Error(), "did not recover") {
			t.Fatalf("a start with a vswitchd that never activates must fail the step, got %v", err)
		}
		start := -1
		for i, c := range calls {
			if c == "systemctl start openvswitch-switch" {
				start = i
			}
		}
		if n := preStartSets(t, calls, start); n != len(wantSets) {
			t.Fatalf("all %d settings must be on the db before a failing start, got %d: %v", len(wantSets), n, calls)
		}
	})

	t.Run("a silent ovsdb-server stops the start", func(t *testing.T) {
		calls, err := run(t, false, "active", func() (string, error) { return "", fmt.Errorf("no answer") })
		if err == nil || !strings.Contains(err.Error(), "did not answer") {
			t.Fatalf("a never-answering ovsdb-server must fail the wait, got %v", err)
		}
		for _, c := range calls {
			if c == "systemctl start openvswitch-switch" {
				t.Fatalf("the wrapper must not be started against an unverified db: %v", calls)
			}
		}
	})
}

// waitOvsdb retries until ovsdb-server answers — the unit reports active as
// soon as systemd forks the daemon, but the socket and a freshly created
// conf.db can lag — and gives up with a bounded error when it never does.
func TestWaitOvsdb(t *testing.T) {
	oldPoll := ovsdbPoll
	ovsdbPoll = time.Millisecond
	defer func() { ovsdbPoll = oldPoll }()

	a := &Agent{}
	answer := ""
	a.execFn = func(_ []string, _ time.Duration, name string, _ bool, args []string) (string, error) {
		if name != "ovs-vsctl" || args[0] != "-t" {
			return "", fmt.Errorf("unexpected exec: %s %v", name, args)
		}
		if answer == "" {
			return "", fmt.Errorf("no answer yet")
		}
		return answer, nil
	}

	answer = "uuid\n"
	if err := a.waitOvsdb(5 * time.Second); err != nil {
		t.Fatalf("waitOvsdb with an answering server: %v", err)
	}

	answer = ""
	if err := a.waitOvsdb(5 * time.Millisecond); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("a silent ovsdb-server must time out naming the budget, got %v", err)
	}
}

// The OVS steps' skip gates: without an inventory (bare Agent) every gate
// below the skip returns before any exec — a lab node without Mellanox
// must walk to Ready with these steps Done, exactly like the bash's
// switchdev blocks which only fire on the rail inventory.
func TestOvsStepsSkipGates(t *testing.T) {
	a := &Agent{}
	np := &v1alpha1.NodePrep{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
	legacy := &v1alpha1.NodePrepProfile{}
	legacy.Spec.EastWest.EswitchMode = "legacy"
	legacy.Spec.EastWest.NumVFs = 1

	if st, msg := stepOvsSetup(a, np, legacy); st != v1alpha1.StepDone || !strings.Contains(msg, "eswitch mode is not switchdev") {
		t.Fatalf("legacy profile must skip ovsSetup, got %s %q", st, msg)
	}
	if st, msg := stepOvsBridges(a, np, legacy); st != v1alpha1.StepDone || !strings.Contains(msg, "eswitch mode is not switchdev") {
		t.Fatalf("legacy profile must skip ovsBridges, got %s %q", st, msg)
	}

	switchdev := &v1alpha1.NodePrepProfile{}
	switchdev.Spec.EastWest.EswitchMode = "switchdev"
	switchdev.Spec.EastWest.NumVFs = 1
	if st, msg := stepOvsSetup(a, np, switchdev); st != v1alpha1.StepDone || !strings.Contains(msg, "no Mellanox hardware") {
		t.Fatalf("switchdev without Mellanox must skip ovsSetup, got %s %q", st, msg)
	}
	if st, msg := stepOvsBridges(a, np, switchdev); st != v1alpha1.StepDone || !strings.Contains(msg, "no Mellanox hardware") {
		t.Fatalf("switchdev without Mellanox must skip ovsBridges, got %s %q", st, msg)
	}

	// The fn_set_vfs any-VF-demand gate: switchdev with no VFs requested
	// skips the OVS setup even on Mellanox hardware.
	switchdev.Spec.EastWest.NumVFs = 0
	inventory := &Agent{mellanoxFns: []pciDevice{{pci: "0000:05:00.0", devType: "ConnectX7", rail: "r0", netdev: "eth_r0"}}}
	if st, msg := stepOvsSetup(inventory, np, switchdev); st != v1alpha1.StepDone || !strings.Contains(msg, "no VFs requested") {
		t.Fatalf("switchdev without VFs must skip ovsSetup (fn_set_vfs gate), got %s %q", st, msg)
	}
	// ovsBridges has no such gate in the bash (fn_add_pfs_to_rail_bridges
	// fires on the mode alone) — with OVS tooling absent it Blocks instead.
	if st, msg := stepOvsBridges(inventory, np, switchdev); st != v1alpha1.StepBlocked || !strings.Contains(msg, "ovs-vsctl not found") {
		t.Fatalf("switchdev ovsBridges without OVS tooling must Block, got %s %q", st, msg)
	}
}

// The rail-key grammar must not flip when VFs appear: a VF enumerates under
// its PF's bus:device (0000:05:00.2 → fn 05:00), and counting it in the
// multi-function check keyed every rail r0_p0 the moment numvfs>0 — the
// OVS/udev steps then re-applied against eth_r0_p0/br-rail-r0_p0, which do
// not exist (found live on DSX Air, 0.1.70: pre-reboot keys r0_p0, post-
// reboot keys r0, both "correct" for their boot's inventory). The bash
// scans PFs only (mst status), so the count is PFs only.
func TestAssignRailsIgnoresVFs(t *testing.T) {
	rails := map[string]string{"05:00": "r0", "06:00": "r1"}
	fns := []pciDevice{
		{pci: "0000:05:00.0", fn: "05:00"},
		{pci: "0000:05:00.2", fn: "05:00", isVF: true},
		{pci: "0000:06:00.0", fn: "06:00"},
		{pci: "0000:06:00.1", fn: "06:00"},
	}
	assignRails(fns, rails)
	want := []string{"r0", "r0", "r1_p0", "r1_p1"}
	for i, w := range want {
		if fns[i].rail != w {
			t.Fatalf("fn %s: rail = %q, want %q", fns[i].pci, fns[i].rail, w)
		}
	}
}
