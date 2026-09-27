package recovery

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"kubenest.io/cli/pkg/controlplane"
)

// ControlPlaneCheckpoint is the checkpoint-specific part of a recovery set: the
// control plane's own version, and the Postgres the dump came out of.
//
// It is the shape `controlplane.EligibleCheckpoint` publishes, kept as an
// interface here so the eligibility rules can be exercised without a cluster.
type ControlPlaneCheckpoint interface {
	CheckpointVersion() string
	CheckpointPostgresMajor() int
	CheckpointPostgresImage() string
	CheckpointUploadComplete() bool
}

// bareCheckpoint adapts the on-cluster checkpoint document to this package's
// view of it.
type bareCheckpoint struct {
	version string
	major   int
	image   string
}

func (c bareCheckpoint) CheckpointVersion() string       { return c.version }
func (c bareCheckpoint) CheckpointPostgresMajor() int    { return c.major }
func (c bareCheckpoint) CheckpointPostgresImage() string { return c.image }

// CheckpointUploadComplete reports whether the checkpoint's upload finished.
// It is part of the view above rather than a field, because a checkpoint whose
// upload never completed is never a candidate and must not be representable as
// one.
func (c bareCheckpoint) CheckpointUploadComplete() bool { return c.version != "" }

// FromEligible adapts the control plane's own checkpoint document.
func FromEligible(cp *controlplane.EligibleCheckpoint) ControlPlaneCheckpoint {
	if cp == nil {
		return nil
	}
	return bareCheckpoint{version: cp.ControlPlaneVersion, major: cp.PostgresMajor, image: cp.PostgresImage}
}

// ControlPlaneTarget is what this CLI is about to install: the control plane
// version its chart carries, and the Postgres major that chart pins.
type ControlPlaneTarget struct {
	// Version is the chart's control-plane version.
	Version string
	// PostgresMajor is the chart's pinned Postgres major.
	PostgresMajor int
	// PostgresImage is the chart's pinned Postgres image, named in a refusal so
	// the operator knows which image they would have to be running.
	PostgresImage string
}

// CheckCheckpoint refuses a checkpoint this control plane cannot be restored
// from, naming what is needed.
//
// THE COMPARISON IS AGAINST THE CHART, NOT AGAINST THE HOST. A checkpoint
// records the control plane version that produced it and the Postgres it was
// dumped from. Restoring a checkpoint from a NEWER control plane than the chart
// being installed means serving code older than the schema in front of it; a
// different Postgres major or distribution means `pg_restore` is loading into a
// different database than the one the dump came from, which this release does
// not support on any path (PLAN 7.8, "Postgres stays what it is").
func CheckCheckpoint(cp ControlPlaneCheckpoint, want ControlPlaneTarget) error {
	if cp == nil {
		return errors.New("there is no eligible control-plane checkpoint to restore: the management cluster's checkpoint CronJob has none that completed its upload, and an upload that did not complete is never something to restore from")
	}
	if !cp.CheckpointUploadComplete() {
		return errors.New("this checkpoint's upload did not complete, so it is not offered as latest: a partially uploaded checkpoint is a dump the recovery cannot read end to end")
	}
	got := strings.TrimSpace(cp.CheckpointVersion())
	if got == "" {
		return errors.New("this checkpoint records no control-plane version, so there is no way to tell whether the code that will serve it is older or newer than the schema inside it. Restore is refused rather than guessed")
	}
	if want.Version == "" {
		return errors.New("this CLI cannot say which control-plane version its chart installs, so it cannot judge the checkpoint it holds. This is a defect in the CLI build, not in the checkpoint")
	}
	switch compareVersions(got, want.Version) {
	case 1:
		return fmt.Errorf("the newest eligible checkpoint was taken by control plane %s and this CLI installs %s: a checkpoint from a NEWER control plane carries a schema this build does not serve. Install a CLI of at least %s, or restore an older eligible checkpoint with --backup. Nothing was changed",
			got, want.Version, got)
	}
	if want.PostgresMajor == 0 {
		return errors.New("this CLI cannot say which Postgres major its chart pins, so it cannot judge the checkpoint it holds")
	}
	if gotMajor := cp.CheckpointPostgresMajor(); gotMajor != want.PostgresMajor {
		return fmt.Errorf("the newest eligible checkpoint was dumped from PostgreSQL %d (%s) and this chart runs PostgreSQL %d (%s): this release restores same-major only, because a cross-major load is a migration with its own tested procedure and not a recovery. Restore an eligible checkpoint from PostgreSQL %d, or move the control plane with a supported migration. Nothing was changed",
			gotMajor, cp.CheckpointPostgresImage(), want.PostgresMajor, want.PostgresImage, want.PostgresMajor)
	}
	return nil
}

// compareVersions compares two dotted numeric versions, tolerating a suffix
// after a dash or a plus. It returns -1, 0 or 1. A component that is not a
// number compares as zero, so "1.2.0-rc1" is 1.2.0 rather than an error: a
// candidate version string must not make a recovery impossible.
func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out []int
	for _, part := range strings.Split(v, ".") {
		n := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int(r-'0')
		}
		out = append(out, n)
	}
	return out
}

// Fact is one item of desired state a side reports, reduced to a comparable
// value: an inventory revision, a bundle version, a window.
type Fact struct {
	// Value is what that side holds. An empty Value means "not recorded",
	// which is a fact in its own right and is never treated as a match.
	Value string
}

// DesiredState is the desired state one side reports, keyed by cluster.
//
// The checkpoint side comes from the restored control plane; the live side is
// what surviving clusters report through their own agents after the recovery.
type DesiredState struct {
	Inventories map[string]Fact
	Bundles     map[string]Fact
	Windows     map[string]Fact
}

// Difference is one disagreement, named so the operator can decide rather than
// discover.
type Difference struct {
	Cluster string
	What    string
	// Checkpoint is what the restored control plane holds.
	Checkpoint string
	// Live is what the surviving cluster reports now. It is the NEWER truth
	// whenever the cluster moved on after the checkpoint was taken.
	Live string
}

// Report renders the differences as the block an operator reads, in a stable
// order.
func Report(w io.Writer, diffs []Difference) {
	if len(diffs) == 0 {
		fmt.Fprint(w, "  the restored desired state agrees with what the surviving clusters report; nothing needs a decision\n")
		return
	}
	fmt.Fprintf(w, "  %d difference(s) between the restored desired state and what surviving clusters report:\n", len(diffs))
	for _, d := range diffs {
		fmt.Fprintf(w, "    cluster %s %s: the checkpoint holds %q, the cluster reports %q\n", d.Cluster, d.What, d.Checkpoint, d.Live)
	}
}

// CompareProvisional returns the differences between the desired state the
// restored control plane holds and what surviving clusters report, WITHOUT
// choosing a winner.
//
// THIS IS THE WHOLE POINT OF STEP 4, AND IT IS DELIBERATELY NOT A RECONCILE.
// A checkpoint is a moment; a cluster that kept running after it has moved on,
// and pushing the checkpoint's values back at it would revert work an operator
// did after the checkpoint was taken — the recovery would damage the fleet it
// was recovering. So the differences are shown, the newer side is named, and
// the decision is the operator's. Nothing here writes anything.
func CompareProvisional(checkpoint, live DesiredState) []Difference {
	var out []Difference
	compare := func(what string, from, to map[string]Fact) {
		for _, cluster := range union(from, to) {
			got, hasGot := from[cluster]
			want, hasWant := to[cluster]
			switch {
			case !hasWant:
				out = append(out, Difference{Cluster: cluster, What: what, Checkpoint: got.Value, Live: "not reported by the cluster"})
			case !hasGot:
				out = append(out, Difference{Cluster: cluster, What: what, Checkpoint: "not in the checkpoint", Live: want.Value})
			case got.Value != want.Value:
				out = append(out, Difference{Cluster: cluster, What: what, Checkpoint: got.Value, Live: want.Value})
			}
		}
	}
	compare("inventory", checkpoint.Inventories, live.Inventories)
	compare("bundle", checkpoint.Bundles, live.Bundles)
	compare("window", checkpoint.Windows, live.Windows)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Cluster != out[j].Cluster {
			return out[i].Cluster < out[j].Cluster
		}
		return out[i].What < out[j].What
	})
	return out
}

func union(a, b map[string]Fact) []string {
	seen := map[string]struct{}{}
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NewControlPlaneCheckpoint builds the checkpoint view from the fields a caller
// holds directly, so the eligibility rules can be exercised without a cluster.
func NewControlPlaneCheckpoint(version string, postgresMajor int, postgresImage string, uploadComplete bool) ControlPlaneCheckpoint {
	cp := bareCheckpoint{version: version, major: postgresMajor, image: postgresImage}
	if !uploadComplete {
		return incompleteCheckpoint{cp}
	}
	return cp
}

// incompleteCheckpoint is a checkpoint whose upload never finished. It answers
// the upload question with no and everything else from the partial document,
// because the refusal it produces must name which of the two facts is wrong.
type incompleteCheckpoint struct{ bareCheckpoint }

func (c incompleteCheckpoint) CheckpointUploadComplete() bool { return false }
