package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/component"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

// HubInClusterURL is the relay URL the CLI hands a cluster when it registers
// it to THIS control plane over the node's own tunnel: the hub's in-cluster
// Service address, not the public wss://hub.<domain>, because DNS for the
// domain does not exist yet at the moment of a first install.
const HubInClusterURL = "ws://kubenest-cp-hub.kubenest-system.svc.cluster.local:8001/ws/operator"

// Service port the backend listens on (chart templates/backend-service.yaml).
const backendPort = "8000"

// Objects the chart creates and this package waits for.
const (
	backendService      = ReleaseName + "-backend"
	postgresStatefulSet = ReleaseName + "-postgresql"
	// The chart's own TLS certificate and Gateway (templates/gateway.yaml)
	// for app., api. and hub.<domain>.
	tlsCertificate = ReleaseName + "-tls"
	gatewayName    = ReleaseName
)

// revisionAnnotation is the pod-template annotation the chart stamps with the
// installRevision value (templates/*-deployment.yaml).
const revisionAnnotation = "kubenest.io/install-revision"

// Apply writes the control-plane chart's HelmChart and returns the install
// revision it applied.
//
// It is separate from WaitReady because a control-plane install has one step
// between the two: the schema migration (migrate.go). The chart is applied
// first so PostgreSQL exists — the migration Job has backoffLimit 0, so a Job
// created before the database accepts connections is a FAILED Job — and the
// readiness check follows the step, so a control plane whose migration failed
// is never reported installed.
func Apply(ctx context.Context, r k3s.Runner, valuesYAML string) (string, error) {
	return ApplyArchive(ctx, r, ChartArchive(), valuesYAML)
}

// ApplyArchive applies a SPECIFIC chart archive with these values, and returns
// the install revision it applied.
//
// IT EXISTS FOR ONE REAL CALLER: the candidate-to-candidate gate (PLAN 7.8,
// S2), which installs the control plane from the PREVIOUS 1.2 candidate — a
// pinned artifact, not one this binary happens to carry — and then upgrades it
// to the chart this binary embeds. A gate that could only ever apply the
// embedded archive would prove nothing about an upgrade, because the state it
// starts from would be the state it ends in.
func ApplyArchive(ctx context.Context, r k3s.Runner, archive []byte, valuesYAML string) (string, error) {
	values, revision, err := withRevisionFor(archive, valuesYAML)
	if err != nil {
		return "", err
	}
	chart := k3s.HelmChart{
		Name:            ReleaseName,
		TargetNamespace: Namespace,
		ChartContent:    base64.StdEncoding.EncodeToString(archive),
		// valuesContent carries the generated secrets, so it is readable by anyone who can read HelmCharts in kube-system — the same cluster-admin audience as the Secret they came from.
		ValuesYAML: values,
		// helm-controller's default, reinstall, answers a failed upgrade of
		// this release with `helm uninstall kubenest-cp` followed by a fresh
		// install: every Deployment, Service, ConfigMap, chart-owned Secret,
		// checkpoint CronJob and the Postgres and Redis StatefulSets are
		// deleted and recreated, with only the PVCs surviving (observed twice
		// on hardware 2026-09-25). abort leaves the failed release in place so
		// the control-plane wait reports the failure instead.
		FailurePolicy: "abort",
	}
	doc, err := chart.Manifest()
	if err != nil {
		return "", err
	}
	if err := k3s.WriteManifest(ctx, r, ReleaseName, doc); err != nil {
		return "", err
	}
	return revision, nil
}

// WaitReady converges until the control plane runs exactly the revision Apply
// applied.
func WaitReady(ctx context.Context, r k3s.Runner, revision string, bundle *manifest.Manifest, rep converge.Reporter) error {
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}
	res, err := converge.Wait(ctx, readyProbe(r, revision), converge.Options{
		Name:     "kubenest-control-plane-ready",
		Deadline: deadline,
		Reporter: rep,
	})
	if err != nil {
		return err
	}
	return res.Err()
}

// Install applies the control-plane chart and converges until its three
// Deployments run exactly what this call applied and its PostgreSQL
// StatefulSet is up.
//
// The chart is applied as a k3s HelmChart holding the archive in
// spec.chartContent (k3s.HelmChart.ChartContent), which is how the pinned
// artifact gets to a cluster that cannot reach a registry: no repo, no OCI
// reference and no version field — the archive carries its own.
func Install(ctx context.Context, r k3s.Runner, valuesYAML string, bundle *manifest.Manifest, rep converge.Reporter) error {
	revision, err := Apply(ctx, r, valuesYAML)
	if err != nil {
		return err
	}
	return WaitReady(ctx, r, revision, bundle, rep)
}

// withRevision adds installRevision to the values document: a hash of the
// chart archive and the values, so it changes exactly when what is applied
// changes. An identical re-run therefore applies an identical HelmChart and
// rolls nothing.
func withRevision(valuesYAML string) (string, string, error) {
	return withRevisionFor(ChartArchive(), valuesYAML)
}

func withRevisionFor(archive []byte, valuesYAML string) (string, string, error) {
	revision, err := RevisionFor(archive, valuesYAML)
	if err != nil {
		return "", "", err
	}

	doc := map[string]any{}
	if err := yaml.Unmarshal([]byte(valuesYAML), &doc); err != nil {
		return "", "", fmt.Errorf("reading the control-plane values: %w", err)
	}
	doc["installRevision"] = revision
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", "", fmt.Errorf("rendering the control-plane values: %w", err)
	}
	return string(out), revision, nil
}

// Revision is the install revision a values document will be applied at: a
// hash of the chart archive and the values, so it changes exactly when what is
// applied changes.
//
// IT IS A PURE FUNCTION OF ITS INPUTS, and that is what makes "the previous
// release's migration Job" decidable BEFORE anything is applied: the migration
// step compares the Job on the cluster with the revision it is about to
// create, and a revision that could only be learned after applying would leave
// the immutability failure with nothing to avoid it.
func Revision(valuesYAML string) (string, error) {
	return RevisionFor(ChartArchive(), valuesYAML)
}

// RevisionFor is Revision against a specific archive, for the same reason
// ApplyArchive exists.
func RevisionFor(archive []byte, valuesYAML string) (string, error) {
	sum := sha256.New()
	sum.Write(archive)
	sum.Write([]byte{0})
	sum.Write([]byte(valuesYAML))
	return hex.EncodeToString(sum.Sum(nil))[:16], nil
}

// readyProbe observes the whole control plane: each Deployment rolled out at
// this install's revision, PostgreSQL with every replica ready, and the
// chart's own certificate and Gateway serving app., api. and hub.<domain>.
//
// "Available" is not enough, and a re-run proved it: k3s's helm-install job
// applies the chart asynchronously, and during a rolling update the previous
// ReplicaSet keeps a Deployment Available. A probe on that condition passed in
// seconds while the old backend was still the one serving, so a new backend
// that could not start would have been reported installed.
//
// Nor are running pods enough. The CLI reaches the backend through an SSH
// tunnel during the install, so nothing in the install itself needs the
// public route, and on a fresh install it can still be settling when the
// command ends. Measured on real hardware on 2026-09-24: the first install's
// closing check of https://api.<domain> timed out, and the same machine
// reached that API minutes later. Added clusters dial hub.<domain> through
// this route, so it is part of the control plane being ready.
func readyProbe(r k3s.Runner, revision string) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		for _, workload := range []string{"backend", "hub", "ui"} {
			done, state, err := rolledOut(ctx, r, ReleaseName+"-"+workload, revision)
			if err != nil || !done {
				return false, state, err
			}
		}
		if done, state, err := postgresReady(ctx, r); err != nil || !done {
			return false, state, err
		}
		if done, state, err := component.CheckCondition(ctx, r, "certificate/"+tlsCertificate, Namespace, "Ready"); err != nil || !done {
			return false, state, err
		}
		return component.CheckCondition(ctx, r, "gateway/"+gatewayName, Namespace, "Programmed")
	}
}

// rolledOut reports whether a Deployment runs exactly the given revision: its
// pod template carries it, the Deployment controller has observed that
// template, and every replica is from it and available, with none of the
// previous ReplicaSet's pods left.
func rolledOut(ctx context.Context, r k3s.Runner, name, revision string) (bool, converge.State, error) {
	var d struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
		Spec struct {
			Replicas *int32 `json:"replicas"`
			Template struct {
				Metadata struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
			} `json:"template"`
		} `json:"spec"`
		Status struct {
			ObservedGeneration int64 `json:"observedGeneration"`
			Replicas           int32 `json:"replicas"`
			UpdatedReplicas    int32 `json:"updatedReplicas"`
			AvailableReplicas  int32 `json:"availableReplicas"`
		} `json:"status"`
	}
	object := "deployment/" + name + " in " + Namespace
	out, err := k3s.Kubectl(ctx, r, "get deployment/"+name+" -o json -n "+Namespace)
	if err != nil {
		return false, converge.State{Object: object, Status: "not found yet"}, err
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return false, converge.State{Object: object, Status: "unparsable"}, err
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	st := d.Status
	switch {
	case d.Spec.Template.Metadata.Annotations[revisionAnnotation] != revision:
		return false, converge.State{
			Object: object,
			Status: "not at this install's revision yet",
			Detail: "the helm-install job has not applied this revision of the chart; the previous one is still what runs",
		}, nil
	case d.Metadata.Generation > st.ObservedGeneration:
		return false, converge.State{Object: object, Status: "the new pod template is not observed yet"}, nil
	case st.UpdatedReplicas < want || st.AvailableReplicas < want:
		return false, converge.State{
			Object: object,
			Status: fmt.Sprintf("%d/%d replicas updated, %d available", st.UpdatedReplicas, want, st.AvailableReplicas),
		}, nil
	case st.Replicas > st.UpdatedReplicas:
		return false, converge.State{
			Object: object,
			Status: fmt.Sprintf("%d replica(s) of the previous revision still running", st.Replicas-st.UpdatedReplicas),
		}, nil
	}
	return true, converge.State{Object: object, Status: fmt.Sprintf("%d/%d at revision %s", st.AvailableReplicas, want, revision)}, nil
}

// postgresReady observes whether every replica of the PostgreSQL StatefulSet
// is ready. A StatefulSet carries no condition to probe — its readiness is
// the replica counts — so this compares status.readyReplicas with the replica
// count the chart asked for.
func postgresReady(ctx context.Context, r k3s.Runner) (bool, converge.State, error) {
	var sts struct {
		Spec struct {
			Replicas *int32 `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ReadyReplicas   *int32 `json:"readyReplicas"`
			CurrentRevision string `json:"currentRevision"`
		} `json:"status"`
	}
	object := "statefulset " + postgresStatefulSet + " in " + Namespace
	out, err := k3s.Kubectl(ctx, r, "get statefulset "+postgresStatefulSet+" -n "+Namespace+" -o json")
	if err != nil {
		return false, converge.State{Object: object, Status: "not found yet"}, err
	}
	if err := json.Unmarshal([]byte(out), &sts); err != nil {
		return false, converge.State{Object: object, Status: "unparsable"}, err
	}
	want := int32(1)
	if sts.Spec.Replicas != nil {
		want = *sts.Spec.Replicas
	}
	ready := int32(0)
	if sts.Status.ReadyReplicas != nil {
		ready = *sts.Status.ReadyReplicas
	}
	if want == 0 || ready < want {
		return false, converge.State{
			Object: object,
			Status: fmt.Sprintf("%d/%d Ready", ready, want),
			Detail: "the database must be accepting connections before the backend finishes migrating",
		}, nil
	}
	return true, converge.State{Object: object, Status: fmt.Sprintf("%d/%d Ready", ready, want)}, nil
}

// BackendAddr is the host:port the CLI reaches the backend on from the server
// node: the backend Service's ClusterIP plus its port. Only the node can route
// to it, which is why pkg/install dials it through the SSH connection it
// already holds.
func BackendAddr(ctx context.Context, r k3s.Runner) (string, error) {
	out, err := k3s.Kubectl(ctx, r, "get service "+backendService+" -n "+Namespace+" -o jsonpath='{.spec.clusterIP}'")
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(out)
	if ip == "" {
		return "", fmt.Errorf("the backend Service %s/%s has no ClusterIP yet: the control-plane chart has not finished installing", Namespace, backendService)
	}
	return ip + ":" + backendPort, nil
}

// The control plane's CA is NOT read from the cluster (kn-t47). It used to be:
// this file read cert-manager/kubenest-ca, the host cluster's platform CA, and
// stored it as the control plane's authority — which made every CLI and agent
// pin the host cluster's identity, so a control plane restored or moved onto
// another cluster was rejected as an unknown authority. The CLI now mints the
// control plane's own CA (Secrets.GatewayCACertificate) and it is that
// certificate every caller trusts.
