// Package recovery is the orchestration shared by the two install paths that
// rebuild a cluster from its bucket: a lost single-server workload host (S6,
// T4.9) and a lost all-in-one host (S11, T4.8).
//
// What lives here is everything about a recovery that does NOT depend on the
// install engine: picking the recovery set for one cluster, proving the
// artifact opens and belongs to that cluster before anything is touched, taking
// recovery ownership outside the cluster, opening the existing Velero
// repository rather than initialising a new one, and refusing a replacement
// host that is too small for the volumes about to be restored.
//
// The install stages in pkg/install drive this package; the operator-facing
// `kubenest recovery-kit check` command asks the same four questions, because
// the recovery install asks them itself before its first change (PLAN 7.9 step
// 2, 7.8).
package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/s3"
)

// Store is the slice of an S3-compatible bucket a recovery reads. *s3.Client
// satisfies it.
//
// List is here and not only Get because a recovery is given a CLUSTER, not an
// artifact: `--cluster <id>` names the immutable cluster id, the sets for it
// live under one prefix, and "latest" means the newest eligible set there. No
// display name is ever consulted.
type Store interface {
	Get(ctx context.Context, key string) ([]byte, error)
	List(ctx context.Context, prefix string) (keys []string, truncated bool, err error)
}

// Request is what one recovery is for.
type Request struct {
	// Kind is which authority the artifact belongs to: a workload cluster's
	// kit, or the instance's own control-plane kit.
	Kind recoverykit.Kind
	// ClusterID is the cluster's immutable id. A display name never
	// authorises adoption, so this is the ONLY selector.
	ClusterID string
	// ArtifactID names one kit artifact. Empty means the newest eligible set
	// for this cluster.
	ArtifactID string
	// Expected is what the operator says this instance, organisation and
	// cluster are. Anything the bucket holds that disagrees is refused.
	Expected recoverykit.Binding
	// Backup, when set, is a backup the set must record as completed.
	Backup string
}

// Selection is what the bucket says a recovery of one cluster starts from: the
// recovery set, the kit it binds, and the backup to restore.
type Selection struct {
	Set    *recoverykit.Set
	Kit    *recoverykit.Kit
	KitDoc []byte
	// Local is this machine's copy of the kit document, when it has one. It is
	// not required: the whole point of S6 is a laptop that never held one.
	Local []byte
	// Backup is the backup this recovery restores.
	Backup recoverykit.Backup
	// SetKey and KitKey are where the two artifacts were read from, so every
	// message can name the object rather than describe it.
	SetKey string
	KitKey string
}

// Validate refuses a request that cannot identify what to recover. It runs
// before anything is read, so a mistyped flag costs nothing.
func (r Request) Validate() error {
	if strings.TrimSpace(r.ClusterID) == "" {
		return errors.New("a recovery is selected by the cluster's IMMUTABLE id, never by its display name: pass --cluster <cluster-id> (the id in the recovery set's binding, not the name the cluster is shown under)")
	}
	if r.Kind != recoverykit.KindCluster && r.Kind != recoverykit.KindControlPlane {
		return fmt.Errorf("a recovery set is either a cluster's or the instance's (%q/%q), not %q", recoverykit.KindCluster, recoverykit.KindControlPlane, r.Kind)
	}
	if r.Kind == recoverykit.KindCluster && (r.Expected.OrganisationID == "" || r.Expected.ClusterID == "") {
		return errors.New("a cluster recovery needs the organisation and cluster ids to check the set against: pass --org, or let the register stage fill them from the cluster record this id names")
	}
	if r.Expected.InstanceID == "" {
		return errors.New("a recovery needs the instance id every kit and set is bound to: pass --instance-id, or read it from the machine that installed the control plane")
	}
	if r.Expected.ClusterID != "" && r.Expected.ClusterID != r.ClusterID {
		return fmt.Errorf("this recovery is for cluster %s and is being asked to check a set bound to %s: a display name never authorises adopting a different cluster", r.ClusterID, r.Expected.ClusterID)
	}
	return nil
}

// setPrefix is where every set for one cluster lives.
func setPrefix(scope string, req Request) string {
	return strings.Trim(scope, "/") + "/" + recoverykit.SetsDir + "/" + req.ClusterID + "/"
}

// Select finds the newest eligible recovery set for the cluster's immutable id,
// proves it belongs to this instance, organisation and cluster, and returns
// the kit it binds.
//
// THE OWNERSHIP CHECK COMES BEFORE ANYTHING IS OPENED. A set from another
// cluster is not a suspect artifact to be examined further: it is the wrong
// artifact, and the refusal names the instance, organisation and cluster it
// actually belongs to so the operator can see which prefix they are reading
// (PLAN 7.8, 7.9 step 2).
func Select(ctx context.Context, store Store, scope string, req Request) (*Selection, error) {
	if store == nil {
		return nil, errors.New("a recovery needs the bucket: no S3 client was built")
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}

	keys, truncated, err := store.List(ctx, setPrefix(scope, req))
	if err != nil {
		return nil, fmt.Errorf("listing the recovery sets for cluster %s under %s: %w", req.ClusterID, setPrefix(scope, req), err)
	}
	if truncated {
		return nil, fmt.Errorf("the recovery sets for cluster %s under %s are truncated: listing them would silently hide the newest one, and picking the wrong set is not a guess this command may make", req.ClusterID, setPrefix(scope, req))
	}
	if req.ArtifactID != "" {
		want := recoverykit.SetKey(scope, req.ClusterID, req.Kind, req.ArtifactID)
		if !contains(keys, want) {
			return nil, fmt.Errorf("there is no recovery set for %s artifact %s: the bucket holds %s", req.Kind, req.ArtifactID, describeKeys(keys))
		}
		keys = []string{want}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no recovery set exists for cluster %s under %s: nothing in the bucket says which kit and which backup a recovery of this cluster starts from. Cluster ids are immutable and never the display name, so check that %s is the id the install recorded — and that this credential can reach this cluster's prefix at all", req.ClusterID, setPrefix(scope, req), req.ClusterID)
	}

	sets := make([]*recoverykit.Set, 0, len(keys))
	byKey := map[*recoverykit.Set]string{}
	for _, key := range keys {
		raw, err := store.Get(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("reading the recovery set at %s: %w", key, err)
		}
		set, err := recoverykit.LoadSet(raw)
		if err != nil {
			return nil, fmt.Errorf("%s is not a readable recovery set: %w", key, err)
		}
		if err := set.Verify(req.Expected); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		if !set.Eligible() {
			// Not a refusal: an incomplete set is simply not a candidate.
			// Refusing here would make one interrupted upload block the
			// recovery of a cluster whose last good set is right beside it.
			continue
		}
		if req.Backup != "" {
			if _, ok := completedBackup(set, req.Backup); !ok {
				continue
			}
		} else if _, ok := set.Latest(); !ok {
			continue
		}
		if req.ArtifactID != "" && set.ArtifactID != req.ArtifactID {
			continue
		}
		sets = append(sets, set)
		byKey[set] = key
	}
	if len(sets) == 0 {
		return nil, fmt.Errorf("cluster %s has %d recovery set(s) under %s and none of them is usable: every one is either an upload that never completed, or names no completed backup. A partial upload is never something to recover from", req.ClusterID, len(keys), setPrefix(scope, req))
	}

	// Newest written first, then newest artifact id as the tie-break, so two
	// sets written in the same second still order deterministically.
	sort.SliceStable(sets, func(i, j int) bool {
		if !sets[i].WrittenAt.Equal(sets[j].WrittenAt) {
			return sets[i].WrittenAt.After(sets[j].WrittenAt)
		}
		return sets[i].ArtifactID > sets[j].ArtifactID
	})
	return materialise(ctx, store, scope, req, sets[0], byKey[sets[0]])
}

// setSuffix returns the file name a set for one artifact has inside its
// cluster's directory, which is what tells a control-plane set from a cluster
// set when a listing returns both.
func setSuffix(kind recoverykit.Kind) string { return string(kind) + "-" }

// SelectControlPlane finds the instance's own recovery set.
//
// IT CANNOT BE SELECTED BY CLUSTER ID, because the operator recovering an
// all-in-one host does not know the management cluster's id — that id is
// inside the artifact they cannot read yet. What identifies the set instead is
// the scope: a --control-plane install's bucket prefix belongs to the
// management cluster alone and holds exactly one control-plane kit, so the
// newest eligible control-plane set under that prefix is THE one. The set's
// binding then says which instance, organisation and cluster it belongs to, and
// a set bound to another instance is refused by name.
func SelectControlPlane(ctx context.Context, store Store, scope, instanceID, artifactID string) (*Selection, error) {
	if store == nil {
		return nil, errors.New("a recovery needs the bucket: no S3 client was built")
	}
	root := strings.Trim(scope, "/") + "/" + recoverykit.SetsDir + "/"
	keys, truncated, err := store.List(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("listing the recovery sets under %s: %w", root, err)
	}
	if truncated {
		return nil, fmt.Errorf("the recovery sets under %s are truncated: listing them would silently hide the newest one, and picking the wrong set is not a guess this command may make", root)
	}
	candidates := make([]string, 0, len(keys))
	for _, key := range keys {
		if !strings.HasSuffix(key, ".json") || !strings.Contains(key, "/"+setSuffix(recoverykit.KindControlPlane)) {
			continue
		}
		if artifactID != "" && !strings.Contains(key, "/"+setSuffix(recoverykit.KindControlPlane)+artifactID+".json") {
			continue
		}
		candidates = append(candidates, key)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no control-plane recovery set exists under %s: nothing in this bucket says which kit and which checkpoint the instance's control plane should be rebuilt from. Check that this is the management cluster's prefix — a --control-plane install writes its kit there — and that this credential can read it", root)
	}
	sets := make([]*recoverykit.Set, 0, len(candidates))
	byKey := map[*recoverykit.Set]string{}
	instances := map[string]struct{}{}
	for _, key := range candidates {
		raw, err := store.Get(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("reading the recovery set at %s: %w", key, err)
		}
		set, err := recoverykit.LoadSet(raw)
		if err != nil {
			return nil, fmt.Errorf("%s is not a readable recovery set: %w", key, err)
		}
		if instanceID != "" {
			if err := set.Verify(recoverykit.Binding{Kind: recoverykit.KindControlPlane, InstanceID: instanceID}); err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
		} else if set.Binding.InstanceID != "" {
			instances[set.Binding.InstanceID] = struct{}{}
		}
		if !set.Eligible() {
			continue
		}
		sets = append(sets, set)
		byKey[set] = key
	}
	if len(sets) == 0 {
		return nil, fmt.Errorf("the %d control-plane recovery set(s) under %s are all uploads that never completed, and a partially uploaded set is never something to recover from", len(candidates), root)
	}
	// WITH NO INSTANCE ID THE SCOPE ANSWERS INSTEAD. A --control-plane install's
	// prefix holds exactly one instance's artifacts, so a listing that finds
	// two instances under it is a bucket someone has combined — and adopting
	// either would be guessing which control plane this host is.
	if instanceID == "" {
		if len(instances) > 1 {
			names := make([]string, 0, len(instances))
			for id := range instances {
				names = append(names, id)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("the control-plane recovery sets under %s belong to %d different instances (%s): this bucket prefix is not one instance's, and which control plane to rebuild is not a guess this command may make. Pass --instance-id to say which, or point --backup-target at that instance's own prefix", root, len(instances), strings.Join(names, ", "))
		}
		for id := range instances {
			instanceID = id
		}
	}
	if instanceID == "" {
		return nil, fmt.Errorf("the control-plane recovery sets under %s record no instance id, so nothing says which fleet they belong to. Pass --instance-id", root)
	}

	// Newest written first, then newest artifact id as the tie-break.
	sort.SliceStable(sets, func(i, j int) bool {
		if !sets[i].WrittenAt.Equal(sets[j].WrittenAt) {
			return sets[i].WrittenAt.After(sets[j].WrittenAt)
		}
		return sets[i].ArtifactID > sets[j].ArtifactID
	})
	set := sets[0]
	// The instance's own set is bound to the MANAGEMENT cluster, so the id is
	// read from it rather than passed in.
	expected := recoverykit.Binding{Kind: recoverykit.KindControlPlane, InstanceID: instanceID, ClusterID: set.Binding.ClusterID}
	return materialise(ctx, store, scope, Request{Kind: recoverykit.KindControlPlane, ClusterID: set.Binding.ClusterID, Expected: expected}, set, byKey[set])
}

// materialise reads the kit a selected set binds, proves the object is the one
// the set was written for, and returns what a recovery will restore.
func materialise(ctx context.Context, store Store, scope string, req Request, set *recoverykit.Set, setKey string) (*Selection, error) {
	if set.Binding.ClusterID == "" {
		return nil, fmt.Errorf("the recovery set at %s is bound to no cluster, so there is nothing to open a kit against", setKey)
	}
	kitKey := recoverykit.KitKey(scope, set.Binding.ClusterID, set.Binding.Kind, set.ArtifactID)
	kitDoc, err := store.Get(ctx, kitKey)
	if err != nil {
		return nil, fmt.Errorf("reading the recovery kit the set names, at %s: %w", kitKey, err)
	}
	if got, want := recoverykit.Digest(kitDoc), set.Checksums["kit"]; want != "" && got != want {
		return nil, fmt.Errorf("the recovery kit at %s digests %s and the recovery set records %s: the object in the bucket is not the kit the set was written for, and nothing here should be recovered from it", kitKey, got, want)
	}
	kit, err := recoverykit.Load(kitDoc)
	if err != nil {
		return nil, fmt.Errorf("%s is not a readable recovery kit: %w", kitKey, err)
	}
	if err := kit.Verify(req.Expected); err != nil {
		return nil, fmt.Errorf("%s: %w", kitKey, err)
	}
	if kit.ArtifactID != set.ArtifactID {
		return nil, fmt.Errorf("the recovery set at %s binds kit artifact %s and the kit at %s is artifact %s: the two do not describe the same artifact", setKey, set.ArtifactID, kitKey, kit.ArtifactID)
	}
	backup, ok := completedBackup(set, req.Backup)
	if !ok {
		// A control-plane recovery starts from its CHECKPOINT, not from the
		// management cluster's workload backup, and the set is written before
		// either exists. An empty selection is reported as such rather than
		// invented.
		backup = recoverykit.Backup{}
	}
	local, err := localCopy(req, set.ArtifactID)
	if err != nil {
		return nil, err
	}
	return &Selection{
		Set:    set,
		Kit:    kit,
		KitDoc: kitDoc,
		Local:  local,
		Backup: backup,
		SetKey: setKey,
		KitKey: kitKey,
	}, nil
}

// localCopy is this machine's copy of the kit, when it has one. A recovery
// from a laptop that never ran the install has none, and that is not an error:
// the uploaded copy is the one a recovery has.
//
// Only the artifact this recovery selected is looked at. A newer local kit for
// another artifact would be a different artifact, and comparing against it
// would report "the upload is not the kit" about the wrong thing.
func localCopy(req Request, artifactID string) ([]byte, error) {
	path, err := recoverykit.LocalPath(req.ClusterID, req.Kind, artifactID)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading this machine's copy of the recovery kit at %s: %w", path, err)
	}
	return raw, nil
}

func completedBackup(set *recoverykit.Set, name string) (recoverykit.Backup, bool) {
	if name == "" {
		return set.Latest()
	}
	for _, b := range set.Backups {
		if b.Name == name && b.Completed() {
			return b, true
		}
	}
	return recoverykit.Backup{}, false
}

func contains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

func describeKeys(keys []string) string {
	if len(keys) == 0 {
		return "none"
	}
	return strings.Join(keys, ", ")
}

// Answer is one answer to one of the four questions a check asks, with the line
// that explains it. It is a struct rather than a bool because four booleans and
// a string are what stop one finding being reported as another — the failure
// mode that matters: "the key is wrong" read as "the upload is damaged" sends
// an operator to the wrong system on the worst day of their year.
type Answer struct {
	OK     bool
	Detail string
}

// Check is the four answers, deliberately not collapsed into one verdict.
type Check struct {
	Upload       Answer
	Decryption   Answer
	Fingerprints Answer
	Set          Answer
}

// AllGood reports whether every answer passed.
func (c Check) AllGood() bool {
	return c.Upload.OK && c.Decryption.OK && c.Fingerprints.OK && c.Set.OK
}

// Failed names the questions that did NOT pass, so a refusal can say which one
// and not "the check failed".
func (c Check) Failed() []string {
	var out []string
	if !c.Upload.OK {
		out = append(out, "the kit's upload is not intact")
	}
	if !c.Decryption.OK {
		out = append(out, "the fleet key supplied does not open the kit")
	}
	if !c.Fingerprints.OK {
		out = append(out, "the opened key material does not match the kit's fingerprints")
	}
	if !c.Set.OK {
		out = append(out, "the recovery set is not complete, or does not belong here")
	}
	return out
}

// Verify opens the selection with the fleet key and answers the four questions
// separately: whether the upload is intact, whether the key opens it, whether
// the opened material matches the kit's fingerprints, and whether the set is
// complete and about this cluster.
//
// A SET BOUND TO ANOTHER CLUSTER IS A REFUSAL, NOT AN ANSWER, and Select has
// already returned it as one. Everything else is reported as a question with an
// answer, because each has a different fix.
func Verify(ctx context.Context, store Store, sel *Selection, fleetKey string) (Check, error) {
	var out Check
	if sel == nil || sel.Set == nil || sel.Kit == nil {
		return out, errors.New("there is nothing to check: no recovery set was selected")
	}
	if strings.TrimSpace(fleetKey) == "" {
		return out, errors.New("a check needs the fleet recovery key: it is what opens the kit, and without it no answer about the key material means anything")
	}

	// 1. Is the upload intact? The set records the digest of the kit it was
	// written for, and a local copy is byte-compared when this machine has one.
	fetched, fetchErr := store.Get(ctx, sel.KitKey)
	switch {
	case fetchErr != nil:
		out.Upload = Answer{Detail: fmt.Sprintf("could not read %s: %v", sel.KitKey, fetchErr)}
	case sel.Set.Checksums["kit"] == "":
		out.Upload = Answer{Detail: fmt.Sprintf("the recovery set at %s records no kit digest, so nothing in the bucket says what the object at %s should be", sel.SetKey, sel.KitKey)}
	case recoverykit.Digest(fetched) != sel.Set.Checksums["kit"]:
		out.Upload = Answer{Detail: fmt.Sprintf("the object at %s is not the kit the recovery set names: it digests %s and the set records %s", sel.KitKey, recoverykit.Digest(fetched), sel.Set.Checksums["kit"])}
	case len(sel.Local) > 0 && recoverykit.Digest(sel.Local) != recoverykit.Digest(fetched):
		out.Upload = Answer{Detail: fmt.Sprintf("the uploaded kit at %s is not byte-identical to this machine's copy: uploaded %s, local %s", sel.KitKey, recoverykit.Digest(fetched), recoverykit.Digest(sel.Local))}
	case len(sel.Local) > 0:
		out.Upload = Answer{OK: true, Detail: fmt.Sprintf("the object at %s is byte-identical to this machine's copy, and both match the digest the recovery set records", sel.KitKey)}
	default:
		out.Upload = Answer{OK: true, Detail: fmt.Sprintf("the object at %s matches the digest the recovery set records (%s); this machine holds no copy of its own, which is the state a recovery from a fresh laptop is in", sel.KitKey, sel.Set.Checksums["kit"])}
	}

	// 2 and 3. Does the key open it, and is what opened the material the
	// header was written for? Both copies that exist are opened.
	opened := map[string]*recoverykit.Kit{"uploaded": sel.Kit}
	if len(sel.Local) > 0 {
		if local, err := recoverykit.Load(sel.Local); err == nil {
			opened["local"] = local
		}
	}
	var names []string
	decryptFail := []string{}
	fpFail := []string{}
	for name, kit := range opened {
		secrets, err := kit.Secrets(fleetKey)
		if err != nil {
			decryptFail = append(decryptFail, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		names = append(names, name)
		if err := kit.FingerprintsMatch(secrets); err != nil {
			fpFail = append(fpFail, fmt.Sprintf("%s: %v", name, err))
		}
	}
	sort.Strings(names)
	sort.Strings(decryptFail)
	sort.Strings(fpFail)
	keyNames := strings.Join(sel.Kit.KeyNames(), ", ")
	switch {
	case len(names) == 0:
		out.Decryption = Answer{Detail: fmt.Sprintf("the fleet key supplied does NOT open the kit: %s. This is not the key the kit was sealed to, or it was mistyped — and a kit that does not open is not recovered by retrying it", strings.Join(decryptFail, "; "))}
	case len(decryptFail) > 0:
		out.Decryption = Answer{Detail: fmt.Sprintf("the fleet key supplied opens %s but not %s — the copies are not the same kit, and the bucket's copy is the one a recovery has, so this counts as not opening: %s", strings.Join(names, ", "), strings.Join(decryptFail, "; "), strings.Join(decryptFail, "; "))}
	default:
		out.Decryption = Answer{OK: true, Detail: fmt.Sprintf("the fleet key supplied opens %s (key material carried: %s)", strings.Join(names, " and "), keyNames)}
	}
	switch {
	case len(names) == 0:
		out.Fingerprints = Answer{Detail: "fingerprints could not be checked: no copy opened"}
	case len(fpFail) == 0:
		out.Fingerprints = Answer{OK: true, Detail: fmt.Sprintf("what opened is the key material the kit's header was written for (%s)", keyNames)}
	default:
		out.Fingerprints = Answer{Detail: fmt.Sprintf("the kit opened but its fingerprints disagree, so the sealed payload is not the one the header describes: %s", strings.Join(fpFail, "; "))}
	}

	// 4. Is the set complete, and does it belong here? Select has already
	// verified the binding; this states it for the record.
	switch {
	case !sel.Set.Complete:
		out.Set = Answer{Detail: fmt.Sprintf("%s records an upload that never completed, and a partial upload is never something to recover from", sel.SetKey)}
	case sel.Backup.Name == "":
		out.Set = Answer{Detail: fmt.Sprintf("%s names no completed backup", sel.SetKey)}
	default:
		out.Set = Answer{OK: true, Detail: fmt.Sprintf("%s is complete and belongs to %s; it names backup %q, completed at %s, covering %s", sel.SetKey, sel.Set.Binding, sel.Backup.Name, sel.Backup.CompletedAt.UTC().Format(time.RFC3339), coverage(sel.Backup))}
	}
	return out, nil
}

func coverage(b recoverykit.Backup) string {
	if len(b.Coverage) == 0 {
		return "no namespaces recorded"
	}
	return strings.Join(b.Coverage, ", ")
}

// Scope is the bucket prefix one cluster's artifacts live under, derived from
// the location a kit records. It is here so the install and the check command
// agree on where a kit's prefix came from rather than each trimming their own.
func Scope(loc recoverykit.Location) string { return strings.Trim(loc.Prefix, "/") }

// IsNotFound reports whether an object read failed because the object is not
// there, so a caller can tell "the upload never completed" from "the bucket is
// unreachable".
func IsNotFound(err error) bool { return errors.Is(err, s3.ErrNotFound) }
