package install

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/component/certmanager"
	"kubenest.io/cli/pkg/component/day2"
	"kubenest.io/cli/pkg/component/traefik"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/storage"
)

// Stage 13: the five acceptance checks from install.mdx, run automatically
// and reported.
//
// They are also the checks an operator runs by hand when something looks
// wrong, and the criteria the release tests assert against a real cluster.
// Every one is a CONVERGENCE check with a deadline from the bundle, never a
// snapshot — a clean k3s install puts helm-install-traefik into Error before
// it retries and completes, and a check that sampled at that moment would
// fail a healthy install and send the operator to debug an installer that was
// about to succeed.

// platformNamespaces is where core lives. Each name comes from the component
// package that installs into it, so a component that moves namespace cannot
// leave this list checking an empty one.
func platformNamespaces() []string {
	return []string{
		traefik.Namespace,     // Traefik + the platform Gateway
		certmanager.Namespace, // cert-manager
		storage.Namespace,     // OpenEBS Local PV LVM
		backup.Namespace,      // Velero
		day2.Namespace,        // system-upgrade-controller
	}
}

// verifyNamespace holds the storage acceptance check's throwaway workload.
const verifyNamespace = "kubenest-verify"

// verifyImage is the smallest thing that can mount a volume and stay running.
// It is a test workload, not a platform component, which is why it is not a
// bundle pin — but it is pinned here rather than :latest so a verify run is
// reproducible.
const verifyImage = "busybox:1.37.0"

// Verify runs all five acceptance checks in order and returns the first
// failure, having named it.
func Verify(ctx context.Context, s *Session) error {
	checks := []struct {
		name string
		run  func(context.Context, *Session) error
	}{
		{"every node is Ready", verifyNodesReady},
		{"every core component is Running", verifyComponentsRunning},
		{"storage provisions a volume for real", verifyStorageProvisions},
		{"the cluster reports in", verifyClusterReportsIn},
		{"the recorded manifest matches reality", verifyRecordMatchesReality},
	}
	for i, check := range checks {
		s.Logf("  verify %d/5: %s", i+1, check.name)
		if err := check.run(ctx, s); err != nil {
			return fmt.Errorf("acceptance check %d/5 (%s): %w", i+1, check.name, err)
		}
	}
	return nil
}

func verifyNodesReady(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	return k3s.WaitNodesReady(ctx, server, s.Bundle, len(s.Nodes), s.Reporter)
}

// verifyComponentsRunning waits for every platform namespace to be Ready.
//
// CrashLoopBackOff and Pending are NOT immediate failures: images pull,
// dependencies order themselves, a Helm hook retries. They fail only if they
// are still present at the deadline, and what the failure reports is the pod,
// its state and its last event — because "traefik is Pending, no node matches
// its node selector" is a fix and "install failed" is not.
func verifyComponentsRunning(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	deadline, err := s.Bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}
	namespaces := platformNamespaces()
	if ns := s.agentNamespace(); ns != "" {
		namespaces = append(namespaces, ns)
	}

	probe := func(ctx context.Context) (bool, converge.State, error) {
		for _, ns := range namespaces {
			done, state, err := k3s.CheckPodsReady(ctx, server, ns)
			if err != nil || !done {
				return false, state, err
			}
		}
		// kured is a DaemonSet in kube-system, which also holds coredns and
		// the helm-install jobs; checking the whole namespace would wait on
		// things that are not ours.
		return day2KuredReady(ctx, server)
	}
	res, err := converge.Wait(ctx, probe, converge.Options{
		Name: "core-components-running", Deadline: deadline, Reporter: s.Reporter,
	})
	if err != nil {
		return err
	}
	return res.Err()
}

func day2KuredReady(ctx context.Context, r k3s.Runner) (bool, converge.State, error) {
	out, err := k3s.Kubectl(ctx, r,
		"get daemonset -n "+day2.KuredNamespace+" kured -o jsonpath='{.status.desiredNumberScheduled} {.status.numberReady}'")
	object := "daemonset kured in " + day2.KuredNamespace
	if err != nil {
		return false, converge.State{Object: object, Status: "not created yet"}, err
	}
	fields := strings.Fields(strings.Trim(out, "'"))
	if len(fields) != 2 {
		return false, converge.State{Object: object, Status: "no status yet"}, nil
	}
	state := converge.State{Object: object, Status: fields[1] + "/" + fields[0] + " Ready"}
	return fields[0] != "0" && fields[0] == fields[1], state, nil
}

// verifyStorageProvisions binds a real PersistentVolumeClaim with a real
// consumer, rather than checking that a StorageClass exists.
//
// A StorageClass that cannot actually provision is the most common way a
// storage install looks successful and is not — and with WaitForFirstConsumer
// binding, a PVC alone proves nothing either: it stays Pending by design
// until something schedules against it. So this runs a pod.
func verifyStorageProvisions(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	if err := storage.Verify(ctx, server, s.Bundle, s.Reporter); err != nil {
		return err
	}
	deadline, err := s.Bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}

	doc := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: verify
  namespace: %[1]s
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: %[2]s
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: verify
  namespace: %[1]s
spec:
  restartPolicy: Never
  containers:
    - name: verify
      image: %[3]s
      command: ["sh", "-c", "echo kubenest > /data/probe && sync && sleep 3600"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: verify
`, verifyNamespace, storage.StorageClassName, verifyImage)

	// Applied directly rather than through the auto-deploy directory: this is
	// a throwaway probe, and a file left in the auto-deploy directory would be
	// reconciled back after deletion.
	if err := kubectlApply(ctx, server, doc); err != nil {
		return err
	}
	defer cleanupVerifyNamespace(context.WithoutCancel(ctx), server)

	res, err := converge.Wait(ctx, boundVolumeProbe(server), converge.Options{
		Name: "storage-provisions", Deadline: deadline, Reporter: s.Reporter,
	})
	if err != nil {
		return err
	}
	return res.Err()
}

// boundVolumeProbe reports whether the claim bound AND its consumer is
// running on the volume.
func boundVolumeProbe(r k3s.Runner) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, r, "get pvc verify -n "+verifyNamespace+" -o json")
		if err != nil {
			return false, converge.State{Object: "pvc verify in " + verifyNamespace, Status: "not created yet"}, err
		}
		var claim struct {
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		}
		if err := json.Unmarshal([]byte(out), &claim); err != nil {
			return false, converge.State{Object: "pvc verify", Status: "unparsable"}, err
		}
		if claim.Status.Phase != "Bound" {
			// The pod's events carry why — "no volume group kubenest-vg on
			// node x" is the fix, "Pending" is not.
			detail, _ := k3s.Kubectl(ctx, r,
				"get events -n "+verifyNamespace+" --field-selector involvedObject.name=verify -o jsonpath='{.items[-1:].message}'")
			return false, converge.State{
				Object: "pvc verify in " + verifyNamespace,
				Status: claim.Status.Phase,
				Detail: strings.Trim(strings.TrimSpace(detail), "'"),
			}, nil
		}
		return k3s.CheckPodsReady(ctx, r, verifyNamespace)
	}
}

func cleanupVerifyNamespace(ctx context.Context, r k3s.Runner) {
	// Best effort: a leftover verify namespace is untidy, not dangerous, and
	// a failed cleanup must not turn a passing install into a failing one.
	_, _ = k3s.Kubectl(ctx, r, "delete namespace "+verifyNamespace+" --wait=false --ignore-not-found")
}

// verifyClusterReportsIn is the check that the cluster is not just installed
// but MANAGED: the control plane shows it connected, which only happens once
// the agent has dialled the hub and the first heartbeat has arrived.
func verifyClusterReportsIn(ctx context.Context, s *Session) error {
	if s.API == nil || s.Jnl.ClusterID == "" {
		return fmt.Errorf("no registered cluster to check: stage 2 must run before stage 13")
	}
	deadline, err := s.Bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}
	clusterID := s.Jnl.ClusterID

	probe := func(ctx context.Context) (bool, converge.State, error) {
		health, err := s.API.ClusterHealth(ctx, clusterID)
		if err != nil {
			return false, converge.State{Object: "cluster " + clusterID, Status: "the control plane is not answering"}, err
		}
		state := converge.State{Object: "cluster " + health.Name, Status: health.Status}
		switch health.Status {
		case "install_failed", "error":
			// Still an observation, not a verdict: a stage that failed and
			// was fixed leaves this status until the next transition.
			state.Detail = "the control plane still records a failed install"
			return false, state, nil
		}
		if health.LastHeartbeat == nil {
			state.Detail = "no fleet-telemetry heartbeat yet; the agent dials the hub outbound, so check that the cluster can reach it"
			return false, state, nil
		}
		state.Status = health.Status + ", first heartbeat " + health.LastHeartbeat.Format(time.RFC3339)
		return true, state, nil
	}

	res, err := converge.Wait(ctx, probe, converge.Options{
		Name: "cluster-connected", Deadline: deadline, Reporter: s.Reporter,
	})
	if err != nil {
		return err
	}
	return res.Err()
}

// verifyRecordMatchesReality compares the bundle's pins against what is
// actually installed, component by component.
//
// Nothing else in the day-2 story is trustworthy if this drifts: upgrade
// orchestration, the compatibility matrix and every support answer start from
// the record.
func verifyRecordMatchesReality(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}

	var mismatches []string

	// k3s, from the node itself.
	wantK3s, err := s.Bundle.Core.Version("k3s")
	if err != nil {
		return err
	}
	out, err := k3s.Kubectl(ctx, server, `get nodes -o jsonpath='{.items[*].status.nodeInfo.kubeletVersion}'`)
	if err != nil {
		return err
	}
	for _, got := range strings.Fields(strings.Trim(out, "'")) {
		if got != wantK3s {
			mismatches = append(mismatches, fmt.Sprintf("k3s: a node runs %s, the bundle pins %s", got, wantK3s))
		}
	}

	// Chart-installed components, from the HelmChart resources k3s reconciles.
	installed, err := installedChartVersions(ctx, server)
	if err != nil {
		return err
	}
	for name, chartName := range chartComponents(s) {
		want, err := s.Bundle.Core.Version(name)
		if err != nil {
			return err
		}
		got, ok := installed[chartName]
		if !ok {
			mismatches = append(mismatches, fmt.Sprintf("%s: the bundle pins %s but nothing on the cluster installs it", name, want))
			continue
		}
		if got != want {
			mismatches = append(mismatches, fmt.Sprintf("%s: the cluster has %s, the bundle pins %s", name, got, want))
		}
	}

	if len(mismatches) > 0 {
		return fmt.Errorf("what is installed does not match bundle %s:\n  %s\n\nthe record is what every day-2 operation trusts, so this is a defect in the install, not a cosmetic difference",
			s.Bundle.Bundle, strings.Join(mismatches, "\n  "))
	}
	return nil
}

// chartComponents maps a manifest core key to the HelmChart resource name the
// installer used.
//
// Each name comes from the installer package that writes it, never from a
// literal here. A literal is how this check told the operator that "nothing
// on the cluster installs openebs-lvm-localpv" about a cluster that had it —
// the check was wrong, and a check that cries wolf about the record is worse
// than no check, because the record is the thing every day-2 operation
// trusts.
func chartComponents(s *Session) map[string]string {
	out := map[string]string{}
	add := func(key string, chart k3s.HelmChart, err error) {
		if err != nil || chart.Name == "" {
			return
		}
		out[key] = chart.Name
	}
	traefikChart, err := traefik.Chart(s.Bundle)
	add("traefik", traefikChart, err)
	certChart, err := certmanager.Chart(s.Bundle)
	add("cert-manager", certChart, err)
	veleroChart, err := backup.Chart(s.Bundle)
	add("velero", veleroChart, err)
	kuredChart, err := day2.Chart(s.Bundle)
	add("kured", kuredChart, err)
	out[storage.ComponentKey] = storage.ChartResourceName

	if creds, ok := s.Creds.(*api.AgentCredentials); ok && creds != nil {
		agentChart, err := agent.Chart(s.Bundle, creds)
		add("kubenest-agent", agentChart, err)
	}
	return out
}

func installedChartVersions(ctx context.Context, r k3s.Runner) (map[string]string, error) {
	out, err := k3s.Kubectl(ctx, r, "get helmchart -n kube-system -o json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Version string `json:"version"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, err
	}
	versions := map[string]string{}
	for _, item := range list.Items {
		versions[item.Metadata.Name] = item.Spec.Version
	}
	return versions, nil
}

// agentNamespace is where stage 10 put the agent, from the credentials this
// process minted.
func (s *Session) agentNamespace() string {
	creds, ok := s.Creds.(*api.AgentCredentials)
	if !ok || creds == nil {
		return ""
	}
	return creds.Operator.Namespace
}

// encodeBase64 keeps a multi-document YAML off the command line's quoting
// rules entirely.
func encodeBase64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// kubectlApply applies a document from stdin on the server node.
func kubectlApply(ctx context.Context, r k3s.Runner, doc string) error {
	encoded := encodeBase64(doc)
	cmd := fmt.Sprintf("printf '%%s' %s | base64 -d | sudo -n k3s kubectl apply -f -", encoded)
	res, err := r.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("applying the storage probe: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
