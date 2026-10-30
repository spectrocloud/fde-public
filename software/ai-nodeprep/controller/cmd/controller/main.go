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
	"spectrocloud.com/nodeprep/internal/crdupgrade"
)

func main() {
	// The chart runs the binary as a pre-upgrade hook job with this
	// subcommand; everything else is the long-running controller.
	if len(os.Args) > 1 && os.Args[1] == "crd-upgrade" {
		os.Exit(runCRDUpgrade(os.Args[2:]))
	}

	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig; empty uses in-cluster config")
	ns := flag.String("namespace", "nodeprep-system", "namespace for component-scoped resources (leases in v0.2); events for cluster-scoped objects are recorded in default")
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

// runCRDUpgrade applies the CRDs embedded in this binary (the chart's
// pre-upgrade hook). It exits non-zero on the first CRD that cannot be
// applied, so the Job's backoff retries and Helm fails the upgrade instead
// of moving on with a stale schema.
func runCRDUpgrade(args []string) int {
	fs := flag.NewFlagSet("crd-upgrade", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig; empty uses in-cluster config")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfig(*kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[nodeprep] crd-upgrade config: %v\n", err)
		return 1
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[nodeprep] crd-upgrade dynamic client: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Println("[nodeprep] crd-upgrade starting")
	applied, err := crdupgrade.Apply(ctx, dyn)
	for _, name := range applied {
		fmt.Printf("[nodeprep] CRD %s applied\n", name)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[nodeprep] crd-upgrade failed: %v\n", err)
		return 1
	}
	fmt.Println("[nodeprep] crd-upgrade complete")
	return 0
}

func loadConfig(path string) (*rest.Config, error) {
	if path == "" {
		return rest.InClusterConfig()
	}
	return clientcmd.BuildConfigFromFlags("", path)
}
