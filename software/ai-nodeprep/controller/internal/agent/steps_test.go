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

	if err := downloadFile(srv.URL+"/artifacts", dest, want); err != nil {
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

	if err := downloadFile(srv.URL+"/artifacts", dest, "deadbeef"); err == nil {
		t.Fatalf("sha256 mismatch not detected")
	}
	if err := downloadFile(srv.URL+"/missing", dest, ""); err == nil {
		t.Fatalf("HTTP 404 not detected")
	}
}
