package agent

import "testing"

// Ubuntu Pro FIPS kernels carry "fips" in the release string; generic
// kernels never do. The gate keys off exactly that — a substring match is
// deliberate, since the prefix differs across supported series
// (5.15.0-1070-fips, 6.8.0-1015-fips, 6.17.0-29-generic).
func TestFipsKernel(t *testing.T) {
	cases := []struct {
		release string
		want    bool
	}{
		{"6.17.0-29-generic", false},
		{"6.8.0-1015-fips", true},
		{"5.15.0-1070-fips", true},
		{"6.8.0-1015-fips-<version>", true},
		{"6.8.0-1015-FIPS", true},
		{"", false},
		{"fips", true},
	}
	for _, c := range cases {
		if got := fipsKernel(c.release); got != c.want {
			t.Errorf("fipsKernel(%q) = %v, want %v", c.release, got, c.want)
		}
	}
}
