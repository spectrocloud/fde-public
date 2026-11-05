package agent

import (
	"strings"
	"time"
)

// Retry robustness (1.1.0, Kevin): a node failed nodeprep because a step's
// five counted retries were spent while the control plane was still forming
// or DNS was briefly unavailable. Two changes make the walk survive that
// class of failure:
//
//   - the counted budget is 10 attempts, paced with a steeper exponential
//     backoff (three retries now span about a minute, not ten seconds);
//   - failures that read as transient infrastructure — a DNS lookup that
//     cannot resolve, or the Kubernetes API server being unreachable — are
//     paced but never counted, so the step keeps retrying until the
//     infrastructure heals.

// stepBackoff paces a failed step: doubling from twice the poll interval,
// capped at two minutes. With the default 5s interval the waits are 10s,
// 20s, 40s, 80s, then 2m — the first three retries span ~70s, so a flaky
// mirror or a forming control plane is no longer burned through in one poll
// window. n is the consecutive failure count (counted or transient).
func stepBackoff(interval time.Duration, n int) time.Duration {
	wait := interval << min(n, 5)
	if wait > 2*time.Minute {
		wait = 2 * time.Minute
	}
	return wait
}

// transientPatterns are substrings that mark a failure as infrastructure-
// level rather than a real step defect. Matched case-insensitively against
// the step's failure message. Deliberately specific: a bare "timeout" is
// not here, because the agent's own long-running host units (apt, dpkg)
// report timeouts that are not necessarily self-healing.
var transientPatterns = []string{
	// DNS server unavailable — NOT a name that does not exist. "no such
	// host" (Go) and "name or service not known" (glibc, EAI_NONAME) mean
	// the record is absent, which is structural (a mistyped FQDN, a missing
	// zone entry); they are deliberately not transient. EAI_AGAIN and
	// resolver timeouts are, because the infrastructure can heal.
	"server misbehaving",
	"temporary failure in name resolution",
	"could not resolve host",

	// Kubernetes API server / control plane unreachable.
	"connection refused",
	"connection reset by peer",
	"connection timed out",
	"broken pipe",
	"unexpected eof",
	"tls handshake timeout",
	"the server is currently unable to handle the request",
	"service unavailable",
	"etcdserver: request timed out",
	"etcdserver: leader changed",
	"network is unreachable",
	"no route to host",
	"client.timeout exceeded",
	"context deadline exceeded",
	"i/o timeout",
	"request timed out",
	"too many requests",
	"apiserver",
}

// transientRetryError reports whether a step failure looks like transient
// infrastructure. Such a failure is paced and retried without consuming the
// counted budget: the control plane may still be forming, or DNS may be
// briefly unavailable, and both are expected to resolve themselves.
func transientRetryError(message string) bool {
	m := strings.ToLower(message)
	for _, p := range transientPatterns {
		if strings.Contains(m, p) {
			return true
		}
	}
	return false
}
