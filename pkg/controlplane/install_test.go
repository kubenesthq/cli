package controlplane

import (
	"context"
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

// installBundle is the manifest the install path reads its deadline from: a
// missing timeout is an error, never a default.
func installBundle(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(`
bundle: "1.0"
limits:
  timeouts:
    component-ready: 1m
`))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// conditionProbeCmd is the command component.CheckCondition runs for an
// object in this namespace.
func conditionProbeCmd(resource string) string {
	return "sudo -n k3s kubectl get " + resource + " -o json -n " + Namespace
}

func conditionJSON(condType, status string) sshx.Result {
	return sshx.Result{Stdout: fmt.Sprintf(`{"status":{"conditions":[{"type":%q,"status":%q}]}}`, condType, status)}
}

// The CLI reaches the backend through an SSH tunnel during the install, so a
// control plane whose pods are all ready can still be unreachable at
// https://api.<domain>. On real hardware the first install's closing
// reachability check timed out while the route was still settling. The
// public route is what added clusters use, so the control plane is not ready
// until its certificate is issued and its Gateway is programmed.
func TestReadinessWaitsForTheControlPlanesCertificateAndGateway(t *testing.T) {
	const revision = "0123456789abcdef"
	rolled := sshx.Result{Stdout: deploymentJSON(revision, 1, 1, 1, 1, 1)}
	base := map[string]sshx.Result{
		deploymentProbeCmd(ReleaseName + "-backend"): rolled,
		deploymentProbeCmd(ReleaseName + "-hub"):     rolled,
		deploymentProbeCmd(ReleaseName + "-ui"):      rolled,
		"sudo -n k3s kubectl get statefulset " + postgresStatefulSet + " -n " + Namespace + " -o json": {
			Stdout: `{"spec":{"replicas":1},"status":{"readyReplicas":1}}`,
		},
	}
	cases := []struct {
		name        string
		certificate sshx.Result
		gateway     sshx.Result
		ready       bool
	}{
		{"the certificate is not issued yet", conditionJSON("Ready", "False"), conditionJSON("Programmed", "True"), false},
		{"the Gateway is not programmed yet", conditionJSON("Ready", "True"), conditionJSON("Programmed", "False"), false},
		{"both serve", conditionJSON("Ready", "True"), conditionJSON("Programmed", "True"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			answers := map[string]sshx.Result{
				conditionProbeCmd("certificate/" + tlsCertificate): tc.certificate,
				conditionProbeCmd("gateway/" + gatewayName):        tc.gateway,
			}
			for k, v := range base {
				answers[k] = v
			}
			r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
				res, ok := answers[command]
				if !ok {
					t.Fatalf("unscripted command: %q", command)
				}
				return res, nil
			}}
			ready, state, err := readyProbe(r, revision)(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if ready != tc.ready {
				t.Errorf("ready = %v (%s: %s), want %v", ready, state.Object, state.Status, tc.ready)
			}
		})
	}
}

// deploymentProbeCmd is the command the readiness probe runs for a Deployment
// in this namespace.
func deploymentProbeCmd(name string) string {
	return "sudo -n k3s kubectl get deployment/" + name + " -o json -n " + Namespace
}

// deploymentJSON is a Deployment as kubectl reports it: the revision its pod
// template carries, and its generation and replica counts.
func deploymentJSON(revision string, generation, observed int64, replicas, updated, available int32) string {
	return fmt.Sprintf(`{"metadata":{"generation":%d},"spec":{"replicas":1,"template":{"metadata":{"annotations":{%q:%q}}}},"status":{"observedGeneration":%d,"replicas":%d,"updatedReplicas":%d,"availableReplicas":%d}}`,
		generation, revisionAnnotation, revision, observed, replicas, updated, available)
}

// The chart reaches the cluster inside the HelmChart resource itself
// (spec.chartContent), which is the whole reason this package exists: no
// registry, no chart repository, no network. It also has to carry the values
// document as written, and to wait for all four workloads rather than the
// first one that answers.
func TestInstallAppliesTheEmbeddedChartAndWaitsForTheControlPlane(t *testing.T) {
	ctx := context.Background()
	settings := Settings{Domain: "kn.example.com", AdminEmail: "admin@kn.example.com"}
	sec := Secrets{JWTSecret: "jwt", EncryptionKey: "enc", PostgresPassword: "pg", AdminPassword: "adm"}
	values, err := Values(settings, sec)
	if err != nil {
		t.Fatal(err)
	}
	applied, revision, err := withRevision(values)
	if err != nil {
		t.Fatal(err)
	}

	rolled := sshx.Result{Stdout: deploymentJSON(revision, 2, 2, 1, 1, 1)}
	answers := map[string]sshx.Result{
		deploymentProbeCmd(ReleaseName + "-backend"): rolled,
		deploymentProbeCmd(ReleaseName + "-hub"):     rolled,
		deploymentProbeCmd(ReleaseName + "-ui"):      rolled,
		"sudo -n k3s kubectl get statefulset " + postgresStatefulSet + " -n " + Namespace + " -o json": {
			Stdout: `{"spec":{"replicas":1},"status":{"readyReplicas":1}}`,
		},
		conditionProbeCmd("certificate/" + tlsCertificate): conditionJSON("Ready", "True"),
		conditionProbeCmd("gateway/" + gatewayName):        conditionJSON("Programmed", "True"),
	}
	r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		if strings.HasPrefix(command, "sudo -n install -m 0600 ") {
			return sshx.Result{}, nil
		}
		res, ok := answers[command]
		if !ok {
			t.Fatalf("unscripted command: %q", command)
		}
		return res, nil
	}}

	var events []converge.Event
	rep := converge.ReporterFunc(func(e converge.Event) { events = append(events, e) })

	if err := Install(ctx, r, values, installBundle(t), rep); err != nil {
		t.Fatal(err)
	}

	inputs := r.Inputs()
	if len(inputs) != 1 {
		t.Fatalf("streamed %d documents to the host, want the one HelmChart", len(inputs))
	}
	var doc map[string]any
	if err := yaml.Unmarshal(inputs[0], &doc); err != nil {
		t.Fatalf("the applied manifest is not valid YAML: %v", err)
	}
	if doc["apiVersion"] != "helm.cattle.io/v1" || doc["kind"] != "HelmChart" {
		t.Errorf("applied %v/%v, want helm.cattle.io/v1 HelmChart", doc["apiVersion"], doc["kind"])
	}
	metadata := section(t, doc, "metadata")
	if metadata["name"] != ReleaseName || metadata["namespace"] != "kube-system" {
		t.Errorf("applied to %v/%v, want kube-system/%s", metadata["namespace"], metadata["name"], ReleaseName)
	}
	spec := section(t, doc, "spec")
	want := []string{"chartContent", "createNamespace", "failurePolicy", "targetNamespace", "valuesContent"}
	if got := sortedKeys(spec); !equalStrings(got, want) {
		t.Errorf("spec keys = %v, want exactly %v: chartContent excludes repo, chart and version", got, want)
	}
	if spec["targetNamespace"] != Namespace {
		t.Errorf("targetNamespace = %v, want %s", spec["targetNamespace"], Namespace)
	}
	if spec["chartContent"] != base64.StdEncoding.EncodeToString(ChartArchive()) {
		t.Error("chartContent is not the embedded chart archive: the install would fetch a chart from somewhere instead")
	}
	if spec["valuesContent"] != applied {
		t.Errorf("valuesContent = %v, want the values document plus its installRevision", spec["valuesContent"])
	}
	var sent map[string]any
	if err := yaml.Unmarshal([]byte(applied), &sent); err != nil {
		t.Fatal(err)
	}
	var original map[string]any
	if err := yaml.Unmarshal([]byte(values), &original); err != nil {
		t.Fatal(err)
	}
	if sent["installRevision"] != revision {
		t.Errorf("installRevision = %v, want %s", sent["installRevision"], revision)
	}
	delete(sent, "installRevision")
	if !reflect.DeepEqual(sent, original) {
		t.Errorf("the applied values differ from Values() beyond installRevision:\n got %v\nwant %v", sent, original)
	}

	// The chart has to be READY, not merely applied.
	if len(events) == 0 {
		t.Fatal("no progress was reported")
	}
	last := events[len(events)-1]
	if last.Outcome != converge.Pass || last.Check != "kubenest-control-plane-ready" {
		t.Errorf("last event = %s %s, want a pass of kubenest-control-plane-ready", last.Check, last.Outcome)
	}
}

// The control plane is upgraded by re-running the install, so readiness has
// to mean "running what this run applied". On real hardware a probe on the
// Available condition passed in seconds while the previous backend still
// served, because a rolling update keeps the old ReplicaSet Available. Each
// case here is a Deployment that is Available and must still not count.
func TestReadinessWaitsForThisRevisionToRollOut(t *testing.T) {
	const revision = "0123456789abcdef"
	cases := []struct {
		name       string
		deployment string
		ready      bool
	}{
		{"the helm job has not applied this revision", deploymentJSON("fedcba9876543210", 1, 1, 1, 1, 1), false},
		{"the controller has not observed the new template", deploymentJSON(revision, 2, 1, 1, 0, 1), false},
		{"the new pod is not available yet", deploymentJSON(revision, 2, 2, 1, 1, 0), false},
		{"a pod of the previous revision is still running", deploymentJSON(revision, 2, 2, 2, 1, 2), false},
		{"rolled out at this revision", deploymentJSON(revision, 2, 2, 1, 1, 1), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
				if command != deploymentProbeCmd(ReleaseName+"-backend") {
					t.Fatalf("unscripted command: %q", command)
				}
				return sshx.Result{Stdout: tc.deployment}, nil
			}}
			ready, state, err := rolledOut(context.Background(), r, ReleaseName+"-backend", revision)
			if err != nil {
				t.Fatal(err)
			}
			if ready != tc.ready {
				t.Errorf("ready = %v (%s), want %v", ready, state.Status, tc.ready)
			}
		})
	}
}

// The CLI reaches the backend through a tunnel it opens from the node, so
// what it needs is the Service's ClusterIP and its port, and nothing less
// specific than that will do.
func TestBackendAddrIsTheServiceClusterIPAndPort(t *testing.T) {
	ctx := context.Background()
	cmd := "sudo -n k3s kubectl get service " + backendService + " -n " + Namespace + " -o jsonpath='{.spec.clusterIP}'"

	addr, err := BackendAddr(ctx, fakeRunner(t, map[string]sshx.Result{cmd: {Stdout: "10.43.0.10\n"}}))
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.43.0.10:8000" {
		t.Errorf("BackendAddr = %q, want 10.43.0.10:8000", addr)
	}

	if _, err := BackendAddr(ctx, fakeRunner(t, map[string]sshx.Result{cmd: {Stdout: "\n"}})); err == nil {
		t.Error("a Service with no ClusterIP must be an error, not an empty address")
	}
}

// The control plane's certificate authority is minted by the install and never
// read from the cluster (kn-t47). What a caller trusts is
// Secrets.GatewayCACertificate, handed to the chart as gatewayCA and stored as
// Config.ControlPlaneCA — so there is no host-cluster CA to read, and
// TestEnsureSecretsMintsTheControlPlaneCAOnceAndKeepsIt is where that CA is
// checked.

// A failed control-plane upgrade must leave the running control plane in
// place. k3s's helm-controller defaults failurePolicy to reinstall: when the
// release is failed the next helm-install pod runs `helm uninstall
// kubenest-cp` and then `helm install`, deleting and recreating every
// Deployment, Service, ConfigMap, chart-owned Secret, checkpoint CronJob and
// the Postgres and Redis StatefulSets — with only the PVCs surviving. It
// happened twice on hardware 2026-09-25, both times after a chart upgrade
// changed the backend image. With abort the failed release stays in place and
// the control-plane wait reports the failure.
func TestControlPlaneHelmChartAbortsOnFailure(t *testing.T) {
	ctx := context.Background()
	settings := Settings{Domain: "kn.example.com", AdminEmail: "admin@kn.example.com"}
	sec := Secrets{JWTSecret: "jwt", EncryptionKey: "enc", PostgresPassword: "pg", AdminPassword: "adm"}
	values, err := Values(settings, sec)
	if err != nil {
		t.Fatal(err)
	}
	r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		if strings.HasPrefix(command, "sudo -n install -m 0600 ") {
			return sshx.Result{}, nil
		}
		t.Fatalf("unscripted command: %q", command)
		return sshx.Result{}, nil
	}}
	if _, err := Apply(ctx, r, values); err != nil {
		t.Fatal(err)
	}

	inputs := r.Inputs()
	if len(inputs) != 1 {
		t.Fatalf("streamed %d documents to the host, want the one HelmChart", len(inputs))
	}
	var doc struct {
		Spec map[string]any `yaml:"spec"`
	}
	if err := yaml.Unmarshal(inputs[0], &doc); err != nil {
		t.Fatalf("the applied manifest is not valid YAML: %v", err)
	}
	if doc.Spec["failurePolicy"] != "abort" {
		t.Errorf("spec.failurePolicy = %v, want abort: leaving it unset makes helm-controller uninstall and reinstall the whole control plane on a failed upgrade", doc.Spec["failurePolicy"])
	}
}
