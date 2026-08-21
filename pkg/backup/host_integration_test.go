//go:build host

// Real-host integration test for kn-mzn's install half — the wave-1 gate for
// the backup component: run standalone against an ephemeral-env host-profile
// node and reach the verify condition, then prove the page's claims:
//
//   - Velero installs UNCONFIGURED and that state is visible, not blocking
//     (install.mdx / backup-restore.mdx: `backup: unconfigured`).
//   - A customer-supplied S3-compatible target (real MinIO on a second host —
//     the S3 API over the wire, not a mock) is configured and PROVEN:
//     Velero validates the location Available with the supplied credentials.
//   - The default schedule from the manifest lands: daily 02:00, ttl 336h.
//   - `backup now` completes and the backup — including PVC volume data via
//     file-system backup — actually lands in the bucket, asserted from the
//     bucket's side with mc, not from Velero's word alone.
//   - The weekly drill restores that backup into scratch, compares objects and
//     real PVC bytes, records a pass, and removes scratch.
//   - Corrupting the selected backup records an actionable FAILED drill rather
//     than passing or waiting out the two-hour deadline.
//   - Hourly/24-kept embedded-etcd snapshots reach the same external target,
//     and the documented S3 disaster restore recovers a mutated sentinel.
//
// Run from the umbrella workspace:
//
//	# Provision two isolated hosts: node 1 is the cluster, node 2 is the
//	# external S3 service that remains reachable while node 1's k3s is stopped.
//	cd kubenest-cli && go test -tags host -timeout 45m -run TestBackupOnRealHost -v ./pkg/backup \
//	  -host "$KUBENEST_LAB_NODE1_IP" -ssh-user "$KUBENEST_LAB_SSH_USER" -ssh-key ~/.ssh/id_ed25519 \
//	  -s3-host "$KUBENEST_LAB_NODE2_IP" -s3-private "$KUBENEST_LAB_NODE2_PRIVATE_IP" \
//	  -known-hosts /tmp/kn-f9lm-known-hosts \
//	  -bundle "$PWD/../kubenest-contracts/bundles/platform-1.0.yaml"
//	./scripts/ephemeral-env.sh down --profile host   # ALWAYS — it bills by the hour
//
// The k3s install below is TEST SCAFFOLDING at the version the bundle pins
// (productizing that stage is kn-7k8), and MinIO is test scaffolding for the
// customer's bucket. The PVC uses the cluster's default StorageClass — on a
// node where kn-1nn's storage gate ran first that is openebs-lvm, which is
// the honest core pairing.
package backup_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

var (
	hostAddr   = flag.String("host", "", "target node address")
	s3HostAddr = flag.String("s3-host", "", "second node hosting the external S3 test service")
	s3Private  = flag.String("s3-private", "", "private address of the external S3 test node")
	sshUser    = flag.String("ssh-user", "root", "SSH user on the node")
	sshKey     = flag.String("ssh-key", "", "SSH private key path")
	knownHosts = flag.String("known-hosts", "", "isolated known_hosts path for ephemeral gate nodes")
	bundlePath = flag.String("bundle", "", "bundle manifest path (platform-1.0.yaml)")
)

// Scaffolding pins — the S3-compatible store standing in for the customer's
// bucket. Not bundle components, so not in the manifest.
const (
	minioImage = "minio/minio:RELEASE.2025-09-07T16-13-09Z"
	mcImage    = "minio/mc:RELEASE.2025-08-13T08-35-41Z"

	minioNamespace = "velero-e2e"
	proofNamespace = "kn-mzn-proof"
	bucket         = "kubenest-backups"
	minioUser      = "kubenest-e2e"
	minioPassword  = "kubenest-e2e-secret"
	minioNodePort  = 30900
)

func TestBackupOnRealHost(t *testing.T) {
	if *hostAddr == "" || *s3HostAddr == "" || *s3Private == "" || *bundlePath == "" {
		t.Skip("needs -host, -s3-host, -s3-private and -bundle (see the file comment)")
	}
	ctx := context.Background()

	m, err := manifest.Load(*bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	componentReady, err := m.Limits.Timeouts.For("component-ready")
	if err != nil {
		t.Fatal(err)
	}

	sshOptions := sshx.Options{User: *sshUser, KeyPath: *sshKey, KnownHostsPath: *knownHosts}
	ep, err := sshx.Resolve(*hostAddr, sshOptions)
	if err != nil {
		t.Fatal(err)
	}
	client, err := sshx.Dial(ctx, ep, sshOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	s3Endpoint, err := sshx.Resolve(*s3HostAddr, sshOptions)
	if err != nil {
		t.Fatal(err)
	}
	s3Client, err := sshx.Dial(ctx, s3Endpoint, sshOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer s3Client.Close()

	reporter := converge.NewTextReporter(testWriter{t})

	// --- Test scaffolding: k3s at the bundle's pinned version (kn-7k8's
	// stage, not this component). A no-op when the shared node has it. ---
	k3sVersion, err := m.Core.Version("k3s")
	if err != nil {
		t.Fatal(err)
	}
	nodeReady, err := m.Limits.Timeouts.For("node-ready")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("scaffolding: ensuring embedded-etcd k3s %s on cluster node", k3sVersion)
	ensureK3s(t, ctx, client, k3sVersion)
	waitFor(t, ctx, client, "node-ready", nodeReady, nodeReadyProbe(client))
	t.Logf("scaffolding: ensuring isolated k3s %s for external S3", k3sVersion)
	ensureK3s(t, ctx, s3Client, k3sVersion)
	waitFor(t, ctx, s3Client, "s3-node-ready", nodeReady, nodeReadyProbe(s3Client))

	// --- The component under test: install, UNCONFIGURED. ---
	if err := backup.Install(ctx, client, m, reporter); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Keep retries honest after an interrupted gate: remove only the target
	// managed by this test so the unconfigured assertion below is exercised
	// again. Configure recreates this exact resource later.
	kubectlOrFatal(t, ctx, client, "delete backupstoragelocation "+backup.StorageLocationName+
		" -n "+backup.Namespace+" --ignore-not-found")
	// The page's claim: no target is a visible state, never a failure.
	unconfigured, err := backup.Unconfigured(ctx, client)
	if err != nil {
		t.Fatalf("Unconfigured: %v", err)
	}
	if !unconfigured {
		t.Fatal("fresh install must report backup: unconfigured (a BackupStorageLocation already exists)")
	}

	// --- Scaffolding: MinIO as the customer-supplied S3-compatible bucket,
	// plus a workload whose PVC data must reach it. ---
	apply(t, ctx, s3Client, minioManifests())
	// A prior interrupted gate can leave terminal Job pods behind. The
	// namespace-wide readiness probe is intentionally strict, so remove only
	// this gate's exact Job names before asking whether MinIO itself is ready.
	kubectlOrFatal(t, ctx, s3Client,
		"delete job make-bucket check-datastore-proof check-bucket corrupt-backup -n "+minioNamespace+" --ignore-not-found")
	waitFor(t, ctx, s3Client, "minio-ready", componentReady, podsReadyIn(s3Client, minioNamespace))
	runJob(t, ctx, s3Client, "make-bucket", minioNamespace, componentReady,
		fmt.Sprintf("mc alias set t http://minio.%s.svc:9000 %s %s && mc mb -p t/%s", minioNamespace, minioUser, minioPassword, bucket))

	kubectlOrFatal(t, ctx, client, "delete namespace "+proofNamespace+" --ignore-not-found --wait=true")
	proof := fmt.Sprintf("kn-mzn-proof-%d", time.Now().Unix())
	apply(t, ctx, client, proofManifests(proof))
	// The writer must be RUNNING at backup time — file-system backup only
	// covers volumes of running pods — so it writes, then sleeps.
	waitFor(t, ctx, client, "proof-workload-ready", componentReady, podsReadyIn(client, proofNamespace))

	// --- Configure the target and prove it. ---
	target := backup.Target{
		Endpoint: fmt.Sprintf("http://%s:%d", *s3Private, minioNodePort),
		Bucket:   bucket,
		Region:   "main",

		AccessKeyID:     minioUser,
		SecretAccessKey: minioPassword,
	}
	if err := backup.Configure(ctx, client, m, target, reporter); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	unconfigured, err = backup.Unconfigured(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if unconfigured {
		t.Fatal("configured cluster still reports unconfigured")
	}
	if err := backup.ConfigureDatastoreSnapshots(ctx, client, m, target, reporter); err != nil {
		t.Fatalf("ConfigureDatastoreSnapshots: %v", err)
	}
	datastoreSchedule, err := m.Backup.Defaults.Datastore()
	if err != nil {
		t.Fatal(err)
	}
	wantDatastoreCron, err := backup.Cron(datastoreSchedule.Interval.Duration())
	if err != nil {
		t.Fatal(err)
	}
	datastoreConfig := remoteOut(t, ctx, client, "sudo -n cat /etc/rancher/k3s/config.yaml.d/30-kubenest-backup.yaml")
	if !strings.Contains(datastoreConfig, "etcd-snapshot-schedule-cron: "+wantDatastoreCron) ||
		!strings.Contains(datastoreConfig, fmt.Sprintf("etcd-snapshot-retention: %d", datastoreSchedule.Keep)) {
		t.Fatalf("datastore schedule is not hourly/24-kept:\n%s", datastoreConfig)
	}
	runJob(t, ctx, s3Client, "check-datastore-proof", minioNamespace, componentReady,
		fmt.Sprintf("mc alias set t http://minio.%s.svc:9000 %s %s && test -n \"$(mc find t/%s/datastore --print '{{.Key}}')\"", minioNamespace, minioUser, minioPassword, bucket))

	// Install the exact bundle-pinned operator. Its WebSocket target is
	// deliberately unreachable in this slice; reconnection must not stop the
	// leader-elected restore scheduler from doing cluster-local work.
	agentVersion, err := m.Core.Version("kubenest-agent")
	if err != nil {
		t.Fatal(err)
	}
	installDrillOperator(t, ctx, client, agentVersion)
	waitFor(t, ctx, client, "operator-running", componentReady, podsRunningIn(client, "kubenest-system"))
	waitFor(t, ctx, client, "restore-proof-source-ready", componentReady,
		restoreProofReadyForOperator(client))

	// The schedule on the cluster is the manifest's (decision E), not a
	// constant's: daily at 02:00, retention 14 × 24h.
	cron := kubectlOut(t, ctx, client, "get schedule daily -n velero -o jsonpath='{.spec.schedule}'")
	if cron != "0 2 * * *" {
		t.Errorf("schedule cron = %q, want 0 2 * * *", cron)
	}
	ttl := kubectlOut(t, ctx, client, "get schedule daily -n velero -o jsonpath='{.spec.template.ttl}'")
	if ttl != "336h0m0s" {
		t.Errorf("schedule ttl = %q, want 336h0m0s", ttl)
	}

	// --- backup now: complete, and land in the actual bucket. ---
	name := "manual-" + time.Now().UTC().Format("20060102-150405")
	if err := backup.TakeBackup(ctx, client, m, name, reporter); err != nil {
		t.Fatalf("TakeBackup: %v", err)
	}

	// Volume data went through file-system backup: the proof pod's PVC has a
	// Completed PodVolumeBackup under this backup. The hard assert is scoped
	// to the proof namespace — MinIO's own volume also gets a PVB (the
	// "bucket" living in-cluster is a test artifact; a customer's bucket is
	// external), and its state is logged, not gating.
	pvbPhases := kubectlOut(t, ctx, client,
		"get podvolumebackups -n velero -l velero.io/backup-name="+name+` -o jsonpath='{range .items[*]}{.spec.pod.namespace}/{.spec.volume}={.status.phase}{"\n"}{end}'`)
	t.Logf("pod volume backups under %s:\n%s", name, pvbPhases)
	proofDone := false
	for _, line := range strings.Split(strings.TrimSpace(pvbPhases), "\n") {
		if !strings.HasPrefix(line, proofNamespace+"/") {
			continue
		}
		if strings.HasSuffix(line, "=Completed") {
			proofDone = true
		} else {
			t.Errorf("proof volume backup not Completed: %s", line)
		}
	}
	if !proofDone {
		t.Errorf("no Completed PodVolumeBackup for the proof volume under %s; got:\n%s", name, pvbPhases)
	}

	// Asserted from the BUCKET's side: velero's manifest object for this
	// backup exists in object storage. mc stat exits non-zero on a missing
	// object, failing the job.
	runJob(t, ctx, s3Client, "check-bucket", minioNamespace, componentReady,
		fmt.Sprintf("mc alias set t http://minio.%s.svc:9000 %s %s && mc stat t/%s/workload/backups/%s/velero-backup.json", minioNamespace, minioUser, minioPassword, bucket, name))

	// --- Weekly path, invoked now: restore objects AND bytes, persist proof,
	// then tear scratch down. This is the same path the scheduler invokes. ---
	passed, err := backup.RequestDrill(ctx, client, m, reporter)
	if err != nil {
		t.Fatalf("verified restore drill: %v", err)
	}
	if passed.Status != "passed" || passed.Backup != name || passed.Verification == nil {
		t.Fatalf("restore drill did not persist a full pass: %#v", passed)
	}
	if passed.Verification.Objects.Restored != passed.Verification.Objects.Matched ||
		passed.Verification.PVCData.Restored == 0 ||
		passed.Verification.PVCData.Restored != passed.Verification.PVCData.Matched {
		t.Fatalf("restore drill did not match every object and PVC data set: %#v", passed.Verification)
	}
	assertNoScratch(t, ctx, client)

	// Corrupt the newest backup's metadata in the real object store. The
	// Backup CR remains Completed, forcing the drill to prove it can read the
	// bytes rather than trusting status metadata.
	runJob(t, ctx, s3Client, "corrupt-backup", minioNamespace, componentReady,
		fmt.Sprintf("mc alias set t http://minio.%s.svc:9000 %s %s && printf '{corrupt' >/tmp/bad && mc cp /tmp/bad t/%s/workload/backups/%s/velero-backup.json && test \"$(mc cat t/%s/workload/backups/%s/velero-backup.json)\" = '{corrupt'", minioNamespace, minioUser, minioPassword, bucket, name, bucket, name))
	failedResult, drillErr := backup.RequestDrill(ctx, client, m, reporter)
	if drillErr == nil {
		t.Fatal("corrupted backup passed the verified restore drill")
	}
	if failedResult.Status != "failed" || failedResult.Failure == nil || failedResult.Failure.Stage == "" || failedResult.Failure.ReasonCode == "" {
		t.Fatalf("corrupted backup failure was not actionable: result=%#v err=%v", failedResult, drillErr)
	}
	t.Logf("corruption failed loudly: stage=%s reason=%s", failedResult.Failure.Stage, failedResult.Failure.ReasonCode)
	assertNoScratch(t, ctx, client)

	// --- Datastore runbook gate: snapshot a known object to external S3,
	// mutate it, stop k3s, reset from S3, and assert the old value returns. ---
	apply(t, ctx, client, `apiVersion: v1
kind: ConfigMap
metadata: {name: kn-f9lm-datastore-sentinel, namespace: default}
data: {value: before-restore}
`)
	res, err := client.Run(ctx, "sudo -n k3s etcd-snapshot save --name kn-f9lm-runbook --s3")
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("save runbook datastore snapshot: err=%v exit=%d stderr=%s", err, res.ExitCode, res.Stderr)
	}
	snapshot := remoteOut(t, ctx, client,
		"sudo -n k3s etcd-snapshot list --s3 | awk '$1 ~ /^kn-f9lm-runbook-/ {print $1}' | tail -1")
	if snapshot == "" {
		t.Fatal("runbook snapshot was not listed from S3")
	}
	kubectlOrFatal(t, ctx, client,
		`patch configmap kn-f9lm-datastore-sentinel -n default --type merge -p '{"data":{"value":"after-snapshot"}}'`)
	startedRestore := time.Now()
	if err := backup.RestoreDatastoreSnapshotFromS3(ctx,
		[]backup.DatastoreServer{{Name: *hostAddr, Runner: client}}, m, target, snapshot, reporter); err != nil {
		t.Fatalf("restore datastore from S3: %v", err)
	}
	if got := kubectlOut(t, ctx, client,
		"get configmap kn-f9lm-datastore-sentinel -n default -o jsonpath='{.data.value}'"); got != "before-restore" {
		t.Fatalf("datastore restore returned sentinel %q, want before-restore", got)
	}
	if remoteOut(t, ctx, client, "sudo -n test ! -e /etc/rancher/k3s/config.yaml.d/40-kubenest-restore.yaml && printf removed") != "removed" {
		t.Fatal("temporary datastore restore credentials remained on the server")
	}
	t.Logf("S3 datastore restore passed in %s using %s", time.Since(startedRestore).Round(time.Second), snapshot)

	// --- Idempotence: re-running install and configure converges on the
	// already-good state instead of breaking it. ---
	if err := backup.Install(ctx, client, m, reporter); err != nil {
		t.Fatalf("second Install (idempotence): %v", err)
	}
	if err := backup.Configure(ctx, client, m, target, reporter); err != nil {
		t.Fatalf("second Configure (idempotence): %v", err)
	}

	// Cleanup of test scaffolding only (velero itself is a core component
	// and stays). The host is torn down by ephemeral-env down — never leave
	// it billing.
	kubectlOrFatal(t, ctx, client, "delete namespace "+proofNamespace+" --ignore-not-found")
	kubectlOrFatal(t, ctx, s3Client, "delete namespace "+minioNamespace+" --ignore-not-found")
}

func ensureK3s(t *testing.T, ctx context.Context, r k3s.Runner, version string) {
	t.Helper()
	res, err := r.Run(ctx, fmt.Sprintf(
		"command -v k3s >/dev/null || curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=%q sh -s - server --cluster-init --disable traefik", version))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("k3s install: exit %d: %s", res.ExitCode, res.Stderr)
	}
}

func installDrillOperator(t *testing.T, ctx context.Context, r k3s.Runner, version string) {
	t.Helper()
	apply(t, ctx, r, fmt.Sprintf(`apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata: {name: operator, namespace: kube-system}
spec:
  chart: oci://ghcr.io/kubenesthq/charts/kubenest-operator-2
  version: %s
  targetNamespace: kubenest-system
  createNamespace: true
  valuesContent: |-
    bootstrap:
      argocd: {enabled: true}
      certManager: {enabled: false}
      gitea: {enabled: false}
      vault: {enabled: false}
    kubenest:
      backendURL: ws://127.0.0.1:1/ws/operator
      clusterID: 00000000-0000-0000-0000-00000000f9a1
      jwtSecret: real-cluster-gate-only
`, version))
}

func assertNoScratch(t *testing.T, ctx context.Context, r k3s.Runner) {
	t.Helper()
	left := kubectlOut(t, ctx, r,
		"get namespaces -l kubenest.io/restore-drill=scratch -o jsonpath='{.items[*].metadata.name}'")
	if left != "" {
		t.Fatalf("restore drill left scratch namespaces behind: %s", left)
	}
}

func restoreProofReadyForOperator(r k3s.Runner) converge.Probe {
	type proofPod struct {
		Metadata struct {
			CreationTimestamp string `json:"creationTimestamp"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Image string `json:"image"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	type proofConfig struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	return func(ctx context.Context) (bool, converge.State, error) {
		state := converge.State{Object: "restore proof source", Status: "not ready"}
		image, err := k3s.Kubectl(ctx, r,
			"get deployment operator-kubenest-operator-2-controller-manager -n kubenest-system "+
				"-o jsonpath='{.spec.template.spec.containers[0].image}'")
		if err != nil {
			return false, state, err
		}
		image = strings.TrimSpace(image)
		if image == "" {
			state.Status = "waiting for deployed operator image"
			return false, state, nil
		}
		raw, err := k3s.Kubectl(ctx, r,
			"get pod kubenest-restore-proof -n kubenest-restore-drill-source -o json")
		if err != nil {
			return false, state, err
		}
		var pod proofPod
		if err := json.Unmarshal([]byte(raw), &pod); err != nil {
			return false, state, err
		}
		if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Image != image {
			state.Status = "waiting for bundle-pinned proof image"
			return false, state, nil
		}
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
				break
			}
		}
		if !ready {
			state.Status = "bundle-pinned proof pod is not Ready"
			return false, state, nil
		}

		raw, err = k3s.Kubectl(ctx, r,
			"get configmap kubenest-restore-proof -n kubenest-restore-drill-source -o json")
		if err != nil {
			return false, state, err
		}
		var config proofConfig
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			return false, state, err
		}
		readyAt, err := time.Parse(time.RFC3339, config.Metadata.Annotations["kubenest.io/restore-drill-ready-at"])
		if err != nil {
			state.Status = "waiting for proof readiness timestamp"
			return false, state, nil
		}
		createdAt, err := time.Parse(time.RFC3339, pod.Metadata.CreationTimestamp)
		if err != nil {
			return false, state, err
		}
		if readyAt.Before(createdAt) {
			state.Status = "proof readiness predates the pinned pod"
			return false, state, nil
		}
		state.Status = "Ready"
		state.Detail = image
		return true, state, nil
	}
}

func minioManifests() string {
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata: {name: %[1]s}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: minio, namespace: %[1]s}
spec:
  replicas: 1
  selector: {matchLabels: {app: minio}}
  template:
    metadata: {labels: {app: minio}}
    spec:
      containers:
        - name: minio
          image: %[2]s
          args: ["server", "/data"]
          env:
            - {name: MINIO_ROOT_USER, value: %[3]s}
            - {name: MINIO_ROOT_PASSWORD, value: %[4]s}
          ports: [{containerPort: 9000}]
          volumeMounts: [{name: data, mountPath: /data}]
      volumes:
        - name: data
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata: {name: minio, namespace: %[1]s}
spec:
  type: NodePort
  selector: {app: minio}
  ports: [{port: 9000, targetPort: 9000, nodePort: %[5]d}]
`, minioNamespace, minioImage, minioUser, minioPassword, minioNodePort)
}

func proofManifests(proof string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata: {name: %[1]s}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: proof-data, namespace: %[1]s}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Gi}}
---
apiVersion: v1
kind: Pod
metadata: {name: writer, namespace: %[1]s}
spec:
  containers:
    - name: w
      image: busybox:1.36
      command: ["sh", "-c", "echo %[2]s > /data/proof && sync && sleep 86400"]
      volumeMounts: [{name: v, mountPath: /data}]
  volumes:
    - name: v
      persistentVolumeClaim: {claimName: proof-data}
`, proofNamespace, proof)
}

// runJob applies a one-shot Job running an mc command and waits for success.
func runJob(t *testing.T, ctx context.Context, r k3s.Runner, name, namespace string, deadline time.Duration, command string) {
	t.Helper()
	kubectlOrFatal(t, ctx, r, "delete job "+name+" -n "+namespace+" --ignore-not-found")
	apply(t, ctx, r, fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata: {name: %s, namespace: %s}
spec:
  backoffLimit: 6
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: mc
          image: %s
          command: ["sh", "-c", %q]
`, name, namespace, mcImage, command))
	waitFor(t, ctx, r, name+"-done", deadline, func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, r, "get job "+name+" -n "+namespace+" -o jsonpath='{.status.succeeded}'")
		if err != nil {
			return false, converge.State{Object: "job " + name, Status: "unobservable"}, err
		}
		if strings.TrimSpace(out) == "1" {
			return true, converge.State{Object: "job " + name, Status: "Succeeded"}, nil
		}
		return false, converge.State{Object: "job " + name, Status: "not succeeded yet"}, nil
	})
}

func apply(t *testing.T, ctx context.Context, r k3s.Runner, doc string) {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString([]byte(doc))
	res, err := r.Run(ctx, fmt.Sprintf("printf '%%s' %s | base64 -d | sudo -n k3s kubectl apply -f -", encoded))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("kubectl apply: %s", res.Stderr)
	}
}

func kubectlOut(t *testing.T, ctx context.Context, r k3s.Runner, args string) string {
	t.Helper()
	out, err := k3s.Kubectl(ctx, r, args)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func kubectlOrFatal(t *testing.T, ctx context.Context, r k3s.Runner, args string) {
	t.Helper()
	if _, err := k3s.Kubectl(ctx, r, args); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, ctx context.Context, r k3s.Runner, name string, deadline time.Duration, probe converge.Probe) {
	t.Helper()
	res, err := converge.Wait(ctx, probe, converge.Options{Name: name, Deadline: deadline, Reporter: converge.NewTextReporter(testWriter{t})})
	if err != nil {
		t.Fatal(err)
	}
	if e := res.Err(); e != nil {
		t.Fatal(e)
	}
}

func podsReadyIn(r k3s.Runner, namespace string) converge.Probe {
	return k3s.PodsReadyProbe(r, namespace)
}

func podsRunningIn(r k3s.Runner, namespace string) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, r,
			"get pods -n "+namespace+` -o jsonpath='{.items[*].status.phase}'`)
		if err != nil {
			return false, converge.State{Object: "pods in " + namespace, Status: "unobservable"}, err
		}
		phases := strings.Fields(out)
		if len(phases) > 0 {
			for _, phase := range phases {
				if phase != "Running" {
					return false, converge.State{Object: "pods in " + namespace, Status: strings.Join(phases, " ")}, nil
				}
			}
			return true, converge.State{Object: "pods in " + namespace, Status: "Running"}, nil
		}
		return false, converge.State{Object: "pods in " + namespace, Status: "none"}, nil
	}
}

func remoteOut(t *testing.T, ctx context.Context, r k3s.Runner, command string) string {
	t.Helper()
	res, err := r.Run(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("remote command failed with exit %d: %s", res.ExitCode, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout)
}

func nodeReadyProbe(r k3s.Runner) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		// Single-quoted for the remote shell: jsonpath's ?() and quotes are
		// shell syntax otherwise.
		out, err := k3s.Kubectl(ctx, r, `get nodes -o jsonpath='{.items[*].status.conditions[?(@.type=="Ready")].status}'`)
		if err != nil {
			return false, converge.State{Object: "nodes", Status: "unobservable"}, err
		}
		s := strings.TrimSpace(out)
		if s != "" && !strings.Contains(s, "False") && !strings.Contains(s, "Unknown") {
			return true, converge.State{Object: "nodes", Status: "Ready"}, nil
		}
		return false, converge.State{Object: "nodes", Status: "not Ready yet: " + s}, nil
	}
}

// testWriter adapts t.Logf for the text reporter.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
