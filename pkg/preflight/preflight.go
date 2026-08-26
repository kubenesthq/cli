// Package preflight is stage 1: every check in install.mdx's table, against
// every node, before the first byte is written anywhere.
//
// Preflight is the whole reason a failed install is cheap. It writes nothing
// to any machine and creates nothing in the control plane, so abandoning an
// install at this point costs exactly nothing — which is what makes being
// thorough here worth it, and why every check runs even after one has failed.
// An operator who fixes one condition, re-runs, and hits the next one has
// been failed by the installer, not by their infrastructure.
//
// Thresholds and the tested OS matrix come from the bundle manifest, never
// from constants here, and every size is compared in BINARY units because
// that is what /proc/meminfo and statfs report. A floor written "4 GB" and
// compared against MemTotal-as-GiB refuses a machine that meets the
// specification, and the floor fails rather than warns (kn-bkwa).
package preflight

import (
	"context"
	"fmt"
	"strings"

	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

// Check names, exactly as install.mdx's table names them. A check that fails
// must be findable on the page the customer was told to read.
const (
	CheckControlPlane = "Control plane"
	CheckSSH          = "SSH reachability"
	CheckOS           = "Operating system"
	CheckPrivilege    = "Privilege"
	CheckExistingK8s  = "Existing Kubernetes"
	CheckVolumeGroup  = "Volume group"
	CheckPorts        = "Node-to-node ports"
	CheckEgress       = "Outbound egress"
	CheckResources    = "Host resources"
	CheckNodeCount    = "Node count"
	CheckBundle       = "Bundle availability"
)

// Outcome is one check's verdict. Warn exists for exactly one reason: the
// recommended sizing is a provisional number, and a provisional number must
// never be able to refuse a correctly-specified customer host.
type Outcome string

const (
	Pass Outcome = "pass"
	Warn Outcome = "warn"
	Fail Outcome = "fail"
)

// Result is one check against one node (or the whole request).
type Result struct {
	Check string
	// Node is the address checked, or empty for request-wide checks.
	Node    string
	Outcome Outcome
	// Detail is what was observed — the measured value, the found binary,
	// the refused device.
	Detail string
	// Fix is what to do about it. A check that fails without one is a check
	// that has told the operator they have a problem and nothing more.
	Fix string
}

func (r Result) String() string {
	var b strings.Builder
	b.WriteString(r.Check)
	if r.Node != "" {
		b.WriteString(" on " + r.Node)
	}
	b.WriteString(": " + r.Detail)
	if r.Fix != "" {
		b.WriteString("\n      fix: " + r.Fix)
	}
	return b.String()
}

// Report is every check that ran.
type Report struct {
	Results []Result
}

func (rep *Report) add(r Result) { rep.Results = append(rep.Results, r) }

// Failures returns the checks that refused the install.
func (rep Report) Failures() []Result { return rep.filter(Fail) }

// Warnings returns the checks that passed but are worth saying out loud.
func (rep Report) Warnings() []Result { return rep.filter(Warn) }

func (rep Report) filter(o Outcome) []Result {
	var out []Result
	for _, r := range rep.Results {
		if r.Outcome == o {
			out = append(out, r)
		}
	}
	return out
}

// Err is the aggregate refusal: EVERY failing check, so one re-run fixes
// everything rather than uncovering the next problem.
func (rep Report) Err() error {
	failures := rep.Failures()
	if len(failures) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "preflight refused the install (%d of %d checks failed):\n", len(failures), len(rep.Results))
	for _, f := range failures {
		fmt.Fprintf(&b, "  [fail] %s\n", f)
	}
	return fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
}

// Node is one target host to check.
type Node struct {
	Address string
	// Role decides the node-count arithmetic and which ports must be open.
	Role string
	// Runner is the open connection. Dialing it IS the SSH reachability
	// check, so a nil Runner means that check already failed.
	Runner k3s.Runner
	// DialErr is why the connection could not be made, if it could not.
	DialErr error
	// ExistingK3sIsOurs marks a node this journal already installed k3s on.
	// Without it, resume — which always re-runs preflight — would be refused
	// by the very check that forbids adopting an existing cluster.
	ExistingK3sIsOurs bool
	// StorageIsOurs marks a node whose kubenest-vg this install created, for
	// the same reason: after stage 7, "--storage-device must be blank" would
	// otherwise refuse the installer's own work on every resume.
	StorageIsOurs bool
}

// EgressTarget is one URL the install needs to reach from the nodes. The list
// is assembled from the component installers' own chart repositories rather
// than duplicated here, so a bundle that moves a repo cannot leave preflight
// checking the old one.
type EgressTarget struct {
	Name string
	URL  string
}

// Catalog is the control plane, as far as preflight needs it: the call that
// proves reachable-and-authenticated and the catalog that proves the bundle
// exists. *api.Client satisfies it.
type Catalog interface {
	ListBundles(ctx context.Context) ([]BundleEntry, error)
}

// BundleEntry is one offered bundle version.
type BundleEntry struct {
	Version  string
	HATiers  []string
	Profiles []string
}

// Options is what preflight checks the nodes against.
type Options struct {
	Bundle *manifest.Manifest
	// BundleVersion is what the operator asked for on the command line; it
	// is checked against the catalog, not assumed from the manifest.
	BundleVersion string
	HATier        string
	Profiles      []string
	// StorageDevice is --storage-device: empty means the customer created
	// kubenest-vg themselves, which is the default path.
	StorageDevice string
	Nodes         []Node
	Egress        []EgressTarget
	// Catalog is where the offered bundles are read from: the control plane
	// in a registered install, the versions built into this binary in a
	// standalone one. Preflight does not care which — the request is checked
	// against the offer either way.
	Catalog Catalog
	// Standalone is an install with no control plane at all (kn-l827,
	// decision 2026-08-26: nothing is hosted by us). It changes what the
	// control-plane check REPORTS, not whether the bundle is checked: an
	// unregistered cluster still may not be handed a tier or a profile its
	// bundle does not offer.
	Standalone bool
}

// Run executes every check against every node and returns the full report.
//
// It returns a report even when checks fail; the error is the aggregate
// refusal. A caller that wants to print warnings on a successful preflight
// needs both.
func Run(ctx context.Context, opts Options) (Report, error) {
	var rep Report

	checkControlPlaneAndBundle(ctx, opts, &rep)
	checkNodeCount(opts, &rep)

	for _, node := range opts.Nodes {
		checkNode(ctx, opts, node, &rep)
	}
	checkPorts(ctx, opts, &rep)

	return rep, rep.Err()
}

// checkControlPlaneAndBundle covers two of the eleven: where this cluster will
// register — a reachable, logged-in control plane, or nothing at all under
// --standalone — and whether the requested bundle exists and offers the
// requested tier and profiles.
//
// The second check is the same in both modes. Only the first differs, and it
// differs in what it reports rather than in whether it runs: a check that
// silently disappeared in one mode would make the difference between the two
// invisible in the report an operator reads.
func checkControlPlaneAndBundle(ctx context.Context, opts Options, rep *Report) {
	bundles, ok := offeredBundles(ctx, opts, rep)
	if !ok {
		return
	}

	var found *BundleEntry
	var offered []string
	for i := range bundles {
		offered = append(offered, bundles[i].Version)
		if bundles[i].Version == opts.BundleVersion {
			found = &bundles[i]
		}
	}
	if found == nil {
		detail := fmt.Sprintf("this control plane does not offer bundle %q", opts.BundleVersion)
		fix := "it offers " + orNone(offered)
		if opts.Standalone {
			detail = fmt.Sprintf("this CLI does not carry bundle %q", opts.BundleVersion)
			fix = "it carries " + orNone(offered) + "; upgrade the CLI to a release that ships the bundle you want"
		}
		rep.add(Result{Check: CheckBundle, Outcome: Fail, Detail: detail, Fix: fix})
		return
	}
	if !contains(found.HATiers, opts.HATier) {
		rep.add(Result{
			Check: CheckBundle, Outcome: Fail,
			Detail: fmt.Sprintf("bundle %s does not offer the %q tier", found.Version, opts.HATier),
			Fix:    "it offers " + orNone(found.HATiers),
		})
		return
	}
	for _, p := range opts.Profiles {
		if !contains(found.Profiles, p) {
			rep.add(Result{
				Check: CheckBundle, Outcome: Fail,
				Detail: fmt.Sprintf("bundle %s does not offer a profile named %q", found.Version, p),
				Fix:    "it offers " + orNone(found.Profiles),
			})
			return
		}
	}
	rep.add(Result{
		Check: CheckBundle, Outcome: Pass,
		Detail: fmt.Sprintf("bundle %s offers the %s tier and every requested profile", found.Version, opts.HATier),
	})
}

// offeredBundles resolves the catalog the request is checked against, and
// reports the control-plane check while it is there.
//
// The two modes differ in what "Control plane" MEANS, not in whether the
// bundle is checked. Registered: it must be reachable and this CLI logged in.
// Standalone: there is nothing to reach, and saying "pass" would be a check
// reporting on something that does not exist — so it reports what is actually
// true, which is that this cluster will register with nothing.
func offeredBundles(ctx context.Context, opts Options, rep *Report) ([]BundleEntry, bool) {
	if opts.Catalog == nil {
		if opts.Standalone {
			// Not a login problem: the binary is supposed to carry its own
			// catalog, so this is a build that shipped without one.
			rep.add(Result{
				Check: CheckBundle, Outcome: Fail,
				Detail: "this CLI carries no bundle catalog, so the requested bundle cannot be checked",
				Fix:    "reinstall the CLI from a published release",
			})
			return nil, false
		}
		rep.add(Result{
			Check: CheckControlPlane, Outcome: Fail,
			Detail: "no control plane configured",
			Fix:    "run `kubenest login` first, or install without one: `kubenest platform install --standalone`",
		})
		return nil, false
	}

	bundles, err := opts.Catalog.ListBundles(ctx)
	if err != nil {
		if opts.Standalone {
			rep.add(Result{
				Check: CheckBundle, Outcome: Fail,
				Detail: "the bundle catalog built into this CLI could not be read: " + err.Error(),
				Fix:    "reinstall the CLI from a published release",
			})
			return nil, false
		}
		rep.add(Result{
			Check: CheckControlPlane, Outcome: Fail,
			Detail: "the control plane could not be reached, or this CLI is not logged in: " + err.Error(),
			Fix:    "check the control-plane URL and run `kubenest login`",
		})
		// Without the catalog there is nothing to check the bundle against.
		rep.add(Result{
			Check: CheckBundle, Outcome: Fail,
			Detail: "not checked: the control plane is unreachable",
			Fix:    "fix the control plane, then re-run",
		})
		return nil, false
	}

	if opts.Standalone {
		rep.add(Result{
			Check: CheckControlPlane, Outcome: Pass,
			Detail: fmt.Sprintf("not applicable: installing standalone, so this cluster registers with nothing and %d bundle(s) come from this CLI", len(bundles)),
		})
		return bundles, true
	}
	rep.add(Result{Check: CheckControlPlane, Outcome: Pass, Detail: fmt.Sprintf("reachable, %d bundle(s) offered", len(bundles))})
	return bundles, true
}

// checkNodeCount is the arithmetic the tier requires. It is checked here as
// well as at the flag surface because the tier is permanent and installing
// the wrong one is not a mistake anyone can undo cheaply.
func checkNodeCount(opts Options, rep *Report) {
	servers, agents := 0, 0
	for _, n := range opts.Nodes {
		if n.Role == "server" {
			servers++
		} else {
			agents++
		}
	}
	switch opts.HATier {
	case "ha":
		if servers < 3 {
			rep.add(Result{
				Check: CheckNodeCount, Outcome: Fail,
				Detail: fmt.Sprintf("the ha tier needs three control-plane nodes, got %d", servers),
				Fix:    "pass three --server addresses, or install --ha single-server",
			})
			return
		}
	case "single-server":
		if servers != 1 {
			rep.add(Result{
				Check: CheckNodeCount, Outcome: Fail,
				Detail: fmt.Sprintf("the single-server tier takes exactly one --server, got %d", servers),
				Fix:    "pass one --server address, or install --ha ha with three",
			})
			return
		}
	default:
		rep.add(Result{
			Check: CheckNodeCount, Outcome: Fail,
			Detail: fmt.Sprintf("%q is not a tier", opts.HATier),
			Fix:    "the tiers are single-server and ha",
		})
		return
	}
	rep.add(Result{
		Check: CheckNodeCount, Outcome: Pass,
		Detail: fmt.Sprintf("%d server(s), %d agent(s) for the %s tier", servers, agents, opts.HATier),
	})
}

func contains(haystack []string, want string) bool {
	for _, h := range haystack {
		if h == want {
			return true
		}
	}
	return false
}

func orNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}
