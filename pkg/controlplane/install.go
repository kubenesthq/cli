package controlplane

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

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
)

// The platform CA cert-manager signs the bundle's certificates from. It is a
// self-signed CA, so the same certificate appears under both keys: ca.crt is
// the canonical one and tls.crt the fallback.
const (
	caNamespace  = "cert-manager"
	caSecretName = "kubenest-ca"
	caCertKey    = "ca.crt"
	caTLSKey     = "tls.crt"
)

// revisionAnnotation is the pod-template annotation the chart stamps with the
// installRevision value (templates/*-deployment.yaml).
const revisionAnnotation = "kubenest.io/install-revision"

// Install applies the control-plane chart and converges until its three
// Deployments run exactly what this call applied and its PostgreSQL
// StatefulSet is up.
//
// The chart is applied as a k3s HelmChart holding the archive in
// spec.chartContent (k3s.HelmChart.ChartContent), which is how the pinned
// artifact gets to a cluster that cannot reach a registry: no repo, no OCI
// reference and no version field — the archive carries its own.
func Install(ctx context.Context, r k3s.Runner, valuesYAML string, bundle *manifest.Manifest, rep converge.Reporter) error {
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}
	values, revision, err := withRevision(valuesYAML)
	if err != nil {
		return err
	}
	chart := k3s.HelmChart{
		Name:            ReleaseName,
		TargetNamespace: Namespace,
		ChartContent:    base64.StdEncoding.EncodeToString(ChartArchive()),
		// valuesContent carries the generated secrets, so it is readable by anyone who can read HelmCharts in kube-system — the same cluster-admin audience as the Secret they came from.
		ValuesYAML: values,
	}
	doc, err := chart.Manifest()
	if err != nil {
		return err
	}
	if err := k3s.WriteManifest(ctx, r, ReleaseName, doc); err != nil {
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

// withRevision adds installRevision to the values document: a hash of the
// chart archive and the values, so it changes exactly when what is applied
// changes. An identical re-run therefore applies an identical HelmChart and
// rolls nothing.
func withRevision(valuesYAML string) (string, string, error) {
	sum := sha256.New()
	sum.Write(ChartArchive())
	sum.Write([]byte{0})
	sum.Write([]byte(valuesYAML))
	revision := hex.EncodeToString(sum.Sum(nil))[:16]

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

// readyProbe observes the whole control plane: each Deployment rolled out at
// this install's revision, and PostgreSQL with every replica ready.
//
// "Available" is not enough, and a re-run proved it: k3s's helm-install job
// applies the chart asynchronously, and during a rolling update the previous
// ReplicaSet keeps a Deployment Available. A probe on that condition passed in
// seconds while the old backend was still the one serving, so a new backend
// that could not start would have been reported installed.
func readyProbe(r k3s.Runner, revision string) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		for _, workload := range []string{"backend", "hub", "ui"} {
			done, state, err := rolledOut(ctx, r, ReleaseName+"-"+workload, revision)
			if err != nil || !done {
				return false, state, err
			}
		}
		return postgresReady(ctx, r)
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

// PlatformCA is the platform's certificate authority in PEM, read from
// cert-manager's copy of it. The CLI stores it as the control plane's CA, so
// every later command verifies the API without a public trust anchor — the
// control plane is served by a certificate this CA signed.
func PlatformCA(ctx context.Context, r k3s.Runner) ([]byte, error) {
	out, err := k3s.Kubectl(ctx, r, "get secret "+caSecretName+" -n "+caNamespace+" -o json")
	if err != nil {
		return nil, fmt.Errorf("reading the platform CA from secret %s/%s: %w", caNamespace, caSecretName, err)
	}
	var stored struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &stored); err != nil {
		return nil, fmt.Errorf("secret %s/%s is unparsable: %w", caNamespace, caSecretName, err)
	}
	// A self-signed CA stores itself under both keys; ca.crt is the canonical
	// one and is empty on some issuer versions, which is why tls.crt is read
	// as the fallback.
	pem, err := decodeCAKey(stored.Data, caCertKey)
	if err != nil {
		return nil, err
	}
	if len(pem) == 0 {
		pem, err = decodeCAKey(stored.Data, caTLSKey)
		if err != nil {
			return nil, err
		}
	}
	if !x509.NewCertPool().AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("secret %s/%s holds no x509 certificate: the platform CA is not a PEM certificate, so the CLI cannot verify the control plane it just installed",
			caNamespace, caSecretName)
	}
	return pem, nil
}

// decodeCAKey returns one base64 key of the platform CA Secret as raw bytes.
// A key that is not there is empty rather than an error: which of ca.crt and
// tls.crt the CA is stored under varies, so the caller has to be able to try
// the next one.
func decodeCAKey(data map[string]string, key string) ([]byte, error) {
	encoded, ok := data[key]
	if !ok {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("secret %s/%s key %q is not base64: %w", caNamespace, caSecretName, key, err)
	}
	return raw, nil
}
