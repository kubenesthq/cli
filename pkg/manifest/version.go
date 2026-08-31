package manifest

import (
	"fmt"
	"strconv"
	"strings"
)

// SemverParts turns "v1.35.7+k3s1" into {1, 35, 7}.
//
// It lives here rather than in pkg/upgrade because two packages now need to
// order the versions a bundle pins: the upgrade gates, to refuse a hop of more
// than one bundle, and pkg/component/agent, to refuse rendering a credential
// into a chart too old to have the value (kn-rnyl.2). A second copy is how the
// two would drift apart, and a version comparison that disagrees with itself
// is worse than none.
//
// Build metadata and pre-release suffixes are cut, not ordered: every version
// this repo pins is a plain MAJOR.MINOR.PATCH plus at most a build tag like
// "+k3s1", and pretending to implement pre-release precedence we do not need
// would be a bigger claim than the code makes good on.
func SemverParts(version string) ([3]int, error) {
	var out [3]int
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if i := strings.IndexAny(v, "+-"); i >= 0 {
		v = v[:i]
	}
	fields := strings.Split(v, ".")
	if len(fields) != 3 {
		return out, fmt.Errorf("%q is not a version", version)
	}
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			return out, fmt.Errorf("%q is not a version", version)
		}
		out[i] = n
	}
	return out, nil
}

// CompareVersions returns -1 if a sorts before b, 0 if they are equal, and 1
// if a sorts after b. An unparseable version on either side is an error, never
// a silent ordering — a gate that treats a version it cannot read as "old
// enough" or "new enough" fails open.
func CompareVersions(a, b string) (int, error) {
	pa, err := SemverParts(a)
	if err != nil {
		return 0, err
	}
	pb, err := SemverParts(b)
	if err != nil {
		return 0, err
	}
	for i := range pa {
		switch {
		case pa[i] < pb[i]:
			return -1, nil
		case pa[i] > pb[i]:
			return 1, nil
		}
	}
	return 0, nil
}
