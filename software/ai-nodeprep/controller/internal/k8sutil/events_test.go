package k8sutil

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientfake "k8s.io/client-go/kubernetes/fake"
)

func TestClampEventMessage(t *testing.T) {
	if got := clampEventMessage("short"); got != "short" {
		t.Fatalf("short messages must pass through, got %q", got)
	}
	got := clampEventMessage(strings.Repeat("x", 5000))
	if len(got) > maxEventMessage {
		t.Fatalf("clamped message exceeds the ceiling: %d bytes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("clamped message must stay valid UTF-8")
	}
	// A rune-heavy message must not be cut mid-rune (é is 2 bytes).
	heavy := strings.Repeat("é", 2000)
	if got := clampEventMessage(heavy); !utf8.ValidString(got) || len(got) > maxEventMessage {
		t.Fatalf("rune-boundary clamp failed: %d bytes", len(got))
	}
}

// The live failure this guards: SriovDownsizePending names all eight rails
// with per-device advice, blew the 1024-byte ceiling, and the API server
// discarded the whole event.
func TestEmitRecordsClampedEvent(t *testing.T) {
	c := clientfake.NewSimpleClientset()
	Emit(context.Background(), c, "NodePrep", "node-1", corev1.EventTypeWarning,
		"SriovDownsizePending", strings.Repeat("device advice; ", 200))
	evs, err := c.CoreV1().Events(metav1.NamespaceDefault).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(evs.Items) != 1 {
		t.Fatalf("exactly one event expected, got %d", len(evs.Items))
	}
	if len(evs.Items[0].Message) > maxEventMessage {
		t.Fatalf("recorded event message too long: %d bytes", len(evs.Items[0].Message))
	}
}
