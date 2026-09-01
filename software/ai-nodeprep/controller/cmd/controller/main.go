// Command controller is the NodePrep cluster controller (design NP-CTRL-001):
// adoption, taint contract, CAPI pause, worker-role label, flash window.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"spectrocloud.com/nodeprep/internal/controller"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig; empty uses in-cluster config")
	ns := flag.String("namespace", "nodeprep-system", "namespace for events and leases")
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
