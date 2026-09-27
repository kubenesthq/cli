package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Mode 2: restore one workload's stranded volumes IN PLACE (plan 7.5, kn-t43).
//
// After a worker node dies, a workload's local volumes are gone while its pods
// cannot start: the claims are stranded on a node that will not come back, and
// the data exists only in the Velero backup. Restoring the whole namespace
// would destroy the volumes of the same workload that are still fine on live
// nodes, so this mode restores only the claims the operator names, for all of
// one workload's stranded claims together — restoring one claim of a pod while
// another stays stranded leaves the pod unable to start.
//
// THE MECHANISM IS PROBE P5, MEASURED ON HARDWARE (lab/p5-velero-probe):
//
//   - persistentvolumes must be in the restore's type filter next to
//     persistentvolumeclaims and pods. Without it Velero creates NO
//     PodVolumeRestore at all (Velero 1.18 skips the volume restore when the
//     type filter excludes PV/PVC) while still injecting its restore-wait init
//     container — so the pod waits for ever and the restore reports "Completed
//     with 0 errors". Run 4 of P5 did exactly that.
//   - a resource modifier must change the restored pod's pod-template-hash.
//     Velero strips ownerReferences, so an unchanged pod is adopted by the
//     scaled-to-zero owner and deleted at once.
//   - THE MODIFIER MUST NAME THE PODS THE BACKUP HOLDS, not the pods that mount
//     the claims today. Velero restores the pod the backup captured, under the
//     name it had at backup time, and builds that pod's PodVolumeRestores from
//     the PodVolumeBackups that name IT by pod name (pkg/podvolume/util.go:75,
//     getVolumeBackupInfoForPod -> isPVBMatchPod, Velero 1.18.1). A rule aimed
//     at a pod the ReplicaSet replaced matches nothing, so the restored pod
//     keeps its old pod-template-hash, the owner adopts and deletes it, and its
//     PodVolumeRestores never start (kn-t43-restore-workload-s-stranded-11o7.3:
//     the backup's dead-b87c65446-d7vlj against the live dead-b87c65446-r9zpb).
//   - the owner stays at zero until every named claim's PodVolumeRestore is
//     Completed. Only then does the restored pod become the workload's data.
//   - a strategic merge patch must remove every volume that was NOT named from
//     the restored pod, together with its container mounts and Velero's own
//     /restores/<volume> mount. Velero restores the data of every volume the
//     pod mounts that has a backup, even when the claim object itself is
//     skipped as "already exists" — so without the patch the UNNAMED claim's
//     newer data is overwritten with the backup's.
//   - the cost is a PartiallyFailed restore with exactly one error per removed
//     volume ("volume not found in pod"). That is expected here, and anything
//     else — any error for a NAMED volume, any other failure reason — is not.
//
// A volume of the same workload that was not named keeps its current contents.
// That is the invariant this mode exists for, and a mode-2 run that disturbs an
// unnamed claim of the same pod is a data-loss bug, not a slower path.

// volumePlan is mode 2's plan: the claims to refill, the pods that mount them,
// the owners to hold at zero, and the Velero selection built from them.
type volumePlan struct {
	Namespace string
	Backup    BackupFacts
	Skipped   []string

	Claims []VolumeRef
	// Pods are the live pods that mount a named claim. They are what step 3
	// scales down and what the owner walk reads; they are NOT what the
	// modifier names, because they are not what Velero restores.
	Pods []PodState
	// BackupPods are the pods the CHOSEN BACKUP holds for the named claims,
	// derived from its PodVolumeBackups. They are the pods the restore creates
	// and the only names the modifier's rules match. Nil means "not derived
	// yet": a resume rebuilds mode 2's plan from the record, which carries the
	// claims and no pods, so the run derives this set again from the same
	// backup (see ensureBackupPods).
	BackupPods []backupPod
	// Owners are the controllers those pods belong to, with the replica counts
	// activation puts back.
	Owners []WorkloadState
	// Labels are the selector labels the restore is narrowed by: each owner's
	// own pod selector, plus each named claim's labels, so the claims
	// themselves are matched too.
	LabelSelectors []map[string]string
	// Unstranded names the claims that look bound rather than stranded. They are
	// still restored, but the plan says so before anything is deleted.
	Unstranded []string

	Age         time.Duration
	AgeKnown    bool
	Policy      RecoveryPointPolicy
	AgeAccepted bool
	Mode        string
	PVCs        []string
}

// backupPod is one pod the RESTORE creates, as the BACKUP holds it.
//
// THE NAME IS THE WHOLE POINT. Velero restores the pod its PodVolumeBackups
// name, under that same name (pkg/podvolume/backupper.go:513-528 records it,
// restorer.go:126 finds the volumes to fill through it), so a modifier rule
// keyed on today's pod matches nothing after the pod was replaced — the node
// loss case this mode exists for.
type backupPod struct {
	// Name is the pod's name in the BACKUP.
	Name string
	// Volumes is every volume the backup holds for this pod, and Named the
	// ones among them that ARE the named claims. The difference is what the
	// modifier must remove from the restored pod: Velero restores the data of
	// every volume of the pod that has a PodVolumeBackup, whether or not the
	// claim object itself is skipped as already existing (probe P5's selective
	// fixture), so a volume the backup holds but the operator did not name
	// would have its live contents overwritten.
	Volumes []string
	Named   []string
	// Strip is the strategic merge patch that removes every unnamed volume the
	// backup holds for this pod, with its mounts. Empty when there is none, in
	// which case no rule is written for this pod at all.
	Strip string
}

// unnamed is the volumes the backup holds for this pod that are not named
// claims: exactly the set the strip patch removes.
func (p backupPod) unnamed() []string {
	named := map[string]bool{}
	for _, volume := range p.Named {
		named[volume] = true
	}
	var out []string
	for _, volume := range p.Volumes {
		if !named[volume] {
			out = append(out, volume)
		}
	}
	return out
}

// removedVolumes is every (pod, volume) pair the strip patches take out of the
// restored pods. It is what Velero will report one "volume not found in pod"
// error for — the one non-Completed verdict this mode tolerates — and the run
// needs the pairs, not a count, to tell those errors from a real failure.
func (p *volumePlan) removedVolumes() []RemovedVolume {
	var out []RemovedVolume
	for _, pod := range p.BackupPods {
		for _, volume := range pod.unnamed() {
			out = append(out, RemovedVolume{Pod: pod.Name, Volume: volume})
		}
	}
	return out
}

// runVolumeRestore is mode 2.
func (r *restoreRun) runVolumeRestore(ctx context.Context) error {
	if err := r.resolve(ctx); err != nil {
		return err
	}
	// THE DRILL FIRST. Velero runs one restore at a time and the operator's
	// drill holds the only worker; a restore that starts behind one queues for
	// ever (probe P5, run 1). This is checked before anything is read, changed
	// or planned.
	// A RESUME PLANS FROM THE RECORD, exactly as mode 1 does: the request it
	// continues is immutable, so there is nothing new to choose or confirm.
	if r.opts.recovery().ID() == "" {
		if err := r.refuseWhileDrillRuns(ctx); err != nil {
			return err
		}
		if err := r.chooseVolumeBackup(ctx); err != nil {
			return err
		}
		if err := r.buildVolumePlan(ctx); err != nil {
			return err
		}
		r.renderVolumePlan()
		if !r.opts.Confirm {
			ok, err := r.prompt()
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("nothing has been changed: the restore was not confirmed")
			}
		}
	} else if err := r.refuseWhileDrillRuns(ctx); err != nil {
		return err
	}
	if err := r.openRecord(ctx); err != nil {
		return err
	}
	runErr := r.executeVolumeRestore(ctx)
	r.release(ctx, runErr)
	return runErr
}

// refuseWhileDrillRuns refuses while the operator's restore drill holds
// Velero's restore worker, and says how to free it.
func (r *restoreRun) refuseWhileDrillRuns(ctx context.Context) error {
	drill, err := r.deps.Cluster.DrillRestore(ctx)
	if err != nil {
		return err
	}
	if drill == nil || restorePhaseTerminal(drill.Phase) {
		return nil
	}
	return fmt.Errorf("the operator's restore drill %s is in progress (phase %s) and Velero runs one restore at a time, so this restore would queue behind it for ever (probe P5, run 1). Wait for the drill to finish, or delete its Restore to free the worker:\n  sudo -n k3s kubectl -n velero delete restore %s --cascade=foreground\nNothing has been changed",
		drill.Name, orUnknown(drill.Phase), drill.Name)
}

// chooseVolumeBackup is mode 1's backup choice, shared: the eligibility rules
// do not depend on what the restore will do with the data.
func (r *restoreRun) chooseVolumeBackup(ctx context.Context) error {
	plan := &volumePlan{Namespace: r.opts.Namespace, Mode: "volumes", PVCs: append([]string(nil), r.opts.PVCs...)}
	switch {
	case r.opts.From != "" && r.opts.Latest:
		return fmt.Errorf("--from and --latest are mutually exclusive: name the backup or let the command pick the newest eligible one")
	case r.opts.From != "":
		facts, err := r.deps.Backups.NamedBackup(ctx, r.opts.Namespace, r.opts.From)
		if err != nil {
			if errors.Is(err, ErrNoBackup) {
				return fmt.Errorf("backup %s is not on this cluster: nothing has been changed", r.opts.From)
			}
			return err
		}
		if reason := facts.IneligibleReason(); reason != "" {
			return fmt.Errorf("backup %s cannot refill the claims of namespace %s: %s. Nothing has been changed", facts.Name, r.opts.Namespace, describeIneligible(facts, reason))
		}
		plan.Backup = facts
	case r.opts.Latest:
		list, err := r.backups(ctx)
		if err != nil {
			return err
		}
		var chosen *BackupFacts
		for i := range list {
			if list[i].IneligibleReason() == "" {
				chosen = &list[i]
				break
			}
		}
		if chosen == nil {
			if len(list) == 0 {
				return fmt.Errorf("this cluster has no terminal backup for namespace %s: nothing has been changed", r.opts.Namespace)
			}
			return fmt.Errorf("no backup can refill the claims of namespace %s: every terminal backup is ineligible (%s). Nothing has been changed", r.opts.Namespace, ineligibleSummary(list))
		}
		for i := range list {
			if list[i].Name == chosen.Name {
				break
			}
			plan.Skipped = append(plan.Skipped, fmt.Sprintf("%s: %s", list[i].Name, describeIneligible(list[i], list[i].IneligibleReason())))
		}
		plan.Backup = *chosen
	default:
		return fmt.Errorf("choose a backup: pass --from NAME or --latest")
	}
	plan.Age, plan.AgeKnown = plan.Backup.Age(r.deps.Now())
	plan.Policy = r.policy
	plan.AgeAccepted = r.opts.AcceptDataAge
	r.volumePlan = plan
	return nil
}

// buildVolumePlan resolves the affected set and the restore's selection.
//
// THE OWNER WALK IS THE POINT: a pod with no controller owner is refused by
// name rather than scaled down by hand, because a pod nobody owns cannot be put
// back, and "the restore filled the volume but the workload never came back" is
// not a restore.
func (r *restoreRun) buildVolumePlan(ctx context.Context) error {
	plan := r.volumePlan
	state, err := r.deps.Cluster.Namespace(ctx, plan.Namespace)
	if err != nil {
		return err
	}
	if state == nil {
		return fmt.Errorf("namespace %s does not exist, so there is nothing to refill in place: a volume restore needs the live namespace its workload runs in. Nothing has been changed", plan.Namespace)
	}
	claims, err := r.deps.Cluster.Claims(ctx, plan.Namespace)
	if err != nil {
		return err
	}
	byName := make(map[string]VolumeRef, len(claims))
	for _, claim := range claims {
		byName[claim.Name] = claim
	}
	for _, name := range plan.PVCs {
		claim, ok := byName[name]
		if !ok {
			return fmt.Errorf("claim %s/%s is not in the namespace, so there is nothing to refill: a stranded claim exists and cannot be used. Nothing has been changed", plan.Namespace, name)
		}
		plan.Claims = append(plan.Claims, claim)
		if claim.Detail != "" && !strings.Contains(claim.Detail, "phase Pending") {
			plan.Unstranded = append(plan.Unstranded, fmt.Sprintf("%s/%s (%s)", claim.Namespace, claim.Name, claim.Detail))
		}
	}
	pods, err := r.deps.Cluster.Pods(ctx, plan.Namespace)
	if err != nil {
		return err
	}
	workloads, err := r.deps.Cluster.Workloads(ctx, plan.Namespace)
	if err != nil {
		return err
	}
	byWorkload := map[string]WorkloadState{}
	for _, w := range workloads {
		byWorkload[w.Kind+"/"+w.Name] = w
	}
	named := map[string]bool{}
	for _, claim := range plan.Claims {
		named[claim.Name] = true
	}
	ownerSeen := map[string]bool{}
	for _, pod := range pods {
		if !mountsAny(pod, named) {
			continue
		}
		owner, err := r.resolveOwner(ctx, pod)
		if err != nil {
			return err
		}
		plan.Pods = append(plan.Pods, pod)
		key := strings.ToLower(owner.Kind) + "/" + owner.Name
		if ownerSeen[key] {
			continue
		}
		ownerSeen[key] = true
		workload, known := byWorkload[key]
		replicas := int32(1)
		selector := map[string]string(nil)
		if known {
			replicas = workload.Replicas
			selector = workload.Selector
		}
		plan.Owners = append(plan.Owners, WorkloadState{
			Kind:     strings.ToLower(owner.Kind),
			Name:     owner.Name,
			Replicas: replicas,
			Selector: selector,
		})
		// The selector labels the restore is narrowed by. The pods that mount
		// these claims carry them by definition (they were created from that
		// selector), and so must the claims: a claim Velero's selector cannot
		// match would never be recreated after the delete, so it is checked
		// below rather than discovered as a missing claim after the fact.
		if len(selector) > 0 {
			plan.LabelSelectors = append(plan.LabelSelectors, selector)
		}
	}
	if len(plan.Pods) == 0 {
		return fmt.Errorf("no pod in %s mounts %s, so there is nothing to stop and no restored pod to fill the claims: nothing has been changed. If the workload is scaled to zero, scale it up first",
			plan.Namespace, strings.Join(plan.PVCs, ", "))
	}
	if len(plan.LabelSelectors) == 0 {
		return fmt.Errorf("none of the workloads that mount %s reports a pod selector, so a Velero restore could not be limited to them: nothing has been changed",
			strings.Join(plan.PVCs, ", "))
	}
	for _, claim := range plan.Claims {
		labels := labelsOfClaim(claims, claim.Name)
		if !matchesAnySelector(labels, plan.LabelSelectors) {
			return fmt.Errorf("claim %s/%s carries none of the labels the restore is narrowed by (%s), so Velero's label selector could not bring it back after it is deleted. Nothing has been changed: label the claim to match its workload — `kubectl label persistentvolumeclaim %s -n %s <key>=<value>` — and re-run",
				claim.Namespace, claim.Name, describeSelectors(plan.LabelSelectors), claim.Name, claim.Namespace)
		}
	}
	sort.Slice(plan.Owners, func(i, j int) bool {
		if plan.Owners[i].Kind != plan.Owners[j].Kind {
			return plan.Owners[i].Kind < plan.Owners[j].Kind
		}
		return plan.Owners[i].Name < plan.Owners[j].Name
	})
	// LAST, AND IT IS THE MODIFIER'S HALF OF THE PLAN: which pods the BACKUP
	// holds for these claims, and which of their volumes the modifier has to
	// take back out of the restored pod.
	return r.ensureBackupPods(ctx, pods)
}

// ensureBackupPods derives, from the chosen backup, the pods the restore will
// create and the volume-strip patch each of them needs.
//
// WHY NOT plan.Pods. The pods mounting the claims today are what step 3 scales
// down; they are not what Velero restores. Velero restores the pod the backup
// captured, under its backup-time name, and it finds the volumes to fill for
// that pod by matching the PodVolumeBackups' spec.pod.name against the pod it
// is restoring (Velero 1.18.1, pkg/podvolume/restorer.go:126 ->
// pkg/podvolume/util.go:75, getVolumeBackupInfoForPod/isPVBMatchPod). So a
// modifier keyed on a pod name the ReplicaSet has since replaced changes
// nothing on the pod that is actually created: the restored pod keeps its
// pod-template-hash, the scaled-to-zero owner adopts and deletes it, and its
// PodVolumeRestores never start. That is
// kn-t43-restore-workload-s-stranded-11o7.3: the backup's
// dead-b87c65446-d7vlj, today's dead-b87c65446-r9zpb.
//
// THE CLAIM LINK IS THE CLAIM'S UID: the PodVolumeBackup's velero.io/pvc-uid
// label against the named claim's UID. It is the claim's identity, it is what
// Velero writes for exactly this purpose (pkg/apis/velero/v1/
// labels_annotations.go: PVCUIDLabel, set at pkg/podvolume/backupper.go:546),
// and it is what this command already reads back for PodVolumeRestores
// (claimNamesByUID, velero_resources_test.go). The claim a stranded restore
// names is the claim that was copied — it is Pending, not deleted — so its UID
// is still the one the backup recorded. The pvc-name annotation upstream also
// writes is deliberately NOT used: hardware (lab w3, 2026-09-27) carried the
// UID label and no claim-name annotation.
//
// A NAMED CLAIM THE BACKUP HOLDS NO VOLUME FOR IS REFUSED BY NAME, at plan
// time: probe P5's run 4 reported "Completed with 0 errors" having restored
// nothing at all, which is what a plan that cannot fill a claim looks like from
// further away.
//
// live is the pods the plan read and the source of the strip patch's merge keys
// (container names and mount paths). It is empty on a resume, where the run
// rebuilds the plan from the record and reads the pods itself.
func (r *restoreRun) ensureBackupPods(ctx context.Context, live []PodState) error {
	plan := r.volumePlan
	held, err := r.deps.Cluster.PodVolumeBackups(ctx, plan.Backup.Name)
	if err != nil {
		return err
	}
	if len(live) == 0 {
		if live, err = r.deps.Cluster.Pods(ctx, plan.Namespace); err != nil {
			return err
		}
	}
	claimOfUID := map[string]string{}
	for _, claim := range plan.Claims {
		if claim.UID != "" {
			claimOfUID[claim.UID] = claim.Name
		}
	}
	byPod := map[string][]BackupVolumeState{}
	var order []string
	for _, volume := range held {
		if volume.Namespace != plan.Namespace || volume.Pod == "" || volume.Volume == "" {
			continue
		}
		if _, seen := byPod[volume.Pod]; !seen {
			order = append(order, volume.Pod)
		}
		byPod[volume.Pod] = append(byPod[volume.Pod], volume)
	}
	sort.Strings(order)
	pods := []backupPod{}
	found := map[string]bool{}
	for _, name := range order {
		pod := backupPod{Name: name}
		for _, volume := range byPod[name] {
			pod.Volumes = append(pod.Volumes, volume.Volume)
			if claim, named := claimOfUID[volume.ClaimUID]; named {
				found[claim] = true
				pod.Named = append(pod.Named, volume.Volume)
			}
		}
		// A pod the backup holds only volumes of claims this restore did not
		// name is not a pod this restore creates: the restore is narrowed by
		// the owners of the LIVE pods, and its rules name the pods that carry
		// a named claim.
		if len(pod.Named) == 0 {
			continue
		}
		pods = append(pods, pod)
	}
	var missing []string
	for _, claim := range plan.Claims {
		if !found[claim.Name] {
			missing = append(missing, claim.Namespace+"/"+claim.Name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("backup %s holds no PodVolumeBackup for %s, so it cannot refill %s: a PodVolumeBackup is the backup's own record of a volume it copied, and a named claim without one was not copied by this backup. Nothing has been changed: choose a backup that holds every named claim, or name only the claims this one does",
			plan.Backup.Name, strings.Join(missing, ", "), namedClaimPhrase(len(missing)))
	}
	for i := range pods {
		strip := pods[i].unnamed()
		if len(strip) == 0 {
			continue
		}
		patch, err := volumeStripPatch(pods[i].Name, strip, live)
		if err != nil {
			return err
		}
		pods[i].Strip = patch
	}
	plan.BackupPods = pods
	return nil
}

// namedClaimPhrase keeps the refusal readable for one claim and for several.
func namedClaimPhrase(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// resolveOwner walks a pod to the object that has a replica count to put back.
//
// A pod owned by a ReplicaSet is the ordinary case for a Deployment, and the
// ReplicaSet is not what the operator thinks of as the workload; so a
// ReplicaSet is resolved one step up, and a pod (or ReplicaSet) with no
// controller owner at all is REFUSED BY NAME rather than scaled down by hand:
// a pod nobody owns cannot be put back, and "the volume was refilled but the
// workload never came back" is not a restore.
func (r *restoreRun) resolveOwner(ctx context.Context, pod PodState) (*OwnerRef, error) {
	owner := controllerOf(pod)
	if owner == nil {
		return nil, fmt.Errorf("pod %s/%s mounts a claim this restore would refill but has no controller owner, so nothing can scale it down and put it back. Nothing has been changed: give it an owner (a Deployment or a StatefulSet) and re-run",
			pod.Namespace, pod.Name)
	}
	if !strings.EqualFold(owner.Kind, "ReplicaSet") {
		return owner, nil
	}
	above, err := r.deps.Cluster.ControllerOwner(ctx, pod.Namespace, "replicaset", owner.Name)
	if err != nil {
		return nil, err
	}
	if above == nil {
		return nil, fmt.Errorf("pod %s/%s is owned by ReplicaSet %s, which has no controller owner, so there is no workload to scale down and put back. Nothing has been changed",
			pod.Namespace, pod.Name, owner.Name)
	}
	return above, nil
}

// labelsOfClaim reads a claim's labels out of the plan's detail line. The plan
// deliberately does not carry a second copy of the claim: the labels are only
// needed for this one check, and VolumeRef.Detail already carries them.
func labelsOfClaim(claims []VolumeRef, name string) map[string]string {
	for _, claim := range claims {
		if claim.Name != name {
			continue
		}
		return parseLabels(detailPart(claim.Detail, "labels "))
	}
	return nil
}

func detailPart(detail, prefix string) string {
	i := strings.Index(detail, prefix)
	if i < 0 {
		return ""
	}
	return detail[i+len(prefix):]
}

func parseLabels(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(strings.TrimSpace(raw), ",") {
		key, value, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}

func matchesAnySelector(labels map[string]string, selectors []map[string]string) bool {
	if len(labels) == 0 {
		return false
	}
	for _, selector := range selectors {
		matched := true
		for key, value := range selector {
			if labels[key] != value {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func describeSelectors(selectors []map[string]string) string {
	parts := make([]string, 0, len(selectors))
	for _, selector := range selectors {
		parts = append(parts, describeLabels(selector))
	}
	return strings.Join(parts, " | ")
}

func mountsAny(pod PodState, named map[string]bool) bool {
	for _, claim := range pod.ClaimNames {
		if named[claim] {
			return true
		}
	}
	return false
}

// controllerOf returns the pod's controller owner. A ReplicaSet owner is not
// followed further: the object to scale is the ReplicaSet's own Deployment in
// the ordinary case, and the CLI scales what has a replica count, so a
// ReplicaSet is resolved to its Deployment by name when the Deployment exists.
func controllerOf(pod PodState) *OwnerRef {
	for i := range pod.Owners {
		if pod.Owners[i].Controller {
			return &pod.Owners[i]
		}
	}
	return nil
}

// renderVolumePlan prints mode 2's plan before anything changes.
func (r *restoreRun) renderVolumePlan() {
	p := r.volumePlan
	fmt.Fprintf(r.out, "Volume restore plan for namespace %s of %s.\n", p.Namespace, r.opts.Cluster)
	fmt.Fprintf(r.out, "  backup:        %s\n", p.Backup.Name)
	fmt.Fprintf(r.out, "  completed:     %s\n", orUnknown(p.Backup.CompletedAt))
	if p.AgeKnown {
		fmt.Fprintf(r.out, "  data age:      %s, measured from the capture start %s\n", p.Age.Round(time.Minute), p.Backup.CaptureStartedAt)
	} else {
		fmt.Fprintf(r.out, "  data age:      UNKNOWN: the backup records no capture start\n")
	}
	fmt.Fprintf(r.out, "  policy:        %s, from %s\n", p.Policy.Max, p.Policy.Source)
	if p.AgeAccepted {
		fmt.Fprintf(r.out, "  data age:      accepted explicitly by --accept-data-age\n")
	}
	fmt.Fprintf(r.out, "  coverage:      %s\n", describeCoverage(p.Backup))
	fmt.Fprintf(r.out, "  consistency:   %s\n", describeConsistency(p.Backup))
	for _, s := range p.Skipped {
		fmt.Fprintf(r.out, "  --latest:      passed over %s\n", s)
	}
	for _, claim := range p.Claims {
		fmt.Fprintf(r.out, "  claim:         %s/%s (uid %s)%s — deleted and refilled\n", claim.Namespace, claim.Name, claim.UID, claimDetail(claim))
	}
	if len(p.Unstranded) > 0 {
		fmt.Fprintf(r.out, "  NOT STRANDED:  these claims look bound, and deleting one discards the volume it holds now:\n")
		for _, u := range p.Unstranded {
			fmt.Fprintf(r.out, "                   - %s\n", u)
		}
	}
	for _, owner := range p.Owners {
		fmt.Fprintf(r.out, "  workload:      %s %s held at 0 replicas (was %d) until every claim is refilled\n", owner.Kind, owner.Name, owner.Replicas)
	}
	for _, pod := range p.Pods {
		fmt.Fprintf(r.out, "  pod:           %s (mounts the claims today; scaled to 0, and replaced by the restore)\n", pod.Name)
	}
	// BOTH SETS ARE PRINTED, AND THE DIFFERENCE IS THE POINT. The live pods are
	// what step 3 stops; the BACKUP's pods are what Velero creates and what the
	// modifier matches. After a node loss the two names differ, and a plan that
	// showed only the live name is exactly how the modifier came to name the
	// wrong pod (kn-t43-restore-workload-s-stranded-11o7.3).
	for _, pod := range p.BackupPods {
		fmt.Fprintf(r.out, "  restored pod:  %s — the pod %s holds, with the named volumes %s; the restore creates it and the modifier matches it\n",
			pod.Name, p.Backup.Name, strings.Join(pod.Named, ", "))
		if unnamed := pod.unnamed(); len(unnamed) > 0 {
			fmt.Fprintf(r.out, "                 the modifier removes %s from it (with their mounts), so those live volumes are not overwritten\n",
				strings.Join(unnamed, ", "))
		}
	}
	fmt.Fprintf(r.out, "  selection:     the restore is narrowed to %s\n", describeSelectors(p.LabelSelectors))
	fmt.Fprintf(r.out, "  unnamed:       every volume of these pods that is NOT named above keeps its current contents, byte for byte\n")
}

// volumeRestoreSpec builds the one Velero Restore mode 2 asks for.
func (r *restoreRun) volumeRestoreSpec() (restoreRequest, error) {
	plan := r.volumePlan
	modifier, err := volumeModifierDocument(r.handle.OperationID(), plan)
	if err != nil {
		return restoreRequest{}, err
	}
	spec := restoreRequest{
		Backup:    plan.Backup.Name,
		Namespace: plan.Namespace,
		// THE TYPE FILTER IS P5'S, and persistentvolumes in it is the field
		// that decides whether Velero creates PodVolumeRestores at all.
		IncludedResources: []string{"persistentvolumeclaims", "persistentvolumes", "pods"},
		OrLabelSelectors:  plan.LabelSelectors,
		ResourceModifier:  modifier,
		NamedVolumes:      plan.Claims,
		// THE PRICE OF KEEPING THE UNNAMED VOLUMES, handed to the verdict check
		// before Velero is even asked: the strip patches remove these pairs, and
		// the one error each of them costs is the only non-Completed verdict
		// this mode accepts.
		RemovedVolumes: plan.removedVolumes(),
	}
	if len(plan.LabelSelectors) == 1 {
		spec.LabelSelector = plan.LabelSelectors[0]
		spec.OrLabelSelectors = nil
	}
	return spec, nil
}

// volumeModifierDocument renders the two rules probe P5 settled.
//
// THE PODS ARE THE BACKUP'S. Both rules' conditions name the pods the chosen
// backup holds for the named claims (plan.BackupPods), never today's pods that
// mount them: Velero creates the pod the backup captured, so a rule keyed on
// the replacement matches nothing and the restored pod is adopted and deleted
// (kn-t43-restore-workload-s-stranded-11o7.3). The set is derived from the
// backup, so a resumed run derives the same names from the same backup and
// builds the same document the interrupted run did.
//
// RULE 1 takes the restored pods out of their owner's selector by changing
// pod-template-hash: Velero strips ownerReferences, so an unchanged pod is
// adopted by the scaled-to-zero owner and deleted before it can be filled.
//
// RULE 2 removes every volume the BACKUP holds for a restored pod that was NOT
// named, with its container mounts and Velero's own /restores/<volume> mount,
// each by its merge key. Velero restores the data of every volume of the pod
// that has a PodVolumeBackup even when the claim object is skipped as already
// existing, so without this rule an unnamed claim's newer data is overwritten
// (P5's selective fixture). The volumes come from the backup's PodVolumeBackups
// and not from the live pod, because the live pod is not the pod being patched:
// a volume only the live pod has would be a deletion of something the restored
// pod does not carry. The cost is one "volume not found in pod" error per
// removed volume, which is what makes the restore PartiallyFailed and what the
// verdict check tolerates — and only that.
func volumeModifierDocument(operationID string, plan *volumePlan) (*modifierConfigMap, error) {
	if len(plan.BackupPods) == 0 {
		return nil, fmt.Errorf("no pod of backup %s is in the plan, so a modifier could not name the pod Velero will restore. This is a bug: the plan derives the backup's pods from its PodVolumeBackups before it renders", plan.Backup.Name)
	}
	podRegexParts := make([]string, 0, len(plan.BackupPods))
	for _, pod := range plan.BackupPods {
		podRegexParts = append(podRegexParts, regexpQuote(pod.Name))
	}
	rules := []any{
		map[string]any{
			"conditions": map[string]any{
				"groupResource":     "pods",
				"resourceNameRegex": "^(" + strings.Join(podRegexParts, "|") + ")$",
			},
			"patches": []any{
				map[string]any{
					"operation": "replace",
					"path":      "/metadata/labels/pod-template-hash",
					"value":     "kubenest-restored",
				},
			},
		},
	}
	for _, pod := range plan.BackupPods {
		if pod.Strip == "" {
			continue
		}
		rules = append(rules, map[string]any{
			"conditions": map[string]any{
				"groupResource":     "pods",
				"resourceNameRegex": "^(" + regexpQuote(pod.Name) + ")$",
			},
			"strategicPatches": []any{
				map[string]any{"patchData": pod.Strip},
			},
		})
	}
	return modifierDocument(operationID, rules)
}

// volumeStripPatch builds the strategic merge patch that removes the volumes
// the BACKUP holds for one restored pod that are not named claims, with their
// mounts. Each deletion is addressed by a merge key (the volume's name, the
// container's name plus the mountPath), which is why the mount points have to
// come from a pod's spec.
//
// WHERE THOSE MOUNTS COME FROM. The pod being patched is the BACKUP's, and its
// spec is inside the backup, which this CLI does not read: Velero performs the
// restore server-side and no object-store reader exists here (the only S3
// client in this repository is k3s's etcd snapshot configuration). The live
// pods are the running instance of the same workload's pod template, so their
// container names and mount paths for a volume with the same NAME are the merge
// keys the restored pod carries. A volume the backup holds that no live pod
// declares is REFUSED rather than guessed at: a patch naming a mount the
// restored pod does not have cannot be built, and one that removed the wrong
// mount would be worse than a refusal.
func volumeStripPatch(name string, strip []string, live []PodState) (string, error) {
	volumes := []any{}
	containers := map[string][]any{}
	initContainers := map[string][]any{}
	for _, volume := range strip {
		mounts := mountsOfVolume(live, volume)
		if len(mounts) == 0 {
			return "", fmt.Errorf("backup's pod %s holds volume %s, which is not one of the named claims, so the restore has to take it out of the restored pod before Velero fills it with the backup's data — but no pod in the namespace declares that volume, so the mounts a strategic merge patch must name cannot be read. Nothing has been changed: scale the workload up so a pod of it exists, then re-run", name, volume)
		}
		volumes = append(volumes, map[string]any{"name": volume, "$patch": "delete"})
		// VELERO'S OWN MOUNT IS NOT IN THE LIVE POD: it injects a restore-wait
		// init container that mounts every backed-up volume at
		// /restores/<volume>, and its resource modifiers run AFTER that
		// injection. Removing the volume without this mount leaves the pod
		// invalid ("volumeMounts[0].name: Not found", probe P5 run 6), so the
		// mount is deleted by its merge keys alongside the volume. Its path is
		// the volume's own name, so it is derived here and not read from a pod.
		initContainers["restore-wait"] = append(initContainers["restore-wait"], map[string]any{
			"mountPath": "/restores/" + volume,
			"$patch":    "delete",
		})
		for _, mount := range mounts {
			if mount.Container == "restore-wait" {
				continue
			}
			entry := map[string]any{"mountPath": mount.Path, "$patch": "delete"}
			if mount.Init {
				initContainers[mount.Container] = append(initContainers[mount.Container], entry)
				continue
			}
			containers[mount.Container] = append(containers[mount.Container], entry)
		}
	}
	if len(volumes) == 0 {
		return "", nil
	}
	spec := map[string]any{"volumes": volumes}
	if len(containers) > 0 {
		list := []any{}
		for _, name := range sortedKeys(containers) {
			list = append(list, map[string]any{"name": name, "volumeMounts": containers[name]})
		}
		spec["containers"] = list
	}
	if len(initContainers) > 0 {
		list := []any{}
		for _, name := range sortedKeys(initContainers) {
			list = append(list, map[string]any{"name": name, "volumeMounts": initContainers[name]})
		}
		spec["initContainers"] = list
	}
	raw, err := json.Marshal(map[string]any{"spec": spec})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// mountsOfVolume finds where the pods declare and mount one volume, by the
// volume's NAME. The pods are the ones the run read — today's instances of the
// workload's pod template — and a volume's merge keys (its container names and
// mount paths) are the template's, which is the same template the backed-up pod
// was created from. Two pods that declare the same volume name with different
// mounts would be two different templates sharing a name; the first is used,
// because the pods read here are the ones that mount the named claims.
func mountsOfVolume(live []PodState, volume string) []PodMount {
	for _, pod := range live {
		for _, declared := range pod.Volumes {
			if declared.Name == volume && len(declared.Mounts) > 0 {
				return declared.Mounts
			}
		}
	}
	return nil
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// regexpQuote quotes a literal for use inside a regular expression. Pod names
// are DNS labels, so nothing but the dots of a name could matter; quoting is
// still right, because a name is data and not a pattern.
func regexpQuote(literal string) string {
	var b strings.Builder
	for _, ch := range literal {
		switch ch {
		case '.', '+', '*', '?', '(', ')', '[', ']', '{', '}', '^', '$', '|', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(ch)
	}
	return b.String()
}

// executeVolumeRestore is mode 2's order: hold first (Argo CD's self-heal would
// otherwise scale the workloads back up mid-restore), then the owner at zero,
// then the claims, then one Velero Restore, then the proof that each named
// claim was filled.
func (r *restoreRun) executeVolumeRestore(ctx context.Context) error {
	// A RESUME REBUILDS THE PLAN FROM THE RECORD, and the record carries the
	// claims and no pods (restore.go's volumePlanFromRecord), so the pods the
	// modifier must name are derived again — from the same backup, which the
	// record names — before anything is applied. Without this a resumed run
	// would build a modifier that matches no pod at all.
	if r.volumePlan.BackupPods == nil {
		if err := r.ensureBackupPods(ctx, nil); err != nil {
			return err
		}
	}
	if err := r.ensurePause(ctx); err != nil {
		return err
	}
	// The drill can have started between the plan and the hold; a restore
	// created behind one queues for ever, so it is asked again here, where the
	// project is already paused and nothing has been destroyed.
	if err := r.refuseWhileDrillRuns(ctx); err != nil {
		return err
	}
	if err := r.verifyVolumeIdentities(ctx); err != nil {
		return err
	}
	if err := r.holdOwnersAtZero(ctx); err != nil {
		return err
	}
	if err := r.deleteStrandedClaims(ctx); err != nil {
		return err
	}
	spec, err := r.volumeRestoreSpec()
	if err != nil {
		return err
	}
	restores, err := r.runVeleroRestore(ctx, "kubenest-restore-volumes-"+r.handle.OperationID(), spec)
	if err != nil {
		return err
	}
	if err := r.verifyRefilledClaims(ctx, restores); err != nil {
		return err
	}
	return r.markAwaitingActivation(ctx)
}

// verifyVolumeIdentities re-reads the named claims' UIDs immediately before the
// first destructive step, for the same reason mode 1 does: a claim recreated
// under the same name is a different claim, and refilling it is not the restore
// the operator confirmed.
func (r *restoreRun) verifyVolumeIdentities(ctx context.Context) error {
	recorded := map[string]string{}
	for _, claim := range r.volumePlan.Claims {
		recorded["named-persistentvolumeclaim/"+claim.Namespace+"/"+claim.Name] = claim.UID
	}
	live, err := r.liveIdentities(ctx)
	if err != nil {
		return err
	}
	// A claim THIS operation already deleted is the step the record says is
	// done, not a moved identity; the refilled claim it put back legitimately
	// has a new UID.
	consumed := map[string]bool{}
	if r.handle != nil {
		consumed = consumedIdentities(r.handle.Record(), r.opts.Namespace)
	}
	if moved := sameIdentity(recorded, live, consumed); len(moved) > 0 {
		return fmt.Errorf("a claim changed since the plan was confirmed, so this is no longer the restore that was planned:\n  %s\nNothing has been destroyed, and the pause is left in place. Re-run the command to plan against what is there now",
			strings.Join(moved, "\n  "))
	}
	fmt.Fprintf(r.out, "  identities:    %d claim identity/ies re-read and unchanged\n", len(recorded))
	return nil
}

// holdOwnersAtZero scales the mounting workloads to zero and waits for their
// pods to be gone, recording the counts activation puts back.
func (r *restoreRun) holdOwnersAtZero(ctx context.Context) error {
	for _, owner := range r.volumePlan.Owners {
		if err := r.recordPending(ctx, owner.Kind+"/"+owner.Name, "restore-replicas", fmt.Sprintf("%d", owner.Replicas)); err != nil {
			return err
		}
		if err := r.stage("scale/"+owner.Kind+"/"+owner.Name).ScaleWorkload(ctx, owner.Kind, owner.Name, r.opts.Namespace, 0); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "  hold:          %s %s scaled to 0 (was %d)\n", owner.Kind, owner.Name, owner.Replicas)
	}
	deadline, err := r.holdTimeout()
	if err != nil {
		return err
	}
	podNames := map[string]bool{}
	for _, pod := range r.volumePlan.Pods {
		podNames[pod.Name] = true
	}
	return r.waitUntil(ctx, deadline, "the pods holding the claims to be gone", func(ctx context.Context) (bool, string, error) {
		pods, err := r.deps.Cluster.Pods(ctx, r.opts.Namespace)
		if err != nil {
			return false, err.Error(), err
		}
		var remaining []string
		for _, pod := range pods {
			if podNames[pod.Name] {
				remaining = append(remaining, pod.Name)
			}
		}
		if len(remaining) > 0 {
			return false, "still running: " + strings.Join(remaining, ", "), nil
		}
		return true, "the pods are gone", nil
	})
}

// deleteStrandedClaims deletes each named claim and waits until it is gone:
// Velero provisions a FRESH volume for a claim that does not exist, which is
// the only way the data lands on a live node instead of on the one that died.
func (r *restoreRun) deleteStrandedClaims(ctx context.Context) error {
	deadline, err := r.holdTimeout()
	if err != nil {
		return err
	}
	for _, claim := range r.volumePlan.Claims {
		if err := r.stage("delete-claim/"+claim.Namespace+"/"+claim.Name).DeleteClaim(ctx, claim.Namespace, claim.Name); err != nil {
			return err
		}
		name := claim.Name
		if err := r.waitUntil(ctx, deadline, "claim "+claim.Namespace+"/"+claim.Name+" to be gone", func(ctx context.Context) (bool, string, error) {
			claims, err := r.deps.Cluster.Claims(ctx, claim.Namespace)
			if err != nil {
				return false, err.Error(), err
			}
			for _, live := range claims {
				if live.Name == name {
					return false, "claim " + name + " is still there", nil
				}
			}
			return true, "claim gone", nil
		}); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "  claims:        %s/%s deleted (its volume is gone with the node)\n", claim.Namespace, claim.Name)
	}
	return nil
}

// verifyRefilledClaims proves what the restore's phase cannot: that every named
// claim was actually filled, and that it landed on a live node.
//
// THE CLAIM IS THE UNIT. Probe P5's run 4 reported "Completed with 0 errors"
// and restored nothing at all, and a Completed restore says nothing about WHICH
// volume it filled. So each named claim must have its own Completed
// PodVolumeRestore (checked in runVeleroRestore, against the named set), and
// each must now be Bound to a volume on a node that is Ready.
//
// WHAT THIS DOES NOT PROVE, STATED PLAINLY: it does not compare the restored
// bytes with the backup, nor the uid/gid/mode of the restored files. That
// comparison needs the helper in the pinned operator image (KUBENEST_OPERATOR_IMAGE,
// `--restore-drill-helper=verify` in op3/cmd/manager/main.go) to read the
// backup's recorded metadata, and that helper has no mode for it today: both
// its modes compare an expected value the CALLER already knows, and a mode-2
// restore has nothing to compare against but the backup itself. Until op3
// grows that mode, this step reports what Velero's own evidence establishes —
// a Completed PodVolumeRestore per named claim, and a claim bound on a node
// that is Ready — and nothing more. T4.3's report records the gap.
func (r *restoreRun) verifyRefilledClaims(ctx context.Context, restores []VolumeRestoreState) error {
	deadline, err := r.holdTimeout()
	if err != nil {
		return err
	}
	for _, claim := range r.volumePlan.Claims {
		name := claim.Name
		var binding *ClaimBinding
		err := r.waitUntil(ctx, deadline, "claim "+claim.Namespace+"/"+claim.Name+" to be bound on a live node", func(ctx context.Context) (bool, string, error) {
			current, err := r.deps.Cluster.ClaimBinding(ctx, claim.Namespace, name)
			if err != nil {
				return false, err.Error(), err
			}
			if current == nil {
				return false, "the claim does not exist", nil
			}
			binding = current
			if !current.Bound {
				return false, fmt.Sprintf("claim %s is %s", name, orUnknown(current.Phase)), nil
			}
			if current.Node == "" {
				return false, fmt.Sprintf("claim %s is bound to %s, which records no node", name, current.Volume), nil
			}
			ready, err := r.deps.Cluster.NodeReady(ctx, current.Node)
			if err != nil {
				return false, err.Error(), err
			}
			if !ready {
				return false, fmt.Sprintf("claim %s is bound to %s on node %s, which is not Ready", name, current.Volume, current.Node), nil
			}
			return true, "bound on a live node", nil
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(r.out, "  verified:      %s/%s is Bound to %s on live node %s (PodVolumeRestore Completed)\n",
			claim.Namespace, claim.Name, binding.Volume, binding.Node)
	}
	// The restored pods are what filled the volumes, and their names come from
	// the PodVolumeRestores Velero wrote — not from the run's own plan, because
	// a resumed run no longer has the live pods to read them from. They are
	// recorded as owed work so activation deletes them: leaving them running
	// would start the workload before its activation.
	seen := map[string]bool{}
	for _, restore := range restores {
		if restore.Pod == "" || seen[restore.Pod] {
			continue
		}
		seen[restore.Pod] = true
		if err := r.recordPending(ctx, "pod/"+restore.Pod, "restore-pod", restore.Pod); err != nil {
			return err
		}
	}
	return nil
}
