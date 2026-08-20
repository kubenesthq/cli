//go:build host

// Real-host integration test for kn-mzn's install half — the wave-1 gate for
// the backup component: run standalone against an ephemeral-env host-profile
// node and reach the verify condition, then prove the page's claims:
//
//   - Velero installs UNCONFIGURED and that state is visible, not blocking
//     (install.mdx / backup-restore.mdx: `backup: unconfigured`).
//   - A customer-supplied S3-compatible target (a real in-cluster MinIO —
//     the S3 API over the wire, not a mock) is configured and PROVEN:
//     Velero validates the location Available with the supplied credentials.
//   - The default schedule from the manifest lands: daily 02:00, ttl 336h.
//   - `backup now` completes and the backup — including PVC volume data via
//     file-system backup — actually lands in the bucket, asserted from the
//     bucket's side with mc, not from Velero's word alone.
//
// Run from the umbrella workspace:
//
//	./scripts/ephemeral-env.sh up --profile host
//	source lab/hetzner/.lab-env.sh
//	cd kubenest-cli && go test -tags host -timeout 45m -run TestBackupOnRealHost -v ./pkg/backup \
//	  -host "$KN_LAB_NODE0_IP" -ssh-user "$KN_LAB_SSH_USER" -ssh-key "$KN_LAB_SSH_KEY" \
//	  -bundle ../kubenest-contracts/bundles/platform-1.0.yaml
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
	sshUser    = flag.String("ssh-user", "root", "SSH user on the node")
	sshKey     = flag.String("ssh-key", "", "SSH private key path")
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
)

func TestBackupOnRealHost(t *testing.T) {
	if *hostAddr == "" || *bundlePath == "" {
		t.Skip("needs -host and -bundle (see the file comment)")
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

	ep, err := sshx.Resolve(*hostAddr, sshx.Options{User: *sshUser, KeyPath: *sshKey})
	if err != nil {
		t.Fatal(err)
	}
	client, err := sshx.Dial(ctx, ep, sshx.Options{KeyPath: *sshKey})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	reporter := converge.NewTextReporter(testWriter{t})

	// --- Test scaffolding: k3s at the bundle's pinned version (kn-7k8's
	// stage, not this component). A no-op when the shared node has it. ---
	k3sVersion, err := m.Core.Version("k3s")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("scaffolding: ensuring k3s %s", k3sVersion)
	res, err := client.Run(ctx, fmt.Sprintf(
		"command -v k3s >/dev/null || curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=%q sh -s - server --disable traefik", k3sVersion))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("k3s install: exit %d: %s", res.ExitCode, res.Stderr)
	}
	nodeReady, err := m.Limits.Timeouts.For("node-ready")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, client, "node-ready", nodeReady, nodeReadyProbe(client))

	// --- The component under test: install, UNCONFIGURED. ---
	if err := backup.Install(ctx, client, m, reporter); err != nil {
		t.Fatalf("Install: %v", err)
	}
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
	apply(t, ctx, client, minioManifests())
	waitFor(t, ctx, client, "minio-ready", componentReady, podsReadyIn(client, minioNamespace))
	runJob(t, ctx, client, "make-bucket", minioNamespace, componentReady,
		fmt.Sprintf("mc alias set t http://minio.%s.svc:9000 %s %s && mc mb -p t/%s", minioNamespace, minioUser, minioPassword, bucket))

	proof := fmt.Sprintf("kn-mzn-proof-%d", time.Now().Unix())
	apply(t, ctx, client, proofManifests(proof))
	// The writer must be RUNNING at backup time — file-system backup only
	// covers volumes of running pods — so it writes, then sleeps.
	waitFor(t, ctx, client, "proof-workload-ready", componentReady, podsReadyIn(client, proofNamespace))

	// --- Configure the target and prove it. ---
	target := backup.Target{
		Endpoint: fmt.Sprintf("http://minio.%s.svc:9000", minioNamespace),
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

	// The schedule on the cluster is the manifest's (decision E), not a
	// constant's: daily at 02:00, retention 14 × 24h.
	cron := kubectlOut(t, ctx, client, "get schedule daily -n velero -o jsonpath={.spec.schedule}")
	if cron != "0 2 * * *" {
		t.Errorf("schedule cron = %q, want 0 2 * * *", cron)
	}
	ttl := kubectlOut(t, ctx, client, "get schedule daily -n velero -o jsonpath={.spec.template.ttl}")
	if ttl != "336h0m0s" {
		t.Errorf("schedule ttl = %q, want 336h0m0s", ttl)
	}

	// --- backup now: complete, and land in the actual bucket. ---
	name := "manual-" + time.Now().UTC().Format("20060102-150405")
	if err := backup.TakeBackup(ctx, client, m, name, reporter); err != nil {
		t.Fatalf("TakeBackup: %v", err)
	}

	// Volume data went through file-system backup: the proof pod's PVC has a
	// Completed PodVolumeBackup under this backup, and nothing failed.
	pvbPhases := kubectlOut(t, ctx, client,
		"get podvolumebackups -n velero -l velero.io/backup-name="+name+" -o jsonpath={range .items[*]}{.spec.pod.namespace}/{.spec.volume}={.status.phase}{\"\\n\"}{end}")
	if !strings.Contains(pvbPhases, proofNamespace+"/") || !strings.Contains(pvbPhases, "=Completed") {
		t.Errorf("no Completed PodVolumeBackup for the proof volume under %s; got:\n%s", name, pvbPhases)
	}
	if strings.Contains(pvbPhases, "=Failed") {
		t.Errorf("a PodVolumeBackup failed under %s:\n%s", name, pvbPhases)
	}

	// Asserted from the BUCKET's side: velero's manifest object for this
	// backup exists in object storage. mc stat exits non-zero on a missing
	// object, failing the job.
	runJob(t, ctx, client, "check-bucket", minioNamespace, componentReady,
		fmt.Sprintf("mc alias set t http://minio.%s.svc:9000 %s %s && mc stat t/%s/backups/%s/velero-backup.json", minioNamespace, minioUser, minioPassword, bucket, name))

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
	kubectlOrFatal(t, ctx, client, "delete namespace "+minioNamespace+" --ignore-not-found")
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
  selector: {app: minio}
  ports: [{port: 9000, targetPort: 9000}]
`, minioNamespace, minioImage, minioUser, minioPassword)
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
		out, err := k3s.Kubectl(ctx, r, "get job "+name+" -n "+namespace+" -o jsonpath={.status.succeeded}")
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

func nodeReadyProbe(r k3s.Runner) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, r, "get nodes -o jsonpath={.items[*].status.conditions[?(@.type==\"Ready\")].status}")
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
