// Command agent is the NodePrep node agent (design NP-CTRL-001): it walks
// the NodePrep phase machine on its node, runs steps, tracks boot_id, and
// verifies at boot before Ready. v0.1 is detect-only unless -host-mutations.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"spectrocloud.com/nodeprep/internal/agent"
)

const defaultRebootCommand = "nsenter -t 1 -m -u -i -n -- systemctl reboot"

func main() {
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig; empty uses in-cluster config")
	nodeName := flag.String("node-name", os.Getenv("NODE_NAME"), "Kubernetes node this agent runs on (defaults to NODE_NAME)")
	ns := flag.String("namespace", "nodeprep-system", "namespace for component-scoped resources (leases in v0.2); events for cluster-scoped objects are recorded in default")
	interval := flag.Duration("interval", 5*time.Second, "reconcile poll interval")
	allowReboot := flag.Bool("allow-reboot", false, "permit nodeprep-initiated reboots (design §5.2)")
	hostMutations := flag.Bool("host-mutations", false, "allow steps to mutate the host (v0.2 steps; v0.1 stays detect-only without this)")
	rebootCommand := flag.String("reboot-command", defaultRebootCommand, "command executed for nodeprep-initiated reboots")
	verbose := flag.Bool("verbose", false, "log every host exec in full — quiet sweeps and tool dumps (mlxconfig/flint queries, ACS lspci/setpci traffic) become visible; for troubleshooting")
	flag.Parse()
	// NODEPREP_VERBOSE=true turns verbose on without touching the manifest
	// args: kubectl -n nodeprep-system set env daemonset/nodeprep-agent
	// NODEPREP_VERBOSE=true (and NODEPREP_VERBOSE- to turn it back off).
	if envVerbose(os.Getenv("NODEPREP_VERBOSE")) {
		*verbose = true
	}

	if *nodeName == "" {
		fmt.Fprintln(os.Stderr, "node name required: set NODE_NAME or pass -node-name")
		os.Exit(1)
	}

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

	mode := "detect-only"
	if *hostMutations {
		mode = "host-mutations ENABLED"
	}
	rebootMode := "reboots disabled"
	if *allowReboot {
		rebootMode = "reboots ENABLED"
	}
	logMode := "compact logs"
	if *verbose {
		logMode = "verbose logs"
	}
	fmt.Printf("[nodeprep-agent] mode: %s, %s, %s\n", mode, rebootMode, logMode)

	a := agent.New(client, dyn, *nodeName, *ns, *interval, *allowReboot, *hostMutations, *verbose, *rebootCommand)
	if err := a.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "agent exited: %v\n", err)
		os.Exit(1)
	}
}

func loadConfig(path string) (*rest.Config, error) {
	if path == "" {
		return rest.InClusterConfig()
	}
	return clientcmd.BuildConfigFromFlags("", path)
}

// envVerbose reads the NODEPREP_VERBOSE toggle: true/1/yes enable.
func envVerbose(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes":
		return true
	}
	return false
}
