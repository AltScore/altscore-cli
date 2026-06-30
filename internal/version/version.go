package version

import (
	"strconv"
	"strings"
)

// Version is the CLI version, injected at release time via -ldflags
// (-X .../internal/version.Version=vX.Y.Z). Local builds report "dev".
var Version = "dev"

// Compare orders two semver-ish tags of the form "vMAJOR.MINOR.PATCH".
// It returns -1 if a < b, 0 if equal, +1 if a > b.
//
// If either input is not a clean release tag (e.g. "dev", a pre-release, or
// otherwise unparseable), it returns 0 so callers treat the pair as "do not
// notify" rather than guessing a direction.
func Compare(a, b string) int {
	pa, oka := parseSemver(a)
	pb, okb := parseSemver(b)
	if !oka || !okb {
		return 0
	}
	for i := 0; i < 3; i++ {
		if pa[i] < pb[i] {
			return -1
		}
		if pa[i] > pb[i] {
			return 1
		}
	}
	return 0
}

// parseSemver extracts the three numeric components of a "vX.Y.Z" tag.
// Returns ok=false for anything with a leading "v" stripped that does not
// parse cleanly into exactly three integers (e.g. "dev", "v1.2.3-rc1").
func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
