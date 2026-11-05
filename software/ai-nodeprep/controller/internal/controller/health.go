package controller

import (
	"context"
	"fmt"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// healthCheckTimeout bounds the API probe behind the health endpoints. The
// kubelet's probe timeout is slightly larger so the handler's error response
// (rather than the probe timeout) is what fails the check.
const healthCheckTimeout = 5 * time.Second

// HealthHandler returns the controller's liveness and readiness endpoints.
//
// Both ask the API server — the controller's only dependency — for a
// one-item node list. A controller that has lost its API connection is
// alive but no longer reconciling, so process liveness alone would never
// notice; failing the probe instead lets the kubelet restart the pod, whose
// informers re-sync and recover when the API returns (1.1.0, Kevin: a
// single-control-plane repave left the controller running and silent while
// it stopped adopting nodes).
func HealthHandler(client kubernetes.Interface) http.Handler {
	mux := http.NewServeMux()
	check := func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
		defer cancel()
		if _, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
			http.Error(w, fmt.Sprintf("apiserver unreachable: %v", err), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	}
	mux.HandleFunc("/healthz", check)
	mux.HandleFunc("/readyz", check)
	return mux
}
