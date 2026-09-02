package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

const (
	// bootHookUnitPath and bootHookScriptPath are the two files the agent
	// renders on the host (design §6.2). They replace the bash script's
	// rc.local entry and are the only OS boot integration nodeprep installs.
	bootHookUnitPath   = "/etc/systemd/system/nodeprep-boot.service"
	bootHookScriptPath = "/usr/local/sbin/nodeprep-boot-hook"
	bootHookUnitName   = "nodeprep-boot.service"

	// hostEtcSystemd / hostUsrLocalSbin are the /host-mounted roots of the
	// two paths above inside the agent container.
	hostEtcSystemd   = "/host/etc/systemd/system"
	hostUsrLocalSbin = "/host/usr/local/sbin"
)

// renderBootHook renders the nodeprep-boot.service unit and its script from
// the profile (design §6.2). The script carries the two boot-time repairs
// the bash script did in rc.local and fn_ensure_state:
//
//   - the mlnx_interface_mgr wait, gated on BOTH the manager script being
//     installed and Mellanox hardware being enumerated — the manager's unit
//     only exists when its hardware does, and an unconditional wait could
//     never succeed on a node without it, burning the full timeout at every
//     boot;
//   - the guarded kubelet manager-state reset per hostBoot.kubeletStateReset.
//
// The content-hash comment lets the agent detect out-of-band edits: the file
// on disk is compared against the freshly rendered text, and anything that
// differs is re-rendered away. Every action is best-effort — a hook failure
// must never block boot.
func renderBootHook(nodeName string, hb v1alpha1.HostBootSpec) (unit, script string) {
	unit = `[Unit]
Description=nodeprep pre-kubelet boot hook (managed — do not edit)
Before=kubelet.service

[Service]
Type=oneshot
ExecStart=` + bootHookScriptPath + `

[Install]
WantedBy=multi-user.target
`

	var b strings.Builder
	b.WriteString(`#!/bin/sh
# nodeprep-boot-hook — rendered by the nodeprep agent; managed, do not edit.
# nodeprep: content-hash=@@HASH@@
# Runs before kubelet at every boot (design NP-CTRL-001 §6.2); every action
# is best-effort — a hook failure must never block boot.

log() { logger -t nodeprep-boot "$@"; }

# reset_kubelet_state clears the kubelet CPU/Memory manager state the way
# nodeprep-v105.sh fn_ensure_state does: never delete under a running
# kubelet (it holds manager state in memory and re-persists it, so an
# rm-while-running races that write and the stale file survives) — stop,
# delete, restart. At boot kubelet is not yet up (Before=kubelet.service
# ordered us first), so this reduces to a plain rm.
reset_kubelet_state() {
  if systemctl is-active --quiet kubelet; then
    log "stopping kubelet to clear manager state"
    systemctl stop kubelet
    rm -f /var/lib/kubelet/cpu_manager_state /var/lib/kubelet/memory_manager_state
    systemctl start kubelet
    log "restarted kubelet with cleared manager state"
  else
    rm -f /var/lib/kubelet/cpu_manager_state /var/lib/kubelet/memory_manager_state
    log "cleared kubelet manager state (kubelet was not running)"
  fi
}

# readycheck: skip the reset only when the NodePrep object says Ready.
# Any error — no kubelet.conf, no curl, API unreachable, unexpected output —
# reads as "reset", the bash script's fail-safe.
readycheck() {
  kc=/etc/kubernetes/kubelet.conf
  [ -f "$kc" ] || return 1
  command -v curl >/dev/null 2>&1 || return 1
  server=$(awk '/^[[:space:]]*server:/{print $2; exit}' "$kc")
  [ -n "$server" ] || return 1
  ca=$(awk '/certificate-authority:/{print $2; exit}' "$kc")
  crt=$(awk '/client-certificate:/{print $2; exit}' "$kc")
  key=$(awk '/client-key:/{print $2; exit}' "$kc")
  out=$(curl -sS --max-time 5 --cacert "$ca" --cert "$crt" --key "$key" \
    "$server/apis/nodeprep.spectrocloud.com/v1alpha1/nodepreps/` + nodeName + `" 2>/dev/null) || return 1
  printf '%s' "$out" | grep -q '"phase":"Ready"'
}
`)
	switch hb.MlnxInterfaceMgr {
	case "wait", "":
		// The manager only exists when its hardware does — wait only when
		// BOTH hold, else the wait can never succeed and every boot pays
		// the full timeout (design §6.2; the bash script's
		// [ -f mlnx_interface_mgr.sh ] && lspci -d 15b3 check, sysfs-native).
		b.WriteString(`
if [ -f /usr/bin/mlnx_interface_mgr.sh ] && grep -qs 0x15b3 /sys/bus/pci/devices/*/vendor; then
  n=0
  until systemctl status system-mlnx_interface_mgr.slice >/dev/null 2>&1; do
    n=$((n+5))
    [ "$n" -ge 600 ] && break
    sleep 5
  done
  if systemctl status system-mlnx_interface_mgr.slice >/dev/null 2>&1; then
    log "mlnx interface manager initialized"
  else
    log "WARNING mlnx interface manager not up after 600s, continuing"
  fi
  if [ -x /usr/sbin/netplan ]; then
    log "re-running netplan apply to restore VLAN subinterfaces"
    netplan apply
  fi
fi
`)
	case "disable":
		// The agent masks the unit outright (ensureBootHook); the hook has
		// nothing to wait for.
	}
	b.WriteString(`
case "` + hb.KubeletStateReset + `" in
  always) reset_kubelet_state ;;
  readyCheck) readycheck || reset_kubelet_state ;;
esac

exit 0
`)
	script = b.String()
	h := sha256.Sum256([]byte(unit + script))
	return unit, strings.Replace(script, "@@HASH@@", hex.EncodeToString(h[:8]), 1)
}

// ensureBootHook renders and installs the boot hook when the profile asks
// for it. Requires host mutations; stepKubeletState gates on that before
// calling. Existing files are compared byte-for-byte: identical content
// means "unchanged" (and only re-checks the enablement), any difference —
// profile change or out-of-band edit — is re-rendered away.
func (a *Agent) ensureBootHook(profile *v1alpha1.NodePrepProfile) (string, error) {
	hb := profile.Spec.HostBoot
	if !hb.BootHookOn() {
		return "boot hook disabled (hostBoot.bootHook=false)", nil
	}
	env := os.Environ()

	unit, script := renderBootHook(a.nodeName, hb)
	unitPath := filepath.Join(hostEtcSystemd, filepath.Base(bootHookUnitPath))
	scriptPath := filepath.Join(hostUsrLocalSbin, filepath.Base(bootHookScriptPath))
	oldUnit, errU := os.ReadFile(unitPath)
	oldScript, errS := os.ReadFile(scriptPath)
	unchanged := errU == nil && errS == nil && string(oldUnit) == unit && string(oldScript) == script
	// Steady state: files current AND already checked this process lifetime —
	// no execs. The corrective systemctl work below runs once per process
	// (and again whenever the rendered content changes), not every cycle.
	if unchanged && a.hookDone == unit+"\x00"+script {
		return "", nil
	}

	// mlnx_interface_mgr handling (design §6.2): wait is the hook's
	// hardware-gated wait; disable masks the manager outright; ignore and
	// "" leave the host alone.
	maskMsg := ""
	switch hb.MlnxInterfaceMgr {
	case "disable":
		if _, err := a.hostExec(env, 30*time.Second, "systemctl", "mask", "mlnx_interface_mgr.service"); err != nil {
			return "", fmt.Errorf("masking mlnx_interface_mgr: %v", err)
		}
		maskMsg = "; mlnx_interface_mgr.service masked"
	default:
		_, _ = a.hostExec(env, 30*time.Second, "systemctl", "unmask", "mlnx_interface_mgr.service")
	}

	if unchanged {
		a.hookDone = unit + "\x00" + script
		if _, err := a.hostExec(env, 30*time.Second, "systemctl", "is-enabled", "--quiet", bootHookUnitName); err != nil {
			if _, err := a.hostExec(env, 30*time.Second, "systemctl", "enable", bootHookUnitName); err != nil {
				return "", fmt.Errorf("enabling %s: %v", bootHookUnitName, err)
			}
			return "boot hook installed (unchanged); enabled" + maskMsg, nil
		}
		return "boot hook installed (unchanged)" + maskMsg, nil
	}
	for _, d := range []string{hostEtcSystemd, hostUsrLocalSbin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", fmt.Errorf("mkdir %s: %v", d, err)
		}
	}
	// Syntax gate before anything is installed (found in live testing: a
	// stray quote in the template made dash reject the script at boot, the
	// kubelet state reset silently never ran, and the node crashlooped after
	// reboot). The candidate is validated by the HOST's /bin/sh — the same
	// shell that will execute it — via nsenter, and only then moved into
	// place; an enabled unit can never point at an unvalidated script.
	candDir := "/host/tmp"
	candPath := filepath.Join(candDir, "nodeprep-boot-hook.candidate")
	if err := os.MkdirAll(candDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %v", candDir, err)
	}
	if err := os.WriteFile(candPath, []byte(script), 0o755); err != nil {
		return "", fmt.Errorf("writing candidate: %v", err)
	}
	if _, err := a.hostExec(env, 30*time.Second, "sh", "-n", "/tmp/nodeprep-boot-hook.candidate"); err != nil {
		_ = os.Remove(candPath)
		return "", fmt.Errorf("rendered boot hook fails sh -n (refusing to install): %v", err)
	}
	if err := os.Rename(candPath, scriptPath); err != nil {
		return "", fmt.Errorf("installing %s: %v", bootHookScriptPath, err)
	}
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %v", bootHookUnitPath, err)
	}
	a.logf("boot hook rendered to %s and %s (sh -n verified)", bootHookScriptPath, bootHookUnitPath)
	if _, err := a.hostExec(env, 30*time.Second, "systemctl", "daemon-reload"); err != nil {
		return "", fmt.Errorf("daemon-reload: %v", err)
	}
	if _, err := a.hostExec(env, 30*time.Second, "systemctl", "enable", bootHookUnitName); err != nil {
		return "", fmt.Errorf("enabling %s: %v", bootHookUnitName, err)
	}
	a.hookDone = unit + "\x00" + script
	return "boot hook rendered (" + bootHookUnitPath + ", Before=kubelet.service)" + maskMsg, nil
}

// staleKubeletStateFiles lists the kubelet manager state files that exist on
// the host — exactly the two fn_ensure_state deletes.
func staleKubeletStateFiles(dir string) []string {
	var stale []string
	for _, f := range []string{"cpu_manager_state", "memory_manager_state"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			stale = append(stale, f)
		}
	}
	return stale
}
