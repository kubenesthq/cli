package manifest_test

import (
	"testing"

	"kubenest.io/cli/pkg/manifest"
)

func TestSemverPartsReadsWhatThisRepoPins(t *testing.T) {
	for _, c := range []struct {
		in   string
		want [3]int
	}{
		{"2.3.5", [3]int{2, 3, 5}},
		{"2.2.0", [3]int{2, 2, 0}},
		{"v1.35.7+k3s1", [3]int{1, 35, 7}},
		{"  v1.35.6+k3s1  ", [3]int{1, 35, 6}},
		{"12.1.0", [3]int{12, 1, 0}},
	} {
		got, err := manifest.SemverParts(c.in)
		if err != nil {
			t.Errorf("SemverParts(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("SemverParts(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// An unreadable version must be an error, never a default ordering. A gate
// that silently treats a version it cannot parse as new enough fails open,
// which is the shape of defect this repo keeps finding.
func TestSemverPartsRefusesWhatItCannotRead(t *testing.T) {
	for _, in := range []string{"", "2.3", "2.3.5.1", "latest", "2.x.5", "v"} {
		if _, err := manifest.SemverParts(in); err == nil {
			t.Errorf("SemverParts(%q) accepted an unreadable version", in)
		}
	}
}

func TestCompareVersionsOrdersNumericallyNotLexically(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"2.2.0", "2.3.5", -1},
		{"2.3.5", "2.2.0", 1},
		{"2.3.5", "2.3.5", 0},
		{"2.4.0", "2.3.5", 1},
		// Lexically "2.10.0" < "2.9.0"; numerically it is not. This is the
		// case a string comparison gets wrong, and chart minors will reach
		// double digits.
		{"2.10.0", "2.9.0", 1},
		{"v1.35.6+k3s1", "v1.35.7+k3s1", -1},
	} {
		got, err := manifest.CompareVersions(c.a, c.b)
		if err != nil {
			t.Errorf("CompareVersions(%q, %q): %v", c.a, c.b, err)
			continue
		}
		if got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareVersionsPropagatesAnUnreadableVersion(t *testing.T) {
	if _, err := manifest.CompareVersions("latest", "2.3.5"); err == nil {
		t.Error("an unreadable left-hand version was compared rather than refused")
	}
	if _, err := manifest.CompareVersions("2.3.5", "latest"); err == nil {
		t.Error("an unreadable right-hand version was compared rather than refused")
	}
}
