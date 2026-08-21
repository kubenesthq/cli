package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/component/certmanager"
	"kubenest.io/cli/pkg/component/day2"
	"kubenest.io/cli/pkg/component/traefik"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/storage"
)

// Stage 7: verify. The post-upgrade checks, run automatically and reported.
//
// A post-upgrade cluster is in far more motion than a freshly installed one —
// every drained pod is rescheduling at once — so these are convergence checks
// with deadlines on the same three-outcome rule as the install's, never
// snapshots. A check that samples once and judges will fail a healthy
// upgrade that was still settling.
//
// Failing here does not roll anything back. The cluster is on N+1 and the
// report names the failing check; what it changes is that the cluster is NOT
// recorded as successfully upgraded, because stage 8 never runs.
func stageVerify(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	checks := []struct {
		name string
		run  func(context.Context, *Session, k3s.Runner) error
	}{
		{"every node is Ready and on the expected version", verifyNodes},
		{"every core component is Running at its new version", verifyComponents},
		{"storage still provisions", verifyStorage},
		{"ingress still serves and certificates are still valid", verifyIngress},
		{"the recorded bundle version matches reality", verifyVersions},
	}
	for i, check := range checks {
		s.Logf("  verify %d/%d: %s", i+1, len(checks), check.name)
		if err := check.run(ctx, s, server); err != nil {
			return fmt.Errorf("post-upgrade check %d/%d (%s): %w", i+1, len(checks), check.name, err)
		}
	}
	return nil
}

func verifyNodes(ctx context.Context, s *Session, server k3s.Runner) error {
	target, err := s.To.Core.Version("k3s")
	if err != nil {
		return err
	}
	deadline, err := s.To.Limits.Timeouts.For("node-ready")
	if err != nil {
		return err
	}
	res, err := converge.Wait(ctx, allNodesProbe(server, target, len(s.Nodes)), converge.Options{
		Name: "nodes-upgraded", Deadline: deadline, Reporter: s.Reporter,
	})
	if err != nil {
		return err
	}
	return res.Err()
}

// allNodesProbe requires every node Ready, on the target version, and NOT
// left cordoned — a node that upgraded and was never uncordoned is a cluster
// quietly short of capacity, which is the kind of thing nobody notices until
// the next incident.
func allNodesProbe(r k3s.Runner, target string, want int) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, r, "get nodes -o json")
		if err != nil {
			return false, converge.State{Object: "nodes", Status: "unobservable"}, err
		}
		var nodes nodeVersions
		if err := json.Unmarshal([]byte(out), &nodes); err != nil {
			return false, converge.State{Object: "nodes", Status: "unparsable"}, err
		}
		ready := 0
		var stuck converge.State
		for _, n := range nodes.Items {
			isReady := false
			for _, c := range n.Status.Conditions {
				if c.Type == "Ready" && c.Status == "True" {
					isReady = true
				}
			}
			switch {
			case n.Status.NodeInfo.KubeletVersion != target:
				stuck = converge.State{Object: "node " + n.Metadata.Name,
					Status: "on " + n.Status.NodeInfo.KubeletVersion + ", expected " + target}
			case !isReady:
				stuck = converge.State{Object: "node " + n.Metadata.Name, Status: "not Ready"}
			case n.Spec.Unschedulable:
				stuck = converge.State{Object: "node " + n.Metadata.Name, Status: "still cordoned",
					Detail: "an upgraded node left cordoned is capacity the cluster silently does not have"}
			default:
				ready++
			}
		}
		if ready >= want && len(nodes.Items) >= want {
			return true, converge.State{Object: "nodes", Status: fmt.Sprintf("%d/%d on %s, Ready and schedulable", ready, want, target)}, nil
		}
		if stuck.Object == "" {
			stuck = converge.State{Object: "nodes", Status: fmt.Sprintf("%d/%d on %s", ready, want, target)}
		}
		return false, stuck, nil
	}
}

func verifyComponents(ctx context.Context, s *Session, server k3s.Runner) error {
	deadline, err := s.To.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}
	namespaces := []string{traefik.Namespace, certmanager.Namespace, storage.Namespace, backup.Namespace, day2.Namespace}
	probe := func(ctx context.Context) (bool, converge.State, error) {
		for _, ns := range namespaces {
			done, state, err := k3s.CheckWorkloadsReady(ctx, server, ns)
			if err != nil || !done {
				return false, state, err
			}
		}
		return true, converge.State{Object: "core components", Status: "all Ready"}, nil
	}
	res, err := converge.Wait(ctx, probe, converge.Options{
		Name: "core-components", Deadline: deadline, Reporter: s.Reporter,
	})
	if err != nil {
		return err
	}
	return res.Err()
}

func verifyStorage(ctx context.Context, s *Session, server k3s.Runner) error {
	return storage.Verify(ctx, server, s.To, s.Reporter)
}

// verifyIngress checks the Gateway is still Programmed and the platform's own
// certificate is still Ready. An upgrade that leaves ingress down is an
// outage regardless of how healthy the pods look.
func verifyIngress(ctx context.Context, s *Session, server k3s.Runner) error {
	deadline, err := s.To.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}
	for _, check := range []struct{ name, resource, namespace, cond string }{
		{"gateway-programmed", "gateway/" + traefik.GatewayName, traefik.Namespace, "Programmed"},
		{"default-cert-ready", "certificate/kubenest-gateway-default", traefik.Namespace, "Ready"},
	} {
		res, err := converge.Wait(ctx, conditionProbe(server, check.resource, check.namespace, check.cond),
			converge.Options{Name: check.name, Deadline: deadline, Reporter: s.Reporter})
		if err != nil {
			return err
		}
		if err := res.Err(); err != nil {
			return err
		}
	}
	return nil
}

func conditionProbe(r k3s.Runner, resource, namespace, condition string) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, r,
			fmt.Sprintf("get %s -n %s -o jsonpath='{.status.conditions[?(@.type==%q)].status}'", resource, namespace, condition))
		object := resource + " in " + namespace
		if err != nil {
			return false, converge.State{Object: object, Status: "not found yet"}, err
		}
		if got := strings.Trim(strings.TrimSpace(out), "'"); got != "True" {
			return false, converge.State{Object: object, Status: condition + "=" + got}, nil
		}
		return true, converge.State{Object: object, Status: condition}, nil
	}
}

// verifyVersions compares every pin against what is installed. Nothing else
// in the day-2 story is trustworthy if the record drifts, and an upgrade is
// exactly when it would.
func verifyVersions(ctx context.Context, s *Session, server k3s.Runner) error {
	installed, err := chartVersions(ctx, server)
	if err != nil {
		return err
	}
	var mismatches []string
	for _, c := range coreComponents() {
		want, err := s.To.Core.Version(c.key)
		if err != nil {
			return err
		}
		resource := chartResource(c.key)
		if resource == "" {
			// Release-manifest components (gateway-api,
			// system-upgrade-controller) have no HelmChart resource to
			// compare against, so their version is verified by their own
			// readiness rather than by a pin lookup that would always miss.
			continue
		}
		got, ok := installed[resource]
		if !ok {
			mismatches = append(mismatches, fmt.Sprintf("%s: the bundle pins %s but nothing on the cluster installs it", c.key, want))
			continue
		}
		if got != want {
			mismatches = append(mismatches, fmt.Sprintf("%s: the cluster has %s, bundle %s pins %s", c.key, got, s.To.Bundle, want))
		}
	}
	if len(mismatches) > 0 {
		return fmt.Errorf("what is installed does not match bundle %s:\n  %s",
			s.To.Bundle, strings.Join(mismatches, "\n  "))
	}
	return nil
}

// chartResource maps a manifest key to the HelmChart resource name the
// installer created. The names come from the packages that write them.
func chartResource(key string) string {
	switch key {
	case "traefik":
		return "kubenest-traefik"
	case "cert-manager":
		return "kubenest-cert-manager"
	case "velero":
		return "kubenest-velero"
	case "kured":
		return "kured"
	case "openebs-lvm-localpv":
		return storage.ChartResourceName
	default:
		// gateway-api and system-upgrade-controller are release manifests
		// rather than charts, and have no HelmChart resource to compare.
		return ""
	}
}

func chartVersions(ctx context.Context, r k3s.Runner) (map[string]string, error) {
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
