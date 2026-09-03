package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spectrocloud.com/nodeprep/api/v1alpha1"
)

// downloadFile must create the destination directory (found in live testing:
// /opt/spectrocloud/spcx/bfb does not exist on a fresh node) and verify the
// payload against the configured sha256.
func TestDownloadFile(t *testing.T) {
	payload := []byte("fake BFB payload for the unit test")
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "opt/spectrocloud/spcx/bfb", "test.bfb")
	a := &Agent{} // logf only needs the receiver

	if err := a.downloadFile(srv.URL+"/artifacts", dest, want); err != nil {
		t.Fatalf("download into a missing directory: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("downloaded file missing: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload mismatch")
	}
	if _, err := os.Stat(dest + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp file left behind")
	}

	if err := a.downloadFile(srv.URL+"/artifacts", dest, "deadbeef"); err == nil {
		t.Fatalf("sha256 mismatch not detected")
	}
	if err := a.downloadFile(srv.URL+"/missing", dest, ""); err == nil {
		t.Fatalf("HTTP 404 not detected")
	}
}

// A local copy that hashes to the configured sha256 is reused without any
// network access (the URL here is unreachable on purpose: connection refused
// within the test means the code never went to the wire).
func TestDownloadFileReusesVerifiedLocalCopy(t *testing.T) {
	payload := []byte("already-on-the-host payload")
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])

	dest := filepath.Join(t.TempDir(), "bfb", "test.bfb")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{}

	if err := a.downloadFile("http://127.0.0.1:1/artifacts", dest, want); err != nil {
		t.Fatalf("verified local copy must be reused without downloading: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatalf("reused file was modified")
	}

	// An empty wantSHA cannot establish known-good: re-download, which here
	// fails against the unreachable URL.
	if err := a.downloadFile("http://127.0.0.1:1/artifacts", dest, ""); err == nil {
		t.Fatalf("unverified local copy must not be trusted")
	}
}

// A hash-mismatched local copy (rotated or truncated upstream artifact) is
// replaced by a fresh download, not trusted.
func TestDownloadFileReplacesMismatchedLocalCopy(t *testing.T) {
	stale := []byte("stale local contents")
	fresh := []byte("fresh upstream payload")
	sum := sha256.Sum256(fresh)
	want := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fresh)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bfb", "test.bfb")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, stale, 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agent{}

	if err := a.downloadFile(srv.URL+"/artifacts", dest, want); err != nil {
		t.Fatalf("mismatched local copy must be re-downloaded: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(fresh) {
		t.Fatalf("dest not replaced with fresh payload: %q", got)
	}
	if _, err := os.Stat(dest + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp file left behind")
	}
}

// expandPkg: the one supported placeholder expands from the host kernel
// release; any other shell expression is refused rather than handed to apt
// as literal text.
func TestExpandPkg(t *testing.T) {
	got, err := expandPkg("linux-headers-$(uname -r)", "6.14.0-37-generic")
	if err != nil || got != "linux-headers-6.14.0-37-generic" {
		t.Fatalf("headers placeholder = %q, %v; want linux-headers-6.14.0-37-generic, nil", got, err)
	}
	if got, err := expandPkg("gcc-12", "6.14.0-37-generic"); err != nil || got != "gcc-12" {
		t.Fatalf("plain package must pass through: %q, %v", got, err)
	}
	if _, err := expandPkg("foo-$(whoami)", "x"); err == nil {
		t.Fatal("a non-uname command substitution must be refused")
	}
	if _, err := expandPkg("foo`id`", "x"); err == nil {
		t.Fatal("backticks must be refused")
	}
	if _, err := expandPkg("a-$(b)-$(uname -r)", "k"); err == nil {
		t.Fatal("a mix of supported and unsupported substitutions must be refused")
	}
	if _, err := expandPkg("linux-headers-$uramen-$(uname -r)", "k"); err == nil {
		t.Fatal("the placeholder must match exactly — $uramen is still an unsupported expression")
	}
	if got, err := expandPkg("x-$(uname -r)-y", "k"); err != nil || got != "x-k-y" {
		t.Fatalf("placeholder between other text = %q, %v; want x-k-y, nil", got, err)
	}
}

// stepAptUpgrade gates: policy off skips, detect-only blocks; the simulation
// parser reads apt's plan summary.
func TestStepAptUpgradeGates(t *testing.T) {
	np := &v1alpha1.NodePrep{}
	profile := &v1alpha1.NodePrepProfile{}

	if state, msg := stepAptUpgrade(&Agent{}, np, profile); state != v1alpha1.StepDone || !strings.Contains(msg, "aptUpgrade=false") {
		t.Fatalf("policy off must skip: %s %s", state, msg)
	}
	yes := true
	on := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		Firmware: v1alpha1.FirmwareSource{AptUpgrade: true},
		Policy:   v1alpha1.PolicySpec{HostMutations: &yes},
	}}
	if state, msg := stepAptUpgrade(&Agent{}, np, on); state != v1alpha1.StepBlocked || !strings.Contains(msg, "-host-mutations") {
		t.Fatalf("detect-only must block: %s %s", state, msg)
	}
}

func TestParseUpgradableCount(t *testing.T) {
	out := "Reading package lists...\nBuilding dependency tree...\nCalculating upgrade...\n" +
		"The following packages have been kept back:\n" +
		"5 upgraded, 2 newly installed, 0 to remove and 1 not upgraded.\n"
	if got := parseUpgradableCount(out); got != 5 {
		t.Fatalf("parseUpgradableCount = %d, want 5", got)
	}
	if got := parseUpgradableCount("0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n"); got != 0 {
		t.Fatalf("clean system must read 0, got %d", got)
	}
	if got := parseUpgradableCount(""); got != 0 {
		t.Fatalf("empty output must read 0, got %d", got)
	}
}

// stepNfsRdma gates: policy off skips, detect-only blocks.
func TestStepNfsRdmaGates(t *testing.T) {
	np := &v1alpha1.NodePrep{}
	if state, msg := stepNfsRdma(&Agent{}, np, &v1alpha1.NodePrepProfile{}); state != v1alpha1.StepDone || !strings.Contains(msg, "nfsRdma disabled") {
		t.Fatalf("policy off must skip: %s %s", state, msg)
	}
	yes := true
	on := &v1alpha1.NodePrepProfile{Spec: v1alpha1.NodePrepProfileSpec{
		NFSRDMA: v1alpha1.NFSRDMASpec{Enabled: true},
		Policy:  v1alpha1.PolicySpec{HostMutations: &yes},
	}}
	if state, msg := stepNfsRdma(&Agent{}, np, on); state != v1alpha1.StepBlocked || !strings.Contains(msg, "-host-mutations") {
		t.Fatalf("detect-only must block: %s %s", state, msg)
	}
}
