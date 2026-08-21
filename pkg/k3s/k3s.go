// Package k3s applies platform components to a k3s cluster the way k3s
// itself ships addons: manifests written to the server's auto-deploy
// directory, where the embedded deploy controller and helm-controller
// reconcile them. A HelmChart custom resource dropped there IS a Helm
// install — retried by the cluster itself, surviving reboots, requiring no
// helm binary on the installer machine and no kubeconfig leaving the host.
//
// This is the one apply mechanism for every component installer (S3:
// render → apply → converge.Wait → verify). The helm-install job it spawns
// is exactly the thing kn-bkwa observed entering Error before retrying and
// completing, which is why waiting on it is only ever done through
// pkg/converge.
//
// Everything here talks to the server node over SSH (pkg/sshx) and runs
// kubectl as `k3s kubectl` on the host — no local kubeconfig is created,
// per the workspace testing rules.
package k3s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/sshx"
)

// Runner runs one command on the k3s server node. *sshx.Client implements
// it; tests substitute a fake.
type Runner interface {
	Run(ctx context.Context, command string) (sshx.Result, error)
}

// ManifestDir is k3s's auto-deploy directory on a server node. Files placed
// here are applied and kept applied by k3s itself.
const ManifestDir = "/var/lib/rancher/k3s/server/manifests"

var manifestName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// WriteManifest writes one YAML document set to the auto-deploy directory as
// <name>.yaml. Content travels base64-encoded so nothing needs shell quoting.
func WriteManifest(ctx context.Context, r Runner, name string, content []byte) error {
	if !manifestName.MatchString(name) {
		return fmt.Errorf("manifest name %q must be lowercase alphanumerics and hyphens", name)
	}
	path := ManifestDir + "/" + name + ".yaml"
	encoded := base64.StdEncoding.EncodeToString(content)
	cmd := fmt.Sprintf("printf '%%s' %s | base64 -d | sudo -n tee %s >/dev/null", encoded, path)
	res, err := r.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("write %s: exit %d: %s", path, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

// HelmChart describes one chart install expressed as a k3s HelmChart custom
// resource. Version is REQUIRED and comes from the bundle manifest's core
// pins — there is no "latest" here.
type HelmChart struct {
	// Name names the custom resource; k3s runs the install as a job called
	// helm-install-<name>.
	Name string
	Repo string
	// Chart is the chart name within the repo, or a full oci:// reference,
	// in which case Repo is left empty.
	Chart   string
	Version string
	// TargetNamespace is where the release lands; it is created if missing.
	TargetNamespace string
	// ValuesYAML is the chart values document, verbatim. Empty means chart
	// defaults.
	ValuesYAML string
}

// Manifest renders the HelmChart custom resource.
func (h HelmChart) Manifest() ([]byte, error) {
	oci := strings.HasPrefix(h.Chart, "oci://")
	if h.Name == "" || h.Chart == "" || h.TargetNamespace == "" || (h.Repo == "" && !oci) {
		return nil, fmt.Errorf("HelmChart needs Name, Repo, Chart and TargetNamespace (an oci:// chart carries its own registry and needs no Repo)")
	}
	if h.Version == "" {
		return nil, fmt.Errorf("HelmChart %s has no version: pins come from the bundle manifest's core section", h.Name)
	}
	spec := map[string]any{
		"chart":           h.Chart,
		"version":         h.Version,
		"targetNamespace": h.TargetNamespace,
		"createNamespace": true,
	}
	// An oci:// reference carries its own registry; helm-controller rejects a
	// HelmChart that sets both. The kubenest-agent chart is distributed that
	// way (oci://ghcr.io/kubenesthq/charts/kubenest-operator-2, kn-z6e4).
	if !oci {
		spec["repo"] = h.Repo
	}
	if h.ValuesYAML != "" {
		spec["valuesContent"] = h.ValuesYAML
	}
	doc := map[string]any{
		"apiVersion": "helm.cattle.io/v1",
		"kind":       "HelmChart",
		"metadata": map[string]any{
			"name": h.Name,
			// k3s's own addons live here; the helm-controller watches it.
			"namespace": "kube-system",
		},
		"spec": spec,
	}
	return yaml.Marshal(doc)
}

// Kubectl runs `k3s kubectl <args>` on the server node and returns stdout.
// A non-zero exit is an error carrying stderr — for converge probes that is
// an observation, never a verdict (pkg/converge doc).
func Kubectl(ctx context.Context, r Runner, args string) (string, error) {
	res, err := r.Run(ctx, "sudo -n k3s kubectl "+args)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("kubectl %s: exit %d: %s", args, res.ExitCode, firstLine(res.Stderr))
	}
	return res.Stdout, nil
}

// podList is the slice of `kubectl get pods -o json` the ready check reads.
type podList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Phase      string `json:"phase"`
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"conditions"`
			ContainerStatuses []struct {
				Ready bool `json:"ready"`
				State struct {
					Waiting *struct {
						Reason  string `json:"reason"`
						Message string `json:"message"`
					} `json:"waiting"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// CheckPodsReady observes whether every pod in a namespace is Ready (or
// Succeeded), reporting the first pod that is not, with the reason that
// names the fix. It is one observation — callers loop it via converge.Wait,
// alone (PodsReadyProbe) or inside a larger probe.
func CheckPodsReady(ctx context.Context, r Runner, namespace string) (bool, converge.State, error) {
	out, err := Kubectl(ctx, r, "get pods -n "+namespace+" -o json")
	if err != nil {
		return false, converge.State{Object: "pods in " + namespace, Status: "unobservable"}, err
	}
	var pods podList
	if err := json.Unmarshal([]byte(out), &pods); err != nil {
		return false, converge.State{Object: "pods in " + namespace, Status: "unparsable"}, err
	}
	if len(pods.Items) == 0 {
		return false, converge.State{
			Object: "namespace " + namespace,
			Status: "no pods yet",
			Detail: "the helm-install job has not created workloads yet",
		}, nil
	}
	for _, p := range pods.Items {
		if p.Status.Phase == "Succeeded" {
			continue
		}
		object := "pod " + p.Metadata.Name + " in " + namespace
		ready := false
		detail := ""
		for _, c := range p.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready = true
			}
			// The scheduler and kubelet put the fix-shaped text here,
			// e.g. "0/1 nodes are available: ...".
			if c.Status != "True" && c.Message != "" && detail == "" {
				detail = c.Message
			}
		}
		if ready {
			continue
		}
		status := p.Status.Phase
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && !cs.Ready {
				status = w.Reason
				if w.Message != "" {
					detail = w.Message
				}
				break
			}
		}
		return false, converge.State{Object: object, Status: status, Detail: detail}, nil
	}
	return true, converge.State{
		Object: "pods in " + namespace,
		Status: fmt.Sprintf("%d/%d Ready", len(pods.Items), len(pods.Items)),
	}, nil
}

// PodsReadyProbe wraps CheckPodsReady as a standalone converge.Probe.
func PodsReadyProbe(r Runner, namespace string) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		return CheckPodsReady(ctx, r, namespace)
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
