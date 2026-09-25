//go:build e2e

// T4.6's acceptance on real hosts: the fleet recovery key, the two kit scopes,
// the recovery sets, and what the bucket exposes.
//
// It runs the S1 steps, on the same fixture the rest of the wave gate uses:
//
//  1. a control plane on host 1, with a backup target, printing its fleet
//     recovery key exactly once;
//  2. a second cluster from a second laptop that holds ONLY the public
//     recipient — the private key never exists there;
//  3. a workload with a PLANTED Secret on each cluster, mounted into a pod
//     volume so the value is in the volume data as well as in the objects;
//  4. the first backup through `kubenest backup now`, then the exposure
//     matrix read out of the bucket itself.
//
// What it asserts, and why each is the property that matters:
//
//   - the fleet key was printed once, in the control-plane install's output;
//   - the control-plane kit was DECRYPTED from both its local and its uploaded
//     copy, and the kit stage finished before any backup completed (timestamp
//     ordering against the journal, not a re-run);
//   - the second cluster's kit was uploaded and DIGEST-verified before its
//     first backup could exist, and that install never held or printed a
//     private key;
//   - the planted Secret is not readable from the volume backup or the
//     datastore snapshot and IS readable from Velero's resource archive —
//     the exposure the docs state, measured rather than assumed;
//   - `backup set-target` refuses a credential that can read the control-plane
//     prefix.
//
// Run from the umbrella workspace:
//
//	source lab/hetzner/.lab-env.sh
//	export KUBENEST_CONTROL_PLANE=http://localhost:8000
//	export KUBENEST_CLI_TOKEN=knp_...
//	export KUBENEST_LAB_NODE2_IP=<second host>
//	export KUBENEST_BACKUP_TARGET='s3://bucket?endpoint=minio:9000&region=main'
//	export KUBENEST_BACKUP_ACCESS_KEY_ID=... KUBENEST_BACKUP_SECRET_ACCESS_KEY=...
//	export KUBENEST_BACKUP_CLUSTER_A_PREFIX=cluster-a
//	export KUBENEST_BACKUP_CLUSTER_B_PREFIX=cluster-b
//	export KUBENEST_BACKUP_WIDE_KEY_ID=... KUBENEST_BACKUP_WIDE_SECRET=...
//	cd kubenest-cli && go test -tags e2e -v -timeout 90m ./e2e/ -run TestKitAndBucketExposure
package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/cmd"
	"kubenest.io/cli/pkg/config"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/s3"
	"kubenest.io/cli/pkg/sshx"
)

// kitEnv is the fixture this gate needs beyond gateEnvironment's.
type kitEnv struct {
	// secondServer is the host the second cluster is built on. A cluster per
	// host is the point: two laptops, one bucket.
	secondServer string
	// targetFlag is the s3:// target both clusters use, with a prefix each.
	// ONE bucket, because the boundary this checks is the credential's prefix
	// scope, not a bucket boundary.
	targetFlag    string
	prefixA       string
	prefixB       string
	accessKeyID   string
	accessSecret  string
	wideKeyID     string
	wideSecret    string
	plantedSecret string
	plantedNS     string
	secondStorage string
}

func kitEnvironment(t *testing.T) kitEnv {
	t.Helper()
	env := kitEnv{
		secondServer:  os.Getenv("KUBENEST_LAB_NODE2_IP"),
		targetFlag:    os.Getenv("KUBENEST_BACKUP_TARGET"),
		prefixA:       envOr("KUBENEST_BACKUP_CLUSTER_A_PREFIX", "cluster-a"),
		prefixB:       envOr("KUBENEST_BACKUP_CLUSTER_B_PREFIX", "cluster-b"),
		accessKeyID:   os.Getenv("KUBENEST_BACKUP_ACCESS_KEY_ID"),
		accessSecret:  os.Getenv("KUBENEST_BACKUP_SECRET_ACCESS_KEY"),
		wideKeyID:     os.Getenv("KUBENEST_BACKUP_WIDE_KEY_ID"),
		wideSecret:    os.Getenv("KUBENEST_BACKUP_WIDE_SECRET"),
		plantedNS:     "kit-exposure",
		secondStorage: os.Getenv("KUBENEST_LAB_NODE2_STORAGE_DEVICE"),
	}
	// The planted value is derived, never a literal that could be found by
	// accident — and never printed, so a leaked test log says nothing.
	env.plantedSecret = "kubenest-kit-exposure-" + randomSuffix()
	if env.secondServer == "" {
		t.Skip("KUBENEST_LAB_NODE2_IP not set: this gate needs two hosts, one cluster each")
	}
	if env.targetFlag == "" || env.accessKeyID == "" || env.accessSecret == "" {
		t.Skip("KUBENEST_BACKUP_TARGET and KUBENEST_BACKUP_ACCESS_KEY_ID / KUBENEST_BACKUP_SECRET_ACCESS_KEY not set: the bucket arms cannot be checked without a real S3-compatible store")
	}
	return env
}

// TestKitAndBucketExposure is T4.6's real-hardware acceptance.
func TestKitAndBucketExposure(t *testing.T) {
	env := gateEnvironment(t)
	kit := kitEnvironment(t)
	ctx := context.Background()

	// Two HOMEs, because two laptops. The second is seeded with a copy of the
	// first's CLI state — the way someone who logged in on this machine has it
	// — and the fleet key is in neither, because it is written nowhere.
	homeOne := t.TempDir()
	homeTwo := t.TempDir()

	controlPlane, err := api.New(env.controlPlane, api.WithToken(env.token))
	if err != nil {
		t.Fatal(err)
	}
	rawBundle, err := controlPlane.BundleManifest(ctx, env.bundle)
	if err != nil {
		t.Fatalf("fetching bundle %s: %v", env.bundle, err)
	}
	bundle, err := manifest.Parse(rawBundle)
	if err != nil {
		t.Fatalf("parsing bundle %s: %v", env.bundle, err)
	}
	bundlePath := filepath.Join(homeOne, "bundle-"+env.bundle+".yaml")
	if err := os.WriteFile(bundlePath, rawBundle, 0o600); err != nil {
		t.Fatal(err)
	}

	clusterOne := env.cluster + "-cp"
	clusterTwo := env.cluster + "-b"

	// ---------------------------------------------------------------- phase 1
	// The first cluster: the control plane, and the fleet recovery key. The
	// journal lives at the CANONICAL path under this HOME so that later CLI
	// commands (`backup now`) find it exactly as an operator's would.
	t.Setenv("HOME", homeOne)
	journalOne, err := install.JournalPath(clusterOne)
	if err != nil {
		t.Fatal(err)
	}
	var firstOut bytes.Buffer
	started := time.Now()
	sessionOne := kitSession(t, controlPlane, bundle, journalOne, install.Options{
		Bundle: env.bundle, Name: clusterOne, Servers: []string{env.server}, HATier: "single-server",
		SSHUser: env.sshUser, SSHKey: env.sshKey, StorageDevice: env.storageDevice,
		ControlPlaneInstall: true, Domain: env.server + ".sslip.io", AdminEmail: "admin@" + env.server + ".sslip.io",
		BackupTarget: kitTargetFlag(kit.targetFlag, kit.prefixA),
	}, &firstOut)
	defer sessionOne.Close()
	if _, err := install.Execute(ctx, sessionOne, install.Plan(sessionOne)); err != nil {
		t.Fatalf("the control-plane install failed: %v\n%s", err, firstOut.String())
	}
	installElapsed := time.Since(started)
	t.Logf("control-plane install finished in %s (S1's budget is %s)", installElapsed.Round(time.Second), Budget)
	if installElapsed > Budget {
		t.Errorf("the install took %s, over the %s budget — that is a defect in the installer, not a number to revise upward",
			installElapsed.Round(time.Second), Budget)
	}
	output := firstOut.String()

	t.Run("the fleet recovery key is printed exactly once", func(t *testing.T) {
		if got := strings.Count(output, "AGE-SECRET-KEY-1"); got != 1 {
			t.Errorf("the fleet recovery key appears %d times in the install output, want exactly 1:\n%s", got, output)
		}
		key := extractFleetKey(t, output)
		fleet, err := recoverykit.ParseFleetKey(key)
		if err != nil {
			t.Fatalf("the printed key does not parse as a fleet recovery key: %v", err)
		}
		// It is a recipient-shaped public half too, and the private half is
		// not a recipient: the two must not be interchangeable.
		if !strings.HasPrefix(fleet.Recipient(), "age1") {
			t.Errorf("the fleet key's public half is %q, want an age1 recipient", fleet.Recipient())
		}
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.FleetRecipient != fleet.Recipient() {
			t.Errorf("this machine recorded recipient %q and the printed key's public half is %q: a kit sealed to one could not be opened by the other", cfg.FleetRecipient, fleet.Recipient())
		}
		if cfg.InstanceID == "" {
			t.Error("the install recorded no instance id, so no kit can be bound to this instance")
		}
	})

	t.Run("the control-plane install decrypted both copies of its kit", func(t *testing.T) {
		if !strings.Contains(output, "decrypted from both the local and the uploaded copy") {
			t.Errorf("the first control-plane install holds the identity, so it must open both copies and say so:\n%s", output)
		}
		if !strings.Contains(output, "control-plane recovery kit") {
			t.Errorf("the instance's own kit must be written too:\n%s", output)
		}
	})

	// ---------------------------------------------------------------- phase 2
	// The second cluster, from a laptop that holds only the public recipient.
	seedSecondLaptop(t, homeOne, homeTwo)
	t.Setenv("HOME", homeTwo)
	cfgTwo, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfgTwo.FleetRecipient == "" {
		t.Fatal("the seeded second laptop has no recipient: it could not write a kit the fleet key opens")
	}
	journalTwo, err := install.JournalPath(clusterTwo)
	if err != nil {
		t.Fatal(err)
	}
	var secondOut bytes.Buffer
	sessionTwo := kitSession(t, controlPlane, bundle, journalTwo, install.Options{
		Bundle: env.bundle, Name: clusterTwo, Servers: []string{kit.secondServer}, HATier: "single-server",
		SSHUser: env.sshUser, SSHKey: env.sshKey, StorageDevice: kit.secondStorage,
		ControlPlaneCA: []byte(cfgTwo.ControlPlaneCA),
		BackupTarget:   kitTargetFlag(kit.targetFlag, kit.prefixB),
	}, &secondOut)
	defer sessionTwo.Close()
	if _, err := install.Execute(ctx, sessionTwo, install.Plan(sessionTwo)); err != nil {
		t.Fatalf("the second cluster's install failed: %v\n%s", err, secondOut.String())
	}
	second := secondOut.String()

	t.Run("a later install verifies by digest and never holds the key", func(t *testing.T) {
		if strings.Contains(second, "AGE-SECRET-KEY-1") {
			t.Errorf("the second laptop must never see or print a private key:\n%s", second)
		}
		if !strings.Contains(second, "digest-verified against the local copy") {
			t.Errorf("every later install must verify the upload by digest and say so:\n%s", second)
		}
		if strings.Contains(second, "decrypted from both") {
			t.Errorf("a later install must not claim to have decrypted anything:\n%s", second)
		}
		// The local kit really is there, and really is the object in the
		// bucket: the digest check is a comparison, not a claim.
		localKit, err := recoverykit.NewestLocal(sessionTwo.Jnl.ClusterID, recoverykit.KindCluster)
		if err != nil {
			t.Fatalf("the second install wrote no local kit: %v", err)
		}
		localPath, err := recoverykit.LocalPath(sessionTwo.Jnl.ClusterID, recoverykit.KindCluster, localKit.ArtifactID)
		if err != nil {
			t.Fatal(err)
		}
		localDoc, err := os.ReadFile(localPath)
		if err != nil {
			t.Fatal(err)
		}
		clientB := bucketClient(t, kit.targetFlag, kit.prefixB, kit.accessKeyID, kit.accessSecret)
		uploaded, err := clientB.Get(ctx, recoverykit.KitKey(kit.prefixB, sessionTwo.Jnl.ClusterID, recoverykit.KindCluster, localKit.ArtifactID))
		if err != nil {
			t.Fatalf("the second cluster's kit is not in the bucket: %v", err)
		}
		if recoverykit.Digest(uploaded) != recoverykit.Digest(localDoc) {
			t.Errorf("the uploaded kit is not the local one: uploaded %s, local %s", recoverykit.Digest(uploaded), recoverykit.Digest(localDoc))
		}
	})

	// ---------------------------------------------------------------- phase 3
	// A workload whose Secret value is in the objects AND, because it is
	// mounted, in the volume data. That is what makes the exposure matrix
	// measurable: the same string must be absent from two places and present
	// in a third.
	plantWorkload(t, env.server, env.sshUser, env.sshKey, kit.plantedNS, kit.plantedSecret)

	// ---------------------------------------------------------------- phase 4
	// The first backup, through the real command, so the recovery set is
	// written by the path an operator would use.
	t.Setenv("HOME", homeOne)
	nowCmd := cmd.NewBackupCommand()
	var nowOut bytes.Buffer
	nowCmd.SetOut(&nowOut)
	nowCmd.SetErr(&nowOut)
	nowCmd.SetArgs([]string{
		"now",
		"--cluster", clusterOne,
		"--server", env.server,
		"--ssh-user", env.sshUser,
		"--ssh-key", env.sshKey,
		"--bundle-manifest", bundlePath,
	})
	nowCmd.SetContext(ctx)
	if err := nowCmd.Execute(); err != nil {
		t.Fatalf("`kubenest backup now` failed: %v\n%s", err, nowOut.String())
	}

	runnerOne, err := sessionOne.Server()
	if err != nil {
		t.Fatal(err)
	}
	clientA := bucketClient(t, kit.targetFlag, kit.prefixA, kit.accessKeyID, kit.accessSecret)
	set, setDoc := readSet(t, clientA, kit.prefixA, sessionOne.Jnl.ClusterID)
	if set.ArtifactID == "" {
		t.Fatalf("`backup now` wrote no recovery set, so the backup it took cannot be selected by a recovery:\n%s", nowOut.String())
	}

	t.Run("nothing that can back up ran before the kit stage finished", func(t *testing.T) {
		kitAt, ok := sessionOne.Jnl.Completed(install.StageRecoveryKit)
		if !ok {
			t.Fatalf("the journal has no completed %s entry, so nothing here can be ordered against the kit", install.StageRecoveryKit)
		}
		targetAt, ok := sessionOne.Jnl.Completed(install.StageBackupTarget)
		if !ok {
			t.Fatalf("the journal has no completed %s entry: the stage that first allows an upload did not run", install.StageBackupTarget)
		}
		if targetAt.Before(kitAt) {
			t.Errorf("the backup target was configured at %s, before the kit stage finished at %s",
				targetAt.Format(time.RFC3339), kitAt.Format(time.RFC3339))
		}
		for _, b := range veleroBackups(t, runnerOne) {
			if b.CompletedAt.IsZero() || b.CompletedAt.After(kitAt) {
				continue
			}
			t.Errorf("backup %s exists at %s, at or before the kit stage finished at %s: an upload existed before the kit that opens it",
				b.Name, b.CompletedAt.Format(time.RFC3339), kitAt.Format(time.RFC3339))
		}
		if !set.Complete {
			t.Errorf("the recovery set is not marked complete, so no recovery may select it: %s", setDoc)
		}
		if len(set.Backups) == 0 {
			t.Errorf("the recovery set names no backup: %s", setDoc)
		}
		for _, b := range set.Backups {
			if b.Completed() && !b.CompletedAt.After(kitAt) {
				t.Errorf("the set records backup %s at %s, at or before the kit stage finished at %s",
					b.Name, b.CompletedAt.Format(time.RFC3339), kitAt.Format(time.RFC3339))
			}
		}
		// The set's digest is the kit that is actually in the bucket.
		kitBodies, _, err := clientA.List(ctx, kit.prefixA+"/recovery-kits/")
		if err != nil {
			t.Fatal(err)
		}
		if len(kitBodies) == 0 {
			t.Fatal("no kit is in the bucket")
		}
		body, err := clientA.Get(ctx, kitBodies[0])
		if err != nil {
			t.Fatal(err)
		}
		if set.Checksums["kit"] != recoverykit.Digest(body) {
			t.Errorf("the set records kit digest %s and the object in the bucket digests %s", set.Checksums["kit"], recoverykit.Digest(body))
		}
	})

	t.Run("the second cluster's kit was verified before it could back up", func(t *testing.T) {
		setB, _ := readSet(t, bucketClient(t, kit.targetFlag, kit.prefixB, kit.accessKeyID, kit.accessSecret), kit.prefixB, sessionTwo.Jnl.ClusterID)
		if setB.ArtifactID == "" {
			t.Fatal("no recovery set was written for the second cluster")
		}
		kitAt, ok := sessionTwo.Jnl.Completed(install.StageRecoveryKit)
		if !ok {
			t.Fatalf("the second install's journal has no completed %s entry", install.StageRecoveryKit)
		}
		targetAt, ok := sessionTwo.Jnl.Completed(install.StageBackupTarget)
		if !ok {
			t.Fatalf("the second install's journal has no completed %s entry", install.StageBackupTarget)
		}
		if targetAt.Before(kitAt) {
			t.Errorf("the second cluster's target was configured at %s, before its kit finished at %s", targetAt.Format(time.RFC3339), kitAt.Format(time.RFC3339))
		}
	})

	// -------------------------------------------------------- the bucket matrix
	t.Run("the bucket exposes exactly what the docs say", func(t *testing.T) {
		keys, _, err := clientA.List(ctx, kit.prefixA+"/")
		if err != nil {
			t.Fatalf("listing %s/: %v", kit.prefixA, err)
		}
		if len(keys) == 0 {
			t.Fatalf("the bucket holds nothing under %s/, so this check would be vacuous", kit.prefixA)
		}
		// Every object under the cluster's prefix is searched, whatever Velero
		// and k3s named it, so this cannot pass because a path moved.
		hits := map[string]bool{}
		for _, key := range keys {
			body, err := clientA.Get(ctx, key)
			if err != nil {
				t.Fatalf("reading %s: %v", key, err)
			}
			if blobContains(body, kit.plantedSecret) {
				hits[key] = true
			}
		}
		var archives, datastores, volumes []string
		for key := range hits {
			switch {
			case strings.HasPrefix(key, filepath.Join(kit.prefixA, "workload")):
				if strings.Contains(key, "resources") {
					archives = append(archives, key)
				} else {
					volumes = append(volumes, key)
				}
			case strings.HasPrefix(key, filepath.Join(kit.prefixA, "datastore")):
				datastores = append(datastores, key)
			default:
				t.Errorf("the planted Secret is readable from %s, which is neither a datastore snapshot, a Velero resource archive nor volume data", key)
			}
		}
		if len(archives) == 0 {
			t.Errorf("the planted Secret is NOT readable from Velero's resource archive, and the docs say it is — in plaintext, protected by the credential's prefix scope alone. Hits: %v", hits)
		}
		if len(datastores) != 0 {
			t.Errorf("the planted Secret is readable from the datastore snapshot(s) %v: --secrets-encryption is not protecting the snapshot", datastores)
		}
		if len(volumes) != 0 {
			t.Errorf("the planted Secret is readable from the volume backup(s) %v: the per-cluster repository password is not protecting the volume data", volumes)
		}
	})

	t.Run("only the fleet key opens a kit", func(t *testing.T) {
		keys, _, err := clientA.List(ctx, kit.prefixA+"/recovery-kits/")
		if err != nil {
			t.Fatalf("listing the recovery kits: %v", err)
		}
		if len(keys) == 0 {
			t.Fatal("no recovery kit was uploaded")
		}
		fleet, err := recoverykit.ParseFleetKey(extractFleetKey(t, output))
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range keys {
			body, err := clientA.Get(ctx, key)
			if err != nil {
				t.Fatalf("reading %s: %v", key, err)
			}
			kit, err := recoverykit.Load(body)
			if err != nil {
				t.Fatalf("%s is not a readable kit: %v", key, err)
			}
			if _, err := kit.Secrets(fleet.SecretKeyString()); err != nil {
				t.Errorf("a kit written by this install does not open with the fleet key it printed: %v", err)
			}
			// The bucket's own object name is not a key: whoever can read the
			// bucket must still not be able to open a kit.
			if _, err := kit.Secrets(key); err == nil {
				t.Errorf("%s opened with its own object key, which is not a key", key)
			}
		}
	})

	// ------------------------------------------------- set-target's scope arm
	t.Run("the scope check refuses a credential that reaches the control plane", func(t *testing.T) {
		if kit.wideKeyID == "" {
			t.Skip("KUBENEST_BACKUP_WIDE_KEY_ID / KUBENEST_BACKUP_WIDE_SECRET not set: there is no over-broad credential to prove the refusal with")
		}
		wide, err := backup.ParseTarget(kitTargetFlag(kit.targetFlag, kit.prefixA), kit.wideKeyID, kit.wideSecret)
		if err != nil {
			t.Fatal(err)
		}
		err = wide.VerifyScope(ctx, wide.Client)
		if err == nil {
			t.Fatal("a credential that can read the control-plane prefix was accepted: read access to the control plane's recovery material is a fleet-wide compromise")
		}
		if !strings.Contains(err.Error(), "ListObjectsV2") && !strings.Contains(err.Error(), "GetObject") {
			t.Errorf("the refusal must name the operation that succeeded: %v", err)
		}
		if !strings.Contains(err.Error(), backup.ControlPlanePrefix) {
			t.Errorf("the refusal must name the prefix that was reached: %v", err)
		}

		// The positive control: the cluster's own scoped credential passes, so
		// the refusal above is about the policy and not about the store.
		narrow, err := backup.ParseTarget(kitTargetFlag(kit.targetFlag, kit.prefixA), kit.accessKeyID, kit.accessSecret)
		if err != nil {
			t.Fatal(err)
		}
		if err := narrow.VerifyScope(ctx, narrow.Client); err != nil {
			t.Errorf("the cluster's own scoped credential must pass: %v", err)
		}
	})
}

// kitTargetFlag puts one cluster's prefix into the shared target.
func kitTargetFlag(target, prefix string) string {
	if i := strings.IndexAny(target, "?"); i >= 0 {
		return strings.TrimSuffix(target[:i], "/") + "/" + prefix + target[i:]
	}
	return strings.TrimSuffix(target, "/") + "/" + prefix
}

// kitSession builds one install run against the lab host, with the install's
// output captured. The fleet key is printed to the terminal and written
// nowhere, so that output IS the artefact this gate reads.
func kitSession(t *testing.T, api_ *api.Client, bundle *manifest.Manifest, journalPath string, opts install.Options, out *bytes.Buffer) *install.Session {
	t.Helper()
	journal, err := install.OpenJournal(journalPath, opts.Identity())
	if err != nil {
		t.Fatal(err)
	}
	s := &install.Session{
		ID:       install.NewRunID(),
		Opts:     opts,
		Bundle:   bundle,
		Jnl:      journal,
		Reporter: converge.NewTextReporter(out),
		Out:      out,
		API:      api_,
	}
	s.Emit = install.Emitters{
		install.TextEmitter{W: out},
		install.NewControlPlaneEmitter(api_, func() string { return journal.ClusterID }),
	}
	return s
}

// extractFleetKey reads the one printed key back out of the install output.
func extractFleetKey(t *testing.T, output string) string {
	t.Helper()
	for _, field := range strings.Fields(output) {
		if strings.HasPrefix(field, "AGE-SECRET-KEY-1") {
			return field
		}
	}
	t.Fatalf("the install printed no fleet recovery key:\n%s", output)
	return ""
}

// seedSecondLaptop copies the operator's CLI state to the second machine: the
// config (control-plane URL, CA, instance id and the PUBLIC recipient), the
// journals, and the kits already written there. The fleet key is in none of
// them, and is not copied because it exists on no disk to copy.
func seedSecondLaptop(t *testing.T, from, to string) {
	t.Helper()
	src := filepath.Join(from, ".kubenest")
	dst := filepath.Join(to, ".kubenest")
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o600)
		if info, err := d.Info(); err == nil {
			mode = info.Mode().Perm()
		}
		return os.WriteFile(target, body, mode)
	})
	if err != nil {
		t.Fatalf("seeding the second laptop from %s: %v", src, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "config.json")); err != nil {
		t.Fatalf("the second laptop has no config to install with: %v", err)
	}
}

// plantWorkload creates a namespace with a Secret whose value is ALSO mounted
// into a pod's volume, so the same string is in the objects, in the volume data
// and in the datastore — the three places the exposure matrix is about.
func plantWorkload(t *testing.T, host, user, key, ns, secret string) {
	t.Helper()
	ctx := context.Background()
	ep, err := sshx.Resolve(host, sshx.Options{User: user, KeyPath: key})
	if err != nil {
		t.Fatal(err)
	}
	client, err := sshx.Dial(ctx, ep, sshx.Options{KeyPath: key})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	doc := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
---
apiVersion: v1
kind: Secret
metadata:
  name: planted
  namespace: %[1]s
type: Opaque
stringData:
  token: %[2]s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: holds-the-secret
  namespace: %[1]s
spec:
  replicas: 1
  selector:
    matchLabels: {app: holds-the-secret}
  template:
    metadata:
      labels: {app: holds-the-secret}
    spec:
      containers:
        - name: holder
          image: registry.k8s.io/pause:3.9
          volumeMounts:
            - name: planted
              mountPath: /etc/planted
      volumes:
        - name: planted
          secret:
            secretName: planted
`, ns, secret)

	res, err := client.RunInput(ctx, "sudo -n k3s kubectl apply -f -", strings.NewReader(doc))
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("planting the workload on %s: %v / exit %d: %s", host, err, res.ExitCode, res.Stderr)
	}
	// Converge on the pod actually running, so the volume is on disk before a
	// backup is taken: a backup of a pod that never started proves nothing.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out, err := k3s.Kubectl(ctx, client, "get pods -n "+ns+" -o jsonpath='{.items[*].status.phase}'")
		if err == nil && strings.Contains(out, "Running") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the planted pod on %s never reached Running (%v, %s)", host, err, out)
		}
		time.Sleep(5 * time.Second)
	}
}

// veleroBackup is the slice of a Velero Backup this gate reads.
type veleroBackup struct {
	Name        string
	CompletedAt time.Time
}

func veleroBackups(t *testing.T, runner k3s.Runner) []veleroBackup {
	t.Helper()
	out, err := k3s.Kubectl(context.Background(), runner, "get backups.velero.io -n "+backup.Namespace+" -o json")
	if err != nil {
		t.Fatalf("listing Velero backups: %v", err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string    `json:"name"`
				CreationTimestamp time.Time `json:"creationTimestamp"`
			} `json:"metadata"`
			Status struct {
				Phase               string    `json:"phase"`
				CompletionTimestamp time.Time `json:"completionTimestamp"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("parsing Velero backups: %v", err)
	}
	backups := make([]veleroBackup, 0, len(list.Items))
	for _, item := range list.Items {
		at := item.Status.CompletionTimestamp
		if at.IsZero() {
			at = item.Metadata.CreationTimestamp
		}
		backups = append(backups, veleroBackup{Name: item.Metadata.Name, CompletedAt: at})
	}
	return backups
}

// readSet reads the cluster's recovery set out of the bucket.
func readSet(t *testing.T, client *s3.Client, prefix, clusterID string) (*recoverykit.Set, []byte) {
	t.Helper()
	ctx := context.Background()
	keys, _, err := client.List(ctx, prefix+"/recovery-sets/"+clusterID+"/")
	if err != nil {
		t.Fatalf("listing the recovery sets: %v", err)
	}
	if len(keys) == 0 {
		return &recoverykit.Set{}, nil
	}
	body, err := client.Get(ctx, keys[0])
	if err != nil {
		t.Fatalf("reading %s: %v", keys[0], err)
	}
	set, err := recoverykit.LoadSet(body)
	if err != nil {
		t.Fatalf("%s is not a readable recovery set: %v", keys[0], err)
	}
	return set, body
}

func bucketClient(t *testing.T, target, prefix, keyID, secret string) *s3.Client {
	t.Helper()
	parsed, err := backup.ParseTarget(kitTargetFlag(target, prefix), keyID, secret)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Client.(*s3.Client)
}

// blobContains reports whether a stored object carries the needle, inflated as
// far as its container layers allow. A byte search alone would miss a value
// inside a gzip stream and report "absent" for the wrong reason — which is
// exactly the mistake that would make this gate pass while the value is
// readable.
func blobContains(blob []byte, needle string) bool {
	if bytes.Contains(blob, []byte(needle)) {
		return true
	}
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return false
	}
	inflated, err := io.ReadAll(zr)
	if err != nil {
		return false
	}
	if bytes.Contains(inflated, []byte(needle)) {
		return true
	}
	tr := tar.NewReader(bytes.NewReader(inflated))
	for {
		hdr, err := tr.Next()
		if err != nil {
			return false
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return false
		}
		if bytes.Contains(body, []byte(needle)) {
			return true
		}
	}
}

// randomSuffix is a short unpredictable string, so the planted Secret's value
// cannot be found in the bucket by anyone who has only read this file.
func randomSuffix() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}
