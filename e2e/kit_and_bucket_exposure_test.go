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
//  3. a workload with a PLANTED Secret on the first cluster, mounted into a pod
//     volume so the value is in the volume data as well as in the objects;
//  4. the first backup through `kubenest backup now`, then the exposure
//     matrix read out of the bucket itself.
//
// What it asserts, and why each is the property that matters:
//
//   - the fleet key was printed once, in the control-plane install's output;
//   - the control-plane kit was DECRYPTED from both its local and its uploaded
//     copy, and the kit stage finished before checkpoint Jobs or backups;
//   - the second cluster's kit was uploaded and DIGEST-verified before its
//     first backup could exist, and that install never held or printed a
//     private key;
//   - the planted Secret is not readable from the volume backup or the
//     datastore snapshot and IS readable from Velero's resource archive —
//     the exposure the docs state, measured rather than assumed;
//   - each cluster has its own scoped S3 principal; `backup set-target`'s
//     scope check refuses principals reaching the control-plane prefix or
//     both cluster prefixes.
//
// Run from the umbrella workspace:
//
//	source lab/hetzner/.lab-env.sh
//	export KUBENEST_CONTROL_PLANE=http://localhost:8000
//	export KUBENEST_CLI_TOKEN=knp_...
//	export KUBENEST_LAB_NODE2_IP=<second host>
//	export KUBENEST_BACKUP_TARGET='s3://bucket?endpoint=minio:9000&region=main'
//	export KUBENEST_BACKUP_ACCESS_KEY_ID=... KUBENEST_BACKUP_SECRET_ACCESS_KEY=...
//	export KUBENEST_BACKUP_CLUSTER_B_ACCESS_KEY_ID=... KUBENEST_BACKUP_CLUSTER_B_SECRET_ACCESS_KEY=...
//	export KUBENEST_BACKUP_CLUSTER_A_PREFIX=cluster-a
//	export KUBENEST_BACKUP_CLUSTER_B_PREFIX=cluster-b
//	export KUBENEST_BACKUP_WIDE_KEY_ID=... KUBENEST_BACKUP_WIDE_SECRET=...
//	export KUBENEST_BACKUP_CROSS_KEY_ID=... KUBENEST_BACKUP_CROSS_SECRET=...
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
	"errors"
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
	"kubenest.io/cli/pkg/controlplane"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/s3"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/storage"
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
	accessKeyIDB  string
	accessSecretB string
	wideKeyID     string
	wideSecret    string
	crossKeyID    string
	crossSecret   string
	plantedSecret string
	plantedNS     string
	secondStorage string
}

func TestS1ReadsDistinctCredentialsForEachCluster(t *testing.T) {
	t.Setenv("KUBENEST_LAB_NODE2_IP", "192.0.2.2")
	t.Setenv("KUBENEST_BACKUP_TARGET", "s3://fixture")
	t.Setenv("KUBENEST_BACKUP_ACCESS_KEY_ID", "cluster-a-key")
	t.Setenv("KUBENEST_BACKUP_SECRET_ACCESS_KEY", "cluster-a-secret")
	t.Setenv("KUBENEST_BACKUP_CLUSTER_B_ACCESS_KEY_ID", "cluster-b-key")
	t.Setenv("KUBENEST_BACKUP_CLUSTER_B_SECRET_ACCESS_KEY", "cluster-b-secret")
	t.Setenv("KUBENEST_BACKUP_WIDE_KEY_ID", "wide-key")
	t.Setenv("KUBENEST_BACKUP_WIDE_SECRET", "wide-secret")
	t.Setenv("KUBENEST_BACKUP_CROSS_KEY_ID", "cross-key")
	t.Setenv("KUBENEST_BACKUP_CROSS_SECRET", "cross-secret")
	kit := kitEnvironment(t)
	if kit.accessKeyIDB != "cluster-b-key" || kit.accessSecretB != "cluster-b-secret" {
		t.Fatal("the second install must receive its own configured S3 principal")
	}
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
		accessKeyIDB:  os.Getenv("KUBENEST_BACKUP_CLUSTER_B_ACCESS_KEY_ID"),
		accessSecretB: os.Getenv("KUBENEST_BACKUP_CLUSTER_B_SECRET_ACCESS_KEY"),
		wideKeyID:     os.Getenv("KUBENEST_BACKUP_WIDE_KEY_ID"),
		wideSecret:    os.Getenv("KUBENEST_BACKUP_WIDE_SECRET"),
		crossKeyID:    os.Getenv("KUBENEST_BACKUP_CROSS_KEY_ID"),
		crossSecret:   os.Getenv("KUBENEST_BACKUP_CROSS_SECRET"),
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
	if env.accessKeyIDB == "" || env.accessSecretB == "" {
		t.Fatal("KUBENEST_BACKUP_CLUSTER_B_ACCESS_KEY_ID and KUBENEST_BACKUP_CLUSTER_B_SECRET_ACCESS_KEY are required for S1's second scoped principal")
	}
	if env.accessKeyID == env.accessKeyIDB {
		t.Fatal("S1 needs distinct S3 principals for the two clusters")
	}
	if env.prefixA == env.prefixB || env.prefixA == "" || env.prefixB == "" {
		t.Fatal("S1 needs two distinct nonempty cluster prefixes")
	}
	if env.wideKeyID == "" || env.wideSecret == "" {
		t.Fatal("KUBENEST_BACKUP_WIDE_KEY_ID and KUBENEST_BACKUP_WIDE_SECRET are required for S1's planted negative")
	}
	if env.crossKeyID == "" || env.crossSecret == "" {
		t.Fatal("KUBENEST_BACKUP_CROSS_KEY_ID and KUBENEST_BACKUP_CROSS_SECRET are required for the other-cluster planted negative")
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
		SSHUser: env.sshUser, SSHKey: env.sshKey, StorageDevices: storage.Devices{All: env.storageDevice},
		ControlPlaneInstall: true, Domain: env.server + ".sslip.io", AdminEmail: "admin@" + env.server + ".sslip.io",
		BackupTarget: kitTargetFlag(kit.targetFlag, kit.prefixA),
	}, &firstOut)
	defer sessionOne.Close()
	if _, err := install.Execute(ctx, sessionOne, install.Plan(sessionOne)); err != nil {
		t.Fatalf("the control-plane install failed: %v", err)
	}
	assertKitStages(t, sessionOne)
	installElapsed := time.Since(started)
	t.Logf("control-plane install finished in %s (S1's budget is %s)", installElapsed.Round(time.Second), Budget)
	if installElapsed > Budget {
		t.Errorf("the install took %s, over the %s budget — that is a defect in the installer, not a number to revise upward",
			installElapsed.Round(time.Second), Budget)
	}
	output := firstOut.String()
	cfgOne, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	runnerOne, err := sessionOne.Server()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("the control-plane kit predates every checkpoint Job", func(t *testing.T) {
		kitAt, ok := sessionOne.Jnl.Completed(install.StageRecoveryKit)
		if !ok {
			t.Fatal("the control-plane kit stage has no completion time")
		}
		out, err := k3s.Kubectl(ctx, runnerOne, "get jobs -n "+controlplane.Namespace+" -o json")
		if err != nil {
			t.Fatal(err)
		}
		var jobs struct {
			Items []struct {
				Metadata struct {
					Name    string    `json:"name"`
					Created time.Time `json:"creationTimestamp"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &jobs); err != nil {
			t.Fatal(err)
		}
		for _, job := range jobs.Items {
			if strings.HasPrefix(job.Metadata.Name, controlplane.CheckpointCronJobName+"-") {
				if job.Metadata.Created.IsZero() || !job.Metadata.Created.After(kitAt) {
					t.Errorf("checkpoint Job %s cannot be shown to start after the kit stage", job.Metadata.Name)
				}
			}
		}
		checkpoint, err := controlplane.ReadEligibleCheckpoint(ctx, runnerOne)
		if err != nil {
			t.Fatal(err)
		}
		if checkpoint != nil {
			at, err := time.Parse(time.RFC3339, checkpoint.At)
			if err != nil || !at.After(kitAt) {
				t.Errorf("the eligible checkpoint cannot be shown to postdate the kit stage (timestamp error %v)", err)
			}
		}
	})

	t.Run("the fleet recovery key is printed exactly once", func(t *testing.T) {
		if got := strings.Count(output, "AGE-SECRET-KEY-1"); got != 1 {
			t.Errorf("the fleet recovery key appears %d times in the install output, want exactly 1", got)
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
			t.Error("the first control-plane install must report opening both kit copies")
		}
		if !strings.Contains(output, "control-plane recovery kit") {
			t.Error("the first install did not report its control-plane recovery kit")
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
	// The second laptop talks to the control plane phase 1 INSTALLED, with the
	// URL, token and CA the first install stored — never to the harness's own
	// control plane. The first real run registered this cluster with the
	// harness's control plane while pinning the new one's CA, and the agent
	// could never connect ("x509: certificate signed by unknown authority").
	fleet := configuredControlPlane(t, cfgTwo)
	var secondOut bytes.Buffer
	sessionTwo := kitSession(t, fleet, bundle, journalTwo, install.Options{
		Bundle: env.bundle, Name: clusterTwo, Servers: []string{kit.secondServer}, HATier: "single-server",
		SSHUser: env.sshUser, SSHKey: env.sshKey, StorageDevices: storage.Devices{All: kit.secondStorage},
		ControlPlaneCA: []byte(cfgTwo.ControlPlaneCA),
		BackupTarget:   kitTargetFlag(kit.targetFlag, kit.prefixB),
	}, &secondOut)
	defer sessionTwo.Close()
	t.Setenv("KUBENEST_BACKUP_ACCESS_KEY_ID", kit.accessKeyIDB)
	t.Setenv("KUBENEST_BACKUP_SECRET_ACCESS_KEY", kit.accessSecretB)
	if _, err := install.Execute(ctx, sessionTwo, install.Plan(sessionTwo)); err != nil {
		t.Fatalf("the second cluster's install failed: %v", err)
	}
	assertKitStages(t, sessionTwo)
	second := secondOut.String()
	runnerTwo, err := sessionTwo.Server()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("both clusters connect and reconcile a project", func(t *testing.T) {
		for _, cluster := range []struct {
			id     string
			runner k3s.Runner
		}{
			{sessionOne.Jnl.ClusterID, runnerOne},
			{sessionTwo.Jnl.ClusterID, runnerTwo},
		} {
			waitKitConnected(t, fleet, cluster.id)
		}
		password := kitAdminPassword(t, output)
		anonymous, err := api.New(cfgOne.ControlPlaneURL, api.WithCACert([]byte(cfgOne.ControlPlaneCA)))
		if err != nil {
			t.Fatal(err)
		}
		token, err := anonymous.PasswordLogin(ctx, "admin@"+env.server+".sslip.io", password)
		if err != nil {
			t.Fatalf("logging in to the new control plane to create S1 projects: %v", err)
		}
		admin, err := api.New(cfgOne.ControlPlaneURL, api.WithToken(token), api.WithCACert([]byte(cfgOne.ControlPlaneCA)))
		if err != nil {
			t.Fatal(err)
		}
		for _, cluster := range []struct {
			id     string
			runner k3s.Runner
		}{
			{sessionOne.Jnl.ClusterID, runnerOne},
			{sessionTwo.Jnl.ClusterID, runnerTwo},
		} {
			project, err := admin.CreateProject(ctx, cluster.id, "s1-project-"+randomSuffix())
			if err != nil {
				t.Fatalf("creating a project through the control plane for %s: %v", cluster.id, err)
			}
			if project == nil || project.Namespace == "" {
				t.Fatalf("the control plane returned no namespace for %s's project", cluster.id)
			}
			waitKitNamespace(t, cluster.runner, project.Namespace)
		}
	})
	t.Run("an occupied k3s host is refused at preflight", func(t *testing.T) {
		probe := kitSession(t, fleet, bundle, filepath.Join(t.TempDir(), "occupied-journal.json"), install.Options{
			Bundle: env.bundle, Name: clusterTwo + "-occupied", Servers: []string{kit.secondServer}, HATier: "single-server",
			SSHUser: env.sshUser, SSHKey: env.sshKey, StorageDevices: storage.Devices{All: kit.secondStorage},
		}, new(bytes.Buffer))
		defer probe.Close()
		plan := install.Plan(probe)
		if len(plan) == 0 || plan[0].Name != install.StagePreflight {
			t.Fatal("the fresh install does not start at preflight")
		}
		err := plan[0].Run(ctx)
		if err == nil || !strings.Contains(err.Error(), "Existing Kubernetes") {
			t.Errorf("preflight must refuse the occupied host as Existing Kubernetes; got %v", err)
		}
	})

	t.Run("a later install verifies by digest and never holds the key", func(t *testing.T) {
		if strings.Contains(second, "AGE-SECRET-KEY-1") {
			t.Error("the second laptop saw a private fleet key")
		}
		if !strings.Contains(second, "digest-verified against the local copy") {
			t.Error("the second install did not report digest verification")
		}
		if strings.Contains(second, "decrypted from both") {
			t.Error("the second install claimed to decrypt a recovery kit")
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
		clientB := bucketClient(t, kit.targetFlag, kit.prefixB, kit.accessKeyIDB, kit.accessSecretB)
		uploaded, err := clientB.Get(ctx, recoverykit.KitKey(kit.prefixB, sessionTwo.Jnl.ClusterID, recoverykit.KindCluster, localKit.ArtifactID))
		if err != nil {
			t.Fatalf("the second cluster's kit is not in the bucket: %v", err)
		}
		if recoverykit.Digest(uploaded) != recoverykit.Digest(localDoc) {
			t.Errorf("the uploaded kit is not the local one: uploaded %s, local %s", recoverykit.Digest(uploaded), recoverykit.Digest(localDoc))
		}
		sealed, err := recoverykit.Load(uploaded)
		if err != nil {
			t.Fatalf("the uploaded second-cluster kit is not a recovery kit: %v", err)
		}
		if _, err := sealed.Secrets(""); err == nil {
			t.Error("the uploaded second-cluster kit opened without the fleet key")
		}
		fleetKey, err := recoverykit.ParseFleetKey(extractFleetKey(t, output))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sealed.Secrets(fleetKey.SecretKeyString()); err != nil {
			t.Errorf("the uploaded second-cluster kit did not open with the first laptop's fleet key: %v", err)
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
	t.Setenv("KUBENEST_BACKUP_ACCESS_KEY_ID", kit.accessKeyID)
	t.Setenv("KUBENEST_BACKUP_SECRET_ACCESS_KEY", kit.accessSecret)
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
		t.Fatalf("`kubenest backup now` failed: %v", err)
	}

	clientA := bucketClient(t, kit.targetFlag, kit.prefixA, kit.accessKeyID, kit.accessSecret)
	set, _ := readSet(t, clientA, kit.prefixA, sessionOne.Jnl.ClusterID)
	if set.ArtifactID == "" {
		t.Fatal("`backup now` wrote no recovery set, so the backup cannot be selected by a recovery")
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
			t.Error("the recovery set is not marked complete, so no recovery may select it")
		}
		if len(set.Backups) == 0 {
			t.Error("the recovery set names no backup")
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
		setB, _ := readSet(t, bucketClient(t, kit.targetFlag, kit.prefixB, kit.accessKeyIDB, kit.accessSecretB), kit.prefixB, sessionTwo.Jnl.ClusterID)
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
		for _, backup := range veleroBackups(t, runnerTwo) {
			if !backup.CompletedAt.IsZero() && !backup.CompletedAt.After(kitAt) {
				t.Errorf("second-cluster backup %s existed at or before its kit stage completed", backup.Name)
			}
		}
		for _, backup := range setB.Backups {
			if backup.Completed() && !backup.CompletedAt.After(kitAt) {
				t.Errorf("second-cluster recovery set includes backup %s from before its kit completed", backup.Name)
			}
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
		// Velero's layout at the pinned version: the resource archive is
		// backups/<name>/<name>.tar.gz (the first hardware run found the Secret
		// there and this test, matching on "resources", filed it as volume data);
		// file-system volume data is the kopia (or restic) repository.
		var archives, datastores, volumes []string
		for key := range hits {
			workload := filepath.Join(kit.prefixA, "workload") + "/"
			switch {
			case strings.HasPrefix(key, workload) && isVeleroResourceArchive(strings.TrimPrefix(key, workload)):
				archives = append(archives, key)
			case strings.HasPrefix(key, workload+"kopia/") || strings.HasPrefix(key, workload+"restic/"):
				volumes = append(volumes, key)
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
		// Not vacuous: the planted claim's data really is in the repository,
		// encrypted, which is what the absence of a plaintext hit is about.
		repository := filepath.Join(kit.prefixA, "workload", "kopia", kit.plantedNS) + "/"
		stored := 0
		for _, key := range keys {
			if strings.HasPrefix(key, repository) {
				stored++
			}
		}
		if stored == 0 {
			t.Errorf("no volume data for %s under %s: the planted claim was never backed up, so 'the volume data does not expose it' proves nothing", kit.plantedNS, repository)
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
	t.Run("set-target scope refuses access to the control plane or the other cluster", func(t *testing.T) {
		wide, err := backup.ParseTarget(kitTargetFlag(kit.targetFlag, kit.prefixA), kit.wideKeyID, kit.wideSecret)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := wide.Client.List(ctx, backup.ControlPlanePrefix); err != nil {
			t.Fatalf("the planted wide credential does not actually reach the control-plane prefix: %v", err)
		}
		_, err = wide.VerifyScope(ctx, wide.Client)
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
		if _, err := narrow.VerifyScope(ctx, narrow.Client); err != nil {
			t.Errorf("the cluster's own scoped credential must pass: %v", err)
		}
		other, err := backup.ParseTarget(kitTargetFlag(kit.targetFlag, kit.prefixB), kit.accessKeyIDB, kit.accessSecretB)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := other.VerifyScope(ctx, other.Client); err != nil {
			t.Errorf("the second cluster's own scoped credential must pass: %v", err)
		}
		cross, err := backup.ParseTarget(kitTargetFlag(kit.targetFlag, kit.prefixA), kit.crossKeyID, kit.crossSecret)
		if err != nil {
			t.Fatal(err)
		}
		for _, prefix := range []string{kit.prefixA, kit.prefixB} {
			if _, _, err := cross.Client.List(ctx, prefix+"/"); err != nil {
				t.Fatalf("the planted cross-cluster credential cannot actually list %s: %v", prefix, err)
			}
		}
		if _, _, err := cross.Client.List(ctx, backup.ControlPlanePrefix); !errors.Is(err, s3.ErrAccessDenied) {
			t.Fatalf("the cross-cluster credential also reaches the control-plane prefix, obscuring which boundary refused it: %v", err)
		}
		if _, err := cross.VerifyScope(ctx, cross.Client); err == nil {
			t.Error("set-target accepted a credential demonstrated to reach both cluster prefixes")
		} else if !strings.Contains(err.Error(), "ListObjectsV2") && !strings.Contains(err.Error(), "GetObject") {
			t.Errorf("the cross-cluster refusal did not identify a successful foreign-prefix probe: %v", err)
		}
		for _, crossing := range []struct {
			name    string
			client  *s3.Client
			owner   *s3.Client
			foreign string
		}{
			{"A reads B", narrow.Client.(*s3.Client), other.Client.(*s3.Client), kit.prefixB},
			{"B reads A", other.Client.(*s3.Client), narrow.Client.(*s3.Client), kit.prefixA},
		} {
			t.Run(crossing.name, func(t *testing.T) {
				keys, _, err := crossing.owner.List(ctx, crossing.foreign+"/recovery-kits/")
				if err != nil || len(keys) == 0 {
					t.Fatalf("the foreign cluster has no readable kit to use as the planted object (error %v)", err)
				}
				if _, _, err := crossing.client.List(ctx, crossing.foreign+"/"); !errors.Is(err, s3.ErrAccessDenied) {
					t.Errorf("listing another cluster's prefix must be AccessDenied; got %v", err)
				}
				if _, err := crossing.client.Get(ctx, keys[0]); !errors.Is(err, s3.ErrAccessDenied) {
					t.Errorf("reading another cluster's prefix must be AccessDenied; got %v", err)
				}
			})
		}
	})
}

func assertKitStages(t *testing.T, session *install.Session) {
	t.Helper()
	for _, stage := range install.Plan(session) {
		if _, ok := session.Jnl.Completed(stage.Name); !ok {
			t.Errorf("install stage %s has no completed journal entry", stage.Name)
		}
	}
}

func kitAdminPassword(t *testing.T, output string) string {
	t.Helper()
	const marker = "admin password: "
	var password string
	for _, line := range strings.Split(output, "\n") {
		if i := strings.Index(line, marker); i >= 0 {
			if password != "" {
				t.Fatal("the first install printed the administrator password more than once")
			}
			password = strings.TrimSpace(line[i+len(marker):])
		}
	}
	if password == "" {
		t.Fatal("the first install did not print the administrator password")
	}
	return password
}

func waitKitConnected(t *testing.T, control *api.Client, clusterID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		cluster, err := control.GetCluster(context.Background(), clusterID)
		if err == nil && cluster.Status == "connected" && cluster.LastHeartbeat != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cluster %s did not become connected with a heartbeat (status %q, error %v)", clusterID, cluster.Status, err)
		}
		time.Sleep(5 * time.Second)
	}
}

func waitKitNamespace(t *testing.T, runner k3s.Runner, namespace string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	for {
		out, err := k3s.Kubectl(context.Background(), runner, "get namespace "+namespace+" -o name")
		if err == nil && strings.TrimSpace(out) == "namespace/"+namespace {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("project namespace %s never reconciled (error %v)", namespace, err)
		}
		time.Sleep(5 * time.Second)
	}
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
	t.Fatal("the install printed no fleet recovery key")
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

// configuredControlPlane is the client a laptop builds from its stored CLI
// state (pkg/cmd's controlPlaneClient): the logged-in URL, its token, and the
// control plane's own CA.
func configuredControlPlane(t *testing.T, cfg *config.Config) *api.Client {
	t.Helper()
	if cfg.ControlPlaneURL == "" {
		t.Fatal("the seeded laptop is logged in to no control plane: the first install must have logged it in")
	}
	creds, err := config.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	token := creds.TokenFor(cfg.ControlPlaneURL)
	if token == "" {
		t.Fatalf("the seeded laptop holds no token for %s", cfg.ControlPlaneURL)
	}
	opts := []api.Option{api.WithToken(token)}
	if cfg.ControlPlaneCA != "" {
		opts = append(opts, api.WithCACert([]byte(cfg.ControlPlaneCA)))
	}
	client, err := api.New(cfg.ControlPlaneURL, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return client
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
# The value must reach VOLUME DATA, not only a Secret volume: Velero's
# file-system backup skips secret volumes, so a Secret mounted as a volume
# never reaches the kopia repository and "the repository password protects
# the volume data" would be vacuous (the first hardware run found no kopia
# data for this namespace at all). An init container copies it onto a claim.
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: holds-the-secret
  namespace: %[1]s
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests: {storage: 1Gi}
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
      initContainers:
        - name: copy
          image: busybox:1.36
          command: [sh, -c, "cp /etc/planted/token /data/token && sync"]
          volumeMounts:
            - {name: planted, mountPath: /etc/planted}
            - {name: data, mountPath: /data}
      containers:
        - name: holder
          image: registry.k8s.io/pause:3.9
          volumeMounts:
            - {name: planted, mountPath: /etc/planted}
            - {name: data, mountPath: /data}
      volumes:
        - name: planted
          secret:
            secretName: planted
        - name: data
          persistentVolumeClaim:
            claimName: holds-the-secret
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

// isVeleroResourceArchive reports whether a key under the workload prefix is a
// backup's resource archive, backups/<name>/<name>.tar.gz.
func isVeleroResourceArchive(key string) bool {
	parts := strings.Split(key, "/")
	return len(parts) == 3 && parts[0] == "backups" && parts[2] == parts[1]+".tar.gz"
}
