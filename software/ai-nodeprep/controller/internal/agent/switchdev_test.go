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

// The 0.1.85→0.1.89 boot-recovery arc, pinned command by command. Live on
// dsx-nwo-267 boots #27-29 the SR-IOV Network Operator's patched
// ExecStartPre hooks turned every ovs-vswitchd start into a crash loop
// (NRestarts=70): each hook opens with a WAITING ovs-vsctl -t 5 write that
// runs before vswitchd exists and dies on its 5s alarm before ExecStart —
// h00, whose unit was never patched, stayed healthy throughout. 0.1.89 is
// therefore layered so a live, converged OVS is never touched: the five
// other_config settings plus the operator's ownership record land on the
// database with --no-wait in both paths (a waiting write blocks until
// vswitchd processes it — measured live in 0.1.86 — and vswitchd may be
// down or mid-churn here); the destructive conf.db reset only runs when
// ovsdb-server is not answering at all (a wedged server would otherwise
// keep serving the removed database's deleted inode); and before any start
// the operator's hooks are defused with a systemd drop-in that resets
// ExecStartPre, so the daemon actually reaches ExecStart. A failed start
// waits out vswitchd's Restart=on-failure recovery (0.1.87), the add-br
// carries --no-wait behind waitBridge's netdev poll (0.1.88), and only a
// vswitchd that never activates is a real failure.
func TestApplyOvsSetupOtherConfigBeforeStart(t *testing.T) {
	oldPoll, oldWait, oldVsw, oldBr := ovsdbPoll, ovsdbWait, vswitchWait, bridgeWait
	ovsdbPoll, ovsdbWait, vswitchWait, bridgeWait = time.Millisecond, 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond
	defer func() { ovsdbPoll, ovsdbWait, vswitchWait, bridgeWait = oldPoll, oldWait, oldVsw, oldBr }()

	rails := []pciDevice{{pci: "0000:05:00.0", devType: "ConnectX7", rail: "r0", netdev: "eth_r0"}}
	profile := &v1alpha1.NodePrepProfile{}

	var wantSets []string
	for _, kv := range [][2]string{
		{"doca-init", "true"}, {"hw-offload", "true"}, {"hw-offload-ct-size", "0"},
		{"max-idle", "300000"}, {"doca-eswitch-max", "1"}, // one rail
	} {
		wantSets = append(wantSets, "ovs-vsctl --no-wait other_config:"+kv[0]+"="+kv[1])
	}
	const wantOwned = `ovs-vsctl --no-wait external_ids:sriov-operator-owned-keys="doca-init hw-offload hw-offload-ct-size max-idle"`

	// run drives applyOvsSetup against a scripted host: every command is
	// recorded in order. db scripts ovsRunning's probe — "up" (a live
	// database answers), "down" (the probe fails but the freshly started
	// ovsdb-server answers waitOvsdb), "silent" (nothing ever answers).
	// startFails fails the openvswitch-switch start; vsActive scripts
	// `systemctl is-active ovs-vswitchd` — "active" (always), "" (never),
	// "recover" (down until the first start attempt, then up — the shape of
	// vswitchd's Restart=on-failure second start). The drop-in path is
	// returned so subtests can assert on the written file.
	run := func(t *testing.T, db string, startFails bool, vsActive string) (calls []string, dropIn string, err error) {
		t.Helper()
		dir := t.TempDir()
		oldRoot := hostToolRoot
		hostToolRoot = dir
		defer func() { hostToolRoot = oldRoot }()

		started := false
		a := &Agent{
			mellanoxFns: rails,
			execFn: func(_ []string, _ time.Duration, name string, _ bool, args []string) (string, error) {
				switch name {
				case "systemctl":
					calls = append(calls, "systemctl "+strings.Join(args, " "))
					switch {
					case args[0] == "start" && args[1] == "openvswitch-switch":
						started = true
						if startFails {
							return "", fmt.Errorf("A dependency job for openvswitch-switch.service failed")
						}
					case args[0] == "is-active" && args[1] == "ovs-vswitchd":
						up := vsActive == "active" || (vsActive == "recover" && started)
						if !up {
							return "", fmt.Errorf("failed")
						}
						return "active\n", nil
					}
					return "", nil
				case "ovs-vsctl":
					switch {
					case args[0] == "get": // ovsRunning's probe
						if db == "up" {
							return "uuid\n", nil
						}
						return "", fmt.Errorf("database unreachable")
					case args[0] == "-t": // waitOvsdb's readiness probe
						if db == "silent" {
							return "", fmt.Errorf("no answer")
						}
						return "uuid\n", nil
					case args[0] == "--no-wait" && args[1] == "set" &&
						(strings.HasPrefix(args[4], "other_config:") || strings.HasPrefix(args[4], "external_ids:")):
						calls = append(calls, "ovs-vsctl --no-wait "+args[4])
						return "", nil
					case args[0] == "--no-wait" && args[1] == "--may-exist":
						calls = append(calls, "ovs-vsctl --no-wait add-br "+args[3])
						return "", nil
					}
					return "", fmt.Errorf("unexpected ovs-vsctl: %v", args)
				case "ip":
					return "", nil
				}
				return "", fmt.Errorf("unexpected exec: %s %v", name, args)
			},
		}
		dropIn = filepath.Join(dir, "etc/systemd/system/ovs-vswitchd.service.d/99-nodeprep-start.conf")
		return calls, dropIn, a.applyOvsSetup(profile, rails)
	}

	// indexOf returns the first index of a recorded call, or -1.
	indexOf := func(calls []string, want string) int {
		for i, c := range calls {
			if c == want {
				return i
			}
		}
		return -1
	}
	// setsBefore counts the five other_config sets recorded before index
	// start, in order, each carrying --no-wait (a waiting ovs-vsctl write
	// blocks until vswitchd processes the change — vswitchd is down in this
	// window, measured live on dsx-nwo-267 in 0.1.85).
	setsBefore := func(t *testing.T, calls []string, start int) int {
		t.Helper()
		n := 0
		for _, c := range calls[:start] {
			if strings.HasPrefix(c, "ovs-vsctl --no-wait other_config:") {
				if n >= len(wantSets) || c != wantSets[n] {
					t.Fatalf("pre-start set %d = %q, want %q", n, c, wantSets[n])
				}
				n++
			}
		}
		return n
	}
	// ownedBefore pins the operator's ownership record before index start.
	ownedBefore := func(t *testing.T, calls []string, start int) {
		t.Helper()
		for _, c := range calls[:start] {
			if c == wantOwned {
				return
			}
		}
		t.Fatalf("the operator ownership record must be set before the start: %v", calls)
	}

	t.Run("a live db is never reset or restarted", func(t *testing.T) {
		// 0.1.89's core invariant, from the h00/h01 comparison: a healthy
		// node's OVS (h00's unit was never patched by the operator and its
		// vswitchd ran fine) must be left entirely alone — no stop, no db
		// reset, no restart, no drop-in. Only the idempotent db writes and
		// the bridge walk run.
		calls, dropIn, err := run(t, "up", false, "active")
		if err != nil {
			t.Fatalf("applyOvsSetup on a converged node: %v", err)
		}
		if _, err := os.Stat(dropIn); !os.IsNotExist(err) {
			t.Fatalf("a live db with vswitchd active must not write the hook drop-in: %v", err)
		}
		for _, c := range calls {
			if strings.HasPrefix(c, "systemctl stop") || c == "systemctl start ovsdb-server" ||
				c == "systemctl start openvswitch-switch" || c == "systemctl daemon-reload" {
				t.Fatalf("a converged OVS must not be touched, got %q", c)
			}
		}
		for _, want := range wantSets {
			if indexOf(calls, want) < 0 {
				t.Fatalf("missing %q: %v", want, calls)
			}
		}
		if indexOf(calls, wantOwned) < 0 {
			t.Fatalf("missing the operator ownership record: %v", calls)
		}
		if indexOf(calls, "ovs-vsctl --no-wait add-br br-rail-r0") < 0 {
			t.Fatalf("the bridge walk must still run: %v", calls)
		}
	})

	t.Run("a live db with vswitchd down gets defused hooks before the start", func(t *testing.T) {
		// The measured h01 shape (boots #27-29): the database is fine, the
		// operator's ExecStartPre hooks crash-loop the start (NRestarts=70,
		// vswitchd never reached ExecStart). The db must be left alone, the
		// hooks defused via drop-in + daemon-reload BEFORE the start, and
		// the settings already on the db for vswitchd's first read.
		calls, dropIn, err := run(t, "up", false, "")
		if err != nil {
			t.Fatalf("applyOvsSetup: %v", err)
		}
		for _, c := range calls {
			if strings.HasPrefix(c, "systemctl stop") || c == "systemctl start ovsdb-server" {
				t.Fatalf("a live db must not be reset, got %q", c)
			}
		}
		data, err := os.ReadFile(dropIn)
		if err != nil {
			t.Fatalf("the hook-defusing drop-in must be written before any start: %v", err)
		}
		if !strings.Contains(string(data), "ExecStartPre=") {
			t.Fatalf("the drop-in must reset ExecStartPre, got %q", data)
		}
		start := indexOf(calls, "systemctl start openvswitch-switch")
		if start < 0 {
			t.Fatalf("openvswitch-switch was never started: %v", calls)
		}
		if reload := indexOf(calls, "systemctl daemon-reload"); reload < 0 || reload > start {
			t.Fatalf("daemon-reload must precede the start: %v", calls)
		}
		if n := setsBefore(t, calls, start); n != len(wantSets) {
			t.Fatalf("want all %d settings pre-start, got %d: %v", len(wantSets), n, calls)
		}
		ownedBefore(t, calls, start)
	})

	t.Run("a dead db is reset and ovsdb-server restarted before the sets", func(t *testing.T) {
		// The bash's reset contract, now reachable only when ovsdb-server is
		// not answering at all: stop the wrapper and the server (a wedged
		// server would keep serving the removed conf.db's deleted inode),
		// remove the database, start the server, and only once waitOvsdb
		// sees it answer do the settings land.
		calls, _, err := run(t, "down", false, "")
		if err != nil {
			t.Fatalf("applyOvsSetup: %v", err)
		}
		stopSw := indexOf(calls, "systemctl stop openvswitch-switch")
		stopDb := indexOf(calls, "systemctl stop ovsdb-server")
		startDb := indexOf(calls, "systemctl start ovsdb-server")
		firstSet := indexOf(calls, wantSets[0])
		if stopSw < 0 || stopDb < stopSw || startDb < stopDb || firstSet < startDb {
			t.Fatalf("reset order must be stop switch < stop ovsdb-server < start ovsdb-server < sets: %v", calls)
		}
		start := indexOf(calls, "systemctl start openvswitch-switch")
		if start < 0 {
			t.Fatalf("openvswitch-switch was never started: %v", calls)
		}
		if n := setsBefore(t, calls, start); n != len(wantSets) {
			t.Fatalf("want all %d settings pre-start, got %d: %v", len(wantSets), n, calls)
		}
		ownedBefore(t, calls, start)
		if indexOf(calls, "ovs-vsctl --no-wait add-br br-rail-r0") < 0 {
			t.Fatalf("the flow must continue past the recovered db: %v", calls)
		}
	})

	t.Run("start failure recovers when vswitchd restarts", func(t *testing.T) {
		// The measured shape (dsx-nwo-267, 0.1.86): the first start fails,
		// vswitchd's Restart=on-failure second start succeeds — the step
		// must ride that recovery to Done, not fail.
		calls, _, err := run(t, "up", true, "recover")
		if err != nil {
			t.Fatalf("a vswitchd auto-restart must recover the failed start, got %v", err)
		}
		if indexOf(calls, "systemctl start openvswitch-switch") < 0 {
			t.Fatalf("the start must have been attempted: %v", calls)
		}
		if indexOf(calls, "ovs-vsctl --no-wait add-br br-rail-r0") < 0 {
			t.Fatalf("the flow must continue past the recovered start: %v", calls)
		}
	})

	t.Run("settings survive an unrecovered start", func(t *testing.T) {
		calls, _, err := run(t, "up", true, "")
		if err == nil || !strings.Contains(err.Error(), "did not recover") {
			t.Fatalf("a start with a vswitchd that never activates must fail the step, got %v", err)
		}
		start := indexOf(calls, "systemctl start openvswitch-switch")
		if start < 0 {
			t.Fatalf("the start must have been attempted: %v", calls)
		}
		if n := setsBefore(t, calls, start); n != len(wantSets) {
			t.Fatalf("all %d settings must be on the db before a failing start, got %d: %v", len(wantSets), n, calls)
		}
	})

	t.Run("a silent ovsdb-server stops the start", func(t *testing.T) {
		calls, _, err := run(t, "silent", false, "active")
		if err == nil || !strings.Contains(err.Error(), "did not answer") {
			t.Fatalf("a never-answering ovsdb-server must fail the wait, got %v", err)
		}
		for _, c := range calls {
			if c == "systemctl start openvswitch-switch" || strings.Contains(c, "add-br") {
				t.Fatalf("nothing may run against an unverified db: %v", calls)
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

// waitBridge polls until vswitchd materializes the bridge's kernel netdev:
// the --no-wait add-br commits instantly, but `ip link set` fails against a
// netdev that does not exist yet, so the poll is what makes the flow
// survive a vswitchd that is up but still initializing (0.1.88).
func TestWaitBridge(t *testing.T) {
	oldPoll := ovsdbPoll
	ovsdbPoll = time.Millisecond
	defer func() { ovsdbPoll = oldPoll }()

	a := &Agent{}
	probes := 0
	a.execFn = func(_ []string, _ time.Duration, name string, _ bool, args []string) (string, error) {
		if name != "ip" || args[0] != "link" || args[1] != "show" || args[2] != "dev" {
			return "", fmt.Errorf("unexpected exec: %s %v", name, args)
		}
		probes++
		if probes < 3 {
			return "", fmt.Errorf("Device %q does not exist", args[3])
		}
		return "", nil
	}
	if err := a.waitBridge("br-rail-r0", 5*time.Second); err != nil {
		t.Fatalf("waitBridge once the netdev appears: %v", err)
	}
	if probes < 3 {
		t.Fatalf("waitBridge must poll until the netdev exists, gave up after %d probes", probes)
	}

	a.execFn = func(_ []string, _ time.Duration, name string, _ bool, args []string) (string, error) {
		if name != "ip" || args[0] != "link" || args[1] != "show" || args[2] != "dev" {
			return "", fmt.Errorf("unexpected exec: %s %v", name, args)
		}
		return "", fmt.Errorf("Device %q does not exist", args[3])
	}
	if err := a.waitBridge("br-rail-r0", 5*time.Millisecond); err == nil || !strings.Contains(err.Error(), "did not appear") {
		t.Fatalf("a netdev that never appears must time out naming the budget, got %v", err)
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
