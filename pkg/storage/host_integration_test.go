//go:build host

// Real-host integration test for kn-1nn — the wave-1 gate for the storage
// component: run standalone against an ephemeral-env host-profile node and
// reach the verify condition, then prove the bead's acceptance list: a PVC
// binds, data survives a pod restart, and the StorageClass is the default.
//
// Run from the umbrella workspace:
//
//	./scripts/ephemeral-env.sh up --profile host
//	source lab/hetzner/.lab-env.sh
//	cd kubenest-cli && go test -tags host -timeout 30m -run TestStorageOnRealHost -v ./pkg/storage \
//	  -host "$KN_LAB_NODE0_IP" -ssh-user "$KN_LAB_SSH_USER" -ssh-key "$KN_LAB_SSH_KEY" \
//	  -storage-device "$KN_LAB_NODE0_STORAGE_DEVICE" \
//	  -bundle ../kubenest-contracts/bundles/platform-1.0.yaml
//	./scripts/ephemeral-env.sh down --profile host   # ALWAYS — it bills by the hour
//
// The k3s install below is TEST SCAFFOLDING at the version the bundle pins:
// productizing the k3s stage is kn-7k8, not this package.
package storage_test

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/storage"
)

var (
	hostAddr   = flag.String("host", "", "target node address")
	sshUser    = flag.String("ssh-user", "root", "SSH user on the node")
	sshKey     = flag.String("ssh-key", "", "SSH private key path")
	device     = flag.String("storage-device", "", "blank device for kubenest-vg (by-id path)")
	bundlePath = flag.String("bundle", "", "bundle manifest path (platform-1.0.yaml)")
)

func TestStorageOnRealHost(t *testing.T) {
	if *hostAddr == "" || *bundlePath == "" || *device == "" {
		t.Skip("needs -host, -storage-device and -bundle (see the file comment)")
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

	// --- Preflight, Option 2: the attached data volume is blank. ---
	own, err := storage.PreflightVolumeGroup(ctx, client, *device)
	if err != nil {
		t.Fatalf("preflight (option 2): %v", err)
	}
	if own != storage.InstallerCreated {
		t.Fatalf("ownership = %q, want installer-created", own)
	}

	// --- Test scaffolding: k3s at the bundle's pinned version (kn-7k8's
	// stage 3, not this component). ---
	k3sVersion, err := m.Core.Version("k3s")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("scaffolding: installing k3s %s", k3sVersion)
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
	r, err := converge.Wait(ctx, nodeReadyProbe(client), converge.Options{
		Name: "node-ready", Deadline: nodeReady, Reporter: reporter,
	})
	if err != nil || r.Err() != nil {
		t.Fatalf("node never became Ready: %v %v", err, r.Err())
	}

	// --- The component under test. ---
	if err := storage.EnsureVolumeGroup(ctx, client, *device); err != nil {
		t.Fatalf("EnsureVolumeGroup: %v", err)
	}
	// Preflight Option 1 must now see the created VG — the same check the
	// installer runs on customer-created volume groups.
	own, err = storage.PreflightVolumeGroup(ctx, client, "")
	if err != nil {
		t.Fatalf("preflight (option 1, after create): %v", err)
	}
	if own != storage.CustomerCreated {
		t.Fatalf("option-1 preflight ownership = %q, want customer-created", own)
	}

	if err := storage.Install(ctx, client, m, reporter); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := storage.Verify(ctx, client, m, reporter); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// Idempotence: a re-run converges immediately on the already-good state.
	if err := storage.Install(ctx, client, m, reporter); err != nil {
		t.Fatalf("second Install (idempotence): %v", err)
	}

	// --- Acceptance (the bead's TESTS): PVC binds, data survives a pod
	// restart, and exactly this StorageClass is the default. ---
	apply(t, ctx, client, pvcAndWriter)
	waitFor(t, ctx, client, "writer-done", componentReady, podSucceededProbe(client, "writer"))

	kubectlOrFatal(t, ctx, client, "delete pod writer -n default --ignore-not-found")
	apply(t, ctx, client, readerPod)
	waitFor(t, ctx, client, "reader-done", componentReady, podSucceededProbe(client, "reader"))

	logs, err := k3s.Kubectl(ctx, client, "logs reader -n default")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "kn-1nn-proof") {
		t.Fatalf("data did not survive the pod restart; reader saw: %q", logs)
	}

	out, err := k3s.Kubectl(ctx, client, "get storageclass -o jsonpath={range .items[*]}{.metadata.name}={.metadata.annotations.storageclass\\.kubernetes\\.io/is-default-class}{\"\\n\"}{end}")
	if err != nil {
		t.Fatal(err)
	}
	defaults := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasSuffix(line, "=true") {
			defaults++
			if !strings.HasPrefix(line, storage.StorageClassName+"=") {
				t.Errorf("unexpected default StorageClass: %s", line)
			}
		}
	}
	if defaults != 1 {
		t.Errorf("default StorageClass count = %d, want exactly 1 (%s)", defaults, storage.StorageClassName)
	}

	// Cleanup of test workloads only. The host itself is torn down by
	// ephemeral-env down — never leave it billing.
	kubectlOrFatal(t, ctx, client, "delete pod reader -n default --ignore-not-found")
	kubectlOrFatal(t, ctx, client, "delete pvc kn-1nn-proof -n default --ignore-not-found")
}

const pvcAndWriter = `apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: kn-1nn-proof, namespace: default}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Gi}}
---
apiVersion: v1
kind: Pod
metadata: {name: writer, namespace: default}
spec:
  restartPolicy: Never
  containers:
    - name: w
      image: busybox:1.36
      command: ["sh", "-c", "echo kn-1nn-proof > /data/proof && sync && cat /data/proof"]
      volumeMounts: [{name: v, mountPath: /data}]
  volumes:
    - name: v
      persistentVolumeClaim: {claimName: kn-1nn-proof}
`

const readerPod = `apiVersion: v1
kind: Pod
metadata: {name: reader, namespace: default}
spec:
  restartPolicy: Never
  containers:
    - name: r
      image: busybox:1.36
      command: ["sh", "-c", "cat /data/proof"]
      volumeMounts: [{name: v, mountPath: /data}]
  volumes:
    - name: v
      persistentVolumeClaim: {claimName: kn-1nn-proof}
`

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

// podSucceededProbe observes one pod until phase Succeeded.
func podSucceededProbe(r k3s.Runner, name string) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, r, "get pod "+name+" -n default -o jsonpath={.status.phase}")
		if err != nil {
			return false, converge.State{Object: "pod " + name, Status: "unobservable"}, err
		}
		phase := strings.TrimSpace(out)
		if phase == "Succeeded" {
			return true, converge.State{Object: "pod " + name, Status: phase}, nil
		}
		return false, converge.State{Object: "pod " + name, Status: phase}, nil
	}
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
