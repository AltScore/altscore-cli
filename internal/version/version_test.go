package version

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.11.0", "v0.12.0", -1},
		{"v0.12.0", "v0.11.0", 1},
		{"v0.12.0", "v0.12.0", 0},
		{"v1.0.0", "v0.99.99", 1},
		{"v0.2.0", "v0.10.0", -1}, // numeric, not lexical
		{"v1.2.10", "v1.2.9", 1},
		{"0.11.0", "v0.12.0", -1}, // missing "v" prefix still parses
		// Unparseable / non-release inputs return 0 ("don't notify").
		{"dev", "v0.12.0", 0},
		{"v0.12.0", "dev", 0},
		{"v1.2.3-rc1", "v1.2.4", 0},
		{"", "v1.0.0", 0},
		{"v1.2", "v1.2.0", 0},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
