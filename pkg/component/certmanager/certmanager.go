// Package certmanager installs cert-manager (stage 6, platform-certs) with
// its Gateway API integration on, so the platform Gateway's TLS is issued
// and renewed by cert-manager from day one.
//
// No bead owned this installer when kn-pgu needed it: the bead's "TLS via
// cert-manager" default and the joint e2e both require it, and the 13-stage
// installer (kn-7k8) needs it regardless, so it lands here on the shared
// component shape.
package certmanager

import (
	"context"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

// Namespace is cert-manager's own namespace (upstream default). CA secrets
// for ClusterIssuers live here (the cluster-resource namespace).
const Namespace = "cert-manager"

const chartRepo = "https://charts.jetstack.io"

// Values: CRDs ship with the chart install, and the Gateway API integration
// is on — the platform Gateway (pkg/component/traefik) is annotated for it.
// Requires the Gateway API CRDs to exist first (stage 5 precedes stage 6).
const Values = `crds:
  enabled: true
config:
  apiVersion: controller.config.cert-manager.io/v1alpha1
  kind: ControllerConfiguration
  gatewayAPI:
    enabled: true
`

// Chart renders the pinned HelmChart custom resource for cert-manager.
func Chart(bundle *manifest.Manifest) (k3s.HelmChart, error) {
	version, err := bundle.Core.Version("cert-manager")
	if err != nil {
		return k3s.HelmChart{}, err
	}
	return k3s.HelmChart{
		Name:            "kubenest-cert-manager",
		Repo:            chartRepo,
		Chart:           "cert-manager",
		Version:         version,
		TargetNamespace: Namespace,
		ValuesYAML:      Values,
	}, nil
}

// Install applies the chart and converges until every cert-manager pod
// (controller, webhook, cainjector) is Ready.
func Install(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, rep converge.Reporter) error {
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
	if err := k3s.WriteManifest(ctx, r, chart.Name, doc); err != nil {
		return err
	}

	res, err := converge.Wait(ctx, k3s.PodsReadyProbe(r, Namespace), converge.Options{
		Name:     "cert-manager-ready",
		Deadline: deadline,
		Reporter: rep,
	})
	if err != nil {
		return err
	}
	return res.Err()
}
