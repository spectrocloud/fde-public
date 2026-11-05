package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// A controller that has lost its API connection must fail its probes: that
// is what makes the kubelet restart it instead of leaving it alive and
// silent (1.1.0, single-control-plane repave).
func TestHealthHandler(t *testing.T) {
	healthy := HealthHandler(k8sfake.NewSimpleClientset())
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		healthy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s with a reachable apiserver: status %d, body %q", path, rec.Code, rec.Body.String())
		}
	}

	lost := k8sfake.NewSimpleClientset()
	lost.PrependReactor("list", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New(`Get "https://10.0.0.1:6443/api": dial tcp 10.0.0.1:6443: connect: connection refused`)
	})
	handler := HealthHandler(lost)
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s with an unreachable apiserver: status %d, want 503", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "apiserver unreachable") {
			t.Fatalf("%s body must explain the failure, got %q", path, rec.Body.String())
		}
	}
}
