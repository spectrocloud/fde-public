package agent

import "strings"

// FIPS-kernel gate (1.1.0, Kevin).
//
// In a FIPS environment the node is first provisioned by MAAS on a non-FIPS
// kernel, and a Palette cluster-profile post-install action then enables
// Ubuntu Pro and installs the FIPS kernel. Nodeprep must not race that
// installation: DOCA packages and a FIPS kernel swap touching the same host
// at once is the failure mode this gate exists to prevent.
//
// The profile opts in with spec.selection.waitForFipsKernel. While it is
// true, the agent holds every action until the running kernel is a FIPS
// kernel. The check is the kernel release string: Ubuntu Pro's FIPS
// validated kernels carry "fips" in `uname -r` (e.g. 6.8.0-1015-fips,
// 5.15.0-1070-fips), and the generic kernels never do. The agent sees the
// host's kernel because containers share it — `uname` needs no nsenter.

// fipsKernel reports whether a kernel release string names a FIPS kernel.
func fipsKernel(release string) bool {
	return strings.Contains(strings.ToLower(release), "fips")
}
