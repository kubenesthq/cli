// Package interlock — probe P1's outcome, recorded here as the bead asks,
// because these tests are what hold it (hardware, 2026-09-25, chart 6.1.0 =
// kured 1.23.0, one bundle-1.1 server + agent cluster; the probe script and
// its log are committed at lab/p1-kured-probe/):
//
//  1. The CLI CAN take kured's own lock and kured honours it. The CLI wrote
//     {"nodeID":"kubenest-cli/p1-probe","metadata":{"unschedulable":false},
//     "created":...,"TTL":0} with an RV-guarded `kubectl replace` of the
//     object it had read. With the lock held, kured did not reboot the agent
//     for 180 s (three of its shortened periods); once the CLI removed its
//     lock, kured took it, drained and rebooted the agent within 188 s, and
//     released 59 s after the node was Ready. That is why Value is kured's
//     wire format rather than one of our own, and why Acquire writes the lock
//     the way kured does.
//  2. Atomic with either actor paused after its last check: yes, both sides
//     are compare-and-swap on the DaemonSet's resourceVersion. The CLI read
//     the DaemonSet (lock free) and paused; kured took the lock 38 s later;
//     the CLI's write from its stale copy was refused — "Error from server
//     (Conflict): ... the object has been modified". That is why writeLock
//     replaces the document it read and never a merge patch. The fake below
//     is a real compare-and-swap server for the same reason: it refuses a
//     stale resourceVersion and ACCEPTS an unconditional one, which is what
//     turns a missing CAS into a visible lost update instead of a passing
//     test.
//  3. The label kubenest.io/auto-reboot=false stops kured on that node
//     promptly (4 s, with the affinity the kured chart now carries) — BUT a
//     node labelled while it held kured's lock left the lock ORPHANED: two
//     minutes later the annotation still named it, the node was still
//     cordoned, and it had not rebooted. With kured's concurrency of 1 an
//     orphaned lock blocks every other node's automatic reboot indefinitely
//     (no --lock-ttl was set). That is why day2.HoldAutomaticReboots releases
//     a lock the held node owns and uncordons it, and never touches a lock
//     another node holds.
//  4. A node that reboots, answers SSH and never becomes Ready KEEPS the lock
//     indefinitely (five minutes after the reboot the lock still named it;
//     with k3s-agent restarted the node was Ready in 2 s and kured released
//     38 s later). That is why Acquire refuses to write a lock without a TTL,
//     why Expired implements kured's own rule, and why the chart sets
//     configuration.lockTtl above limits.timeouts.node-reboot.
//
// The core questions answer yes, so no fallback was taken: automatic reboots
// ship in 1.2.
package interlock

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/sshx"
)

const (
	getDaemonSetCmd = "sudo -n k3s kubectl get daemonset -n kube-system kured -o json"
	replaceCmd      = "sudo -n k3s kubectl replace -f -"
	// conflictStderr is the API server's own wording, as P1 captured it.
	conflictStderr = `Error from server (Conflict): Operation cannot be fulfilled on daemonsets.apps "kured": the object has been modified; please apply your changes to the latest version and try again`
)

// lockTTL is the value the CLI's own hold is given in these tests: what
// LockTTLFor derives from the released bundle (node-drain 15m + node-reboot
// 20m).
const lockTTL = 35 * time.Minute

// kuredAPI is a miniature of the two calls this package makes, with the API
// server's own semantics. It keeps the DaemonSet's resourceVersion and lock
// annotation, serves them on a read, and on a write:
//
//   - refuses a resourceVersion that is not the current one, with the Conflict
//     P1 saw from the CLI's stale copy;
//   - ACCEPTS a write that carries no resourceVersion, because that is what an
//     API server does with an unconditional update — the write wins over
//     whatever arrived first. Accepting it here is deliberate: the failure a
//     missing compare-and-swap must produce is a LOST UPDATE, and a fake that
//     answered "conflict" to a write with no version would hide it.
//
// afterGet lets a test put a competing writer in the gap between a read and
// the write, which is the shape of P1's finding 2.
type kuredAPI struct {
	t        *testing.T
	runner   *componenttest.FakeRunner
	rv       int
	lock     string
	gets     int
	replaces int
	// cordoned is the answer `get node` gives: whether the node was already
	// unschedulable, which the lock's metadata must record.
	cordoned bool
	afterGet func(read int)
}

func newKuredAPI(t *testing.T, rv int, lock string) (*kuredAPI, *componenttest.FakeRunner) {
	t.Helper()
	api := &kuredAPI{t: t, rv: rv, lock: lock}
	// The fake's answer to a write depends on the payload the write streamed —
	// that IS the compare-and-swap — and only the runner sees it, so the runner
	// is wired to the API before the first call.
	api.runner = &componenttest.FakeRunner{Respond: api.respond}
	return api, api.runner
}

func (k *kuredAPI) respond(cmd string) (sshx.Result, error) {
	switch {
	case cmd == getDaemonSetCmd:
		k.gets++
		out := daemonSetJSON(strconv.Itoa(k.rv), k.lock)
		if k.afterGet != nil {
			k.afterGet(k.gets)
		}
		return sshx.Result{Stdout: out}, nil
	case strings.HasPrefix(cmd, "sudo -n k3s kubectl get node "):
		return sshx.Result{Stdout: `{"spec":{"unschedulable":` + strconv.FormatBool(k.cordoned) + `}}`}, nil
	case cmd == replaceCmd:
		k.replaces++
		inputs := k.runner.Inputs()
		if len(inputs) == 0 {
			k.t.Fatal("write recorded no payload")
		}
		var sent struct {
			Metadata struct {
				ResourceVersion string            `json:"resourceVersion"`
				Annotations     map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(inputs[len(inputs)-1], &sent); err != nil {
			k.t.Fatalf("write is not a DaemonSet: %v", err)
		}
		switch sent.Metadata.ResourceVersion {
		case strconv.Itoa(k.rv):
			// The version the caller read.
		case "":
			// No version at all: an unconditional update. The API server
			// accepts these, and so does this fake, because a write that
			// carries no version must fail the test as a LOST UPDATE rather
			// than be politely refused by the harness.
		default:
			return sshx.Result{ExitCode: 1, Stderr: conflictStderr}, nil
		}
		k.rv++
		k.lock = sent.Metadata.Annotations[LockAnnotation]
		return sshx.Result{}, nil
	default:
		k.t.Fatalf("unscripted command: %q", cmd)
		return sshx.Result{}, nil
	}
}

// daemonSetJSON renders the little of kured's DaemonSet this package reads.
// The generation, pod template and status are there so a write can be asserted
// to replace the object that was read rather than rebuild a partial one.
func daemonSetJSON(rv, lock string) string {
	ann := ""
	if lock != "" {
		ann = `,"annotations":{` + strconv.Quote(LockAnnotation) + `:` + strconv.Quote(lock) + `}`
	}
	return `{"apiVersion":"apps/v1","kind":"DaemonSet",` +
		`"metadata":{"name":"kured","namespace":"kube-system","resourceVersion":` + strconv.Quote(rv) + `,"generation":42` + ann + `},` +
		`"spec":{"updateStrategy":{"type":"RollingUpdate"}},"status":{"numberReady":3}}`
}

// lockJSON renders a lock document exactly as kured writes it.
func lockJSON(t *testing.T, v Value) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writeDoc is the single write a call made, checked to be a whole-object
// replace of the document that was read.
type writeDoc struct {
	Metadata struct {
		ResourceVersion string            `json:"resourceVersion"`
		Generation      json.Number       `json:"generation"`
		Annotations     map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec map[string]any `json:"spec"`
}

// singleWrite returns the one write the call made and fails when there was not
// exactly one — a second is a double take, and none at all is a refusal that
// did not happen or a lock nobody took.
func singleWrite(t *testing.T, api *kuredAPI) writeDoc {
	t.Helper()
	inputs := api.runner.Inputs()
	if len(inputs) != 1 {
		t.Fatalf("%d writes, want exactly 1", len(inputs))
	}
	var doc writeDoc
	if err := json.Unmarshal(inputs[0], &doc); err != nil {
		t.Fatalf("write is not the DaemonSet: %v\n%s", err, inputs[0])
	}
	if doc.Metadata.Generation != "42" || doc.Spec["updateStrategy"] == nil {
		t.Errorf("write dropped the fields this package does not name (generation %q, spec %v): a lock write must replace the whole object it read, not rebuild a partial one", doc.Metadata.Generation, doc.Spec)
	}
	return doc
}

// lockOf reads the lock out of a write document.
func (d writeDoc) lockOf(t *testing.T) Value {
	t.Helper()
	raw, ok := d.Metadata.Annotations[LockAnnotation]
	if !ok {
		t.Fatalf("write carries no %s annotation", LockAnnotation)
	}
	var v Value
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("annotation is not kured's lock document: %v", err)
	}
	return v
}

// A free lock is taken, and taken the way kured takes it: kured's own document
// in kured's own annotation, on a whole-object replace carrying the
// resourceVersion of the object that was read.
func TestAcquireTakesAFreeLock(t *testing.T) {
	api, r := newKuredAPI(t, 100, "")
	started := time.Now().UTC()

	held, holder, err := Acquire(context.Background(), r, "n1", lockTTL)
	if err != nil {
		t.Fatal(err)
	}
	if !held || holder != "n1" {
		t.Fatalf("held=%v holder=%q, want the lock taken for n1", held, holder)
	}
	if api.replaces != 1 {
		t.Fatalf("%d writes, want exactly 1", api.replaces)
	}
	doc := singleWrite(t, api)
	lock := doc.lockOf(t)
	if lock.NodeID != "n1" {
		t.Errorf("lock names %q, want n1", lock.NodeID)
	}
	if lock.TTL != lockTTL {
		t.Errorf("lock TTL %v, want %v: the CLI's own hold must expire, or one killed laptop stops every node's reboot for ever", lock.TTL, lockTTL)
	}
	if lock.Created.Before(started.Add(-time.Second)) || lock.Created.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("lock created %s, want the moment it was taken (%s)", lock.Created, started)
	}
	if lock.Metadata.Unschedulable {
		t.Error("lock records the node as unschedulable; it was schedulable when the lock was taken")
	}
	if doc.Metadata.ResourceVersion != "100" {
		t.Errorf("write carried resourceVersion %q, want the 100 it was compared against", doc.Metadata.ResourceVersion)
	}
	// And the DaemonSet now carries that lock, as kured would find it.
	var onServer Value
	if err := json.Unmarshal([]byte(api.lock), &onServer); err != nil {
		t.Fatalf("DaemonSet annotation is not the lock document: %v", err)
	}
	if onServer.NodeID != "n1" {
		t.Errorf("DaemonSet carries %q's lock, want n1's", onServer.NodeID)
	}

	// With no TTL it refuses rather than writing a lock that never expires —
	// P1's finding 4 from the other side.
	_, bareRunner := newKuredAPI(t, 101, "")
	if _, _, err := Acquire(context.Background(), bareRunner, "n1", 0); err == nil || !strings.Contains(err.Error(), "TTL") {
		t.Fatalf("Acquire with no TTL returned %v, want a refusal naming the TTL", err)
	}
	if len(bareRunner.Inputs()) != 0 {
		t.Error("Acquire wrote a lock without a TTL")
	}
}

// A live lock is honoured whoever wrote it — kured's, or another CLI's — and
// the holder is returned so the caller can say who is taking a node down.
// Nothing is written, which is the difference between refusing and losing an
// update.
//
// The lock here carries TTL 0, kured's own way of saying "never expires": P1
// found exactly such a lock still naming its node five minutes after a reboot
// that never came back.
func TestAcquireRefusesWhenAnotherNodeHoldsIt(t *testing.T) {
	api, r := newKuredAPI(t, 100, lockJSON(t, Value{NodeID: "n2", Created: time.Now().UTC().Add(-time.Hour)}))

	held, holder, err := Acquire(context.Background(), r, "n1", lockTTL)
	if err != nil {
		t.Fatal(err)
	}
	if held || holder != "n2" {
		t.Fatalf("held=%v holder=%q, want a refusal naming n2", held, holder)
	}
	if api.replaces != 0 {
		t.Errorf("%d writes: a lock another node holds must never be overwritten", api.replaces)
	}
}

// The conflict case, as P1 observed it from the CLI's side: the write from the
// document we read is REFUSED because the object moved, and the retry decides
// against the state that is there NOW — which is kured holding the lock — and
// writes nothing.
//
// This is where a missing compare-and-swap shows as a lost update: with no
// resourceVersion in the write (or a merge patch, which carries none either)
// the update is unconditional, it wins over the lock kured just took, and
// Acquire reports itself the holder of a lock another node is holding — two
// actors that would then cordon and drain the same cluster.
func TestAcquireRetriesOnResourceVersionConflictAndDoesNotDoubleTake(t *testing.T) {
	api, r := newKuredAPI(t, 100, "")
	api.afterGet = func(read int) {
		if read == 1 {
			// kured takes the lock between our read and our write.
			api.rv++
			api.lock = lockJSON(t, Value{NodeID: "n2", Created: time.Now().UTC(), TTL: lockTTL})
		}
	}

	held, holder, err := Acquire(context.Background(), r, "n1", lockTTL)
	if err != nil {
		t.Fatal(err)
	}
	if held || holder != "n2" {
		t.Fatalf("held=%v holder=%q, want n2: the retry must decide against the lock that is there now, not overwrite it", held, holder)
	}
	if api.gets != 2 {
		t.Errorf("%d reads, want 2: a conflicted write must be followed by a fresh read", api.gets)
	}
	if api.replaces != 1 {
		t.Fatalf("%d writes, want exactly 1: re-sending the document we know is stale is the double take", api.replaces)
	}
	if got := singleWrite(t, api).Metadata.ResourceVersion; got != "100" {
		t.Errorf("write carried resourceVersion %q, want the 100 it was compared against", got)
	}
	var onServer Value
	if err := json.Unmarshal([]byte(api.lock), &onServer); err != nil {
		t.Fatal(err)
	}
	if onServer.NodeID != "n2" {
		t.Errorf("kured's lock was overwritten by %q", onServer.NodeID)
	}
}

// Release removes our own lock — and only ours. kured's own Release refuses
// the same way: the node named in the lock may be mid-reboot, and a lock that
// merely looks stale is not ours to judge.
func TestReleaseOnlyReleasesOurOwnLock(t *testing.T) {
	foreign := lockJSON(t, Value{NodeID: "n2", Created: time.Now().UTC().Add(-time.Hour), TTL: lockTTL})
	api, r := newKuredAPI(t, 100, foreign)

	err := Release(context.Background(), r, "n1")
	if err == nil || !strings.Contains(err.Error(), "n2") {
		t.Fatalf("releasing another node's lock returned %v, want a refusal naming n2", err)
	}
	if api.replaces != 0 {
		t.Errorf("%d writes: another node's lock must be left exactly as it was", api.replaces)
	}
	if api.lock != foreign {
		t.Error("the other node's lock changed")
	}

	// Our own lock goes, and the write is still a whole-object replace of the
	// object that was read.
	mine, r2 := newKuredAPI(t, 100, lockJSON(t, Value{NodeID: "n1", Created: time.Now().UTC().Add(-time.Minute), TTL: lockTTL}))
	if err := Release(context.Background(), r2, "n1"); err != nil {
		t.Fatal(err)
	}
	if mine.replaces != 1 {
		t.Fatalf("%d writes, want exactly 1", mine.replaces)
	}
	doc := singleWrite(t, mine)
	if _, ok := doc.Metadata.Annotations[LockAnnotation]; ok {
		t.Errorf("the lock annotation survived the release")
	}
	if doc.Metadata.ResourceVersion != "100" {
		t.Errorf("release wrote resourceVersion %q, want the 100 it read", doc.Metadata.ResourceVersion)
	}
	if mine.lock != "" {
		t.Errorf("kured's DaemonSet still carries a lock: %q", mine.lock)
	}
}

// A lock whose created + TTL has passed is taken over rather than honoured —
// kured's own expiry rule at 1.23.0. Without it, one node that never comes
// back stops every other node's reboot for ever (P1, finding 4).
//
// The node here is already cordoned, and the lock the CLI writes must say so:
// that field is READ from the node, and it is what stops a later release
// uncordoning a node somebody cordoned on purpose.
func TestExpiredLockIsTakenOver(t *testing.T) {
	api, r := newKuredAPI(t, 100, lockJSON(t, Value{NodeID: "n2", Created: time.Now().UTC().Add(-40 * time.Minute), TTL: lockTTL}))
	api.cordoned = true
	started := time.Now().UTC()

	held, holder, err := Acquire(context.Background(), r, "n1", lockTTL)
	if err != nil {
		t.Fatal(err)
	}
	if !held || holder != "n1" {
		t.Fatalf("held=%v holder=%q, want the expired lock taken over by n1", held, holder)
	}
	lock := singleWrite(t, api).lockOf(t)
	if lock.NodeID != "n1" {
		t.Errorf("lock names %q, want n1", lock.NodeID)
	}
	if lock.Created.Before(started.Add(-time.Second)) {
		t.Errorf("lock carries the EXPIRED holder's created time %s, want the moment it was taken over (%s)", lock.Created, started)
	}
	if lock.TTL != lockTTL {
		t.Errorf("lock TTL %v, want %v", lock.TTL, lockTTL)
	}
	if !lock.Metadata.Unschedulable {
		t.Error("lock records the node as schedulable; it was cordoned before the lock was taken over")
	}
}
