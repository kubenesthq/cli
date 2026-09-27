package backup

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/operation"
)

// T4.3's unit acceptance: the Pod -> owner mapping, the plan's content, the
// refusal of mode 1's flags, and the selective rule — with a fake cluster where
// claim b holds newer data, the built Velero selection must name only claim a,
// and no step may touch b.

// volumeFixture is a namespace whose one Deployment mounts two claims: a
// (named) and b (not named).
func volumeFixture(t *testing.T) *restoreFixture {
	t.Helper()
	facts := eligibleFacts(t, "daily-good", at(-2*time.Hour), at(-1*time.Hour), "a", "b")
	f := newRestoreFixture(t, facts)
	f.cluster.claims = []VolumeRef{
		{Namespace: "payments", Name: "a", UID: "uid-a", Detail: "phase Pending, bound to pv-old-a"},
		{Namespace: "payments", Name: "b", UID: "uid-b", Detail: "phase Bound, bound to pv-live-b"},
	}
	f.cluster.claimLabels["a"] = map[string]string{"app": "payments"}
	f.cluster.claimLabels["b"] = map[string]string{"app": "payments"}
	f.cluster.workloads = []WorkloadState{{
		Kind:     "deployment",
		Name:     "payments",
		Replicas: 1,
		Selector: map[string]string{"app": "payments"},
	}}
	f.cluster.pods = []PodState{{
		Name:       "payments-6d9f-abcde",
		Namespace:  "payments",
		Phase:      "Pending",
		ClaimNames: []string{"a", "b"},
		Owners:     []OwnerRef{{Kind: "ReplicaSet", Name: "payments-6d9f", Controller: true}},
		Volumes: []PodVolumeState{
			{Name: "a", ClaimName: "a", Mounts: []PodMount{{Container: "app", Path: "/a"}}},
			{Name: "b", ClaimName: "b", Mounts: []PodMount{{Container: "app", Path: "/b"}}},
			// A volume the backup does not hold at all: nothing restores data
			// into it, so the modifier must leave it out of the patch it writes
			// for the restored pod.
			{Name: "cache", Mounts: []PodMount{{Container: "app", Path: "/cache"}}},
		},
	}}
	f.cluster.resolveReplicaSetOwner = &OwnerRef{Kind: "Deployment", Name: "payments", Controller: true}
	// THE BACKUP'S POD IS NOT TODAY'S POD. Velero restores the pod the backup
	// captured — payments-6d9f-qqqqq — while the pod mounting the claims now is
	// payments-6d9f-abcde, which is the node-loss case the modifier has to
	// survive (kn-t43-restore-workload-s-stranded-11o7.3: dead-b87c65446-d7vlj
	// against dead-b87c65446-r9zpb).
	f.cluster.podVolumeBackups = map[string][]BackupVolumeState{
		"daily-good": {
			{Pod: "payments-6d9f-qqqqq", Namespace: "payments", Volume: "a", ClaimUID: "uid-a"},
			{Pod: "payments-6d9f-qqqqq", Namespace: "payments", Volume: "b", ClaimUID: "uid-b"},
		},
	}
	f.cluster.volumes = []VolumeRestoreState{
		{Name: "pvr-a", Pod: "payments-6d9f-abcde", Volume: "a", ClaimName: "a", Phase: "Completed"},
	}
	f.cluster.bindings["a"] = &ClaimBinding{Claim: "a", Volume: "pvc-fresh-a", Node: "lab-node-1", Phase: "Bound", Bound: true}
	f.cluster.bindings["b"] = &ClaimBinding{Claim: "b", Volume: "pv-live-b", Node: "lab-node-2", Phase: "Bound", Bound: true}
	f.cluster.readyNodes["lab-node-1"] = true
	f.cluster.readyNodes["lab-node-2"] = true
	return f
}

// TestVolumeRestorePlanNamesOnlyTheNamedClaims is the selective rule: claim b
// holds newer data, only claim a is named, and nothing the run does touches b.
func TestVolumeRestorePlanNamesOnlyTheNamedClaims(t *testing.T) {
	f := volumeFixture(t)
	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", PVCs: []string{"a"}, Latest: true, Confirm: true}, "")
	if err != nil {
		t.Fatalf("the volume restore failed: %v\n%s", err, out)
	}

	// The plan names what it will delete and what it will not touch.
	if !strings.Contains(out, "payments/a") || !strings.Contains(out, "deleted and refilled") {
		t.Errorf("the plan does not name the claim it will refill:\n%s", out)
	}
	if !strings.Contains(out, "keeps its current contents, byte for byte") {
		t.Errorf("the plan does not state the invariant that the unnamed volume keeps its data:\n%s", out)
	}
	if !strings.Contains(out, "payments-6d9f-abcde") {
		t.Errorf("the plan does not name the pod the restore fills through:\n%s", out)
	}
	if !strings.Contains(out, "deployment payments held at 0 replicas (was 1)") {
		t.Errorf("the plan does not say the owner is held at zero:\n%s", out)
	}

	// Every step that changes anything names claim a and never claim b.
	for _, deleted := range f.cluster.deletedClaims {
		if deleted != "a" {
			t.Errorf("the run deleted claim %s: only the named claims may be deleted", deleted)
		}
	}
	if len(f.cluster.deletedClaims) != 1 {
		t.Errorf("deleted claims = %v, want exactly the one named claim", f.cluster.deletedClaims)
	}
	for _, scaled := range f.cluster.scaled {
		if scaled != "deployment/payments=0" {
			t.Errorf("the run scaled %s: only the pods that mount the named claim may be stopped", scaled)
		}
	}
	if f.cluster.deleted {
		t.Error("a volume restore deleted the namespace: that is mode 1, and it would destroy the volumes that are still fine")
	}

	// The Velero request is P5's: the three types, PVs among them, and a
	// label-narrowed restore.
	if len(f.cluster.restoresMade) != 1 {
		t.Fatalf("Velero restores created = %d, want exactly one", len(f.cluster.restoresMade))
	}
	spec := f.cluster.restoresMade[0]
	for _, want := range []string{"persistentvolumeclaims", "persistentvolumes", "pods"} {
		if !contains(spec.IncludedResources, want) {
			t.Errorf("the type filter %v does not carry %s: without persistentvolumes Velero creates no PodVolumeRestore at all (probe P5, run 4)", spec.IncludedResources, want)
		}
	}
	if spec.Namespace != "payments" {
		t.Errorf("the restore is scoped to %q, want the namespace", spec.Namespace)
	}

	// The modifier is what keeps claim b's newer data: it must remove b's
	// volume, its mount and restore-wait's mount, and leave a alone. It is
	// written for the pod the BACKUP holds — the pod Velero creates — not the
	// pod that mounts the claims today.
	rules := modifierRules(t, f)
	if len(rules) < 2 {
		t.Fatalf("the modifier holds %d rule(s), want the pod-template-hash rule and one volume-strip rule", len(rules))
	}
	assertHashRule(t, rules[0])
	strip := stripPatches(t, rules)
	data := strip["payments-6d9f-qqqqq"]
	if data == "" {
		t.Fatalf("no volume-strip rule targets the pod the backup holds: %v", strip)
	}
	if _, today := strip["payments-6d9f-abcde"]; today {
		t.Errorf("a volume-strip rule targets today's pod, which the restore does not create: %v", strip)
	}
	if !strings.Contains(data, `"name":"b"`) && !strings.Contains(data, `"name": "b"`) {
		t.Errorf("the volume-strip patch does not remove the unnamed volume b: %s", data)
	}
	for _, needle := range []string{`"mountPath":"/b"`, `/restores/b`} {
		if !strings.Contains(data, needle) {
			t.Errorf("the volume-strip patch does not remove %s, so the restored pod would be refused (probe P5 run 6): %s", needle, data)
		}
	}
	if strings.Contains(data, `"name":"a"`) || strings.Contains(data, `"mountPath":"/a"`) {
		t.Errorf("the volume-strip patch touches the NAMED volume a, whose data this restore exists to refill: %s", data)
	}
	if strings.Contains(data, "cache") {
		t.Errorf("the volume-strip patch removes a volume the BACKUP does not hold: nothing restores data into it, and the restored pod is not today's pod: %s", data)
	}

	// The verification is per claim, and it landed on a live node.
	if !strings.Contains(out, "pvc-fresh-a") || !strings.Contains(out, "lab-node-1") {
		t.Errorf("the run does not report which volume the claim came back on and which node it lives on:\n%s", out)
	}
	if !strings.Contains(out, RestoredStage) {
		t.Errorf("a volume restore does not stop at %q:\n%s", RestoredStage, out)
	}
}

// THE BEAD'S UNIT ACCEPTANCE (kn-t43-restore-workload-s-stranded-11o7.3): every
// rule the modifier writes selects the pods the chosen BACKUP holds for the
// named claims, and never the pod that mounts them today. The fixture's two
// names differ — today's pod is payments-6d9f-abcde, the backup's is
// payments-6d9f-qqqqq — which is the node-loss case: on hardware the backup
// held dead-b87c65446-d7vlj while the ReplicaSet had already created
// dead-b87c65446-r9zpb, the rule matched neither, the restored pod kept its
// pod-template-hash, and the scaled-to-zero owner deleted it.
func TestVolumeModifierNamesTheBackupsPodsNotTodays(t *testing.T) {
	f := volumeFixture(t)
	const backupPod, livePod = "payments-6d9f-qqqqq", "payments-6d9f-abcde"
	if f.cluster.pods[0].Name != livePod || f.cluster.podVolumeBackups["daily-good"][0].Pod != backupPod {
		t.Fatalf("the fixture does not hold two different pod names, so this test proves nothing")
	}
	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", PVCs: []string{"a"}, Latest: true, Confirm: true}, "")
	if err != nil {
		t.Fatalf("the volume restore failed: %v\n%s", err, out)
	}
	rules := modifierRules(t, f)
	conditions, _ := rules[0]["conditions"].(map[string]any)
	regex, _ := conditions["resourceNameRegex"].(string)
	assertSelects(t, "the pod-template-hash rule", regex, backupPod, livePod)

	strips := 0
	for _, rule := range rules[1:] {
		if _, ok := rule["strategicPatches"]; !ok {
			continue
		}
		strips++
		conditions, _ := rule["conditions"].(map[string]any)
		regex, _ := conditions["resourceNameRegex"].(string)
		assertSelects(t, "the volume-strip rule", regex, backupPod, livePod)
	}
	if strips != 1 {
		t.Errorf("the modifier holds %d volume-strip rule(s), want one, for the backup's pod", strips)
	}
	// BOTH SETS ARE SHOWN, because they are not the same set: the live pod is
	// what step 3 stops, the backup's pod is what Velero creates.
	if !strings.Contains(out, backupPod) || !strings.Contains(out, livePod) {
		t.Errorf("the plan shows only one of the two pod sets (%s, %s):\n%s", livePod, backupPod, out)
	}
}

// assertSelects checks one rule's pod-name condition against the pod the BACKUP
// holds and the pod that mounts the claim today: the first must match, the
// second must not.
func assertSelects(t *testing.T, what, regex, backupPod, livePod string) {
	t.Helper()
	if regex == "" {
		t.Fatalf("%s carries no pod-name condition at all", what)
	}
	compiled, err := regexp.Compile(regex)
	if err != nil {
		t.Fatalf("%s is not a usable regular expression (%q): %v", what, regex, err)
	}
	if !compiled.MatchString(backupPod) {
		t.Errorf("%s does not select the pod the backup holds (%s): %q", what, backupPod, regex)
	}
	if compiled.MatchString(livePod) {
		t.Errorf("%s selects today's pod (%s), which Velero does not restore: %q", what, livePod, regex)
	}
}

// TestVolumeRestoreRefusesANamedClaimTheBackupDoesNotHold is the planted
// negative: the chosen backup holds no PodVolumeBackup for a named claim, so
// nothing in it can refill that claim. The plan refuses, naming it, before
// anything is scaled or deleted — an unnamed refusal here is a restore that
// reports Completed having filled nothing (probe P5, run 4).
func TestVolumeRestoreRefusesANamedClaimTheBackupDoesNotHold(t *testing.T) {
	f := volumeFixture(t)
	// Only claim b was copied by this backup.
	f.cluster.podVolumeBackups["daily-good"] = []BackupVolumeState{
		{Pod: "payments-6d9f-qqqqq", Namespace: "payments", Volume: "b", ClaimUID: "uid-b"},
	}
	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", PVCs: []string{"a"}, Latest: true, Confirm: true}, "")
	if err == nil {
		t.Fatalf("a named claim the backup holds nothing for must be refused:\n%s", out)
	}
	if !strings.Contains(err.Error(), "payments/a") || !strings.Contains(err.Error(), "daily-good") {
		t.Errorf("the refusal does not name the claim and the backup: %v", err)
	}
	if len(f.cluster.deletedClaims) != 0 || len(f.cluster.scaled) != 0 || len(f.cluster.restoresMade) != 0 || len(f.cluster.restoreDocs) != 0 {
		t.Errorf("the refusal changed the cluster: deleted %v, scaled %v, restores %d, documents %d",
			f.cluster.deletedClaims, f.cluster.scaled, len(f.cluster.restoresMade), len(f.cluster.restoreDocs))
	}
}

// TestVolumeRestoreRefusesAClaimOnlyAnotherBackupHolds: the velero namespace
// holds every backup's PodVolumeBackups, so a claim is matched against the
// CHOSEN backup's — the same claim copied by another backup is not this
// backup's copy, and its pod is not a pod this restore creates.
func TestVolumeRestoreRefusesAClaimOnlyAnotherBackupHolds(t *testing.T) {
	f := volumeFixture(t)
	f.cluster.podVolumeBackups["daily-good"] = []BackupVolumeState{
		{Pod: "payments-6d9f-qqqqq", Namespace: "payments", Volume: "b", ClaimUID: "uid-b"},
	}
	f.cluster.podVolumeBackups["daily-other"] = []BackupVolumeState{
		{Pod: "payments-6d9f-wwwww", Namespace: "payments", Volume: "a", ClaimUID: "uid-a"},
	}
	_, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", PVCs: []string{"a"}, Latest: true, Confirm: true}, "")
	if err == nil {
		t.Fatal("a claim only another backup holds must be refused")
	}
	if !strings.Contains(err.Error(), "payments/a") {
		t.Errorf("the refusal does not name the claim: %v", err)
	}
	if len(f.cluster.deletedClaims) != 0 || len(f.cluster.restoresMade) != 0 {
		t.Error("the refusal changed the cluster")
	}
}

// TestVolumeRestoreRefusesAVolumeTheLivePodsCannotDescribe: the backup holds a
// volume that is not a named claim and no pod in the namespace declares, so the
// container mounts the strip patch has to delete by cannot be read. The plan
// refuses rather than write a patch that removes the wrong mount or leaves the
// volume in place for Velero to overwrite.
func TestVolumeRestoreRefusesAVolumeTheLivePodsCannotDescribe(t *testing.T) {
	f := volumeFixture(t)
	f.cluster.podVolumeBackups["daily-good"] = append(f.cluster.podVolumeBackups["daily-good"],
		BackupVolumeState{Pod: "payments-6d9f-qqqqq", Namespace: "payments", Volume: "left-behind", ClaimUID: "uid-gone"})
	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", PVCs: []string{"a"}, Latest: true, Confirm: true}, "")
	if err == nil {
		t.Fatalf("a volume no live pod declares must be refused:\n%s", out)
	}
	if !strings.Contains(err.Error(), "payments-6d9f-qqqqq") || !strings.Contains(err.Error(), "left-behind") {
		t.Errorf("the refusal does not name the pod and the volume: %v", err)
	}
	if len(f.cluster.deletedClaims) != 0 || len(f.cluster.restoresMade) != 0 {
		t.Error("the refusal changed the cluster")
	}
}

func contains(list []string, want string) bool {
	for _, entry := range list {
		if entry == want {
			return true
		}
	}
	return false
}

// modifierRules reads the resource-modifier document the run applied.
func modifierRules(t *testing.T, f *restoreFixture) []map[string]any {
	t.Helper()
	var doc []byte
	for _, applied := range f.cluster.restoreDocs {
		if strings.Contains(applied.What, "resource modifier") {
			doc = applied.Doc
		}
	}
	if doc == nil {
		t.Fatal("the run applied no resource modifier: the restored pod would be adopted back by its owner and deleted (probe P5 condition 2)")
	}
	var configMap struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(doc, &configMap); err != nil {
		t.Fatalf("the modifier ConfigMap is not readable: %v", err)
	}
	raw := configMap.Data["resource-modifier.yaml"]
	if raw == "" {
		t.Fatalf("the modifier ConfigMap carries no rules: %v", configMap.Data)
	}
	var rules struct {
		Version string           `yaml:"version"`
		Rules   []map[string]any `yaml:"resourceModifierRules"`
	}
	if err := yaml.Unmarshal([]byte(raw), &rules); err != nil {
		t.Fatalf("the modifier rules are not readable: %v\n%s", err, raw)
	}
	if rules.Version != "v1" {
		t.Errorf("the modifier version is %q, want v1", rules.Version)
	}
	return rules.Rules
}

// assertHashRule checks the first rule is the one that takes the restored pod
// out of its owner's selector.
func assertHashRule(t *testing.T, rule map[string]any) {
	t.Helper()
	conditions, _ := rule["conditions"].(map[string]any)
	if conditions["groupResource"] != "pods" {
		t.Errorf("the first rule does not target pods: %v", conditions)
	}
	patches, _ := rule["patches"].([]any)
	for _, patch := range patches {
		entry, _ := patch.(map[string]any)
		if entry["path"] == "/metadata/labels/pod-template-hash" {
			return
		}
	}
	t.Errorf("the first rule does not change the pod's pod-template-hash, so Velero's owner-stripped copy would be adopted by the scaled-to-zero owner and deleted: %v", rule)
}

// stripPatches returns each pod's volume-strip patch, by the pod name its
// condition selects.
func stripPatches(t *testing.T, rules []map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, rule := range rules {
		patches, ok := rule["strategicPatches"].([]any)
		if !ok {
			continue
		}
		conditions, _ := rule["conditions"].(map[string]any)
		regex, _ := conditions["resourceNameRegex"].(string)
		for _, patch := range patches {
			entry, _ := patch.(map[string]any)
			data, _ := entry["patchData"].(string)
			out[strings.Trim(regex, "^()$")] = data
		}
	}
	return out
}

// TestVolumeRestoreRefusesAPodWithoutAController: a pod nobody owns cannot be
// put back, so it is refused by name and nothing is scaled or deleted.
func TestVolumeRestoreRefusesAPodWithoutAController(t *testing.T) {
	f := volumeFixture(t)
	f.cluster.pods[0].Owners = nil

	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", PVCs: []string{"a"}, Latest: true, Confirm: true}, "")
	if err == nil {
		t.Fatalf("a pod with no controller owner must be refused:\n%s", out)
	}
	if !strings.Contains(err.Error(), "payments-6d9f-abcde") {
		t.Errorf("the refusal does not name the pod: %v", err)
	}
	if len(f.cluster.deletedClaims) != 0 || len(f.cluster.scaled) != 0 {
		t.Errorf("the refusal scaled %v and deleted %v", f.cluster.scaled, f.cluster.deletedClaims)
	}
	if len(f.cluster.restoresMade) != 0 {
		t.Error("the refusal created a restore")
	}
}

// TestVolumeRestoreRefusesAReplicaSetWithoutAnOwner: a ReplicaSet is not what
// the operator scales, so a ReplicaSet with no controller owner is refused too.
func TestVolumeRestoreRefusesAReplicaSetWithoutAnOwner(t *testing.T) {
	f := volumeFixture(t)
	f.cluster.resolveReplicaSetOwner = nil

	_, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", PVCs: []string{"a"}, Latest: true, Confirm: true}, "")
	if err == nil {
		t.Fatal("a ReplicaSet with no controller owner must be refused")
	}
	if !strings.Contains(err.Error(), "ReplicaSet") {
		t.Errorf("the refusal does not name the ReplicaSet: %v", err)
	}
}

// TestVolumeRestoreRefusesAClaimVeleroCannotMatch: a label-filtered restore
// cannot recreate a claim that carries none of the labels it selects by, so
// that claim is refused BEFORE it is deleted rather than silently lost.
func TestVolumeRestoreRefusesAClaimVeleroCannotMatch(t *testing.T) {
	f := volumeFixture(t)
	f.cluster.claimLabels["a"] = map[string]string{"tier": "db"}

	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", PVCs: []string{"a"}, Latest: true, Confirm: true}, "")
	if err == nil {
		t.Fatalf("a claim the restore's selector cannot match must be refused:\n%s", out)
	}
	if !strings.Contains(err.Error(), "payments/a") || !strings.Contains(err.Error(), "label") {
		t.Errorf("the refusal does not name the claim and the fix: %v", err)
	}
	if len(f.cluster.deletedClaims) != 0 {
		t.Error("the claim was deleted before the refusal: it would never come back")
	}
}

// TestVolumeRestoreRefusesWhileADrillRuns: Velero runs one restore at a time,
// and the operator's drill holds the only worker (probe P5, run 1).
func TestVolumeRestoreRefusesWhileADrillRuns(t *testing.T) {
	f := volumeFixture(t)
	f.cluster.drill = &DrillRestore{Name: DrillRestoreNamePrefix + "20260927-120000-ab12cd", Phase: "InProgress"}

	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", PVCs: []string{"a"}, Latest: true, Confirm: true}, "")
	if err == nil {
		t.Fatalf("a restore that would queue behind the drill must be refused:\n%s", out)
	}
	for _, want := range []string{DrillRestoreNamePrefix, "InProgress", "delete restore"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if len(f.cluster.restoresMade) != 0 || len(f.cluster.deletedClaims) != 0 {
		t.Error("the refusal changed the cluster")
	}
	// A terminal drill is not in progress, and does not block anything.
	f.cluster.drill = &DrillRestore{Name: DrillRestoreNamePrefix + "old", Phase: "Completed"}
	if _, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", PVCs: []string{"a"}, Latest: true, Confirm: true}, ""); err != nil {
		t.Errorf("a finished drill blocked a volume restore: %v", err)
	}
}

// TestVolumeRestoreResumeDoesNotRepeatACompletedStep: an interrupted volume
// restore resumes, and a step whose postcondition holds is not repeated.
func TestVolumeRestoreResumeDoesNotRepeatACompletedStep(t *testing.T) {
	f := volumeFixture(t)
	// The first run's restore never reaches a terminal phase.
	f.cluster.outcome = &RestoreOutcome{Name: "r", Phase: "InProgress"}

	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", PVCs: []string{"a"}, Latest: true, Confirm: true}, "")
	if err == nil {
		t.Fatalf("a restore that never completes must fail:\n%s", out)
	}
	record := f.kube.recordFor(operation.Name)
	if record == nil {
		t.Fatal("the interrupted volume restore left no record")
	}
	if record.Request.Kind != operation.KindRestoreVolume {
		t.Errorf("the record's kind is %s, want %s", record.Request.Kind, operation.KindRestoreVolume)
	}
	opID := record.OperationID

	resumed := volumeFixture(t)
	resumed.kube = f.kube
	resumed.cluster.deletedClaims = append([]string(nil), f.cluster.deletedClaims...)
	resumed.cluster.annotations = f.cluster.annotations
	before := resumed.kube.commandCount("delete persistentvolumeclaim a")
	var resumedOut strings.Builder
	if err := RunRestore(context.Background(), &resumedOut, strings.NewReader(""), resumed.options(t, RestoreOptions{
		Namespace: "payments",
		PVCs:      []string{"a"},
		Resume:    opID,
	}), resumed.deps()); err != nil {
		t.Fatalf("--resume failed: %v\n%s", err, resumedOut.String())
	}
	if after := resumed.kube.commandCount("delete persistentvolumeclaim a"); after != before {
		t.Errorf("--resume re-submitted the claim deletion (%d -> %d)", before, after)
	}
	if _, still := resumed.cluster.annotations[PauseAnnotationKey]; !still {
		t.Error("--resume lifted the pause: resuming never activates")
	}
	if !strings.Contains(resumedOut.String(), RestoredStage) {
		t.Errorf("the resume did not finish at %q:\n%s", RestoredStage, resumedOut.String())
	}
}

// TestVolumeStripPatchLeavesAnUnnamedVolumeAlone is the selective rule at the
// patch level, with no cluster in the way: the backup's pod holds a (named) and
// b (not named), so the patch removes b's volume, b's mount and restore-wait's
// mount, and never a's. The volumes are the BACKUP's — the caller passes the
// ones it found in the PodVolumeBackups — and the merge keys are the live
// pods'.
func TestVolumeStripPatchLeavesAnUnnamedVolumeAlone(t *testing.T) {
	live := []PodState{{
		Name: "web-1",
		Volumes: []PodVolumeState{
			{Name: "a", ClaimName: "a", Mounts: []PodMount{{Container: "app", Path: "/a"}}},
			{Name: "b", ClaimName: "b", Mounts: []PodMount{{Container: "app", Path: "/b"}}},
		},
	}}
	patch, err := volumeStripPatch("web-1", []string{"b"}, live)
	if err != nil {
		t.Fatalf("building the strip patch: %v", err)
	}
	for _, want := range []string{`"name":"b"`, `"mountPath":"/b"`, `"mountPath":"/restores/b"`} {
		if !strings.Contains(strings.ReplaceAll(patch, " ", ""), want) {
			t.Errorf("the patch does not remove %s: %s", want, patch)
		}
	}
	if strings.Contains(patch, `"name":"a"`) || strings.Contains(patch, `"mountPath":"/a"`) {
		t.Errorf("the patch touches the named volume: %s", patch)
	}
	if strings.Contains(patch, "cache") {
		t.Errorf("the patch removes a volume the backup does not hold: %s", patch)
	}
	// A backup pod with nothing unnamed needs no patch, and gets none: an empty
	// one would be an object Velero has to parse for nothing.
	if only, err := volumeStripPatch("web-1", nil, live); err != nil || only != "" {
		t.Errorf("a pod with no unnamed volume got (%q, %v)", only, err)
	}
	// A volume the backup holds that no live pod declares is a refusal, not a
	// guess: the merge keys a strategic merge patch deletes by are the pod's
	// own, and a patch that cannot name them is invalid.
	if _, err := volumeStripPatch("web-1", []string{"left-behind"}, live); err == nil {
		t.Error("a volume no live pod declares must be refused")
	}
}

// TestVolumeRestoreRefusesAMissingClaim: naming a claim that is not there is a
// refusal, not an empty restore.
func TestVolumeRestoreRefusesAMissingClaim(t *testing.T) {
	f := volumeFixture(t)
	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", PVCs: []string{"a", "gone"}, Latest: true, Confirm: true}, "")
	if err == nil {
		t.Fatalf("a claim that is not in the namespace must be refused:\n%s", out)
	}
	if !strings.Contains(err.Error(), "gone") {
		t.Errorf("the refusal does not name the missing claim: %v", err)
	}
	if len(f.cluster.deletedClaims) != 0 {
		t.Error("the run deleted a claim before refusing the set")
	}
}

// TestRestoreDocumentCarriesProbesConditions pins the three fields of the
// restore request that probe P5 measured as load-bearing.
func TestRestoreDocumentCarriesProbesConditions(t *testing.T) {
	doc, err := restoreDocument(restoreRequest{
		Name:              "kubenest-restore-volumes-abc123",
		Backup:            "daily-good",
		Namespace:         "payments",
		OperationID:       "abc123",
		IncludedResources: []string{"persistentvolumeclaims", "persistentvolumes", "pods"},
		OrLabelSelectors:  []map[string]string{{"app": "payments"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Spec struct {
			BackupName         string           `yaml:"backupName"`
			IncludedNamespaces []string         `yaml:"includedNamespaces"`
			IncludedResources  []string         `yaml:"includedResources"`
			RestorePVs         bool             `yaml:"restorePVs"`
			OrLabelSelectors   []map[string]any `yaml:"orLabelSelectors"`
			ExcludedResources  []string         `yaml:"excludedResources"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(doc, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Spec.BackupName != "daily-good" || len(parsed.Spec.IncludedNamespaces) != 1 || parsed.Spec.IncludedNamespaces[0] != "payments" {
		t.Errorf("the restore is not scoped to the backup and namespace: %+v", parsed.Spec)
	}
	if !parsed.Spec.RestorePVs {
		t.Error("restorePVs is not set, so the volume DATA would not be restored")
	}
	if len(parsed.Spec.OrLabelSelectors) != 1 {
		t.Errorf("the label narrowing is missing: %+v", parsed.Spec.OrLabelSelectors)
	}
	// Mode 1 excludes Jobs unless they are asked for; a mode-2 request carries
	// its own type filter and restores no workloads, so the exclusion is written
	// only when the run is mode 1.
	if len(parsed.Spec.ExcludedResources) != 0 {
		t.Errorf("a mode-2 request excluded %v, which its type filter already excludes", parsed.Spec.ExcludedResources)
	}

	doc, err = restoreDocument(restoreRequest{Name: "r", Backup: "b", Namespace: "payments", OperationID: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	parsed = struct {
		Spec struct {
			BackupName         string           `yaml:"backupName"`
			IncludedNamespaces []string         `yaml:"includedNamespaces"`
			IncludedResources  []string         `yaml:"includedResources"`
			RestorePVs         bool             `yaml:"restorePVs"`
			OrLabelSelectors   []map[string]any `yaml:"orLabelSelectors"`
			ExcludedResources  []string         `yaml:"excludedResources"`
		} `yaml:"spec"`
	}{}
	if err := yaml.Unmarshal(doc, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Spec.ExcludedResources) != 1 || parsed.Spec.ExcludedResources[0] != "jobs" {
		t.Errorf("mode 1 does not exclude Jobs (%v): a restored Job runs before activation", parsed.Spec.ExcludedResources)
	}
	if len(parsed.Spec.IncludedResources) != 0 {
		t.Errorf("mode 1 narrowed the type filter (%v): a hand-written list drops whatever the customer's own resources are", parsed.Spec.IncludedResources)
	}
	if fmt.Sprint(parsed.Spec.RestorePVs) != "true" {
		t.Error("mode 1 does not restore volume data")
	}
}

// restoreDocSelector is the labelSelector half of a rendered Restore, parsed
// the way the API server holds it.
type restoreDocSelector struct {
	MatchLabels      map[string]string `yaml:"matchLabels"`
	MatchExpressions []struct {
		Key      string   `yaml:"key"`
		Operator string   `yaml:"operator"`
		Values   []string `yaml:"values"`
	} `yaml:"matchExpressions"`
}

// TestRestoreDocumentCarriesLabelsAndExpressionsInOneSelector pins the shape of
// the restore's narrowing: matchLabels and matchExpressions are two halves of
// ONE labelSelector, which is what lets mode 1 keep the pod a Job created out
// of the restore while mode 2 narrows the restore by a workload's labels.
func TestRestoreDocumentCarriesLabelsAndExpressionsInOneSelector(t *testing.T) {
	doc, err := restoreDocument(restoreRequest{
		Name: "r", Backup: "b", Namespace: "payments", OperationID: "abc123",
		LabelSelector:    map[string]string{"app": "payments"},
		LabelExpressions: JobPodsExcluded(),
	})
	if err != nil {
		t.Fatalf("rendering a restore with both halves of a labelSelector: %v", err)
	}
	var parsed struct {
		Spec struct {
			LabelSelector *restoreDocSelector `yaml:"labelSelector"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(doc, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Spec.LabelSelector == nil {
		t.Fatalf("the restore carries no labelSelector:\n%s", doc)
	}
	if parsed.Spec.LabelSelector.MatchLabels["app"] != "payments" {
		t.Errorf("matchLabels = %v, want the app label the request named", parsed.Spec.LabelSelector.MatchLabels)
	}
	if len(parsed.Spec.LabelSelector.MatchExpressions) != 1 {
		t.Fatalf("matchExpressions = %+v, want the one requirement the request named — in the SAME labelSelector, not a second one the API server would reject", parsed.Spec.LabelSelector.MatchExpressions)
	}
	expression := parsed.Spec.LabelSelector.MatchExpressions[0]
	if expression.Key != JobPodLabelKey || expression.Operator != "DoesNotExist" {
		t.Errorf("matchExpressions[0] = %+v, want %s DoesNotExist", expression, JobPodLabelKey)
	}
	// Kubernetes rejects `values` on DoesNotExist, so the renderer must not
	// write it.
	if len(expression.Values) != 0 {
		t.Errorf("the requirement carries values %v: DoesNotExist takes none", expression.Values)
	}
}

// TestRestoreDocumentRefusesASelectorAndOrSelectors together: Velero treats
// labelSelector and orLabelSelectors as contradictory, so the renderer refuses
// the pair rather than writing a Restore the API server would reject. Neither
// mode produces it today — mode 1 sets no orLabelSelectors, and mode 2 sets one
// shape or the other — which is exactly why the refusal has to be written down
// here rather than discovered on a cluster.
func TestRestoreDocumentRefusesASelectorAndOrSelectors(t *testing.T) {
	cases := map[string]restoreRequest{
		"matchLabels and orLabelSelectors": {
			Name: "r", Backup: "b", Namespace: "payments", OperationID: "abc123",
			LabelSelector:    map[string]string{"app": "payments"},
			OrLabelSelectors: []map[string]string{{"app": "storefront"}},
		},
		"matchExpressions and orLabelSelectors": {
			Name: "r", Backup: "b", Namespace: "payments", OperationID: "abc123",
			LabelExpressions: JobPodsExcluded(),
			OrLabelSelectors: []map[string]string{{"app": "storefront"}},
		},
	}
	for name, request := range cases {
		if _, err := restoreDocument(request); err == nil {
			t.Errorf("%s: the renderer wrote a Restore with both selector shapes, which Velero refuses", name)
		}
	}
}

// On hardware (2026-09-27, S4 on lab w3) a namespace restore sat InProgress
// for 48 minutes: the restored claim kept the volumeName of a PersistentVolume
// that went with the deleted namespace, so its Pod never started and no
// PodVolumeRestore ran. The Restore set includeClusterResources to false,
// which keeps Velero from processing the volumes at all, so it never cleared
// that name. P5 measured the working restores with the key left unset.
func TestRestoreDocumentLeavesClusterResourcesToVelero(t *testing.T) {
	for name, req := range map[string]restoreRequest{
		"mode 1": {Name: "r", Backup: "b", Namespace: "payments", OperationID: "abc123"},
		"mode 2": {Name: "r", Backup: "b", Namespace: "payments", OperationID: "abc123",
			IncludedResources: []string{"persistentvolumeclaims", "persistentvolumes", "pods"}},
	} {
		doc, err := restoreDocument(req)
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			Spec map[string]any `yaml:"spec"`
		}
		if err := yaml.Unmarshal(doc, &parsed); err != nil {
			t.Fatal(err)
		}
		if v, set := parsed.Spec["includeClusterResources"]; set && v == false {
			t.Errorf("%s: the Restore sets includeClusterResources false, so Velero never processes the volumes and a restored claim waits on its deleted one", name)
		}
	}
}
