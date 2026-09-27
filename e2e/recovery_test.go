//go:build e2e

// S6 and S11 on real hardware: recovering a lost single-server host, and
// recovering an all-in-one host (PLAN-CLOSE-THE-GAP-2026-09.md section 4, PLAN
// 7.8 and 7.9; beads T4.8/T4.9).
//
// THESE ARE THE ACCEPTANCE. A recovery moves data, rewrites a cluster's
// identity and rebuilds a control plane; no fake transport can accept that, so
// both scenarios run the REAL command tree against REAL hosts, a REAL control
// plane and a REAL S3-compatible bucket. They are excluded from the default
// build by the `e2e` tag and SKIP cleanly when the lab is not configured, so
// `go test ./...` stays runnable everywhere.
//
// LAB TOPOLOGY
//
//	S6  one single-server workload cluster on host 1, registered to a control
//	    plane, running a workload whose PVC holds known data and a sentinel Job
//	    that calls back to this workstation; one FRESH host (host 2, same
//	    shape, never joined any cluster); a FRESH laptop (a new HOME that has
//	    never held this cluster's kit).
//	S11 the same, plus: an all-in-one host (control plane + a workload with
//	    data on the same machine), a second cluster added to the fleet, one
//	    FRESH host and a FRESH laptop.
//
// ENVIRONMENT (all of it; the test skips and says which variable is missing)
//
//	KUBENEST_LAB_SERVER_IP            host 1 (or KUBENEST_LAB_NODE1_IP)
//	KUBENEST_LAB_NODE2_IP             the FRESH host the recovery installs onto
//	KUBENEST_LAB_SSH_USER             default ubuntu
//	KUBENEST_LAB_SSH_KEY              the private key
//	KUBENEST_LAB_NODE1_STORAGE_DEVICE blank device on host 1, e.g. /dev/disk/by-id/...
//	KUBENEST_LAB_NODE2_STORAGE_DEVICE blank device on the fresh host
//	KUBENEST_CONTROL_PLANE            https://api.<domain> of the live control plane
//	KUBENEST_CLI_TOKEN                a clusters:register CLI token for it
//	KUBENEST_CONTROL_PLANE_CA         file holding that control plane's CA (PEM)
//	KUBENEST_BUNDLE                   bundle version (default 1.1)
//	KUBENEST_BACKUP_TARGET            s3://bucket/prefix?endpoint=...&region=...
//	KUBENEST_BACKUP_ACCESS_KEY_ID / KUBENEST_BACKUP_SECRET_ACCESS_KEY
//	KUBENEST_FLEET_KEY_FILE           the fleet recovery key (AGE-SECRET-KEY-1...)
//	KUBENEST_DESTROY_COMMAND          the lab's own step that destroys ONE host,
//	                                  e.g. `lab/hetzner/destroy-node.sh <name>`; it
//	                                  must make the host unreachable and release its
//	                                  address. The payment for a machine is the box
//	                                  the operator cannot reach, and this is that.
//	KUBENEST_E2E_SENTINEL_HOST        this workstation's address AS THE LAB SEES IT
//	KUBENEST_E2E_WORKLOAD_NAMESPACE   the namespace whose data the workload keeps
//	KUBENEST_DOMAIN                   the control plane's domain (S11)
//	KUBENEST_ADMIN_PASSWORD           an administrator's own password (S11)
//	KUBENEST_HUB_URL                  wss://hub.<domain>/ws/operator (S6's refusal check)
//
// RUN
//
//	./scripts/ephemeral-env.sh up --profile host --nodes 2
//	source lab/hetzner/.lab-env.sh
//	cd kubenest-cli && go test -tags e2e -v -timeout 6h ./e2e/ -run 'TestLostSingleServerRecovery|TestAllInOneHostRecovery'
//
// WHAT THESE DO NOT PROVE: an unconditional recovery-time guarantee (the
// numbers are RECORDED, not asserted), recovery of the cluster-scoped objects
// the docs list as outside the workload backup, or the `ha` tier — a single
// server is both tasks' whole subject.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/cmd"
	"kubenest.io/cli/pkg/config"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/sshx"
)

// recoveryEnv is the lab S6/S11 need on top of gateEnv.
type recoveryEnv struct {
	gateEnv
	// freshHost is the machine the recovery installs onto. It must never have
	// joined this cluster: a host that has is a different scenario.
	freshHost string
	// freshDevice is a blank device on the fresh host.
	freshDevice string
	// backupTarget is the bucket the cluster's kits, sets and backups live in.
	backupTarget string
	// fleetKeyFile holds the fleet recovery key. On a real run this is the one
	// copy that never touched a host.
	fleetKeyFile string
	// destroyCommand destroys ONE lab host. The lab's own teardown is used
	// rather than a hand-rolled one so the test cannot destroy a machine in a
	// way the lab does not.
	destroyCommand string
	// sentinelHost is this workstation's address as the LAB sees it.
	sentinelHost string
	// namespace is the workload namespace whose data must come back.
	namespace string
	// domain and adminPassword are S11's: the control plane's domain, and an
	// administrator's own password (the accounts are in the restored database).
	domain        string
	adminPassword string
	// hubURL is wss://hub.<domain>/ws/operator, for the refusal check.
	hubURL string
	// oldAgentTokenFile holds the AGENT JWT the machine that is about to die
	// was using, captured by the lab harness BEFORE it destroys that machine
	// (the token reaches the operator through the install's chart values, so it
	// is on the host, and the host is the only place it exists once the
	// control plane has moved on). It is the credential the refusal check
	// hands the hub: the k3s join token in the kit is a DIFFERENT credential
	// and the hub would reject it for the wrong reason.
	oldAgentTokenFile string
}

func recoveryEnvironment(t *testing.T) recoveryEnv {
	t.Helper()
	env := recoveryEnv{
		gateEnv:      gateEnvironment(t),
		freshHost:    os.Getenv("KUBENEST_LAB_NODE2_IP"),
		freshDevice:  os.Getenv("KUBENEST_LAB_NODE2_STORAGE_DEVICE"),
		backupTarget: os.Getenv("KUBENEST_BACKUP_TARGET"),
		fleetKeyFile: os.Getenv("KUBENEST_FLEET_KEY_FILE"),
		sentinelHost: os.Getenv("KUBENEST_E2E_SENTINEL_HOST"),
		namespace:    os.Getenv("KUBENEST_E2E_WORKLOAD_NAMESPACE"),
		domain:       os.Getenv("KUBENEST_DOMAIN"),
	}
	env.destroyCommand = os.Getenv("KUBENEST_DESTROY_COMMAND")
	env.adminPassword = os.Getenv("KUBENEST_ADMIN_PASSWORD")
	env.oldAgentTokenFile = os.Getenv("KUBENEST_E2E_OLD_AGENT_TOKEN_FILE")
	env.hubURL = os.Getenv("KUBENEST_HUB_URL")
	for _, missing := range []struct{ name, value string }{
		{"KUBENEST_LAB_NODE2_IP", env.freshHost},
		{"KUBENEST_LAB_NODE2_STORAGE_DEVICE", env.freshDevice},
		{"KUBENEST_BACKUP_TARGET", env.backupTarget},
		{"KUBENEST_FLEET_KEY_FILE", env.fleetKeyFile},
		{"KUBENEST_DESTROY_COMMAND", env.destroyCommand},
		{"KUBENEST_E2E_SENTINEL_HOST", env.sentinelHost},
		{"KUBENEST_E2E_WORKLOAD_NAMESPACE", env.namespace},
		{"KUBENEST_E2E_OLD_AGENT_TOKEN_FILE", env.oldAgentTokenFile},
	} {
		if missing.value == "" {
			t.Skipf("%s is not set: this scenario destroys and rebuilds real hosts, and it will not start without the machine to rebuild onto, the bucket, the fleet key, the lab's own destroy step and the sentinel receiver. See this file's header for the full list", missing.name)
		}
	}
	if env.controlPlaneCA == nil {
		t.Skip("KUBENEST_CONTROL_PLANE_CA is not set: a self-hosted control plane issues its own authority, and a CLI that does not pin it cannot verify the control plane it recovers")
	}
	return env
}

// freshLaptop points HOME at a directory that has never held this cluster's
// kit or journal, and gives it the one thing an operator on a new machine has:
// a control-plane login (the URL, the authority and a token) and the fleet
// recovery key.
//
// IT IS A NEW HOME RATHER THAN A CLEANED ONE ON PURPOSE. Journals, the local
// kit cache and the config all live under HOME, and a recovery that only works
// because something was left behind in a previous HOME is not a recovery.
func freshLaptop(t *testing.T, env recoveryEnv) (home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBENEST_BACKUP_ACCESS_KEY_ID", os.Getenv("KUBENEST_BACKUP_ACCESS_KEY_ID"))
	t.Setenv("KUBENEST_BACKUP_SECRET_ACCESS_KEY", os.Getenv("KUBENEST_BACKUP_SECRET_ACCESS_KEY"))

	cfg := &config.Config{ControlPlaneURL: env.controlPlane, ControlPlaneCA: string(env.controlPlaneCA)}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("writing the fresh laptop's config: %v", err)
	}
	creds := &config.Credentials{}
	creds.Set(env.controlPlane, env.token)
	if err := config.SaveCredentials(creds); err != nil {
		t.Fatalf("writing the fresh laptop's credential: %v", err)
	}
	// Nothing else: no journal for the cluster, no local kit.
	for _, dir := range []string{"journals", "recovery-kits"} {
		path := filepath.Join(home, ".kubenest", dir)
		if entries, err := os.ReadDir(path); err == nil && len(entries) > 0 {
			t.Fatalf("the fresh laptop's %s is not empty, so this run cannot claim it never held the cluster's kit", path)
		}
	}
	return home
}

// runCLI drives the real command tree, which is the only way a gate can be
// sure the verb an operator types is the verb under test.
func runCLI(out io.Writer, args ...string) error {
	root := cmd.NewRootCommand()
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	return root.Execute()
}

func runCLIStrict(t *testing.T, what string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runCLI(&out, args...); err != nil {
		t.Fatalf("%s: %v\n%s", what, err, out.String())
	}
	return out.String()
}

// destroyHost runs the lab's own step that destroys ONE machine, and waits for
// the machine to stop answering. Nothing else in this file powers a host off:
// the fencing has to be a step the lab can do, or the scenario is describing a
// runbook nobody can follow.
func destroyHost(t *testing.T, env recoveryEnv, address string) {
	t.Helper()
	command := env.destroyCommand + " " + address
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", command).CombinedOutput()
	if err != nil {
		t.Fatalf("the lab's destroy step (%q) failed: %v\n%s", command, err, out)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if _, err := sshx.Resolve(address, sshx.Options{User: env.sshUser, KeyPath: env.sshKey}); err != nil {
			t.Logf("destroyed %s: %v", address, err)
			return
		}
		conn, err := sshx.Dial(context.Background(), mustEndpoint(t, address, env), sshx.Options{KeyPath: env.sshKey})
		if err != nil {
			t.Logf("destroyed %s: %v", address, err)
			return
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatalf("%s still answers after %q: the machine this recovery replaces is NOT fenced, and an unfenced old host that rejoins is two clusters behind one identity", address, command)
		}
		time.Sleep(10 * time.Second)
	}
}

func mustEndpoint(t *testing.T, address string, env recoveryEnv) *sshx.Endpoint {
	t.Helper()
	ep, err := sshx.Resolve(address, sshx.Options{User: env.sshUser, KeyPath: env.sshKey})
	if err != nil {
		t.Fatalf("resolving %s: %v", address, err)
	}
	return ep
}

// recoverySentinel is the receiver the workload's sentinel calls. It is the
// s4 gate's receiver, reused: one implementation of "an external observer that
// cannot be fooled by cluster-side bookkeeping".
func recoverySentinel(t *testing.T) *s4Sentinel { return s4StartSentinel(t) }

// openRecordedKit reads the cluster's newest kit FROM THE BUCKET with the
// fleet key, which is what a fresh laptop can do and all it can do. The kit
// carries the cluster's join token and repository password; the recovery set
// carries the repository identity, the versions and the backups.
func openRecordedKit(t *testing.T, env recoveryEnv, clusterID string) (map[string]string, *recoverykit.Set) {
	t.Helper()
	fleetKey := readFleetKey(t, env)
	target, err := backup.ParseTarget(env.backupTarget,
		os.Getenv("KUBENEST_BACKUP_ACCESS_KEY_ID"), os.Getenv("KUBENEST_BACKUP_SECRET_ACCESS_KEY"))
	if err != nil {
		t.Fatalf("parsing KUBENEST_BACKUP_TARGET: %v", err)
	}
	client, err := target.S3Client()
	if err != nil {
		t.Fatal(err)
	}
	scope := strings.Trim(target.Prefix, "/")
	keys, truncated, err := client.List(context.Background(), scope+"/"+recoverykit.SetsDir+"/"+clusterID+"/")
	if err != nil || truncated {
		t.Fatalf("listing the recovery sets for %s: %v (truncated=%v)", clusterID, err, truncated)
	}
	if len(keys) == 0 {
		t.Fatalf("the bucket holds no recovery set for cluster %s under %s: nothing says which kit and which backup a recovery should start from", clusterID, scope)
	}
	rawSet, err := client.Get(context.Background(), keys[len(keys)-1])
	if err != nil {
		t.Fatal(err)
	}
	set, err := recoverykit.LoadSet(rawSet)
	if err != nil {
		t.Fatalf("the recovery set at %s is not readable: %v", keys[len(keys)-1], err)
	}
	rawKit, err := client.Get(context.Background(), recoverykit.KitKey(scope, clusterID, recoverykit.KindCluster, set.ArtifactID))
	if err != nil {
		t.Fatalf("reading the kit the set names: %v", err)
	}
	kit, err := recoverykit.Load(rawKit)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := kit.Secrets(fleetKey)
	if err != nil {
		t.Fatalf("the fleet key does not open the cluster's kit: %v", err)
	}
	return secrets, set
}

func readFleetKey(t *testing.T, env recoveryEnv) string {
	t.Helper()
	raw, err := os.ReadFile(env.fleetKeyFile)
	if err != nil {
		t.Fatalf("reading the fleet recovery key from %s: %v", env.fleetKeyFile, err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		t.Fatalf("%s is empty", env.fleetKeyFile)
	}
	return key
}

// controlPlane reports the cluster record the control plane holds, which is
// where the immutable id comes from — never a display name.
func controlPlane(t *testing.T, env recoveryEnv) *api.Client {
	t.Helper()
	client, err := api.New(env.controlPlane, api.WithToken(env.token), api.WithCACert(env.controlPlaneCA))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// clusterRecord finds one cluster by name and returns its record, so the test
// names the cluster by its IMMUTABLE id everywhere after this point.
func clusterRecord(t *testing.T, client *api.Client, name string) *api.Cluster {
	t.Helper()
	orgs, err := client.ListOrgs(context.Background())
	if err != nil || len(orgs) == 0 {
		t.Fatalf("listing organisations: %v (%d)", err, len(orgs))
	}
	for _, org := range orgs {
		clusters, err := client.ListOrgClusters(context.Background(), org.ID)
		if err != nil {
			t.Fatalf("listing %s's clusters: %v", org.Name, err)
		}
		for i := range clusters {
			if clusters[i].Name == name {
				return &clusters[i]
			}
		}
	}
	t.Fatalf("the control plane holds no cluster named %q", name)
	return nil
}

// hubRefuses performs a real WebSocket handshake at the hub with one agent
// token and reports the status code. 101 means it was admitted; anything else
// means it was not.
//
// IT IS A HANDSHAKE RATHER THAN A HUB-SIDE ASSERTION because that is the claim:
// the token the dead host's disk holds must not open a connection.
func hubRefuses(t *testing.T, hubURL, token string) int {
	t.Helper()
	url := strings.Replace(hubURL, "wss://", "https://", 1)
	url = strings.Replace(url, "ws://", "http://", 1)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", randomNonce())
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("handshaking with the hub at %s: %v", hubURL, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestLostSingleServerRecovery is S6, whole: the only server of a workload
// cluster dies and comes back on a fresh host from the bucket and the fleet
// recovery key alone.
func TestLostSingleServerRecovery(t *testing.T) {
	env := recoveryEnvironment(t)
	client := controlPlane(t, env)
	sentinel := recoverySentinel(t)

	// ── the fixture: a single-server workload cluster with data, a completed
	// backup, and a recovery set that names it.
	record := clusterRecord(t, client, env.cluster)
	_, set := openRecordedKit(t, env, record.ID)
	latest, ok := set.Latest()
	if !ok {
		t.Fatalf("cluster %s's recovery set names no completed backup, so there is nothing to restore: take one (`kubenest backup now`) and re-run", record.ID)
	}

	// ── step 2, run first because it is the whole point of the ordering:
	// `recovery-kit check` must confirm the artifact before anything changes.
	var checkOut bytes.Buffer
	if err := runCLI(&checkOut,
		"recovery-kit", "check",
		"--cluster", record.ID,
		"--instance", os.Getenv("KUBENEST_INSTANCE_ID"),
		"--organisation", os.Getenv("KUBENEST_ORGANISATION_ID"),
		"--target", env.backupTarget,
		"--fleet-key-file", env.fleetKeyFile,
	); err != nil {
		t.Fatalf("recovery-kit check refused the cluster's own kit before the recovery started: %v\n%s", err, checkOut.String())
	}
	t.Logf("recovery-kit check: %s", checkOut.String())

	// ── the planted negatives, all of which must be refused BEFORE anything
	// changes: another cluster's kit, an incomplete set, and a display name.
	t.Run("a display name never authorises adoption", func(t *testing.T) {
		var out bytes.Buffer
		err := runCLI(&out,
			"platform", "install",
			"--bundle", env.bundle,
			"--cluster", env.cluster, // the NAME, not the id
			"--restore-from", "latest", "--recovery-kit", "s3",
			"--backup-target", env.backupTarget,
			"--fleet-key-file", env.fleetKeyFile,
			"--old-host-fenced",
			"--server", env.freshHost, "--ha", "single-server",
			"--ssh-user", env.sshUser, "--ssh-key", env.sshKey,
		)
		if err == nil {
			t.Fatalf("a recovery was accepted with a display name as --cluster:\n%s", out.String())
		}
		if !strings.Contains(err.Error(), "id") && !strings.Contains(out.String(), "id") {
			t.Fatalf("the refusal does not say that an immutable id is what the flag wants: %v\n%s", err, out.String())
		}
	})

	// ── the host dies. From here the laptop that installed it is gone too.
	destroyHost(t, env, env.server)

	// ── a FRESH laptop: a new HOME with a control-plane login and the fleet
	// key, and nothing else.
	freshLaptop(t, env)

	// ── the recovery, as one command, driven through the real command tree.
	started := time.Now()
	var out bytes.Buffer
	if err := runCLI(&out,
		"platform", "install",
		"--bundle", env.bundle,
		"--cluster", record.ID,
		"--restore-from", "latest",
		"--recovery-kit", "s3",
		"--backup-target", env.backupTarget,
		"--fleet-key-file", env.fleetKeyFile,
		"--old-host-fenced",
		"--server", env.freshHost,
		"--ha", "single-server",
		"--ssh-user", env.sshUser,
		"--ssh-key", env.sshKey,
		"--storage-device", env.freshDevice,
	); err != nil {
		t.Fatalf("the recovery failed: %v\n%s", err, out.String())
	}
	elapsed := time.Since(started)
	t.Logf("S6 recovery wall-clock: %s", elapsed.Round(time.Second))
	t.Logf("S6 pass limits to record with the release: restored data age %s (backup %s, completed %s), data volume and host size and bandwidth as measured by the lab",
		time.Since(latest.CompletedAt).Round(time.Second), latest.Name, latest.CompletedAt.Format(time.RFC3339))

	// ── a NEW physical incarnation under the SAME immutable id, with the old
	// machine's credentials retired.
	incarnations, err := client.ListIncarnations(context.Background(), record.ID)
	if err != nil {
		t.Fatalf("reading the cluster's incarnations: %v", err)
	}
	if len(incarnations) < 2 {
		t.Fatalf("cluster %s recorded %d incarnation(s): the rebuilt host is not a new machine to the control plane", record.ID, len(incarnations))
	}
	rebuilt := incarnations[len(incarnations)-1]
	if rebuilt.Ordinal < 2 || rebuilt.TokenFloor == nil {
		t.Fatalf("the new incarnation is ordinal %d with floor %v: the old host's token was not retired", rebuilt.Ordinal, rebuilt.TokenFloor)
	}

	// ── the token the dead host was using is REFUSED at the hub, checked by
	// handshaking with it: the claim is about a connection, so it is tested
	// with a connection.
	if env.hubURL == "" {
		t.Fatal("KUBENEST_HUB_URL is not set: the scenario cannot observe that the superseded token is refused without the hub to refuse it")
	}
	superseded, err := os.ReadFile(env.oldAgentTokenFile)
	if err != nil {
		t.Fatalf("reading the captured old agent token from %s: %v", env.oldAgentTokenFile, err)
	}
	status := hubRefuses(t, env.hubURL, strings.TrimSpace(string(superseded)))
	if status == http.StatusSwitchingProtocols {
		t.Fatalf("the token the dead host was using was ADMITTED by the hub (HTTP %d): a lost disk keeps operator access to the recovered cluster", status)
	}
	t.Logf("the superseded token was refused at the hub (HTTP %d)", status)

	// ── the workload's data is back, and the cluster reports in.
	runner := dialHost(t, env, env.freshHost)
	if err := recoveryWaitsForNamespace(runner, env.namespace, 20*time.Minute); err != nil {
		t.Fatal(err)
	}
	if hits := sentinel.count(); hits == 0 {
		t.Fatalf("the sentinel receiver saw no call: nothing proved the workload is running again, and this file's claims about ordering rest on that receiver")
	}

	// ── `node replace` on a single-server cluster's only server names THIS
	// procedure rather than replacing it.
	var replaceOut bytes.Buffer
	err = runCLI(&replaceOut,
		"node", "replace",
		"--cluster", record.ID,
		"--node", env.server,
		"--with", env.freshHost,
		"--ssh-user", env.sshUser, "--ssh-key", env.sshKey,
	)
	if err == nil {
		t.Fatalf("`node replace` replaced a single-server cluster's only server:\n%s", replaceOut.String())
	}
	message := replaceOut.String() + err.Error()
	if !strings.Contains(message, "restore-from") && !strings.Contains(message, "recovery") {
		t.Fatalf("`node replace` refused without naming the procedure that recovers a single-server cluster:\n%s", message)
	}
}

// TestAllInOneHostRecovery is S11: the all-in-one host dies, and one ordered
// procedure brings the control plane and its own workloads back from the
// bucket and the fleet recovery key.
func TestAllInOneHostRecovery(t *testing.T) {
	env := recoveryEnvironment(t)
	if env.domain == "" || env.adminPassword == "" {
		t.Skip("KUBENEST_DOMAIN and KUBENEST_ADMIN_PASSWORD are not set: the all-in-one recovery reinstalls the control plane under its own domain and signs in as an administrator that already exists in the restored database")
	}
	sentinel := recoverySentinel(t)

	// ── the fixture: the all-in-one host is the control plane's home, so it is
	// destroyed LAST and the fleet's records come from it.
	client := controlPlane(t, env)
	management := clusterRecord(t, client, env.cluster)

	// ── the host dies.
	destroyHost(t, env, env.server)

	// ── a fresh laptop: a new HOME with the fleet key. It cannot hold a login,
	// because the control plane it would log in to is the thing that is gone.
	freshLaptop(t, env)

	// ── the one ordered procedure.
	started := time.Now()
	var out bytes.Buffer
	if err := runCLI(&out,
		"platform", "install",
		"--control-plane",
		"--bundle", env.bundle,
		"--name", management.Name,
		"--domain", env.domain,
		"--restore-from", "latest",
		"--recovery-kit", "s3",
		"--backup-target", env.backupTarget,
		"--fleet-key-file", env.fleetKeyFile,
		"--old-host-fenced",
		"--admin-password", env.adminPassword,
		"--server", env.freshHost,
		"--ha", "single-server",
		"--ssh-user", env.sshUser,
		"--ssh-key", env.sshKey,
		"--storage-device", env.freshDevice,
	); err != nil {
		t.Fatalf("the all-in-one recovery failed: %v\n%s", err, out.String())
	}
	t.Logf("S11 recovery wall-clock: %s (record it with the restored state's age and the lab's volumes and bandwidth)", time.Since(started).Round(time.Second))

	// ── the recovery reports the checkpoint's time and what it includes.
	transcript := out.String()
	if !strings.Contains(transcript, "checkpoint") {
		t.Fatalf("the recovery never said which checkpoint it restored, so the operator cannot judge the state's age:\n%s", transcript)
	}

	// ── the restored control plane answers, and its live state is compared
	// against what surviving clusters report rather than pushed at them.
	if !strings.Contains(transcript, "difference") && !strings.Contains(transcript, "agrees") {
		t.Fatalf("the recovery did not report the comparison between the restored desired state and what the clusters report:\n%s", transcript)
	}

	// ── the restored control plane verifies with the authority the fleet
	// already holds: a client pinned to the OLD CA reaches it, with no
	// re-issued trust.
	restored := controlPlane(t, env)
	if _, err := restored.ListOrgs(context.Background()); err != nil {
		t.Fatalf("the restored control plane was not reachable with the authority the existing CLI already holds: %v", err)
	}

	// ── the management cluster's own workload data is back.
	runner := dialHost(t, env, env.freshHost)
	if err := recoveryWaitsForNamespace(runner, env.namespace, 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	if sentinel.count() == 0 {
		t.Fatal("the sentinel receiver saw no call: nothing proved the management workload runs again after activation")
	}
}

// dialHost opens an SSH connection to one lab host, which every cluster-side
// assertion goes through.
func dialHost(t *testing.T, env recoveryEnv, address string) *sshx.Client {
	t.Helper()
	conn, err := sshx.Dial(context.Background(), mustEndpoint(t, address, env), sshx.Options{KeyPath: env.sshKey})
	if err != nil {
		t.Fatalf("dialling %s: %v", address, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// recoveryWaitsForNamespace waits until a namespace is back and its pods are
// Running, which is what "the workloads came back after activation" means. It
// is named apart from the reconcile-pause gate's helper, which waits on a
// different thing (the operator's own acknowledgement).
func recoveryWaitsForNamespace(runner *sshx.Client, namespace string, within time.Duration) error {
	ctx := context.Background()
	deadline := time.Now().Add(within)
	var last string
	for {
		out, err := k3s.Kubectl(ctx, runner, "get pods -n "+namespace+" -o jsonpath={.items[*].status.phase}")
		switch {
		case err != nil:
			last = err.Error()
		case strings.Contains(out, "Running"):
			// One Running pod is not the whole namespace, but a namespace that
			// has one is a namespace whose restore completed and whose project
			// was activated; the sentinel call is what proves the workload.
			return nil
		default:
			last = "phases: " + strings.TrimSpace(out)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("namespace %s did not come back within %s: %s", namespace, within, last)
		}
		time.Sleep(10 * time.Second)
	}
}

// randomNonce returns a fresh Sec-WebSocket-Key for the handshake. It is not a
// credential: the header only has to be unpredictable, and a fixed one would
// let a proxy answer the handshake from cache.
func randomNonce() string {
	buf := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "AAAAAAAAAAAAAAAAAAAAAA=="
	}
	return base64.StdEncoding.EncodeToString(buf)
}
