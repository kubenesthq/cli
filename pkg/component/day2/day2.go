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
// One safety decision is made here rather than deferred: kured is installed
// watching a sentinel the PLATFORM creates, not Ubuntu's
// /var/run/reboot-required. Ubuntu 24.04 enables unattended-upgrades by
// default, so a stock sentinel plus default kured means a single-server
// cluster can drain and reboot its only control-plane node unattended,
// before any of the day-2 policy that would make that safe exists. Rebooting
// a customer's node on the strength of a component that was merely placed is
// not a default worth having. kn-nqj owns what eventually creates the
// sentinel.
package day2

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"kubenest.io/cli/pkg/component"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

// Namespace is where system-upgrade-controller runs; it is the namespace its
// own release manifests declare.
const Namespace = "system-upgrade"

// KuredNamespace is where kured runs. kube-system, because it is a node
// agent with cluster-wide reach, and that is where its chart puts it.
const KuredNamespace = "kube-system"

// RebootSentinel is the file kured watches. The platform creates it when a
// reboot has been APPROVED by day-2 policy — never Ubuntu's
// /var/run/reboot-required directly. See the package comment.
const RebootSentinel = "/var/run/kubenest-reboot-approved"

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
		if err := writeStreamed(ctx, r, part.name, data); err != nil {
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

// Chart renders kured's HelmChart resource at the bundle's pin.
func Chart(bundle *manifest.Manifest) (k3s.HelmChart, error) {
	version, err := bundle.Core.Version("kured")
	if err != nil {
		return k3s.HelmChart{}, err
	}
	return k3s.HelmChart{
		Name:            "kured",
		Repo:            "https://kubereboot.github.io/charts",
		Chart:           "kured",
		Version:         version,
		TargetNamespace: KuredNamespace,
		ValuesYAML: "configuration:\n" +
			"  rebootSentinel: " + RebootSentinel + "\n",
	}, nil
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

// InputRunner is a Runner that can stream stdin. The controller manifests are
// large enough to be worth streaming rather than inlining.
type InputRunner interface {
	k3s.Runner
	RunInput(ctx context.Context, command string, stdin io.Reader) (sshx.Result, error)
}

func writeStreamed(ctx context.Context, r k3s.Runner, name string, content []byte) error {
	ir, ok := r.(InputRunner)
	if !ok {
		return k3s.WriteManifest(ctx, r, name, content)
	}
	path := k3s.ManifestDir + "/" + name + ".yaml"
	res, err := ir.RunInput(ctx, "sudo -n tee "+path+" >/dev/null", bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("write %s: exit %d: %s", path, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
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
