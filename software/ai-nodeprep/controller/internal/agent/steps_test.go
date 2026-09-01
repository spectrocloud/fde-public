package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
