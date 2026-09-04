package agent

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func devicePluginPod(name, node string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sriovDevicePluginNamespace},
		Spec: corev1.PodSpec{NodeName: node}}
}

func listedPods(t *testing.T, fake *clientfake.Clientset) map[string]bool {
	t.Helper()
	items, err := fake.CoreV1().Pods(sriovDevicePluginNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	got := map[string]bool{}
	for i := range items.Items {
		got[items.Items[i].Name] = true
	}
	return got
}

func TestBounceSriovDevicePlugin(t *testing.T) {
	fake := clientfake.NewSimpleClientset(
		devicePluginPod("network-operator-sriov-device-plugin-abc12", "node-1"), // ours: bounce
		devicePluginPod("network-operator-sriov-device-plugin-def34", "node-2"), // sibling node: keep
		devicePluginPod("sriov-network-config-daemon-xyz", "node-1"),            // other daemonset: keep
	)
	a := &Agent{nodeName: "node-1", client: fake}
	a.bounceSriovDevicePlugin(context.Background())

	got := listedPods(t, fake)
	if got["network-operator-sriov-device-plugin-abc12"] {
		t.Errorf("own device-plugin pod not deleted")
	}
	if !got["network-operator-sriov-device-plugin-def34"] {
		t.Errorf("sibling node's device-plugin pod must survive")
	}
	if !got["sriov-network-config-daemon-xyz"] {
		t.Errorf("unrelated pod must survive")
	}
	if a.sriovPluginBouncePending {
		t.Errorf("successful bounce must clear the pending flag")
	}
}

func TestBounceSriovDevicePluginNoPods(t *testing.T) {
	// Cluster where the taint kept the plugin off the node (or no operator
	// at all): an empty list is success — nothing to bounce, flag cleared.
	fake := clientfake.NewSimpleClientset(
		devicePluginPod("network-operator-sriov-device-plugin-abc12", "node-2"),
	)
	a := &Agent{nodeName: "node-1", client: fake, sriovPluginBouncePending: true}
	a.bounceSriovDevicePlugin(context.Background())
	if a.sriovPluginBouncePending {
		t.Errorf("empty match is a completed bounce; pending flag must clear")
	}
}

func TestBounceSriovDevicePluginDeleteFailureRetries(t *testing.T) {
	fake := clientfake.NewSimpleClientset(
		devicePluginPod("network-operator-sriov-device-plugin-abc12", "node-1"),
	)
	calls := 0
	fake.PrependReactor("delete", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return true, nil, fmt.Errorf("simulated transient API error")
		}
		return false, nil, nil
	})
	a := &Agent{nodeName: "node-1", client: fake}

	a.bounceSriovDevicePlugin(context.Background())
	if !a.sriovPluginBouncePending {
		t.Errorf("failed delete must set the pending flag")
	}
	a.bounceSriovDevicePlugin(context.Background()) // the Ready-phase retry
	if got := listedPods(t, fake); got["network-operator-sriov-device-plugin-abc12"] {
		t.Errorf("retry did not delete the pod")
	}
	if a.sriovPluginBouncePending {
		t.Errorf("successful retry must clear the pending flag")
	}
}

func TestBounceSriovDevicePluginListFailurePending(t *testing.T) {
	fake := clientfake.NewSimpleClientset()
	fake.PrependReactor("list", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("simulated API outage")
	})
	a := &Agent{nodeName: "node-1", client: fake}
	a.bounceSriovDevicePlugin(context.Background())
	if !a.sriovPluginBouncePending {
		t.Errorf("failed list must set the pending flag")
	}
}

func TestBounceSriovDevicePluginNoNamespace(t *testing.T) {
	// Cluster without the Network Operator: the list 404s and the bounce is
	// complete (nothing to bounce), not failed.
	fake := clientfake.NewSimpleClientset()
	fake.PrependReactor("list", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(corev1.Resource("pods"), "")
	})
	a := &Agent{nodeName: "node-1", client: fake, sriovPluginBouncePending: true}
	a.bounceSriovDevicePlugin(context.Background())
	if a.sriovPluginBouncePending {
		t.Errorf("missing operator namespace completes the bounce; pending flag must clear")
	}
}
