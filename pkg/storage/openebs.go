package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

const (
	// ComponentKey is this component's name in the bundle manifest's core
	// section; the pinned chart version lives there and only there.
	ComponentKey = "openebs-lvm-localpv"

	// ChartRepo is recorded in the bundle manifest's chart-repo facts
	// (verified 2026-08-20).
	ChartRepo = "https://openebs.github.io/lvm-localpv"
	ChartName = "lvm-localpv"

	// Namespace is where the release lands.
	Namespace = "openebs"

	// CSIDriverName is the provisioner OpenEBS Local PV LVM registers.
	CSIDriverName = "local.csi.openebs.io"

	// StorageClassName is the platform's default StorageClass
	// (install.mdx: "OpenEBS Local PV LVM and the default StorageClass").
	StorageClassName = "kubenest-local"
)

// storageClassManifest is the default StorageClass, applied through the same
// auto-deploy directory as the chart.
//
//   - WaitForFirstConsumer: a Local PV must be carved on the node the pod
//     lands on, so binding must wait for scheduling.
//   - allowVolumeExpansion: lvextend is one of the few free lunches LVM
//     offers; leaving it off would force delete-and-restore for a resize.
const storageClassManifest = `apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ` + StorageClassName + `
  annotations:
    storageclass.kubernetes.io/is-default-class: "true"
provisioner: ` + CSIDriverName + `
parameters:
  storage: lvm
  volgroup: ` + VolumeGroup + `
  fsType: ext4
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
reclaimPolicy: Delete
`

// chartValues: defaults are right except analytics — a platform component
// must not phone home by default.
const chartValues = `analytics:
  enabled: false
`

// Install applies OpenEBS Local PV LVM at the version the bundle manifest
// pins, plus the default StorageClass, and waits for the component to
// converge (all pods Ready, CSI driver registered on every node). The
// deadline is limits.timeouts.component-ready — from the manifest, as every
// deadline is.
//
// r must reach the k3s server node. EnsureVolumeGroup must have run first on
// every data-bearing node; nothing here touches block devices.
func Install(ctx context.Context, r k3s.Runner, m *manifest.Manifest, rep converge.Reporter) error {
	version, err := m.Core.Version(ComponentKey)
	if err != nil {
		return err
	}
	deadline, err := m.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}

	chart := k3s.HelmChart{
		Name:            ComponentKey,
		Repo:            ChartRepo,
		Chart:           ChartName,
		Version:         version,
		TargetNamespace: Namespace,
		ValuesYAML:      chartValues,
	}
	cr, err := chart.Manifest()
	if err != nil {
		return err
	}
	if err := k3s.WriteManifest(ctx, r, ComponentKey, cr); err != nil {
		return err
	}
	if err := k3s.WriteManifest(ctx, r, "kubenest-storageclass", []byte(storageClassManifest)); err != nil {
		return err
	}

	res, err := converge.Wait(ctx, readyProbe(r), converge.Options{
		Name:     ComponentKey + "-ready",
		Deadline: deadline,
		Reporter: rep,
	})
	if err != nil {
		return err
	}
	return res.Err()
}

// readyProbe observes the component's convergence conditions in order and
// reports the first unmet one: pods Ready, then the CSI driver object, then
// per-node registration. Each observation names what is stuck and what
// would fix it.
func readyProbe(r k3s.Runner) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		done, state, err := k3s.CheckPodsReady(ctx, r, Namespace)
		if err != nil || !done {
			return false, state, err
		}

		if _, err := k3s.Kubectl(ctx, r, "get csidriver "+CSIDriverName+" -o name"); err != nil {
			return false, converge.State{
				Object: "csidriver " + CSIDriverName,
				Status: "not registered",
				Detail: "the lvm-localpv controller creates it once running; check the openebs namespace if this persists",
			}, nil
		}

		missing, err := nodesMissingDriver(ctx, r)
		if err != nil {
			return false, converge.State{Object: "csinodes", Status: "unobservable"}, err
		}
		if len(missing) > 0 {
			return false, converge.State{
				Object: "csinode " + missing[0],
				Status: "driver " + CSIDriverName + " not registered",
				Detail: "the lvm-localpv node daemonset registers the driver per node; check its pod on that node",
			}, nil
		}

		return true, converge.State{
			Object: ComponentKey,
			Status: "Ready",
			Detail: "pods Ready, " + CSIDriverName + " registered on every node",
		}, nil
	}
}

// Verify is the acceptance check: the default StorageClass exists, points at
// the right provisioner and volume group, and binding waits for the
// consumer. It is a convergence check like everything else — the auto-deploy
// controller applies the StorageClass asynchronously, and a snapshot taken
// before it lands would fail a healthy install.
func Verify(ctx context.Context, r k3s.Runner, m *manifest.Manifest, rep converge.Reporter) error {
	deadline, err := m.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}
	res, err := converge.Wait(ctx, storageClassProbe(r), converge.Options{
		Name:     "storageclass-default",
		Deadline: deadline,
		Reporter: rep,
	})
	if err != nil {
		return err
	}
	return res.Err()
}

type storageClassDoc struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Provisioner       string `json:"provisioner"`
	VolumeBindingMode string `json:"volumeBindingMode"`
	Parameters        struct {
		Volgroup string `json:"volgroup"`
	} `json:"parameters"`
}

func storageClassProbe(r k3s.Runner) converge.Probe {
	object := "storageclass " + StorageClassName
	return func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, r, "get storageclass "+StorageClassName+" -o json")
		if err != nil {
			return false, converge.State{
				Object: object,
				Status: "not found",
				Detail: "the auto-deploy manifest kubenest-storageclass.yaml has not applied yet",
			}, err
		}
		var sc storageClassDoc
		if err := json.Unmarshal([]byte(out), &sc); err != nil {
			return false, converge.State{Object: object, Status: "unparsable"}, err
		}
		var wrong []string
		if sc.Provisioner != CSIDriverName {
			wrong = append(wrong, fmt.Sprintf("provisioner is %s, want %s", sc.Provisioner, CSIDriverName))
		}
		if sc.Parameters.Volgroup != VolumeGroup {
			wrong = append(wrong, fmt.Sprintf("volgroup is %q, want %s", sc.Parameters.Volgroup, VolumeGroup))
		}
		if sc.VolumeBindingMode != "WaitForFirstConsumer" {
			wrong = append(wrong, "volumeBindingMode must be WaitForFirstConsumer for node-local volumes")
		}
		if sc.Metadata.Annotations["storageclass.kubernetes.io/is-default-class"] != "true" {
			wrong = append(wrong, "not the default class (annotation storageclass.kubernetes.io/is-default-class)")
		}
		if len(wrong) > 0 {
			return false, converge.State{Object: object, Status: "misconfigured", Detail: strings.Join(wrong, "; ")}, nil
		}
		return true, converge.State{Object: object, Status: "default", Detail: "provisioner " + CSIDriverName + ", volgroup " + VolumeGroup}, nil
	}
}

type csiNodeList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Drivers []struct {
				Name string `json:"name"`
			} `json:"drivers"`
		} `json:"spec"`
	} `json:"items"`
}

// nodesMissingDriver lists nodes whose CSINode does not carry the lvm
// driver yet.
func nodesMissingDriver(ctx context.Context, r k3s.Runner) ([]string, error) {
	out, err := k3s.Kubectl(ctx, r, "get csinodes -o json")
	if err != nil {
		return nil, err
	}
	var nodes csiNodeList
	if err := json.Unmarshal([]byte(out), &nodes); err != nil {
		return nil, err
	}
	if len(nodes.Items) == 0 {
		return []string{"(no csinodes yet)"}, nil
	}
	var missing []string
	for _, n := range nodes.Items {
		found := false
		for _, d := range n.Spec.Drivers {
			if d.Name == CSIDriverName {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, n.Metadata.Name)
		}
	}
	return missing, nil
}
