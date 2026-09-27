package backup

import (
	"context"
	"fmt"
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
		},
	}}
	f.cluster.resolveReplicaSetOwner = &OwnerRef{Kind: "Deployment", Name: "payments", Controller: true}
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
	// volume, its mount and restore-wait's mount, and leave a alone.
	rules := modifierRules(t, f)
	if len(rules) < 2 {
		t.Fatalf("the modifier holds %d rule(s), want the pod-template-hash rule and one volume-strip rule", len(rules))
	}
	assertHashRule(t, rules[0])
	strip := stripPatches(t, rules)
	data := strip["payments-6d9f-abcde"]
	if data == "" {
		t.Fatalf("no volume-strip rule targets the pod that mounts the claims: %v", strip)
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

	// The verification is per claim, and it landed on a live node.
	if !strings.Contains(out, "pvc-fresh-a") || !strings.Contains(out, "lab-node-1") {
		t.Errorf("the run does not report which volume the claim came back on and which node it lives on:\n%s", out)
	}
	if !strings.Contains(out, RestoredStage) {
		t.Errorf("a volume restore does not stop at %q:\n%s", RestoredStage, out)
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
// patch level, with no cluster in the way: a pod whose named claim is a gets a
// patch that removes b's volume and mounts and never a's.
func TestVolumeStripPatchLeavesAnUnnamedVolumeAlone(t *testing.T) {
	pod := PodState{
		Name: "web-1",
		Volumes: []PodVolumeState{
			{Name: "a", ClaimName: "a", Mounts: []PodMount{{Container: "app", Path: "/a"}}},
			{Name: "b", ClaimName: "b", Mounts: []PodMount{
				{Container: "app", Path: "/b"},
				{Container: "restore-wait", Path: "/restores/b", Init: true},
			}},
			{Name: "emptyDir", ClaimName: ""},
		},
	}
	patch := volumeStripPatch(pod, map[string]bool{"a": true})
	for _, want := range []string{`"name":"b"`, `"mountPath":"/b"`, `"mountPath":"/restores/b"`} {
		if !strings.Contains(strings.ReplaceAll(patch, " ", ""), want) {
			t.Errorf("the patch does not remove %s: %s", want, patch)
		}
	}
	if strings.Contains(patch, `"name":"a"`) || strings.Contains(patch, `"mountPath":"/a"`) {
		t.Errorf("the patch touches the named volume: %s", patch)
	}
	if strings.Contains(patch, "emptyDir") {
		t.Errorf("the patch removes a volume that is not a claim at all: %s", patch)
	}
	// A pod that mounts only named claims needs no patch, and gets none: an
	// empty one would be an object Velero has to parse for nothing.
	if only := volumeStripPatch(PodState{Name: "solo", Volumes: []PodVolumeState{{Name: "a", ClaimName: "a"}}}, map[string]bool{"a": true}); only != "" {
		t.Errorf("a pod with no unnamed volume got the patch %s", only)
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
