// Command controller is the NodePrep cluster controller (design NP-CTRL-001):
// adoption, taint contract, CAPI pause, worker-role label, flash window.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"spectrocloud.com/nodeprep/internal/controller"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig; empty uses in-cluster config")
	ns := flag.String("namespace", "nodeprep-system", "namespace for component-scoped resources (leases in v0.2); events for cluster-scoped objects are recorded in default")
	healthAddr := flag.String("health-addr", ":8080", "address for the /healthz and /readyz endpoints; empty disables the health server")
	flag.Parse()

	cfg, err := loadConfig(*kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "client: %v\n", err)
		os.Exit(1)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dynamic client: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The health endpoints probe the API server. If the server cannot bind,
	// or the API stays unreachable past the probe thresholds, the kubelet
	// restarts the pod — a controller that lost its connection otherwise
	// stays alive but stops reconciling (1.1.0).
	if *healthAddr != "" {
		srv := &http.Server{
			Addr:              *healthAddr,
			Handler:           controller.HealthHandler(client),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(os.Stderr, "[nodeprep] health server on %s: %v\n", *healthAddr, err)
				stop()
			}
		}()
		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		}()
	}

	fmt.Println("[nodeprep] controller starting")
	if err := controller.New(client, dyn, *ns).Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "controller exited: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("[nodeprep] controller stopped")
}

func loadConfig(path string) (*rest.Config, error) {
	if path == "" {
		return rest.InClusterConfig()
	}
	return clientcmd.BuildConfigFromFlags("", path)
}
