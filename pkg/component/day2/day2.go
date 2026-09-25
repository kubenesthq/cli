// Package day2 is stage 9: system-upgrade-controller and kured, the two
// components that make OS patching and Kubernetes upgrades a platform
// property rather than a runbook.
//
// SCOPE, deliberately narrow. This package PLACES the components at their
// pinned versions and proves them Ready. It does not write the policy that
// drives them — reboot windows, unattended-upgrades, the single-server
// reboot safeguard and the upgrade Plans are kn-nqj's, in wave 3. Stage 9
// exists in wave 2 because install.mdx's acceptance checks require both
// components Running on a freshly installed cluster, and a core component
// that is in the bundle but not on the cluster would make the recorded
// manifest a lie.
//
// One safety decision lives here rather than being deferred: kured reboots
// only nodes the platform has not held out of its pool. The trigger is
// Ubuntu's own /var/run/reboot-required — the chart's default, kept, because
// no KubeNest sentinel exists — and the HOLD is the node label
// kubenest.io/auto-reboot=false, which Chart below makes kured's DaemonSet
// exclude by node affinity: the DaemonSet controller removes kured from a
// labelled node as soon as the label appears (measured 4 s in probe P1).
// That one label is the platform's whole claim on WHEN a node may reboot, and
// the window kured obeys is delivered separately, by the operator, as a
// HelmChartConfig for this chart; servers carry the label permanently, so
// their reboots are requested rather than automatic.
package day2

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/component"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/interlock"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

// Namespace is where system-upgrade-controller runs; it is the namespace its
// own release manifests declare.
const Namespace = "system-upgrade"

// KuredNamespace is where kured runs. kube-system, because it is a node
// agent with cluster-wide reach, and that is where its chart puts it.
const KuredNamespace = "kube-system"

// NoAutoRebootLabel is the hold, and the whole hold mechanism (plan 7.4):
// kured's DaemonSet excludes this label by node affinity, so a node carrying
// it — see NoAutoRebootValue — has no kured pod and reboots only when an
// operator asks. Servers carry it permanently (T6.1's install-time
// --node-label), a joining node carries it until node add has recorded it,
// every node carries it while a new window is applying, and
// HoldAutomaticReboots applies it for those dynamic cases. It is the single
// source of truth: the chart values below and the label command both come
// from it.
const NoAutoRebootLabel = "kubenest.io/auto-reboot"

// NoAutoRebootValue is the value the hold label must carry. Chart's node
// affinity is NotIn ["false"], which an ABSENT key satisfies, so kured runs on
// every node that does not explicitly say false — including nodes that existed
// before this label did.
const NoAutoRebootValue = "false"

// ReleaseBaseURL is system-upgrade-controller's release download base. A
// variable so tests can point it at a local server.
var ReleaseBaseURL = "https://github.com/rancher/system-upgrade-controller/releases/download"

// UpgradePlanCRD is the CRD the controller owns; the verify step requires it
// Established, because an upgrade Plan applied before it exists is silently
// nothing.
const UpgradePlanCRD = "plans.upgrade.cattle.io"

// Install places both day-2 components and waits for each to be Ready.
//
// The halves are also exported separately, because the installer has to be
// able to say WHICH of them failed: a stage-level component name reports
// system-upgrade-controller when kured is what broke, and a record that names
// the wrong thing is worse than one that names nothing.
func Install(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, rep converge.Reporter) error {
	if err := InstallUpgradeController(ctx, r, bundle, rep); err != nil {
		return err
	}
	return InstallKured(ctx, r, bundle, rep)
}

// InstallUpgradeController places system-upgrade-controller and its CRDs.
func InstallUpgradeController(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, rep converge.Reporter) error {
	version, err := bundle.Core.Version("system-upgrade-controller")
	if err != nil {
		return err
	}
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}

	// The release ships CRDs and controller as separate documents. Both go
	// into the k3s auto-deploy directory, where k3s keeps them applied and
	// retries the ordering itself.
	for _, part := range []struct{ name, asset string }{
		{"kubenest-system-upgrade-crd", "crd.yaml"},
		{"kubenest-system-upgrade-controller", "system-upgrade-controller.yaml"},
	} {
		url := fmt.Sprintf("%s/%s/%s", ReleaseBaseURL, version, part.asset)
		data, err := fetch(ctx, url)
		if err != nil {
			return fmt.Errorf("download system-upgrade-controller %s (%s): %w", version, part.asset, err)
		}
		if err := k3s.WriteManifest(ctx, r, part.name, data); err != nil {
			return err
		}
	}

	res, err := converge.Wait(ctx, component.CRDsEstablishedProbe(r, []string{UpgradePlanCRD}), converge.Options{
		Name: "system-upgrade-controller-crds", Deadline: deadline, Reporter: rep,
	})
	if err != nil {
		return err
	}
	if err := res.Err(); err != nil {
		return err
	}

	// Its WORKLOAD, not every pod in the namespace: system-upgrade-controller
	// runs a Job per node per upgrade and keeps their pods, and one that
	// failed an attempt stays Failed forever. Waiting on every pod means
	// this component can never be reinstalled or reverted on a cluster that
	// has ever been upgraded — which is every cluster this matters for.
	res, err = converge.Wait(ctx, k3s.WorkloadsReadyProbe(r, Namespace), converge.Options{
		Name: "system-upgrade-controller", Deadline: deadline, Reporter: rep,
	})
	if err != nil {
		return err
	}
	return res.Err()
}

// Chart renders kured's HelmChart resource at the bundle's pin, with the
// values the CLI owns.
//
// The CLI keeps owning the HelmChart for a reason (plan 7.4 item 6): the
// maintenance WINDOW is not here. The operator writes it into a
// HelmChartConfig over this chart, k3s merges the config's values over these,
// and so a later re-apply of this document cannot revert the window and the
// deploy controller's drift check cannot flag it. What is here is the part
// that must never depend on the control plane being reachable: which nodes
// kured may run on, and how long it may take to drain and to hold its lock.
func Chart(bundle *manifest.Manifest) (k3s.HelmChart, error) {
	version, err := bundle.Core.Version("kured")
	if err != nil {
		return k3s.HelmChart{}, err
	}
	values, err := kuredValues(bundle)
	if err != nil {
		return k3s.HelmChart{}, err
	}
	return k3s.HelmChart{
		Name:            "kured",
		Repo:            "https://kubereboot.github.io/charts",
		Chart:           "kured",
		Version:         version,
		TargetNamespace: KuredNamespace,
		ValuesYAML:      values,
	}, nil
}

// kuredValues renders the chart values document.
//
// Three settings, each of which is a failure seen on hardware if it is left at
// the chart's default:
//
//   - The node affinity excludes the hold label. `NotIn ["false"]` matches a
//     node where the key is ABSENT, so an unlabelled node still runs kured and
//     only an explicit false holds it out. This is what makes the label the
//     hold: probe P1 saw kured's pod on a labelled node gone in 4 s.
//   - annotateNodes stamps weave.works/kured-reboot-in-progress and
//     weave.works/kured-most-recent-reboot-needed on the nodes kured reboots,
//     which is what T2.7 reads to tell a planned reboot from a dead node.
//   - drainTimeout: kured's own default is 0, which means drain for ever. A
//     node whose drain hangs would hold kured's lock and the whole sequence
//     with it, so the manifest's node-drain deadline is a hard bound.
//
// rebootSentinel is deliberately NOT set. With the label as the entire hold,
// the trigger is the plain file the distro writes — /var/run/reboot-required,
// the chart's own default — so a node kured may reboot reacts to Ubuntu's
// marker and is kept inside the window by kured's window flags instead. A
// KubeNest-specific sentinel would be a second hold mechanism that nothing
// creates, which is what this replaced (7.4).
func kuredValues(bundle *manifest.Manifest) (string, error) {
	drain, err := bundle.Limits.Timeouts.For("node-drain")
	if err != nil {
		return "", err
	}
	// The CLI's own lock carries this TTL too, and the two must agree: a
	// kured lock that outlives the operation that holds the CLI's hold is
	// how a node that never returns stops the whole cluster (P1, finding 4).
	lockTTL, err := interlock.LockTTLFor(bundle)
	if err != nil {
		return "", err
	}
	doc := map[string]any{
		"affinity": map[string]any{
			"nodeAffinity": map[string]any{
				"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
					"nodeSelectorTerms": []any{
						map[string]any{
							"matchExpressions": []any{
								map[string]any{
									"key":      NoAutoRebootLabel,
									"operator": "NotIn",
									"values":   []string{NoAutoRebootValue},
								},
							},
						},
					},
				},
			},
		},
		"configuration": map[string]any{
			"annotateNodes": true,
			"drainTimeout":  drain.String(),
			"lockTtl":       lockTTL.String(),
		},
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("render kured values: %w", err)
	}
	return string(out), nil
}

// HoldAutomaticReboots takes node out of kured's pool: it labels the node with
// NoAutoRebootLabel=NoAutoRebootValue, then clears a kured lock the node still
// owns. It is idempotent (the label is written with --overwrite), because every
// caller may be re-run.
//
// The second half is not bookkeeping, it is the difference between a hold and
// an outage. Probe P1 labelled a node while kured held its lock: kured's pod
// left the node in 4 s, but the lock stayed — naming that node, with the node
// still cordoned — and with kured's concurrency of 1 an orphaned lock stops
// every OTHER node's automatic reboot for ever. So, after labelling:
//
//   - if the lock names another node, do nothing at all: that node may be
//     mid-reboot, and its lock is its own.
//   - if the lock names THIS node and its metadata says the node was
//     schedulable before the lock was taken (metadata.unschedulable false,
//     kured's own recorded view), uncordon it — kured cordoned it on the way
//     to a reboot that the hold has just cancelled.
//   - then release the lock, with the same compare-and-swap protocol, so the
//     sequence can move on.
//
// This covers the dynamic cases only — an operator raising the hold while a
// window applies, and node add holding a joining node until it is recorded.
// Installation labels servers through T6.1's --node-label, never here.
func HoldAutomaticReboots(ctx context.Context, r k3s.Runner, node string) error {
	if !interlock.ValidNodeName(node) {
		return fmt.Errorf("hold node %q: not a node name", node)
	}
	if _, err := k3s.Kubectl(ctx, r,
		"label node "+node+" "+NoAutoRebootLabel+"="+NoAutoRebootValue+" --overwrite"); err != nil {
		return err
	}

	// The lock DOCUMENT, not Holding's bool: a lock that has expired is still
	// a lock naming this node, and this node can still be cordoned because of
	// it — clearing only unexpired locks would leave exactly the orphan P1
	// found. Whether the node may reboot is not the question; whether it is
	// still sitting cordoned waiting for a reboot is.
	_, lock, err := interlock.Holding(ctx, r, node)
	if err != nil {
		return err
	}
	if lock.NodeID != node {
		// Nothing of ours to clear: either kured holds no lock, or it holds
		// one for a node this hold is not about.
		return nil
	}
	if !lock.Metadata.Unschedulable {
		if _, err := k3s.Kubectl(ctx, r, "uncordon "+node); err != nil {
			// The lock is left in place on purpose: the node is cordoned and
			// the lock is what says so, so a retry of this call finds it again
			// and uncordons before releasing. Releasing first would leave a
			// cordoned node nobody knows about.
			return err
		}
	}
	return interlock.Release(ctx, r, node)
}

// InstallKured places kured.
func InstallKured(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, rep converge.Reporter) error {
	chart, err := Chart(bundle)
	if err != nil {
		return err
	}
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}
	doc, err := chart.Manifest()
	if err != nil {
		return err
	}
	if err := k3s.WriteManifest(ctx, r, "kubenest-kured", doc); err != nil {
		return err
	}

	res, err := converge.Wait(ctx, kuredReadyProbe(r), converge.Options{
		Name: "kured", Deadline: deadline, Reporter: rep,
	})
	if err != nil {
		return err
	}
	return res.Err()
}

// kuredReadyProbe watches kured's DaemonSet rather than every pod in
// kube-system, which holds coredns, metrics-server and the helm-install jobs
// as well.
func kuredReadyProbe(r k3s.Runner) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, r,
			"get daemonset -n "+KuredNamespace+" kured -o jsonpath='{.status.desiredNumberScheduled} {.status.numberReady}'")
		if err != nil {
			return false, converge.State{
				Object: "daemonset kured in " + KuredNamespace,
				Status: "not created yet",
				Detail: "the helm-install job has not applied the chart yet",
			}, err
		}
		fields := strings.Fields(strings.Trim(out, "'"))
		if len(fields) != 2 {
			return false, converge.State{Object: "daemonset kured in " + KuredNamespace, Status: "no status yet"}, nil
		}
		desired, ready := fields[0], fields[1]
		state := converge.State{
			Object: "daemonset kured in " + KuredNamespace,
			Status: ready + "/" + desired + " Ready",
		}
		if desired != "0" && desired == ready {
			return true, state, nil
		}
		return false, state, nil
	}
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}
