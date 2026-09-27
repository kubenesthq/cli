package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/k3s"
)

// The cluster side of `kubenest backup restore`: every read the plan and its
// re-verification need, and every change the run makes.
//
// THE INTERFACE IS HERE SO THE WHOLE RUN IS TESTABLE WITHOUT A CLUSTER. The
// command fills it with k3sCluster (below), which speaks `sudo -n k3s kubectl`
// over the SSH transport every other backup command uses — the CLI never holds
// a kubeconfig. Tests fill it with facts and refusals they choose, because the
// decisions this command is accepted on (an ineligible backup, an identity that
// moved between the plan and the execution, a PartiallyFailed restore) are
// decided in this package and must be provable with no host.
//
// WithRunner exists for the operation record: the caller decorates the
// transport with pkg/operation's Guarded for the stages whose commands change
// the cluster, so every action is written down before it is submitted and a
// successor can tell whether it happened (PLAN 7.2).
type RestoreCluster interface {
	WithRunner(r k3s.Runner) RestoreCluster

	// Reads.
	Namespace(ctx context.Context, name string) (*NamespaceState, error)
	Claims(ctx context.Context, namespace string) ([]VolumeRef, error)
	Workloads(ctx context.Context, namespace string) ([]WorkloadState, error)
	Pods(ctx context.Context, namespace string) ([]PodState, error)
	CronJobs(ctx context.Context, namespace string) ([]CronJobState, error)
	Jobs(ctx context.Context, namespace string) ([]JobState, error)
	Applications(ctx context.Context, namespace string) ([]ApplicationState, error)
	ProjectHold(ctx context.Context, namespace string) (*ProjectHold, error)
	DrillRestore(ctx context.Context) (*DrillRestore, error)
	VolumeRestores(ctx context.Context, restoreName string) ([]VolumeRestoreState, error)
	// PodVolumeBackups reads the volumes the CHOSEN BACKUP holds: one entry per
	// PodVolumeBackup Velero wrote, naming the pod the backup captured, the
	// volume within it, and the claim behind that volume by UID. Mode 2's
	// modifier is built from these and not from today's pods — Velero restores
	// the pod the backup holds, so a rule aimed at the pod the ReplicaSet
	// replaced matches nothing (kn-t43-restore-workload-s-stranded-11o7.3).
	PodVolumeBackups(ctx context.Context, backup string) ([]BackupVolumeState, error)
	RestoreOutcome(ctx context.Context, name string) (*RestoreOutcome, error)
	// ClaimBinding reports which volume a claim is bound to, and which node
	// that volume lives on. It is how mode 2 proves the refilled volume landed
	// on a LIVE node rather than on the one that is gone.
	ClaimBinding(ctx context.Context, namespace, claim string) (*ClaimBinding, error)
	NodeReady(ctx context.Context, node string) (bool, error)
	// ControllerOwner reads one object's controller owner reference. Mode 2
	// needs it to get from a pod's ReplicaSet to the Deployment that actually
	// has the replica count to put back.
	ControllerOwner(ctx context.Context, namespace, kind, name string) (*OwnerRef, error)

	// Changes.
	AnnotateProject(ctx context.Context, namespace, key, value string) error
	ClearProjectAnnotation(ctx context.Context, namespace, key string) error
	ScaleWorkload(ctx context.Context, kind, name, namespace string, replicas int32) error
	DeleteNamespace(ctx context.Context, name string) error
	DeleteClaim(ctx context.Context, namespace, name string) error
	SuspendCronJob(ctx context.Context, namespace, name string, suspend bool) error
	SuspendJob(ctx context.Context, namespace, name string, suspend bool) error
	CreateBackup(ctx context.Context, name string, doc []byte) error
	CreateRestore(ctx context.Context, name string, doc []byte) error
	Apply(ctx context.Context, what string, doc []byte) error
	Delete(ctx context.Context, args string) error
}

// NamespaceState is one namespace as the plan reads it.
type NamespaceState struct {
	Name string
	UID  string
}

// WorkloadState is one workload the plan will stop and activation will start
// again.
type WorkloadState struct {
	Kind     string
	Name     string
	Replicas int32
	// Selector is the workload's own pod selector (spec.selector.matchLabels).
	// Mode 2 narrows its Velero restore by it: the pods that mount the stranded
	// claims carry these labels by definition, and a claim without them could
	// never be recreated by a label-filtered restore.
	Selector map[string]string
}

// PodState is one pod, with the volumes it mounts, where each is mounted, and
// the controller that owns it. Mode 2 walks the owner references to find the
// workload to scale down, and the mount points are what a strategic merge patch
// needs to remove one volume by its merge keys.
type PodState struct {
	Name       string
	Namespace  string
	Phase      string
	Labels     map[string]string
	ClaimNames []string
	Owners     []OwnerRef
	Volumes    []PodVolumeState
}

// PodVolumeState is one named volume of a pod: the claim behind it, if any, and
// every place it is mounted.
type PodVolumeState struct {
	Name      string
	ClaimName string
	Mounts    []PodMount
}

// PodMount is one volumeMount of one container. The container NAME and the
// mountPath are the merge keys Velero's strategic merge patch deletes by.
type PodMount struct {
	Container string
	Path      string
	// Init marks a mount of an init container — Velero's own restore-wait is
	// one, and it mounts every backed-up volume at /restores/<volume>.
	Init bool
}

// BackupVolumeState is one volume the BACKUP holds for one pod: a
// PodVolumeBackup Velero wrote while the pod ran. The pod is named as the
// BACKUP holds it, which after a node loss is not the pod carrying the claim
// today, and ClaimUID is the claim the volume was copied from (the
// velero.io/pvc-uid label), which is how a claim is recognized by identity
// rather than by a name a recreated object could share.
type BackupVolumeState struct {
	Pod       string
	Namespace string
	Volume    string
	// ClaimUID is the UID of the claim behind the volume, or "" for a volume
	// with no claim behind it.
	ClaimUID string
}

// OwnerRef is one controller reference.
type OwnerRef struct {
	Kind       string
	Name       string
	Controller bool
}

// CronJobState is one CronJob: its live spec.suspend, and — on a CronJob the
// restore has already brought back — the value the BACKUP held, which is what
// activation puts back.
//
// The two are not the same field. The restore's resource modifier suspends
// every CronJob as Velero creates it, so by the time anything can read the
// object Suspend is true for all of them; RecordedSuspend is the annotation
// that same modifier wrote out of the backup, and it is the only surviving
// record of what the CronJob's own state was.
type CronJobState struct {
	Name     string
	Schedule string
	Suspend  bool
	// RecordedSuspend is the value CronJobSuspendedAnnotationKey carries:
	// "true", "false", or "" when the CronJob has no such annotation (the
	// restore did not put it there, so the backup's value is unknown).
	RecordedSuspend string
}

// JobState is one Job, and whether it was already suspended. A Job restored
// with --include-jobs is suspended at once and started only at activation: a
// Job object that appears unsuspended runs immediately, which is exactly the
// surprise the flag exists to make deliberate.
type JobState struct {
	Name    string
	Suspend bool
}

// ApplicationState is one Argo CD Application whose destination namespace is
// the target. Its phase is what "no sync is in progress" is judged on: turning
// future syncs off does not prove a running one finished. SyncStatus is the
// reconcilers' own comparison of the live objects against the desired state,
// which is the one configuration difference the cluster can report.
type ApplicationState struct {
	Name        string
	Namespace   string
	Destination string
	Phase       string
	SyncStatus  string
}

// ProjectHold is the operator's acknowledgement of the pause annotation, read
// from the Project CR.
type ProjectHold struct {
	PausedBy string
	// ConditionStatus is the ReconcilePaused condition's status, and
	// ConditionReason its reason (PausedByOperation). Both have to hold, and the
	// condition's message has to name this operation.
	ConditionStatus  string
	ConditionReason  string
	ConditionMessage string
}

// DrillRestore is a restore drill the operator is running right now.
type DrillRestore struct {
	Name  string
	Phase string
}

// VolumeRestoreState is one PodVolumeRestore: the proof that a volume was
// actually filled. "Completed with 0 errors" on the Restore is not that proof
// (probe P5, run 4).
type VolumeRestoreState struct {
	Name      string
	Pod       string
	Volume    string
	ClaimName string
	ClaimUID  string
	Phase     string
	Message   string
}

// RestoreOutcome is a Velero Restore's terminal verdict.
type RestoreOutcome struct {
	Name          string
	Phase         string
	FailureReason string
	Errors        int64
	Warnings      int64
	ItemsRestored int64
	ItemsFailed   int64
	ProgressSeen  bool
}

// Terminal reports whether the phase is one the run stops on.
func (o RestoreOutcome) Terminal() bool {
	for _, phase := range restoreTerminalPhases {
		if o.Phase == phase {
			return true
		}
	}
	return false
}

// ClaimBinding is what a claim is bound to: the volume, and the node that
// volume lives on (from the PV's node affinity, which is where a local volume
// records it).
type ClaimBinding struct {
	Claim        string
	Volume       string
	Node         string
	Phase        string
	Bound        bool
	StorageClass string
}

// k3sCluster is the RestoreCluster implementation: reads through
// `k3s.Kubectl` and changes through the same `sudo -n k3s kubectl` line every
// other command uses.
type k3sCluster struct {
	runner k3s.Runner
}

// NewK3sCluster binds the restore's cluster reads and changes to one transport.
func NewK3sCluster(r k3s.Runner) RestoreCluster { return &k3sCluster{runner: r} }

func (c *k3sCluster) WithRunner(r k3s.Runner) RestoreCluster { return &k3sCluster{runner: r} }

// run pipes one document into kubectl's stdin. A document can carry the
// namespace's own manifests, so it travels over STDIN and never in the command
// string (kn-40rd: the command string is the argv of the shell sshd spawns).
func (c *k3sCluster) runInput(ctx context.Context, what, command string, doc []byte) error {
	res, err := c.runner.RunInput(ctx, command, bytes.NewReader(doc))
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s: exit %d: %s", what, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

func (c *k3sCluster) kubectl(ctx context.Context, args string) (string, error) {
	return k3s.Kubectl(ctx, c.runner, args)
}

func notFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "NotFound")
}

func (c *k3sCluster) Namespace(ctx context.Context, name string) (*NamespaceState, error) {
	out, err := c.kubectl(ctx, "get namespace "+name+" -o json")
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading namespace %s: %w", name, err)
	}
	var document struct {
		Metadata struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing `kubectl get namespace %s -o json`: %w", name, err)
	}
	if document.Metadata.UID == "" {
		return nil, fmt.Errorf("namespace %s reports no UID, so the plan cannot be pinned to the namespace it was built against", name)
	}
	return &NamespaceState{Name: document.Metadata.Name, UID: document.Metadata.UID}, nil
}

func (c *k3sCluster) Claims(ctx context.Context, namespace string) ([]VolumeRef, error) {
	out, err := c.kubectl(ctx, "get persistentvolumeclaims -n "+namespace+" -o json")
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the claims in %s: %w", namespace, err)
	}
	var document struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				UID    string            `json:"uid"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				VolumeName string `json:"volumeName"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing the claims in %s: %w", namespace, err)
	}
	refs := make([]VolumeRef, 0, len(document.Items))
	for _, item := range document.Items {
		detail := "phase " + orUnknown(item.Status.Phase)
		if item.Spec.VolumeName != "" {
			detail += ", bound to " + item.Spec.VolumeName
		}
		if len(item.Metadata.Labels) > 0 {
			detail += ", labels " + describeLabels(item.Metadata.Labels)
		}
		refs = append(refs, VolumeRef{
			Namespace: namespace,
			Name:      item.Metadata.Name,
			UID:       item.Metadata.UID,
			Detail:    detail,
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

func describeLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ",")
}

func (c *k3sCluster) Workloads(ctx context.Context, namespace string) ([]WorkloadState, error) {
	out, err := c.kubectl(ctx, "get deployments,statefulsets -n "+namespace+" -o json")
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the workloads in %s: %w", namespace, err)
	}
	var document struct {
		Items []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int32 `json:"replicas"`
				Selector struct {
					MatchLabels map[string]string `json:"matchLabels"`
				} `json:"selector"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing the workloads in %s: %w", namespace, err)
	}
	workloads := make([]WorkloadState, 0, len(document.Items))
	for _, item := range document.Items {
		var replicas int32 = 1
		if item.Spec.Replicas != nil {
			replicas = *item.Spec.Replicas
		}
		workloads = append(workloads, WorkloadState{
			Kind:     strings.ToLower(item.Kind),
			Name:     item.Metadata.Name,
			Replicas: replicas,
			Selector: item.Spec.Selector.MatchLabels,
		})
	}
	sort.Slice(workloads, func(i, j int) bool {
		if workloads[i].Kind != workloads[j].Kind {
			return workloads[i].Kind < workloads[j].Kind
		}
		return workloads[i].Name < workloads[j].Name
	})
	return workloads, nil
}

func (c *k3sCluster) Pods(ctx context.Context, namespace string) ([]PodState, error) {
	out, err := c.kubectl(ctx, "get pods -n "+namespace+" -o json")
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the pods in %s: %w", namespace, err)
	}
	var document struct {
		Items []struct {
			Metadata struct {
				Name            string            `json:"name"`
				Namespace       string            `json:"namespace"`
				Labels          map[string]string `json:"labels"`
				OwnerReferences []struct {
					Kind       string `json:"kind"`
					Name       string `json:"name"`
					Controller *bool  `json:"controller"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				Volumes []struct {
					Name                  string `json:"name"`
					PersistentVolumeClaim *struct {
						ClaimName string `json:"claimName"`
					} `json:"persistentVolumeClaim"`
				} `json:"volumes"`
				Containers []struct {
					Name         string `json:"name"`
					VolumeMounts []struct {
						Name      string `json:"name"`
						MountPath string `json:"mountPath"`
					} `json:"volumeMounts"`
				} `json:"containers"`
				InitContainers []struct {
					Name         string `json:"name"`
					VolumeMounts []struct {
						Name      string `json:"name"`
						MountPath string `json:"mountPath"`
					} `json:"volumeMounts"`
				} `json:"initContainers"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing the pods in %s: %w", namespace, err)
	}
	pods := make([]PodState, 0, len(document.Items))
	for _, item := range document.Items {
		pod := PodState{
			Name:      item.Metadata.Name,
			Namespace: item.Metadata.Namespace,
			Phase:     item.Status.Phase,
			Labels:    item.Metadata.Labels,
		}
		for _, volume := range item.Spec.Volumes {
			state := PodVolumeState{Name: volume.Name}
			if volume.PersistentVolumeClaim != nil {
				state.ClaimName = volume.PersistentVolumeClaim.ClaimName
				if state.ClaimName != "" {
					pod.ClaimNames = append(pod.ClaimNames, state.ClaimName)
				}
			}
			for _, container := range item.Spec.Containers {
				for _, mount := range container.VolumeMounts {
					if mount.Name == volume.Name {
						state.Mounts = append(state.Mounts, PodMount{Container: container.Name, Path: mount.MountPath})
					}
				}
			}
			for _, container := range item.Spec.InitContainers {
				for _, mount := range container.VolumeMounts {
					if mount.Name == volume.Name {
						state.Mounts = append(state.Mounts, PodMount{Container: container.Name, Path: mount.MountPath, Init: true})
					}
				}
			}
			pod.Volumes = append(pod.Volumes, state)
		}
		for _, owner := range item.Metadata.OwnerReferences {
			controller := owner.Controller != nil && *owner.Controller
			pod.Owners = append(pod.Owners, OwnerRef{Kind: owner.Kind, Name: owner.Name, Controller: controller})
		}
		pods = append(pods, pod)
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	return pods, nil
}

func (c *k3sCluster) CronJobs(ctx context.Context, namespace string) ([]CronJobState, error) {
	out, err := c.kubectl(ctx, "get cronjobs -n "+namespace+" -o json")
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the CronJobs in %s: %w", namespace, err)
	}
	var document struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				Schedule string `json:"schedule"`
				Suspend  *bool  `json:"suspend"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing the CronJobs in %s: %w", namespace, err)
	}
	jobs := make([]CronJobState, 0, len(document.Items))
	for _, item := range document.Items {
		suspended := item.Spec.Suspend != nil && *item.Spec.Suspend
		jobs = append(jobs, CronJobState{
			Name:            item.Metadata.Name,
			Schedule:        item.Spec.Schedule,
			Suspend:         suspended,
			RecordedSuspend: item.Metadata.Annotations[CronJobSuspendedAnnotationKey],
		})
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Name < jobs[j].Name })
	return jobs, nil
}

func (c *k3sCluster) Jobs(ctx context.Context, namespace string) ([]JobState, error) {
	out, err := c.kubectl(ctx, "get jobs -n "+namespace+" -o json")
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the Jobs in %s: %w", namespace, err)
	}
	var document struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Suspend *bool `json:"suspend"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing the Jobs in %s: %w", namespace, err)
	}
	jobs := make([]JobState, 0, len(document.Items))
	for _, item := range document.Items {
		jobs = append(jobs, JobState{Name: item.Metadata.Name, Suspend: item.Spec.Suspend != nil && *item.Spec.Suspend})
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Name < jobs[j].Name })
	return jobs, nil
}

// Applications reads the Argo CD Applications that write into the target
// namespace. The API group is absent on a cluster without Argo CD, which is not
// an error: there is then no sync to wait for.
func (c *k3sCluster) Applications(ctx context.Context, namespace string) ([]ApplicationState, error) {
	out, err := c.kubectl(ctx, "get applications.argoproj.io --all-namespaces -o json")
	if err != nil {
		if notFound(err) || strings.Contains(err.Error(), "the server doesn't have a resource type") {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the Argo CD Applications: %w", err)
	}
	var document struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				Destination struct {
					Namespace string `json:"namespace"`
				} `json:"destination"`
			} `json:"spec"`
			Status struct {
				OperationState struct {
					Phase string `json:"phase"`
				} `json:"operationState"`
				Sync struct {
					Status string `json:"status"`
				} `json:"sync"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing the Argo CD Applications: %w", err)
	}
	var apps []ApplicationState
	for _, item := range document.Items {
		if item.Spec.Destination.Namespace != namespace {
			continue
		}
		apps = append(apps, ApplicationState{
			Name:        item.Metadata.Name,
			Namespace:   item.Metadata.Namespace,
			Destination: item.Spec.Destination.Namespace,
			Phase:       item.Status.OperationState.Phase,
			SyncStatus:  item.Status.Sync.Status,
		})
	}
	return apps, nil
}

func (c *k3sCluster) ProjectHold(ctx context.Context, namespace string) (*ProjectHold, error) {
	out, err := c.kubectl(ctx, "get project "+namespace+" -n "+ProjectCRNamespace+" -o json")
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading Project %s/%s: %w", ProjectCRNamespace, namespace, err)
	}
	var document struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing Project %s/%s: %w", ProjectCRNamespace, namespace, err)
	}
	hold := &ProjectHold{PausedBy: document.Metadata.Annotations[PauseAnnotationKey]}
	for _, condition := range document.Status.Conditions {
		if condition.Type != ConditionReconcilePaused {
			continue
		}
		hold.ConditionStatus = condition.Status
		hold.ConditionReason = condition.Reason
		hold.ConditionMessage = condition.Message
	}
	return hold, nil
}

// DrillRestore reports the operator's restore drill if one is in progress. A
// drill holds Velero's only restore worker, so a restore that started while one
// ran queues behind it for ever (probe P5, run 1).
func (c *k3sCluster) DrillRestore(ctx context.Context) (*DrillRestore, error) {
	out, err := c.kubectl(ctx, "get restores.velero.io -n "+Namespace+" -o json")
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the Velero Restores: %w", err)
	}
	var document struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing the Velero Restores: %w", err)
	}
	for _, item := range document.Items {
		isDrill := item.Metadata.Labels[DrillRestoreLabelKey] == DrillRestoreLabelValue ||
			strings.HasPrefix(item.Metadata.Name, DrillRestoreNamePrefix)
		if !isDrill {
			continue
		}
		// EVERY labelled drill restore is returned, whatever its phase: whether
		// it is still holding Velero's worker is the RUN's judgement
		// (restorePhaseTerminal), so the two places that reason about phases
		// cannot disagree.
		return &DrillRestore{Name: item.Metadata.Name, Phase: item.Status.Phase}, nil
	}
	return nil, nil
}

func (c *k3sCluster) VolumeRestores(ctx context.Context, restoreName string) ([]VolumeRestoreState, error) {
	out, err := c.kubectl(ctx, "get podvolumerestores.velero.io -n "+Namespace+" -l velero.io/restore-name="+restoreName+" -o json")
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the PodVolumeRestores for restore %s: %w", restoreName, err)
	}
	var document struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Pod struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"pod"`
				Volume string `json:"volume"`
			} `json:"spec"`
			Status struct {
				Phase   string `json:"phase"`
				Message string `json:"message"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing the PodVolumeRestores for restore %s: %w", restoreName, err)
	}
	// Velero names the restored claim only by the label velero.io/pvc-uid, so
	// the claim's name is read from the namespace's claims, once per namespace.
	// The pod's volume name is the fallback, for a claim that is already gone.
	claimsByUID := map[string]map[string]string{}
	restores := make([]VolumeRestoreState, 0, len(document.Items))
	for _, item := range document.Items {
		claim := ""
		if uid, namespace := item.Metadata.Labels["velero.io/pvc-uid"], item.Spec.Pod.Namespace; uid != "" && namespace != "" {
			byUID, read := claimsByUID[namespace]
			if !read {
				if byUID, err = c.claimNamesByUID(ctx, namespace); err != nil {
					return nil, err
				}
				claimsByUID[namespace] = byUID
			}
			claim = byUID[uid]
		}
		if claim == "" {
			claim = item.Spec.Volume
		}
		restores = append(restores, VolumeRestoreState{
			Name:      item.Metadata.Name,
			Pod:       item.Spec.Pod.Name,
			Volume:    item.Spec.Volume,
			ClaimName: claim,
			ClaimUID:  item.Metadata.Labels["velero.io/pvc-uid"],
			Phase:     item.Status.Phase,
			Message:   item.Status.Message,
		})
	}
	sort.Slice(restores, func(i, j int) bool { return restores[i].Name < restores[j].Name })
	return restores, nil
}

// claimNamesByUID maps each claim in a namespace from its UID to its name.
func (c *k3sCluster) claimNamesByUID(ctx context.Context, namespace string) (map[string]string, error) {
	out, err := c.kubectl(ctx, "get persistentvolumeclaims -n "+namespace+" -o json")
	if err != nil {
		return nil, fmt.Errorf("reading the claims in %s to name the PodVolumeRestores' volumes: %w", namespace, err)
	}
	var document struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
				UID  string `json:"uid"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing the claims in %s: %w", namespace, err)
	}
	byUID := make(map[string]string, len(document.Items))
	for _, item := range document.Items {
		byUID[item.Metadata.UID] = item.Metadata.Name
	}
	return byUID, nil
}

// PodVolumeBackups reads the volumes the chosen backup holds, one entry per
// PodVolumeBackup, in pod then volume order.
//
// THE FIELDS ARE THE PINNED VELERO'S (1.18.1, chart 12.1.0), read from the
// source rather than from what the objects happened to look like:
//
//   - spec.pod is a corev1.ObjectReference whose Name and Namespace name the
//     pod AT BACKUP TIME (pkg/podvolume/backupper.go:513-528,
//     newPodVolumeBackup), and spec.volume is the volume's name within that pod;
//   - the label velero.io/backup-name names the Backup (pkg/apis/velero/v1/
//     labels_annotations.go:20, BackupNameLabel), and the object's owner
//     reference names it too (backupper.go:504-511). The owner reference is
//     preferred for the same reason copyRecordBackup prefers it: the label goes
//     through label.GetValidName, which truncates a long backup name;
//   - the label velero.io/pvc-uid is the claim's UID (backupper.go:546,
//     PVCUIDLabel). Velero also writes the claim's NAME as the annotation
//     velero.io/pvc-name (backupper.go:541,
//     pkg/podvolume/configs/configs.go:6), but on hardware (lab w3, 2026-09-27)
//     the object carried the UID label and no claim-name annotation — the same
//     lesson measured for PodVolumeRestores in velero_resources_test.go — so
//     nothing here reads the annotation.
func (c *k3sCluster) PodVolumeBackups(ctx context.Context, backup string) ([]BackupVolumeState, error) {
	out, err := c.kubectl(ctx, "get podvolumebackups.velero.io -n "+Namespace+" -o json")
	if err != nil {
		if notFound(err) || strings.Contains(err.Error(), "the server doesn't have a resource type") {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the PodVolumeBackups of backup %s: %w", backup, err)
	}
	var document struct {
		Items []struct {
			Metadata struct {
				Labels          map[string]string `json:"labels"`
				OwnerReferences []struct {
					Kind       string `json:"kind"`
					Name       string `json:"name"`
					APIVersion string `json:"apiVersion"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				Pod struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"pod"`
				Volume string `json:"volume"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing the PodVolumeBackups of backup %s: %w", backup, err)
	}
	volumes := []BackupVolumeState{}
	for _, item := range document.Items {
		if copyRecordBackup(item.Metadata.OwnerReferences, item.Metadata.Labels["velero.io/backup-name"]) != backup {
			continue
		}
		if item.Spec.Pod.Name == "" || item.Spec.Volume == "" {
			continue
		}
		volumes = append(volumes, BackupVolumeState{
			Pod:       item.Spec.Pod.Name,
			Namespace: item.Spec.Pod.Namespace,
			Volume:    item.Spec.Volume,
			ClaimUID:  item.Metadata.Labels["velero.io/pvc-uid"],
		})
	}
	sort.Slice(volumes, func(i, j int) bool {
		if volumes[i].Pod != volumes[j].Pod {
			return volumes[i].Pod < volumes[j].Pod
		}
		return volumes[i].Volume < volumes[j].Volume
	})
	return volumes, nil
}

func (c *k3sCluster) RestoreOutcome(ctx context.Context, name string) (*RestoreOutcome, error) {
	out, err := c.kubectl(ctx, "get restore "+name+" -n "+Namespace+" -o json")
	if err != nil {
		return nil, fmt.Errorf("reading Velero Restore %s: %w", name, err)
	}
	var document struct {
		Status struct {
			Phase         string `json:"phase"`
			FailureReason string `json:"failureReason"`
			Errors        int64  `json:"errors"`
			Warnings      int64  `json:"warnings"`
			Progress      *struct {
				ItemsRestored int64 `json:"itemsRestored"`
				ItemsFailed   int64 `json:"itemsFailed"`
			} `json:"progress"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing Velero Restore %s: %w", name, err)
	}
	outcome := &RestoreOutcome{
		Name:          name,
		Phase:         document.Status.Phase,
		FailureReason: document.Status.FailureReason,
		Errors:        document.Status.Errors,
		Warnings:      document.Status.Warnings,
	}
	if document.Status.Progress != nil {
		outcome.ProgressSeen = true
		outcome.ItemsRestored = document.Status.Progress.ItemsRestored
		outcome.ItemsFailed = document.Status.Progress.ItemsFailed
	}
	return outcome, nil
}

// ClaimBinding reads the claim's binding and the node its volume lives on. A
// local volume records its node in the PV's nodeAffinity, which is what makes
// "a live node" checkable rather than assumed: a volume provisioned for the
// node that died would carry that node's name here.
func (c *k3sCluster) ClaimBinding(ctx context.Context, namespace, claim string) (*ClaimBinding, error) {
	out, err := c.kubectl(ctx, "get persistentvolumeclaim "+claim+" -n "+namespace+" -o json")
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading claim %s/%s: %w", namespace, claim, err)
	}
	var pvc struct {
		Spec struct {
			VolumeName       string `json:"volumeName"`
			StorageClassName string `json:"storageClassName"`
		} `json:"spec"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &pvc); err != nil {
		return nil, fmt.Errorf("parsing claim %s/%s: %w", namespace, claim, err)
	}
	binding := &ClaimBinding{
		Claim:        claim,
		Volume:       pvc.Spec.VolumeName,
		Phase:        pvc.Status.Phase,
		Bound:        pvc.Status.Phase == "Bound" && pvc.Spec.VolumeName != "",
		StorageClass: pvc.Spec.StorageClassName,
	}
	if binding.Volume == "" {
		return binding, nil
	}
	pvOut, err := c.kubectl(ctx, "get persistentvolume "+binding.Volume+" -o json")
	if err != nil {
		if notFound(err) {
			return binding, nil
		}
		return nil, fmt.Errorf("reading volume %s: %w", binding.Volume, err)
	}
	var pv struct {
		Spec struct {
			NodeAffinity *struct {
				Required *struct {
					NodeSelectorTerms []struct {
						MatchExpressions []struct {
							Key      string   `json:"key"`
							Operator string   `json:"operator"`
							Values   []string `json:"values"`
						} `json:"matchExpressions"`
					} `json:"nodeSelectorTerms"`
				} `json:"required"`
			} `json:"nodeAffinity"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(pvOut), &pv); err != nil {
		return nil, fmt.Errorf("parsing volume %s: %w", binding.Volume, err)
	}
	if pv.Spec.NodeAffinity != nil && pv.Spec.NodeAffinity.Required != nil {
		for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
			for _, expression := range term.MatchExpressions {
				if expression.Key == "kubernetes.io/hostname" && len(expression.Values) > 0 {
					binding.Node = expression.Values[0]
				}
			}
		}
	}
	return binding, nil
}

func (c *k3sCluster) NodeReady(ctx context.Context, node string) (bool, error) {
	if node == "" {
		return false, nil
	}
	out, err := c.kubectl(ctx, "get node "+node+" -o jsonpath={.status.conditions[?(@.type==\"Ready\")].status}")
	if err != nil {
		if notFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading node %s: %w", node, err)
	}
	return strings.TrimSpace(out) == "True", nil
}

// ControllerOwner reads one object's controller owner. A pod's own owner is
// usually a ReplicaSet, and the objects a CLI scales are the Deployment or
// StatefulSet above it; a ReplicaSet with no controller owner is a refusal the
// caller makes, not an error here.
func (c *k3sCluster) ControllerOwner(ctx context.Context, namespace, kind, name string) (*OwnerRef, error) {
	out, err := c.kubectl(ctx, "get "+kind+" "+name+" -n "+namespace+" -o json")
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s %s/%s: %w", kind, namespace, name, err)
	}
	var document struct {
		Metadata struct {
			OwnerReferences []struct {
				Kind       string `json:"kind"`
				Name       string `json:"name"`
				Controller *bool  `json:"controller"`
			} `json:"ownerReferences"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing %s %s/%s: %w", kind, namespace, name, err)
	}
	for _, owner := range document.Metadata.OwnerReferences {
		if owner.Controller != nil && *owner.Controller {
			return &OwnerRef{Kind: owner.Kind, Name: owner.Name, Controller: true}, nil
		}
	}
	return nil, nil
}

func (c *k3sCluster) AnnotateProject(ctx context.Context, namespace, key, value string) error {
	if _, err := c.kubectl(ctx, "annotate project "+namespace+" -n "+ProjectCRNamespace+" "+key+"="+value+" --overwrite"); err != nil {
		return fmt.Errorf("writing %s=%s on Project %s/%s: %w", key, value, ProjectCRNamespace, namespace, err)
	}
	return nil
}

func (c *k3sCluster) ClearProjectAnnotation(ctx context.Context, namespace, key string) error {
	if _, err := c.kubectl(ctx, "annotate project "+namespace+" -n "+ProjectCRNamespace+" "+key+"-"); err != nil {
		return fmt.Errorf("removing %s from Project %s/%s: %w", key, ProjectCRNamespace, namespace, err)
	}
	return nil
}

func (c *k3sCluster) ScaleWorkload(ctx context.Context, kind, name, namespace string, replicas int32) error {
	if _, err := c.kubectl(ctx, fmt.Sprintf("scale %s %s -n %s --replicas=%d", kind, name, namespace, replicas)); err != nil {
		return fmt.Errorf("scaling %s %s in %s to %d replicas: %w", kind, name, namespace, replicas, err)
	}
	return nil
}

func (c *k3sCluster) DeleteNamespace(ctx context.Context, name string) error {
	if _, err := c.kubectl(ctx, "delete namespace "+name+" --wait=false"); err != nil {
		if notFound(err) {
			return nil
		}
		return fmt.Errorf("deleting namespace %s: %w", name, err)
	}
	return nil
}

func (c *k3sCluster) DeleteClaim(ctx context.Context, namespace, name string) error {
	if _, err := c.kubectl(ctx, "delete persistentvolumeclaim "+name+" -n "+namespace+" --wait=false"); err != nil {
		if notFound(err) {
			return nil
		}
		return fmt.Errorf("deleting claim %s/%s: %w", namespace, name, err)
	}
	return nil
}

func (c *k3sCluster) SuspendCronJob(ctx context.Context, namespace, name string, suspend bool) error {
	patch := fmt.Sprintf(`{"spec":{"suspend":%t}}`, suspend)
	if _, err := c.kubectl(ctx, "patch cronjob "+name+" -n "+namespace+` --type=merge -p '`+patch+`'`); err != nil {
		return fmt.Errorf("setting suspend=%t on CronJob %s/%s: %w", suspend, namespace, name, err)
	}
	return nil
}

func (c *k3sCluster) SuspendJob(ctx context.Context, namespace, name string, suspend bool) error {
	patch := fmt.Sprintf(`{"spec":{"suspend":%t}}`, suspend)
	if _, err := c.kubectl(ctx, "patch job "+name+" -n "+namespace+` --type=merge -p '`+patch+`'`); err != nil {
		return fmt.Errorf("setting suspend=%t on Job %s/%s: %w", suspend, namespace, name, err)
	}
	return nil
}

func (c *k3sCluster) CreateBackup(ctx context.Context, name string, doc []byte) error {
	if len(doc) == 0 {
		return fmt.Errorf("refusing to create Velero Backup %s with no document", name)
	}
	return c.runInput(ctx, "creating Velero Backup "+name, "sudo -n k3s kubectl apply -f -", doc)
}

func (c *k3sCluster) CreateRestore(ctx context.Context, name string, doc []byte) error {
	if len(doc) == 0 {
		return fmt.Errorf("refusing to create Velero Restore %s with no document", name)
	}
	return c.runInput(ctx, "creating Velero Restore "+name, "sudo -n k3s kubectl apply -f -", doc)
}

func (c *k3sCluster) Apply(ctx context.Context, what string, doc []byte) error {
	return c.runInput(ctx, "creating "+what, "sudo -n k3s kubectl apply -f -", doc)
}

func (c *k3sCluster) Delete(ctx context.Context, args string) error {
	if _, err := c.kubectl(ctx, "delete "+args); err != nil && !notFound(err) {
		return fmt.Errorf("deleting %s: %w", args, err)
	}
	return nil
}

// veleroBackups is the BackupSource the command uses: Velero's own objects,
// read through the SSH transport, judged against the expected-coverage record
// T2.10 writes.
type veleroBackups struct {
	runner k3s.Runner
}

// NewVeleroBackups binds the eligibility source to one transport.
func NewVeleroBackups(r k3s.Runner) BackupSource { return &veleroBackups{runner: r} }

// NamedBackup reads one backup, judged for the namespace.
func (v *veleroBackups) NamedBackup(ctx context.Context, namespace, name string) (BackupFacts, error) {
	out, err := k3s.Kubectl(ctx, v.runner, "get backup "+name+" -n "+Namespace+" -o json")
	if err != nil {
		if notFound(err) {
			return BackupFacts{}, fmt.Errorf("%w: %s", ErrNoBackup, name)
		}
		return BackupFacts{}, fmt.Errorf("reading Velero Backup %s: %w", name, err)
	}
	copies, err := v.copyRecords(ctx)
	if err != nil {
		return BackupFacts{}, err
	}
	locations, err := v.storageLocations(ctx)
	if err != nil {
		return BackupFacts{}, err
	}
	record, err := v.coverageRecord(ctx, name)
	if err != nil {
		return BackupFacts{}, err
	}
	return judgeBackup(out, copies[name], record, locations, namespace), nil
}

// TerminalBackups lists every terminal backup, newest first.
func (v *veleroBackups) TerminalBackups(ctx context.Context, namespace string) ([]BackupFacts, error) {
	out, err := k3s.Kubectl(ctx, v.runner, "get backups.velero.io -n "+Namespace+" -o json")
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the Velero Backups: %w", err)
	}
	var document struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing the Velero Backups: %w", err)
	}
	var terminal []json.RawMessage
	for _, item := range document.Items {
		var meta struct {
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		}
		if err := json.Unmarshal(item, &meta); err != nil {
			return nil, fmt.Errorf("parsing a Velero Backup: %w", err)
		}
		for _, phase := range []string{"Completed", "Failed", "PartiallyFailed", "FailedValidation"} {
			if meta.Status.Phase == phase {
				terminal = append(terminal, item)
				break
			}
		}
	}
	if len(terminal) == 0 {
		return nil, nil
	}
	copies, err := v.copyRecords(ctx)
	if err != nil {
		return nil, err
	}
	locations, err := v.storageLocations(ctx)
	if err != nil {
		return nil, err
	}
	records, err := v.coverageRecords(ctx)
	if err != nil {
		return nil, err
	}
	facts := make([]BackupFacts, 0, len(terminal))
	for _, item := range terminal {
		var meta struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(item, &meta); err != nil {
			return nil, fmt.Errorf("parsing a Velero Backup: %w", err)
		}
		facts = append(facts, judgeBackup(string(item), copies[meta.Metadata.Name], records[meta.Metadata.Name], locations, namespace))
	}
	return facts, nil
}

// copyRecord is one volume's copy evidence: the backup's own record of what it
// copied, never inferred from its phase.
type copyRecord struct {
	namespace  string
	volume     string
	volumeUID  string
	successful bool
	detail     string
}

// copyRecords reads every per-item copy result Velero wrote: the
// PodVolumeBackup objects a file-system backup creates, the DataUpload objects
// a CSI snapshot with data movement creates, and the VolumeSnapshot objects a
// native CSI snapshot creates. Their fields are the pinned Velero's (1.18.1,
// chart 12.1.0), the same three surfaces the operator's producer reads.
func (v *veleroBackups) copyRecords(ctx context.Context) (map[string][]copyRecord, error) {
	records := map[string][]copyRecord{}

	out, err := k3s.Kubectl(ctx, v.runner, "get podvolumebackups.velero.io -n "+Namespace+" -o json")
	if err == nil {
		var document struct {
			Items []struct {
				Metadata struct {
					Labels          map[string]string `json:"labels"`
					Annotations     map[string]string `json:"annotations"`
					OwnerReferences []struct {
						Kind       string `json:"kind"`
						Name       string `json:"name"`
						APIVersion string `json:"apiVersion"`
					} `json:"ownerReferences"`
				} `json:"metadata"`
				Spec struct {
					Pod struct {
						Namespace string `json:"namespace"`
					} `json:"pod"`
					Volume string `json:"volume"`
				} `json:"spec"`
				Status struct {
					Phase   string `json:"phase"`
					Message string `json:"message"`
				} `json:"status"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &document); err != nil {
			return nil, fmt.Errorf("parsing the PodVolumeBackups: %w", err)
		}
		for _, item := range document.Items {
			backup := copyRecordBackup(item.Metadata.OwnerReferences, item.Metadata.Labels["velero.io/backup-name"])
			if backup == "" {
				continue
			}
			volume := item.Metadata.Annotations["velero.io/pvc-name"]
			if volume == "" {
				volume = item.Spec.Volume
			}
			records[backup] = append(records[backup], copyRecord{
				namespace:  item.Spec.Pod.Namespace,
				volume:     volume,
				volumeUID:  item.Metadata.Labels["velero.io/pvc-uid"],
				successful: item.Status.Phase == "Completed",
				detail:     copyDetail("PodVolumeBackup", item.Status.Phase, item.Status.Message),
			})
		}
	} else if !notFound(err) && !strings.Contains(err.Error(), "the server doesn't have a resource type") {
		return nil, fmt.Errorf("reading the PodVolumeBackups: %w", err)
	}

	out, err = k3s.Kubectl(ctx, v.runner, "get datauploads.velero.io -n "+Namespace+" -o json")
	if err == nil {
		var document struct {
			Items []struct {
				Metadata struct {
					Labels          map[string]string `json:"labels"`
					OwnerReferences []struct {
						Kind       string `json:"kind"`
						Name       string `json:"name"`
						APIVersion string `json:"apiVersion"`
					} `json:"ownerReferences"`
				} `json:"metadata"`
				Spec struct {
					SourceNamespace string `json:"sourceNamespace"`
					SourcePVC       string `json:"sourcePVC"`
				} `json:"spec"`
				Status struct {
					Phase   string `json:"phase"`
					Message string `json:"message"`
				} `json:"status"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &document); err != nil {
			return nil, fmt.Errorf("parsing the DataUploads: %w", err)
		}
		for _, item := range document.Items {
			backup := copyRecordBackup(item.Metadata.OwnerReferences, item.Metadata.Labels["velero.io/backup-name"])
			if backup == "" {
				continue
			}
			records[backup] = append(records[backup], copyRecord{
				namespace:  item.Spec.SourceNamespace,
				volume:     item.Spec.SourcePVC,
				volumeUID:  item.Metadata.Labels["velero.io/pvc-uid"],
				successful: item.Status.Phase == "Completed",
				detail:     copyDetail("DataUpload", item.Status.Phase, item.Status.Message),
			})
		}
	} else if !notFound(err) && !strings.Contains(err.Error(), "the server doesn't have a resource type") {
		return nil, fmt.Errorf("reading the DataUploads: %w", err)
	}

	out, err = k3s.Kubectl(ctx, v.runner, "get volumesnapshots.snapshot.storage.k8s.io --all-namespaces -o json")
	if err == nil {
		var document struct {
			Items []struct {
				Metadata struct {
					Namespace string            `json:"namespace"`
					Labels    map[string]string `json:"labels"`
				} `json:"metadata"`
				Spec struct {
					Source struct {
						PersistentVolumeClaimName string `json:"persistentVolumeClaimName"`
					} `json:"source"`
				} `json:"spec"`
				Status struct {
					ReadyToUse bool `json:"readyToUse"`
					Error      *struct {
						Message string `json:"message"`
					} `json:"error"`
				} `json:"status"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &document); err != nil {
			return nil, fmt.Errorf("parsing the VolumeSnapshots: %w", err)
		}
		for _, item := range document.Items {
			backup := item.Metadata.Labels["velero.io/backup-name"]
			if backup == "" || item.Spec.Source.PersistentVolumeClaimName == "" {
				continue
			}
			detail := ""
			if !item.Status.ReadyToUse {
				detail = "VolumeSnapshot is not ready"
				if item.Status.Error != nil && item.Status.Error.Message != "" {
					detail = "VolumeSnapshot: " + item.Status.Error.Message
				}
			}
			records[backup] = append(records[backup], copyRecord{
				namespace:  item.Metadata.Namespace,
				volume:     item.Spec.Source.PersistentVolumeClaimName,
				successful: item.Status.ReadyToUse,
				detail:     detail,
			})
		}
	} else if !notFound(err) && !strings.Contains(err.Error(), "the server doesn't have a resource type") {
		return nil, fmt.Errorf("reading the VolumeSnapshots: %w", err)
	}

	return records, nil
}

// copyRecordBackup names the Backup a per-item result belongs to. Velero owns
// its PodVolumeBackups and DataUploads through the Backup and labels them too;
// the owner reference is the one that cannot be truncated.
func copyRecordBackup(owners []struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	APIVersion string `json:"apiVersion"`
}, label string) string {
	for _, owner := range owners {
		if owner.Kind == "Backup" && strings.HasPrefix(owner.APIVersion, "velero.io/") {
			return owner.Name
		}
	}
	return label
}

func copyDetail(kind, phase, message string) string {
	if message != "" {
		return fmt.Sprintf("%s: %s", kind, message)
	}
	if phase == "" {
		phase = "no phase"
	}
	return fmt.Sprintf("%s reached %s", kind, phase)
}

// storageLocations maps a location's name to Velero's own verdict of it. A
// backup whose location is not Available is not off-cluster: "Completed" says
// Velero wrote what it could, not that the data is in the bucket.
func (v *veleroBackups) storageLocations(ctx context.Context) (map[string]string, error) {
	out, err := k3s.Kubectl(ctx, v.runner, "get backupstoragelocations.velero.io -n "+Namespace+" -o json")
	if err != nil {
		return nil, fmt.Errorf("reading the BackupStorageLocations: %w", err)
	}
	var document struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing the BackupStorageLocations: %w", err)
	}
	locations := map[string]string{}
	for _, item := range document.Items {
		locations[item.Metadata.Name] = item.Status.Phase
	}
	return locations, nil
}

// coverageRecords reads every expected-coverage record the cluster holds,
// keyed by backup name. The record's shape is coverage.go's, which is the same
// JSON the operator writes.
func (v *veleroBackups) coverageRecords(ctx context.Context) (map[string]*coverageRecord, error) {
	out, err := k3s.Kubectl(ctx, v.runner, "get configmaps -n "+Namespace+" -l "+coverageLabelKey+"="+coverageLabel+" -o json")
	if err != nil {
		return nil, fmt.Errorf("reading the expected-coverage records: %w", err)
	}
	var document struct {
		Items []struct {
			Data map[string]string `json:"data"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing the expected-coverage records: %w", err)
	}
	records := map[string]*coverageRecord{}
	for _, item := range document.Items {
		raw := item.Data[coverageRecordKey]
		if raw == "" {
			continue
		}
		var record coverageRecord
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			// An unreadable record is "coverage unknown" for its backup, never
			// a reason to fail the read of the others: silence is not coverage,
			// and a record this build cannot read is silence.
			continue
		}
		if record.Backup == "" {
			continue
		}
		records[record.Backup] = &record
	}
	return records, nil
}

func (v *veleroBackups) coverageRecord(ctx context.Context, name string) (*coverageRecord, error) {
	out, err := k3s.Kubectl(ctx, v.runner, "get configmap "+CoverageRecordName(name)+" -n "+Namespace+" -o json")
	if err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the expected-coverage record for %s: %w", name, err)
	}
	var document struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		return nil, fmt.Errorf("parsing the expected-coverage record for %s: %w", name, err)
	}
	raw := document.Data[coverageRecordKey]
	if raw == "" {
		return nil, nil
	}
	var record coverageRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return nil, nil
	}
	if record.Backup == "" {
		record.Backup = name
	}
	return &record, nil
}

// judgeBackup turns one Velero Backup object into BackupFacts scoped to the
// namespace the restore is about.
//
// Mode 1 restores ONE namespace, so eligibility is judged for that namespace:
// the expected set's entry for it, and the copies of its volumes. A backup with
// a missing volume anywhere else is still that backup's problem — it is the
// operator's `backup` group and the recovery point that speaks for the whole
// cluster — but restoring THIS namespace needs this namespace's volumes.
func judgeBackup(raw string, copies []copyRecord, record *coverageRecord, locations map[string]string, namespace string) BackupFacts {
	var backup struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			StorageLocationName string `json:"storageLocation"`
			Hooks               struct {
				Resources []map[string]any `json:"resources"`
			} `json:"hooks"`
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"spec"`
		Status struct {
			Phase               string `json:"phase"`
			StartTimestamp      string `json:"startTimestamp"`
			CompletionTimestamp string `json:"completionTimestamp"`
			HookStatus          struct {
				HooksAttempted *int64 `json:"hooksAttempted"`
			} `json:"hookStatus"`
		} `json:"status"`
	}
	facts := BackupFacts{}
	if err := json.Unmarshal([]byte(raw), &backup); err != nil {
		facts.CoverageUnknown = true
		return facts
	}
	facts.Name = backup.Metadata.Name
	facts.Phase = backup.Status.Phase
	facts.CaptureStartedAt = backup.Status.StartTimestamp
	facts.CompletedAt = backup.Status.CompletionTimestamp
	facts.StorageLocation = backup.Spec.StorageLocationName
	facts.StorageLocationPhase = locations[facts.StorageLocation]
	if facts.StorageLocationPhase == "" {
		facts.StorageLocationPhase = "Missing"
	}
	facts.ConsistencyMethod, facts.Consistency = consistencyOf(backup.Spec.Metadata.Labels, backup.Spec.Hooks.Resources, backup.Status.HookStatus.HooksAttempted)
	if record == nil {
		// Silence is not coverage: a backup whose expected set nobody recorded
		// cannot be shown to hold every volume it claimed.
		facts.CoverageUnknown = true
		return facts
	}
	if record.CaptureStartedAt != nil && facts.CaptureStartedAt == "" {
		facts.CaptureStartedAt = *record.CaptureStartedAt
	}
	for _, ns := range record.Namespaces {
		if ns.Name != namespace {
			continue
		}
		for _, volume := range ns.Volumes {
			facts.Expected = append(facts.Expected, VolumeRef{Namespace: volume.Namespace, Name: volume.Name, UID: volume.UID})
		}
	}
	facts.Missing, facts.Failed = compareCoverage(facts.Expected, copies)
	return facts
}

// compareCoverage judges the recorded expected set against the copies the
// backup made. It iterates the EXPECTED set and never the copies: a volume in
// the set with no copy result at all is MISSING, not "nothing to copy" — a
// claim that got no copy is exactly the incident this record exists to catch.
func compareCoverage(expected []VolumeRef, copies []copyRecord) (missing, failed []VolumeRef) {
	for _, volume := range expected {
		matched := matchingCopies(copies, volume)
		if len(matched) == 0 {
			missing = append(missing, VolumeRef{
				Namespace: volume.Namespace,
				Name:      volume.Name,
				UID:       volume.UID,
				Detail:    "the backup recorded no copy of this volume",
			})
			continue
		}
		for _, copy := range matched {
			if !copy.successful {
				failed = append(failed, VolumeRef{
					Namespace: volume.Namespace,
					Name:      volume.Name,
					UID:       volume.UID,
					Detail:    copy.detail,
				})
				break
			}
		}
	}
	return missing, failed
}

// matchingCopies returns every copy result that belongs to one expected volume.
// The PVC UID decides when both sides carry one — a claim recreated under the
// same name is a different volume — and the pod-volume name is the fallback.
func matchingCopies(copies []copyRecord, want VolumeRef) []copyRecord {
	var matched []copyRecord
	for _, copy := range copies {
		if copy.namespace != want.Namespace {
			continue
		}
		if copy.volumeUID != "" && want.UID != "" {
			if copy.volumeUID == want.UID {
				matched = append(matched, copy)
			}
			continue
		}
		if copy.volume != "" && copy.volume == want.Name {
			matched = append(matched, copy)
		}
	}
	return matched
}

// consistencyOf reports how a backup captured the data, and says it in words
// that claim nothing more than what is known: a hook that exited zero is
// "hook ran", never "consistent".
func consistencyOf(labels map[string]string, hooks []map[string]any, hooksAttempted *int64) (method, text string) {
	if labels[consistencyMethodLabel] == consistencyNativeDump {
		return "native-dump", "native dump (declared)"
	}
	if hookExecutesAny(hooks) || (hooksAttempted != nil && *hooksAttempted > 0) {
		return "hook", "hook ran"
	}
	return "uncoordinated-copy", "uncoordinated copy"
}

const (
	consistencyMethodLabel = "kubenest.io/consistency-method"
	consistencyNativeDump  = "native-dump"
)

func hookExecutesAny(resources []map[string]any) bool {
	for _, resource := range resources {
		for _, phase := range []string{"pre", "post"} {
			hooks, ok := resource[phase].([]any)
			if !ok {
				continue
			}
			for _, hook := range hooks {
				body, ok := hook.(map[string]any)
				if !ok {
					continue
				}
				exec, ok := body["exec"].(map[string]any)
				if !ok {
					continue
				}
				command, ok := exec["command"].([]any)
				if ok && len(command) > 0 {
					return true
				}
			}
		}
	}
	return false
}

// safetyBackupDocument renders the Velero Backup this command takes before it
// destroys anything.
//
// includedNamespaces is set explicitly so workloadExcludedNamespaces does not
// apply: this backup is about ONE namespace, whatever its name. The TTL is the
// plan's seven days, and the labels name the purpose and the operation so a
// later operator can tell this backup from a scheduled one.
func safetyBackupDocument(name, namespace, operationID string, ttl time.Duration) ([]byte, error) {
	doc := map[string]any{
		"apiVersion": "velero.io/v1",
		"kind":       "Backup",
		"metadata": map[string]any{
			"name":      name,
			"namespace": Namespace,
			"labels": map[string]any{
				SafetyBackupPurposeLabel: SafetyBackupPurpose,
				OperationIDLabel:         operationID,
			},
		},
		"spec": map[string]any{
			"includedNamespaces": []string{namespace},
			"ttl":                ttl.String(),
		},
	}
	return yaml.Marshal(doc)
}

// restoreDocument renders one Velero Restore request.
//
// THE FIELD SET IS THE ONE PROBE P5 MEASURED, and each field is load-bearing:
//
//   - includedNamespaces limits the restore to the target namespace;
//   - includedResources carries persistentvolumes NEXT TO persistentvolumeclaims
//     and pods even though every PV itself is filtered out. Without it Velero
//     creates no PodVolumeRestore at all (Velero 1.18 skips the volume restore
//     when the type filter excludes PV/PVC) while still injecting its
//     restore-wait init container, so the restored pod waits for ever and the
//     restore reports "Completed with 0 errors" — run 4 of P5;
//   - restorePVs asks for the volume data, which is the whole point;
//   - includeClusterResources is left unset, as in every restore P5
//     measured working. Set to false, it keeps Velero from processing the
//     volumes at all, so a restored claim keeps the volumeName of a PV that
//     is gone and waits for ever (hardware, 2026-09-27, S4 on lab w3);
//   - existingResourcePolicy none (Velero's default) leaves objects that are
//     still in the cluster alone — that is what keeps an unnamed claim's newer
//     data out of the way of the fill, together with the modifier below.
func restoreDocument(spec restoreRequest) ([]byte, error) {
	body := map[string]any{
		"apiVersion": "velero.io/v1",
		"kind":       "Restore",
		"metadata": map[string]any{
			"name":      spec.Name,
			"namespace": Namespace,
			"labels": map[string]any{
				OperationIDLabel: spec.OperationID,
			},
		},
		"spec": map[string]any{
			"backupName":             spec.Backup,
			"includedNamespaces":     []string{spec.Namespace},
			"restorePVs":             true,
			"existingResourcePolicy": "none",
			"preserveNodePorts":      false,
		},
	}
	m := body["spec"].(map[string]any)
	// An EMPTY type filter is left out on purpose: mode 1 restores everything
	// the backup holds for the namespace, and a hand-written list of "the usual
	// types" would silently drop whatever the customer's own resources are. The
	// default filter is the one that contains both PV and PVC, which is the
	// condition probe P5 measured as load-bearing for PodVolumeRestore creation.
	if len(spec.IncludedResources) > 0 {
		m["includedResources"] = spec.IncludedResources
	}
	// JOBS ARE EXCLUDED so nothing runs by surprise; --include-jobs restores
	// them deliberately, to run at activation. CronJobs are restored (as
	// objects) and mode 1 hands the restore a resource modifier that suspends
	// each one AS VELERO CREATES IT and records the value the backup held —
	// see cronJobModifierDocument, which is what the request's
	// ResourceModifier is for. A Job restored with --include-jobs is suspended
	// by the run afterwards, which is the one window this design still has.
	//
	// Only mode 1 needs to say so: a mode-2 request carries an explicit type
	// filter, and an exclusion beside it would name a resource the filter
	// already leaves out.
	if len(spec.IncludedResources) == 0 && !spec.IncludeJobs {
		m["excludedResources"] = []string{"jobs"}
	}
	if spec.LabelSelector != nil {
		m["labelSelector"] = map[string]any{"matchLabels": spec.LabelSelector}
	}
	if len(spec.OrLabelSelectors) > 0 {
		selectors := make([]any, 0, len(spec.OrLabelSelectors))
		for _, selector := range spec.OrLabelSelectors {
			selectors = append(selectors, map[string]any{"matchLabels": selector})
		}
		m["orLabelSelectors"] = selectors
	}
	if spec.ResourceModifier != nil {
		m["resourceModifier"] = map[string]any{"kind": "ConfigMap", "name": spec.ResourceModifier.Name}
	}
	return yaml.Marshal(body)
}

// restoreRequest is one Velero Restore the command asks for.
type restoreRequest struct {
	Name              string
	Backup            string
	Namespace         string
	OperationID       string
	IncludedResources []string
	IncludeJobs       bool
	LabelSelector     map[string]string
	OrLabelSelectors  []map[string]string
	ResourceModifier  *modifierConfigMap
	// NamedVolumes are the claims this restore is asked to FILL. They are what
	// a non-Completed verdict is judged against: "Completed with 0 errors" is
	// not proof that a volume was restored (probe P5, run 4).
	NamedVolumes []VolumeRef
}

// modifierConfigMap is a Velero resource-modifier ConfigMap the command writes
// before the restore and deletes after it.
type modifierConfigMap struct {
	Name string
	Doc  []byte
}

// modifierName is the resource-modifier ConfigMap's name. Both restore modes
// carry one; the operation id keeps two runs, and two modes, apart.
func modifierName(operationID string) string { return "kubenest-restore-modifier-" + operationID }

// modifierDocument wraps a set of resourceModifierRules in the ConfigMap a
// Velero Restore reads them from. Both modes build their rules and hand them
// here, so the envelope — version v1, the ConfigMap's name, namespace and
// operation label, and the data key Velero looks under — exists once.
func modifierDocument(operationID string, rules []any) (*modifierConfigMap, error) {
	body, err := yaml.Marshal(map[string]any{"version": "v1", "resourceModifierRules": rules})
	if err != nil {
		return nil, err
	}
	doc, err := yaml.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      modifierName(operationID),
			"namespace": Namespace,
			"labels": map[string]any{
				OperationIDLabel: operationID,
			},
		},
		"data": map[string]string{"resource-modifier.yaml": string(body)},
	})
	if err != nil {
		return nil, err
	}
	return &modifierConfigMap{Name: modifierName(operationID), Doc: doc}, nil
}
