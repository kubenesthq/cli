//go:build e2e

// S10 — "Trouble reaches a human, and silence is noticed" (PLAN-CLOSE-THE-GAP-2026-09.md
// section 4; bead kn-9pgx, plan task T2.9), on a REAL control plane and a REAL
// lab cluster, driven through the real command tree.
//
// THE TEST STARTS ITS OWN RECEIVERS. One HTTP listener on this workstation
// serves two paths under a secret prefix: the webhook receiver (/alert) records
// every alert request and can be made to fail, and the heartbeat receiver
// (/heartbeat) records beats and runs a dead-man's switch that notifies a Go
// channel, the scenario's "test channel", once beats stop for longer than a
// sweep and a half.
//
// WHAT IT ASSERTS, in order:
//
//	planted negative: with no destination configured, `kubenest health` says so
//	  in its first line;
//	planted negative: a destination that resolves to a loopback or a cloud
//	  metadata address is refused by `kubenest alerts add-destination`, and
//	  nothing is stored;
//	the receivers are configured with `kubenest alerts add-destination` and
//	  `kubenest alerts set-heartbeat`, and the first-line notice goes away;
//	the healthy cluster: every group reads ok on fresh evidence or says why it is
//	  not applicable, `kubenest health` exits 0 within 30 s of a report, its
//	  --json carries the table's clusters, states and reasons, and with an empty
//	  outbox the live worker keeps the heartbeat beating;
//	then, one at a time with recovery in between, each of the plan's six faults
//	  produces ONE alert and ONE resolve on its check at the receiver, in order,
//	  with one stable incident_id,
//	  each within three delivery cycles of the verdict behind it (a retry of the
//	  same intent is allowed: S10 does not prove exactly-once delivery):
//	    stop the agent                  reporting/CLUSTER_SILENT, critical
//	    age the restore drill           backup/RESTORE_DRILL_STALE
//	    break the S3 snapshot upload    datastore/DATASTORE_SNAPSHOT_FAILED while
//	                                    the local snapshot stays fresh
//	    fail the receiver, restart the backend, restore the receiver: the backlog's
//	                                    size and age are visible while it fails and
//	                                    survive the restart, and the alert arrives
//	                                    after the receiver recovers
//	    stop only the idle delivery worker: the heartbeat stops within the
//	                                    delivery-cycle deadline while the sweeper
//	                                    keeps sweeping, and the switch notifies
//	    stop the backend entirely       no beat after the stop; the switch notifies
//	between the drill and S3 faults, a planted collector failure makes backup
//	  unknown/COLLECTION_FAILED with its reason and exits 3; agent/AGENT_ERRORS
//	  can accompany it without counting as a second fault alert;
//	no request reached the receivers malformed, and the gate ran under 60 minutes.
//
// WHERE THE PLAN AND THE PRODUCT DISAGREE (measured at kubenest-backend d1e9029,
// op3 d07b8e5, kubenest-cli 99b73c4):
//
//   - THERE IS NO DISK BRANCH. Bundle 1.2 declares no data-disk usage threshold
//     (the health block of platform-1.2.yaml) and the operator measures no disk
//     usage. The only disk verdict is the kubelet's DiskPressure flag
//     (nodes/NODE_DISK_PRESSURE, app/services/health/evaluate.py), and reaching
//     it means the kubelet evicting the cluster's workloads, the agent with
//     them. The plan's "fill the data disk" step left the 1.2 acceptance on
//     2026-11-13 and returns with the capability in 1.3 (kn-1f91).
//   - THE DELIVERY WORKER IS NOT A PROCESS OF ITS OWN: it is an asyncio task in
//     the backend (app/main.py), so nothing outside stops it alone. This gate
//     restarts the backend with HEALTH_DELIVERY_INTERVAL_SECONDS far past the
//     delivery-cycle deadline: the worker runs one cycle and never another,
//     while the sweeper keeps sweeping. That is the "idle worker that stopped"
//     the heartbeat's condition exists to catch
//     (app/services/health/heartbeat.py cycle_completed_within_deadline).
//   - A PLAIN-HTTP RECEIVER MUST BE ALLOW-LISTED and the chart offers no value
//     for it: this gate sets HEALTH_OUTBOUND_ALLOWED_INTERNAL_ADDRESSES to the
//     receiver's host:port on the backend Deployment and restores it afterwards.
//   - A SILENT CLUSTER READS critical (reporting/CLUSTER_SILENT,
//     app/services/health/sweeper.py), not unknown, so the
//     KUBENEST_GATE_SILENT_CLUSTER arm of health_view_test.go, which expects
//     unknown, fails against this backend.
//   - A BACKEND OUTAGE LONGER THAN THE SILENCE WINDOW raises CLUSTER_SILENT at
//     the first sweep after the restart: the sweep has no start-up grace. The
//     backend-stop branch logs the incidents it saw and requires each to resolve.
//
// WHAT A HARDWARE RUN NEEDS
//
//   - the candidate control plane (KUBENEST_CONTROL_PLANE, an instance
//     administrator's KUBENEST_CLI_TOKEN, KUBENEST_CONTROL_PLANE_CA) with NO
//     alert destination and NO heartbeat configured;
//   - one registered lab cluster on its OWN host, not the control plane's
//     (KUBENEST_GATE_CLUSTER; its server is KUBENEST_LAB_SERVER_IP or
//     KUBENEST_LAB_NODE1_IP), at bundle 1.2 with a backup target, a completed
//     `kubenest backup now` and a PASSED restore drill;
//   - SSH (KUBENEST_LAB_SSH_KEY, KUBENEST_LAB_SSH_USER) to that server and to the
//     control plane's host, which is KUBENEST_GATE_CP_HOST or is read from a
//     KUBENEST_CONTROL_PLANE of the form https://api.<ipv4>.sslip.io;
//   - KUBENEST_GATE_RECEIVER_HOST=<host>:<port>, where the control plane's
//     backend reaches this workstation's receivers. In the lab that is a
//     reverse tunnel on a lab host's PUBLIC address, as the S4 and S6 sentinel
//     uses: `gateway-ports.sh on` on that host, the port open in the lab
//     firewall, and `ssh -N -R 0.0.0.0:<port>:127.0.0.1:<port> ubuntu@<host>`.
//     The receivers listen on <port> here.
//
// Run from the umbrella workspace:
//
//	source lab/hetzner/.lab-env-<lab>.sh
//	export KUBENEST_CONTROL_PLANE=https://api.<cp-ip>.sslip.io KUBENEST_CLI_TOKEN=knp_...
//	export KUBENEST_CONTROL_PLANE_CA=<pem> KUBENEST_GATE_CLUSTER=<cluster>
//	export KUBENEST_GATE_RECEIVER_HOST=<lab-host-public-ip>:18090
//	cd kubenest-cli && go test -tags e2e -count=1 -v -timeout 120m ./e2e/ -run TestHealthAlertingS10
//
// PASS LIMITS (plan section 4, bead kn-9pgx): exit codes exact; `kubenest health`
// exits 0 within 30 s of the last report; each alert and resolve within three
// delivery cycles of its verdict; the heartbeat stops within two sweep
// intervals; one alert and one resolve per check for the six faults; the whole gate under 60
// minutes. The intervals are the backend's defaults, which the chart does not
// override (app/core/config.py).
//
// WHAT THIS DOES NOT PROVE: exactly-once delivery; that a dead-man's-switch
// service notifies a person (the receiver here is the switch); that a
// slack-format destination renders; behaviour with more than one backend
// replica.
package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/sshx"
)

const (
	// The backend's defaults; the chart sets none of them (app/core/config.py).
	s10SweepInterval    = 60 * time.Second  // HEALTH_SWEEP_INTERVAL_SECONDS
	s10DeliveryInterval = 15 * time.Second  // HEALTH_DELIVERY_INTERVAL_SECONDS
	s10CycleDeadline    = 120 * time.Second // HEALTH_DELIVERY_CYCLE_DEADLINE_SECONDS, two sweeps
	s10BacklogCritical  = 600 * time.Second // HEALTH_BACKLOG_CRITICAL_AGE_SECONDS
	s10BackoffCeiling   = 300 * time.Second // delivery.py BACKOFF_CEILING_SECONDS
	s10ReportInterval   = 60 * time.Second  // op3 pkg/websocket/health.go
	// health.reporting.missed-reports-before-alert (3) x the report interval.
	s10SilenceWindow = 3 * s10ReportInterval
	// platform-1.2.yaml: health.backup.max-restore-drill-age, health.datastore.max-snapshot-age.
	s10DrillMaxAge    = 336 * time.Hour
	s10SnapshotMaxAge = 3 * time.Hour
	// A beat follows every sweep, so a sweep and a half of silence is a missed beat.
	s10DeadManDeadline = 90 * time.Second
	// A healthy verdict rides on a report at most three report intervals old.
	s10EvidenceAge = 3 * s10ReportInterval
	// Three delivery cycles, plus one poll of slack.
	s10DeliveryLimit = 3*s10DeliveryInterval + s10Poll
	s10Poll          = 5 * time.Second

	s10Namespace      = "kubenest-system"
	s10Backend        = "kubenest-cp-backend"                             // the control-plane chart, release kubenest-cp
	s10Agent          = "operator-kubenest-operator-2-controller-manager" // pkg/component/agent
	s10AllowEnv       = "HEALTH_OUTBOUND_ALLOWED_INTERNAL_ADDRESSES"
	s10DeliveryEnv    = "HEALTH_DELIVERY_INTERVAL_SECONDS"
	s10DrillNamespace = "velero"
	s10DrillResult    = "kubenest-restore-drill-result" // op3 pkg/restoredrill/result.go
	s10DrillConfig    = "kubenest-restore-drill"        // pkg/backup/drill_config.go
	s10SnapshotSecret = "kubenest-etcd-snapshot-s3"     // kube-system; pkg/backup/datastore.go

	s10AlertPath     = "/alert"
	s10HeartbeatPath = "/heartbeat"
	s10AlertKind     = "kubenest.fleet_health.alert"
	s10HeartbeatKind = "kubenest.control_plane.heartbeat"
	s10Firing        = "firing"
	s10Resolved      = "resolved"
)

// s10Event is one webhook request as the receiver recorded it.
type s10Event struct {
	Incident  string
	State     string
	Cluster   string
	Check     string
	Reason    string
	At        time.Time
	Delivered bool
}

// s10Payload is the body the control plane posts: the alert intent
// (kubenest-backend app/services/health/delivery.py build_payload) or the beat
// (app/services/health/heartbeat.py HEARTBEAT_PAYLOAD).
type s10Payload struct {
	Kind       string `json:"kind"`
	Event      string `json:"event"`
	IncidentID string `json:"incident_id"`
	ClusterID  string `json:"cluster_id"`
	Check      string `json:"check"`
	ReasonCode string `json:"reason_code"`
}

// s10Receiver is the webhook receiver and the heartbeat receiver, one HTTP
// handler with two paths under a secret prefix: a request outside the prefix is
// a stranger on the open port and is neither answered nor recorded. Every alert
// request is recorded, including the ones a failing receiver refuses, because
// the backend retries those and S10 asserts what was DELIVERED.
type s10Receiver struct {
	mu        sync.Mutex
	prefix    string
	events    []s10Event
	beats     []time.Time
	malformed []string
	failing   bool
	dead      *s10DeadMan
}

func newS10Receiver(deadline time.Duration) *s10Receiver {
	return &s10Receiver{dead: &s10DeadMan{deadline: deadline}}
}

func (r *s10Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !strings.HasPrefix(req.URL.Path, r.prefix) {
		http.NotFound(w, req)
		return
	}
	path := strings.TrimPrefix(req.URL.Path, r.prefix)
	body, _ := io.ReadAll(io.LimitReader(req.Body, 1<<16))
	var p s10Payload
	parsed := json.Unmarshal(body, &p) == nil
	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case path == s10HeartbeatPath && parsed && p.Kind == s10HeartbeatKind:
		r.beats = append(r.beats, now)
		r.dead.beat(now)
		w.WriteHeader(http.StatusOK)
	case path == s10AlertPath && parsed && p.Kind == s10AlertKind && p.IncidentID != "" && p.Event != "":
		r.events = append(r.events, s10Event{
			Incident: p.IncidentID, State: p.Event, Cluster: p.ClusterID, Check: p.Check,
			Reason: p.ReasonCode, At: now, Delivered: !r.failing,
		})
		if r.failing {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		r.malformed = append(r.malformed, path+" "+strings.TrimSpace(string(body)))
		http.Error(w, "not a KubeNest alert or heartbeat", http.StatusBadRequest)
	}
}

func (r *s10Receiver) setFailing(failing bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failing = failing
}

func (r *s10Receiver) eventsFrom(n int) []s10Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n >= len(r.events) {
		return nil
	}
	return append([]s10Event(nil), r.events[n:]...)
}

func (r *s10Receiver) beatsAfter(at time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, b := range r.beats {
		if b.After(at) {
			n++
		}
	}
	return n
}

func (r *s10Receiver) malformedRequests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.malformed...)
}

// s10OneAlertOneResolve reads one fault's deliveries for one cluster: exactly one
// incident, its firing before its resolve, and nothing after the resolve. A
// request the receiver refused is not a delivery, and a repeat of the same
// (incident, state) is a retry, which S10 allows (it does not prove
// exactly-once delivery).
func s10OneAlertOneResolve(events []s10Event, cluster string) (string, error) {
	incident := ""
	fired, resolved := false, false
	for _, e := range events {
		if !e.Delivered || e.Cluster != cluster {
			continue
		}
		if incident == "" {
			incident = e.Incident
		} else if e.Incident != incident {
			return "", fmt.Errorf("a second incident %s arrived after %s: one fault must produce one alert", e.Incident, incident)
		}
		switch e.State {
		case s10Firing:
			if resolved {
				return "", fmt.Errorf("incident %s fired again after its resolve: that is a second alert", incident)
			}
			fired = true
		case s10Resolved:
			if !fired {
				return "", fmt.Errorf("incident %s was resolved before it fired", incident)
			}
			resolved = true
		default:
			return "", fmt.Errorf("incident %s arrived with state %q, which is neither %s nor %s", incident, e.State, s10Firing, s10Resolved)
		}
	}
	switch {
	case incident == "":
		return "", fmt.Errorf("no alert reached the receiver for cluster %s", cluster)
	case !resolved:
		return "", fmt.Errorf("incident %s fired and never received its resolve", incident)
	}
	return incident, nil
}

// Count the fault's check independently: another check can alert during the
// same sweep without becoming a second incident for this fault.
func s10OneAlertOneResolveForCheck(events []s10Event, cluster, check string) (string, error) {
	var selected []s10Event
	for _, e := range events {
		if e.Check == check {
			selected = append(selected, e)
		}
	}
	return s10OneAlertOneResolve(selected, cluster)
}

func s10SelectDelivered(events []s10Event, cluster, state, incident, check string) (s10Event, bool) {
	for _, e := range events {
		if e.Delivered && e.Cluster == cluster && e.State == state && e.Check == check && (incident == "" || e.Incident == incident) {
			return e, true
		}
	}
	return s10Event{}, false
}

// s10Unresolved names, in firing order, every incident for one cluster with a
// delivered firing and no delivered resolve.
func s10Unresolved(events []s10Event, cluster string) []string {
	open := map[string]bool{}
	var order []string
	for _, e := range events {
		if !e.Delivered || e.Cluster != cluster {
			continue
		}
		switch e.State {
		case s10Firing:
			if _, seen := open[e.Incident]; !seen {
				order = append(order, e.Incident)
			}
			open[e.Incident] = true
		case s10Resolved:
			open[e.Incident] = false
		}
	}
	var ids []string
	for _, id := range order {
		if open[id] {
			ids = append(ids, id)
		}
	}
	return ids
}

// s10Missed is the dead-man's switch's notification.
type s10Missed struct {
	LastBeat time.Time
	Noticed  time.Time
}

// s10DeadMan is the heartbeat receiver's switch: once beats have started, a
// silence longer than deadline notifies once, and the next beat re-arms it.
type s10DeadMan struct {
	mu       sync.Mutex
	deadline time.Duration
	last     time.Time
	notified bool
}

func (d *s10DeadMan) beat(at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if at.After(d.last) {
		d.last = at
	}
	d.notified = false
}

func (d *s10DeadMan) check(now time.Time) (s10Missed, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last.IsZero() || d.notified || now.Sub(d.last) <= d.deadline {
		return s10Missed{}, false
	}
	d.notified = true
	return s10Missed{LastBeat: d.last, Noticed: now}, true
}

// s10CPHost is the control plane's host when its URL is the lab's
// https://api.<ipv4>.sslip.io name, and "" for any other URL.
func s10CPHost(controlPlaneURL string) string {
	u, err := url.Parse(controlPlaneURL)
	if err != nil {
		return ""
	}
	host := strings.TrimSuffix(strings.TrimPrefix(u.Hostname(), "api."), ".sslip.io")
	if ip := net.ParseIP(host); ip == nil || ip.To4() == nil || host == u.Hostname() {
		return ""
	}
	return host
}

// s10BackdateDrill moves a passed restore-drill result's completion time and
// nothing else, so the operator still validates it and the backend judges only
// its age (RESTORE_DRILL_STALE past health.backup.max-restore-drill-age).
func s10BackdateDrill(raw string, completedAt time.Time) (string, error) {
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return "", fmt.Errorf("the restore-drill result is not JSON: %w", err)
	}
	if result["status"] != "passed" {
		return "", fmt.Errorf("the restore-drill result is %v, not passed: only a passed drill can be aged into RESTORE_DRILL_STALE", result["status"])
	}
	result["completed_at"] = completedAt.UTC().Format(time.RFC3339)
	out, err := json.Marshal(result)
	return string(out), err
}

// s10InvalidateDrill removes a passed result's verification, which the
// operator's own validation refuses (op3 pkg/restoredrill/result.go validate),
// so the backup collector fails and reports collection_error.
func s10InvalidateDrill(raw string) (string, error) {
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return "", fmt.Errorf("the restore-drill result is not JSON: %w", err)
	}
	delete(result, "verification")
	out, err := json.Marshal(result)
	return string(out), err
}

// s10RequiredProtection can never read not_applicable (kn-9pgx, plan 7.7).
var s10RequiredProtection = []string{"backup", "os_patching"}

// s10HealthProblems is S10's healthy-cluster rule: every group reads ok on a
// report younger than maxEvidenceAge and says when its evidence was measured, a
// group that is not applicable says why, and required protection is present
// and never not applicable. Freshness is the report's arrival (received_at),
// not the measurement time: a healthy backup group is measured at its last
// backup or drill, hours ago (evaluate.py measurement_time_for).
func s10HealthProblems(d api.ClusterHealthDetail, now time.Time, maxEvidenceAge time.Duration) []string {
	var problems []string
	seen := map[string]bool{}
	for _, c := range d.Checks {
		seen[c.Check] = true
		switch c.Status {
		case "ok":
			rawReceived, _ := c.Evidence["received_at"].(string)
			received, err := time.Parse(time.RFC3339, rawReceived)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s reads ok with no report time", c.Check))
			} else if age := now.Sub(received); age > maxEvidenceAge {
				problems = append(problems, fmt.Sprintf("%s reads ok on a report %s old (limit %s)", c.Check, age.Round(time.Second), maxEvidenceAge))
			}
			rawMeasured, _ := c.Evidence["measured_at"].(string)
			if _, err := time.Parse(time.RFC3339, rawMeasured); err != nil {
				problems = append(problems, fmt.Sprintf("%s reads ok with no measurement time", c.Check))
			}
		case "not_applicable", "unsupported":
			if slices.Contains(s10RequiredProtection, c.Check) {
				problems = append(problems, fmt.Sprintf("%s is required protection and reads %s: %s", c.Check, c.Status, c.Message))
			} else if strings.TrimSpace(c.Message) == "" {
				problems = append(problems, fmt.Sprintf("%s reads %s without saying why", c.Check, c.Status))
			}
		default:
			problems = append(problems, fmt.Sprintf("%s reads %s (%s): %s", c.Check, c.Status, c.ReasonCode, c.Message))
		}
	}
	for _, name := range s10RequiredProtection {
		if !seen[name] {
			problems = append(problems, fmt.Sprintf("the cluster reports no %s group", name))
		}
	}
	return problems
}

// ---------------------------------------------------------------- the gate

// s10Run is the gate's handle on the lab.
type s10Run struct {
	env          gateEnv
	client       *api.Client
	orgID        string
	clusterID    string
	recv         *s10Receiver
	base         string // the receivers' URL prefix, as the control plane reaches it
	missed       <-chan s10Missed
	cp           *sshx.Client // the control plane's host
	server       *sshx.Client // the faulted cluster's server
	configuredAt time.Time
}

func TestHealthAlertingS10(t *testing.T) {
	started := time.Now()
	env := gateEnvironment(t)
	advertised := os.Getenv("KUBENEST_GATE_RECEIVER_HOST")
	if advertised == "" {
		t.Skip("KUBENEST_GATE_RECEIVER_HOST is not set: the control plane must reach this workstation's receivers at <host>:<port> (see this file's header)")
	}
	if _, _, err := net.SplitHostPort(advertised); err != nil {
		t.Fatalf("KUBENEST_GATE_RECEIVER_HOST=%q is not <host>:<port>: %v", advertised, err)
	}
	cpHost := envOr("KUBENEST_GATE_CP_HOST", s10CPHost(env.controlPlane))
	if cpHost == "" {
		t.Skip("KUBENEST_GATE_CP_HOST is not set and KUBENEST_CONTROL_PLANE is not https://api.<ipv4>.sslip.io: the gate restarts and stops the control plane's backend over SSH")
	}
	if cpHost == env.server {
		t.Fatalf("the faulted cluster's server %s is the control plane's host: its faults would take the control plane down with them; use a cluster on its own host", env.server)
	}

	s := &s10Run{env: env, client: s10Session(t, env)}
	s.recv, s.base, s.missed = s10StartReceivers(t, advertised)
	s.cp = s10Dial(t, env, cpHost)
	s.server = s10Dial(t, env, env.server)
	s.findCluster(t)
	if a := s.alerting(t); a.Destinations != 0 || a.HeartbeatConfigured {
		t.Fatalf("this gate needs an instance with no alert destination and no heartbeat, and found destinations=%d heartbeat=%t: the first planted negative reads the unconfigured first line, and a configured heartbeat URL cannot be read back to restore it. Remove them first (`kubenest alerts remove-destination --id ...`; the heartbeat has no CLI clear: DELETE /api/v1/instance/heartbeat)",
			a.Destinations, a.HeartbeatConfigured)
	}

	t.Run("with no destination configured, kubenest health says so in its first line", func(t *testing.T) {
		out, _ := healthGateRun(t, "health")
		first := healthGateFirstLine(out)
		lower := strings.ToLower(first)
		if healthGateLooksLikeRow(first) || !strings.Contains(lower, "destination") || !strings.Contains(lower, "heartbeat") {
			t.Errorf("with no destination and no heartbeat the first line is %q:\n%s", first, out)
		}
	})

	// https on purpose: a plaintext URL is refused as plaintext before any
	// address is resolved (app/services/health/outbound.py), which would pass
	// this check for the wrong reason.
	t.Run("a destination that resolves to a loopback or cloud-metadata address is refused", func(t *testing.T) {
		for _, tc := range []struct{ url, want string }{
			{"https://127.0.0.1.sslip.io/kubenest-s10", "loopback"},
			{"https://169.254.169.254/latest/meta-data/", "metadata"},
			{"https://169.254.169.254.sslip.io/kubenest-s10", "metadata"},
		} {
			var out bytes.Buffer
			err := runCLI(&out, "alerts", "add-destination", "--url", tc.url)
			if err == nil {
				if id := s10Field(out.String(), "id:"); id != "" {
					_ = s.client.RemoveAlertDestination(context.Background(), id)
				}
				t.Errorf("%s was accepted as an alert destination", tc.url)
				continue
			}
			if msg := err.Error() + out.String(); !strings.Contains(msg, tc.want) {
				t.Errorf("%s was refused without naming the %s address: %s", tc.url, tc.want, msg)
			}
		}
		if a := s.alerting(t); a.Destinations != 0 {
			t.Errorf("a refused destination was stored: %d destination(s)", a.Destinations)
		}
	})

	s.configureReceivers(t)

	t.Run("the healthy cluster reads ok on fresh evidence and a live idle worker keeps the heartbeat", s.healthy)
	t.Run("stop the agent", s.stopTheAgent)
	t.Run("let the restore drill age past its limit", s.ageTheDrill)
	t.Run("a collector that fails reports unknown with its reason and kubenest health exits 3", s.failACollector)
	t.Run("break the S3 upload of datastore snapshots", s.breakTheSnapshotUpload)
	t.Run("fail the webhook receiver, restart the backend, restore the receiver", s.failTheReceiver)
	t.Run("stop only the idle alert delivery worker", s.stopTheIdleWorker)
	t.Run("stop the backend entirely", s.stopTheBackend)

	if bad := s.recv.malformedRequests(); len(bad) > 0 {
		t.Errorf("%d request(s) reached the receivers malformed: %q", len(bad), bad)
	}
	if elapsed := time.Since(started); elapsed > 60*time.Minute {
		t.Errorf("the gate took %s, over its 60-minute pass limit", elapsed.Round(time.Second))
	} else {
		t.Logf("the gate took %s", elapsed.Round(time.Second))
	}
}

// s10Session points the real command tree at the gate's control plane through
// an isolated HOME, as TestFleetHealthGate does, and returns the API client.
func s10Session(t *testing.T, env gateEnv) *api.Client {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := config.Save(&config.Config{ControlPlaneURL: env.controlPlane, ControlPlaneCA: string(env.controlPlaneCA)}); err != nil {
		t.Fatalf("writing the gate's config: %v", err)
	}
	creds, err := config.LoadCredentials()
	if err != nil {
		t.Fatalf("loading the credential store: %v", err)
	}
	creds.Set(env.controlPlane, env.token)
	if err := config.SaveCredentials(creds); err != nil {
		t.Fatalf("storing the gate's token: %v", err)
	}
	options := []api.Option{api.WithToken(env.token)}
	if len(env.controlPlaneCA) > 0 {
		options = append(options, api.WithCACert(env.controlPlaneCA))
	}
	client, err := api.New(env.controlPlane, options...)
	if err != nil {
		t.Fatalf("building the API client: %v", err)
	}
	return client
}

// s10StartReceivers listens on the port KUBENEST_GATE_RECEIVER_HOST names, because
// the tunnel the lab uses was opened for it, and runs the switch's watchdog.
func s10StartReceivers(t *testing.T, advertised string) (*s10Receiver, string, <-chan s10Missed) {
	t.Helper()
	_, port, _ := net.SplitHostPort(advertised)
	listener, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", port))
	if err != nil {
		t.Fatalf("binding the receivers on port %s: %v", port, err)
	}
	r := newS10Receiver(s10DeadManDeadline)
	r.prefix = "/kubenest-s10-" + randomSuffix()
	server := &http.Server{Handler: r, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	missed := make(chan s10Missed, 8)
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				if m, ok := r.dead.check(now); ok {
					select {
					case missed <- m:
					default:
					}
				}
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		_ = server.Close()
	})
	return r, "http://" + advertised + r.prefix, missed
}

func s10Dial(t *testing.T, env gateEnv, address string) *sshx.Client {
	t.Helper()
	c := dialLabHost(t, env, address)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// s10Field reads a "label value" line from a command's output.
func s10Field(out, label string) string {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), label); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func s10WaitFor(t *testing.T, within time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		ok, observed := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s; last observation: %s", within, what, observed)
		}
		time.Sleep(s10Poll)
	}
}

func s10CheckIn(d api.ClusterHealthDetail, name string) (api.HealthCheck, bool) {
	for _, c := range d.Checks {
		if c.Check == name {
			return c, true
		}
	}
	return api.HealthCheck{}, false
}

func s10DetailTime(detail map[string]any, key string) time.Time {
	raw, _ := detail[key].(string)
	at, _ := time.Parse(time.RFC3339, raw)
	return at
}

func (s *s10Run) findCluster(t *testing.T) {
	t.Helper()
	orgs, err := s.client.ListOrgs(context.Background())
	if err != nil {
		t.Fatalf("reading the organisations: %v", err)
	}
	for _, org := range orgs {
		fleet, err := s.client.FleetHealth(context.Background(), org.ID)
		if err != nil {
			t.Fatalf("reading %s's fleet: %v", org.ID, err)
		}
		for _, c := range fleet.Clusters {
			if c.ClusterName == s.env.cluster {
				s.orgID, s.clusterID = org.ID, c.ClusterID
				return
			}
		}
	}
	t.Fatalf("no organisation this token sees holds a cluster named %s (KUBENEST_GATE_CLUSTER)", s.env.cluster)
}

func (s *s10Run) alerting(t *testing.T) api.InstanceAlerting {
	t.Helper()
	a, err := s.client.InstanceAlerting(context.Background())
	if err != nil {
		t.Fatalf("reading the instance's alerting status: %v", err)
	}
	return a
}

func (s *s10Run) detail(t *testing.T) api.ClusterHealthDetail {
	t.Helper()
	d, err := s.client.ClusterHealthDetail(context.Background(), s.clusterID)
	if err != nil {
		t.Fatalf("reading %s's health: %v", s.env.cluster, err)
	}
	return d
}

func (s *s10Run) kubectl(t *testing.T, on *sshx.Client, args string) string {
	t.Helper()
	out, err := k3s.Kubectl(context.Background(), on, args)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return strings.TrimSpace(out)
}

// undo runs a kubectl that puts a fault back. It reports rather than stops,
// because it runs from cleanups too.
func (s *s10Run) undo(t *testing.T, on *sshx.Client, args string) {
	t.Helper()
	if _, err := k3s.Kubectl(context.Background(), on, args); err != nil {
		t.Errorf("undoing a fault: %v", err)
	}
}

// patch applies a merge patch read from stdin, so no JSON passes through a shell.
func (s *s10Run) patch(on *sshx.Client, args, patch string) error {
	res, err := on.RunInput(context.Background(), "sudo -n k3s kubectl "+args+" --type merge --patch-file /dev/stdin", strings.NewReader(patch))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("kubectl %s: exit %d: %s", args, res.ExitCode, firstLineOfE2E(res.Stderr))
	}
	return nil
}

// backendSettled waits for the backend's rollout and for its API to answer.
func (s *s10Run) backendSettled(t *testing.T) time.Time {
	t.Helper()
	s.kubectl(t, s.cp, "-n "+s10Namespace+" rollout status deployment/"+s10Backend+" --timeout=10m")
	s10WaitFor(t, 5*time.Minute, "the control plane's API to answer after the backend restarted", func() (bool, string) {
		_, err := s.client.InstanceAlerting(context.Background())
		return err == nil, fmt.Sprint(err)
	})
	return time.Now()
}

// setBackendEnv sets one env var on the backend Deployment, or removes it when
// value is empty, and waits for the restart.
func (s *s10Run) setBackendEnv(t *testing.T, name, value string) time.Time {
	t.Helper()
	assignment := name + "-"
	if value != "" {
		assignment = name + "='" + value + "'"
	}
	s.kubectl(t, s.cp, "-n "+s10Namespace+" set env deployment/"+s10Backend+" "+assignment)
	return s.backendSettled(t)
}

// configureReceivers allow-lists the receivers' plaintext address on the
// backend, then adds them through the real CLI. Every change is undone in a
// cleanup, in reverse order.
func (s *s10Run) configureReceivers(t *testing.T) {
	t.Helper()
	u, err := url.Parse(s.base)
	if err != nil {
		t.Fatal(err)
	}
	previous := s.kubectl(t, s.cp, fmt.Sprintf(`-n %s get deployment/%s -o jsonpath='{.spec.template.spec.containers[*].env[?(@.name=="%s")].value}'`,
		s10Namespace, s10Backend, s10AllowEnv))
	allowed := u.Host
	if previous != "" {
		allowed = previous + "," + u.Host
	}
	t.Cleanup(func() {
		assignment := s10AllowEnv + "-"
		if previous != "" {
			assignment = s10AllowEnv + "='" + previous + "'"
		}
		s.undo(t, s.cp, "-n "+s10Namespace+" set env deployment/"+s10Backend+" "+assignment)
	})
	s.setBackendEnv(t, s10AllowEnv, allowed)

	out := runCLIStrict(t, "adding the S10 receiver as the instance's alert destination",
		"alerts", "add-destination", "--url", s.base+s10AlertPath)
	id := s10Field(out, "id:")
	if id == "" {
		t.Fatalf("add-destination printed no id:\n%s", out)
	}
	t.Cleanup(func() {
		if err := s.client.RemoveAlertDestination(context.Background(), id); err != nil {
			t.Errorf("removing the S10 alert destination %s: %v", id, err)
		}
	})
	runCLIStrict(t, "setting the S10 receiver as the instance's heartbeat",
		"alerts", "set-heartbeat", "--url", s.base+s10HeartbeatPath)
	t.Cleanup(func() {
		if err := s.client.ClearHeartbeat(context.Background()); err != nil {
			t.Errorf("clearing the S10 heartbeat: %v", err)
		}
	})
	s.configuredAt = time.Now()
	if a := s.alerting(t); a.Destinations != 1 || !a.HeartbeatConfigured {
		t.Fatalf("after configuring, the alerting status is destinations=%d heartbeat=%t", a.Destinations, a.HeartbeatConfigured)
	}
	if out, _ := healthGateRun(t, "health"); !healthGateLooksLikeRow(healthGateFirstLine(out)) {
		t.Errorf("with a destination and a heartbeat configured the first line is still a notice: %q", healthGateFirstLine(out))
	}
}

// requireHealthy waits until the faulted cluster reads healthy on fresh
// evidence and no incident for it is left open at the receiver.
func (s *s10Run) requireHealthy(t *testing.T) {
	t.Helper()
	s10WaitFor(t, 10*time.Minute, s.env.cluster+" to read healthy on fresh evidence with no incident open", func() (bool, string) {
		d, err := s.client.ClusterHealthDetail(context.Background(), s.clusterID)
		if err != nil {
			return false, err.Error()
		}
		problems := s10HealthProblems(d, time.Now(), s10EvidenceAge)
		if open := s10Unresolved(s.recv.eventsFrom(0), s.clusterID); len(open) > 0 {
			problems = append(problems, fmt.Sprintf("incident(s) %v fired and are not resolved", open))
		}
		return len(problems) == 0, strings.Join(problems, "; ")
	})
}

func (s *s10Run) requireEmptyOutbox(t *testing.T) {
	t.Helper()
	s10WaitFor(t, 2*time.Minute, "the alert outbox to drain", func() (bool, string) {
		a, err := s.client.InstanceAlerting(context.Background())
		if err != nil {
			return false, err.Error()
		}
		return a.Backlog.Pending == 0, fmt.Sprintf("%+v", a.Backlog)
	})
}

// awaitAlert waits for a DELIVERED request on the faulted check and cluster
// (and incident, when given) from index from on. Meanwhile it watches the
// check, to time the verdict the delivery follows.
func (s *s10Run) awaitAlert(t *testing.T, from int, state, incident, check string, within time.Duration) (s10Event, time.Time) {
	t.Helper()
	var got s10Event
	var verdictAt time.Time
	s10WaitFor(t, within, fmt.Sprintf("a delivered %s for %s's %s check", state, s.env.cluster, check), func() (bool, string) {
		if verdictAt.IsZero() {
			if d, err := s.client.ClusterHealthDetail(context.Background(), s.clusterID); err == nil {
				c, present := s10CheckIn(d, check)
				failing := present && c.Status != "ok" && c.Status != "not_applicable" && c.Status != "unsupported"
				if failing == (state == s10Firing) {
					verdictAt = time.Now()
				}
			}
		}
		received := s.recv.eventsFrom(from)
		if e, ok := s10SelectDelivered(received, s.clusterID, state, incident, check); ok {
			got = e
			return true, ""
		}
		return false, fmt.Sprintf("%d request(s) received since the fault", len(received))
	})
	return got, verdictAt
}

func s10DeliveredWithinLimit(t *testing.T, what string, e s10Event, verdictAt time.Time) {
	t.Helper()
	if verdictAt.IsZero() {
		t.Logf("%s: the verdict and the delivery fell inside one poll, so the latency is under %s", what, s10Poll)
		return
	}
	latency := e.At.Sub(verdictAt)
	t.Logf("%s (incident %s, %s/%s) arrived %s after the verdict", what, e.Incident, e.Check, e.Reason, latency.Round(time.Second))
	if latency > s10DeliveryLimit {
		t.Errorf("%s arrived %s after its verdict, past the three-delivery-cycle limit of %s", what, latency.Round(time.Second), s10DeliveryLimit)
	}
}

// s10Fault is one fault branch: what it must raise, how to cause it and undo
// it, and what to observe while it holds. inject registers its own cleanup.
type s10Fault struct {
	check                     string
	reasons                   []string
	companionCheck            string // planted negatives may also report an agent error
	inject                    func(t *testing.T)
	during                    func(t *testing.T, alert s10Event)
	recover                   func(t *testing.T)
	fireWithin, resolveWithin time.Duration
	fireWithinAfterInject     func() time.Duration
}

func (s *s10Run) fault(t *testing.T, f s10Fault) {
	t.Helper()
	s.requireHealthy(t)
	from := len(s.recv.eventsFrom(0))
	f.inject(t)
	fireWithin := f.fireWithin
	if f.fireWithinAfterInject != nil {
		fireWithin = f.fireWithinAfterInject()
	}
	firing, firedAt := s.awaitAlert(t, from, s10Firing, "", f.check, fireWithin)
	if firing.Check != f.check || !slices.Contains(f.reasons, firing.Reason) {
		t.Errorf("the alert is %s/%s, want %s with one of %v", firing.Check, firing.Reason, f.check, f.reasons)
	}
	s10DeliveredWithinLimit(t, "the alert", firing, firedAt)
	if f.during != nil {
		f.during(t, firing)
	}
	f.recover(t)
	resolved, resolvedAt := s.awaitAlert(t, from, s10Resolved, firing.Incident, f.check, f.resolveWithin)
	s10DeliveredWithinLimit(t, "the resolve", resolved, resolvedAt)
	s.requireHealthy(t)
	events := s.recv.eventsFrom(from)
	if f.companionCheck != "" {
		companions := 0
		for _, e := range events {
			if e.Delivered && e.Cluster == s.clusterID && e.Check == f.companionCheck && e.State == s10Firing {
				companions++
				t.Logf("companion %s/%s incident %s fired; its resolve is %t", e.Check, e.Reason, e.Incident,
					slices.ContainsFunc(events, func(candidate s10Event) bool {
						return candidate.Delivered && candidate.Cluster == s.clusterID && candidate.Incident == e.Incident && candidate.State == s10Resolved
					}))
			}
		}
		if companions == 0 {
			t.Logf("no %s companion alert was delivered with the %s planted negative", f.companionCheck, f.check)
		}
	} else if incident, err := s10OneAlertOneResolveForCheck(events, s.clusterID, f.check); err != nil {
		t.Error(err)
	} else {
		t.Logf("one alert and one resolve on %s, incident %s", f.check, incident)
	}
}

func (s *s10Run) healthy(t *testing.T) {
	s.requireHealthy(t)

	// `kubenest health` exits 0 within 30 s of a report.
	before := s.detail(t).ReceivedAt
	var receivedAt time.Time
	s10WaitFor(t, 2*s10ReportInterval+time.Minute, "a new report from "+s.env.cluster, func() (bool, string) {
		d, err := s.client.ClusterHealthDetail(context.Background(), s.clusterID)
		if err != nil {
			return false, err.Error()
		}
		if d.ReceivedAt != nil && (before == nil || d.ReceivedAt.After(*before)) {
			receivedAt = *d.ReceivedAt
			return true, ""
		}
		return false, "no new report yet"
	})
	table, code := healthGateRun(t, "health")
	lag := time.Since(receivedAt)
	t.Logf("kubenest health exited %d, %s after the last report", code, lag.Round(time.Second))
	if code != 0 || lag > 30*time.Second {
		t.Errorf("kubenest health exited %d %s after the last report, want 0 within 30 s:\n%s", code, lag.Round(time.Second), table)
	}

	// --json carries the table's clusters, states and reasons.
	document, documentCode := healthGateRun(t, "health", "--json")
	if documentCode != code {
		t.Errorf("--json exited %d and the table %d", documentCode, code)
	}
	var view api.HealthView
	if err := json.Unmarshal([]byte(document), &view); err != nil {
		t.Fatalf("--json is not the documented document: %v\n%s", err, document)
	}
	rows := healthGateRows(t, table)
	if len(rows) != len(view.Clusters) {
		t.Errorf("the table has %d rows and the document %d clusters", len(rows), len(view.Clusters))
	}
	for i, row := range rows {
		if i >= len(view.Clusters) {
			break
		}
		if c := view.Clusters[i]; c.ClusterName != row.name || c.Status != row.state || c.Reason != row.reason {
			t.Errorf("row %d is %s/%s/%q in the table and %s/%s/%q in the document", i, row.name, row.state, row.reason, c.ClusterName, c.Status, c.Reason)
		}
	}

	// With the outbox empty, the live worker keeps the heartbeat beating.
	s.requireEmptyOutbox(t)
	s10WaitFor(t, 3*s10SweepInterval+time.Minute, "two beats with the outbox empty", func() (bool, string) {
		n := s.recv.beatsAfter(s.configuredAt)
		return n >= 2, fmt.Sprintf("%d beat(s) since the heartbeat was set", n)
	})
	select {
	case m := <-s.missed:
		t.Errorf("the switch reported a missed beat on the healthy control plane (last beat %s)", m.LastBeat.Format(time.RFC3339))
	default:
	}
}

func (s *s10Run) stopTheAgent(t *testing.T) {
	scale := "-n " + s10Namespace + " scale deployment/" + s10Agent + " --replicas="
	replicas := s.kubectl(t, s.server, "-n "+s10Namespace+" get deployment/"+s10Agent+" -o jsonpath={.spec.replicas}")
	if replicas == "" || replicas == "0" {
		t.Fatalf("the agent %s/%s has %q replicas before the fault", s10Namespace, s10Agent, replicas)
	}
	s.fault(t, s10Fault{
		check:   "reporting",
		reasons: []string{"CLUSTER_SILENT"},
		inject: func(t *testing.T) {
			t.Cleanup(func() { s.undo(t, s.server, scale+replicas) })
			s.kubectl(t, s.server, scale+"0")
		},
		during: func(t *testing.T, _ s10Event) {
			if d := s.detail(t); d.Status != "critical" {
				t.Errorf("the silent cluster reads %s, want critical (reporting/CLUSTER_SILENT)", d.Status)
			}
			if _, code := healthGateRun(t, "health", "--cluster", s.env.cluster); code != healthExitCodeMap["critical"] {
				t.Errorf("kubenest health --cluster %s exited %d for a silent cluster, want %d", s.env.cluster, code, healthExitCodeMap["critical"])
			}
		},
		recover:       func(t *testing.T) { s.kubectl(t, s.server, scale+replicas) },
		fireWithin:    s10SilenceWindow + 2*s10SweepInterval + s10DeliveryLimit + 2*time.Minute,
		resolveWithin: 2*time.Minute + s10ReportInterval + s10DeliveryLimit + time.Minute,
	})
}

// drillResult reads the restore-drill result the operator reports from.
func (s *s10Run) drillResult(t *testing.T) string {
	t.Helper()
	raw := s.kubectl(t, s.server, "-n "+s10DrillNamespace+" get configmap "+s10DrillResult+" -o json")
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &cm); err != nil {
		t.Fatalf("the restore-drill result ConfigMap is not JSON: %v", err)
	}
	doc := cm.Data["result.json"]
	if doc == "" {
		t.Fatalf("%s/%s holds no result.json: run `kubenest backup drill` on the fixture first", s10DrillNamespace, s10DrillResult)
	}
	return doc
}

func (s *s10Run) writeDrillResult(doc string) error {
	patch, err := json.Marshal(map[string]any{"data": map[string]string{"result.json": doc}})
	if err != nil {
		return err
	}
	return s.patch(s.server, "-n "+s10DrillNamespace+" patch configmap "+s10DrillResult, string(patch))
}

// swapDrillResult writes doc in place of the original for the rest of the
// subtest. The drill runner starts a drill once completed_at plus its interval
// has passed and would replace the document under test (op3 runner.go), so its
// interval is pushed out meanwhile. Both are put back in cleanups.
func (s *s10Run) swapDrillResult(t *testing.T, original, doc string) {
	t.Helper()
	drillConfig := "-n " + s10DrillNamespace + " patch configmap " + s10DrillConfig
	interval := s.kubectl(t, s.server, "-n "+s10DrillNamespace+" get configmap "+s10DrillConfig+" -o jsonpath={.data.interval}")
	if interval == "" {
		t.Fatalf("%s/%s carries no interval", s10DrillNamespace, s10DrillConfig)
	}
	t.Cleanup(func() {
		if err := s.patch(s.server, drillConfig, fmt.Sprintf(`{"data":{"interval":%q}}`, interval)); err != nil {
			t.Errorf("restoring the drill interval %s: %v", interval, err)
		}
	})
	if err := s.patch(s.server, drillConfig, `{"data":{"interval":"100000h0m0s"}}`); err != nil {
		t.Fatalf("holding the drill runner off: %v", err)
	}
	t.Cleanup(func() {
		if err := s.writeDrillResult(original); err != nil {
			t.Errorf("restoring the restore-drill result: %v", err)
		}
	})
	if err := s.writeDrillResult(doc); err != nil {
		t.Fatalf("writing the restore-drill result under test: %v", err)
	}
}

func (s *s10Run) restoreDrillResult(t *testing.T, original string) {
	t.Helper()
	if err := s.writeDrillResult(original); err != nil {
		t.Fatalf("restoring the restore-drill result: %v", err)
	}
}

func (s *s10Run) ageTheDrill(t *testing.T) {
	original := s.drillResult(t)
	aged, err := s10BackdateDrill(original, time.Now().Add(-s10DrillMaxAge-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	s.fault(t, s10Fault{
		check:         "backup",
		reasons:       []string{"RESTORE_DRILL_STALE"},
		inject:        func(t *testing.T) { s.swapDrillResult(t, original, aged) },
		recover:       func(t *testing.T) { s.restoreDrillResult(t, original) },
		fireWithin:    s10ReportInterval + s10DeliveryLimit + 2*time.Minute,
		resolveWithin: s10ReportInterval + s10DeliveryLimit + 2*time.Minute,
	})
}

func (s *s10Run) failACollector(t *testing.T) {
	original := s.drillResult(t)
	invalid, err := s10InvalidateDrill(original)
	if err != nil {
		t.Fatal(err)
	}
	s.fault(t, s10Fault{
		check:          "backup",
		reasons:        []string{"COLLECTION_FAILED"},
		companionCheck: "agent", // op3 reports the group collection error in agent.errors too.
		inject:         func(t *testing.T) { s.swapDrillResult(t, original, invalid) },
		during: func(t *testing.T, _ s10Event) {
			d := s.detail(t)
			c, ok := s10CheckIn(d, "backup")
			if !ok || c.Status != "unknown" || c.ReasonCode != "COLLECTION_FAILED" || strings.TrimSpace(c.Message) == "" {
				t.Errorf("the failed collector reads %+v, want unknown/COLLECTION_FAILED with its reason", c)
			}
			if d.Status == "ok" {
				t.Error("a cluster with a failed collector reads ok")
			}
			if _, code := healthGateRun(t, "health", "--cluster", s.env.cluster); code != healthExitCodeMap["unknown"] {
				t.Errorf("kubenest health --cluster %s exited %d with a failed collector, want %d", s.env.cluster, code, healthExitCodeMap["unknown"])
			}
			fleet, err := s.client.FleetHealth(context.Background(), s.orgID)
			if err != nil {
				t.Fatalf("reading the fleet: %v", err)
			}
			worst := healthGateWorstState(fleet.Clusters)
			if _, code := healthGateRun(t, "health"); code != healthExitCodeMap["unknown"] {
				t.Errorf("kubenest health exited %d for a fleet whose worst state is %s, want %d", code, worst, healthExitCodeMap["unknown"])
			} else {
				t.Logf("kubenest health exits %d: the fleet's worst state is %s", code, worst)
			}
		},
		recover:       func(t *testing.T) { s.restoreDrillResult(t, original) },
		fireWithin:    s10ReportInterval + s10DeliveryLimit + 2*time.Minute,
		resolveWithin: s10ReportInterval + s10DeliveryLimit + 2*time.Minute,
	})
}

// snapshot forces a recovery snapshot the way the CLI does (pkg/backup/datastore.go).
func (s *s10Run) snapshot() error {
	res, err := s.server.Run(context.Background(), "sudo -n k3s etcd-snapshot save --name kubenest-s10-"+randomSuffix())
	if err != nil {
		return fmt.Errorf("running k3s etcd-snapshot save: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("k3s etcd-snapshot save failed with the bucket restored: exit %d: %s", res.ExitCode, firstLineOfE2E(res.Stderr))
	}
	return nil
}

// k3s's scheduled snapshot saves locally before initializing the S3 client.
// An on-demand save initializes S3 first and would refuse a missing bucket
// without taking any local snapshot or recording the failed upload.
func s10NextScheduledSnapshot(clock string) (time.Duration, error) {
	parsed, err := time.Parse("04:05", strings.TrimSpace(clock))
	if err != nil {
		return 0, err
	}
	return time.Hour - time.Duration(parsed.Minute())*time.Minute - time.Duration(parsed.Second())*time.Second, nil
}

// breakTheSnapshotUpload leaves the bucket broken through k3s's next hourly
// scheduled snapshot. That path records a fresh local snapshot and an S3 error.
func (s *s10Run) breakTheSnapshotUpload(t *testing.T) {
	secret := "-n kube-system patch secret " + s10SnapshotSecret
	original := s.kubectl(t, s.server, "-n kube-system get secret "+s10SnapshotSecret+" -o jsonpath={.data.etcd-s3-bucket}")
	if original == "" {
		t.Fatalf("kube-system/%s names no bucket: this cluster does not upload datastore snapshots", s10SnapshotSecret)
	}
	broken := base64.StdEncoding.EncodeToString([]byte("kubenest-s10-no-such-bucket-" + randomSuffix()))
	bucket := func(b64 string) string { return fmt.Sprintf(`{"data":{"etcd-s3-bucket":%q}}`, b64) }
	before, _ := s10CheckIn(s.detail(t), "datastore")
	previousLocal := s10DetailTime(before.Detail, "local_snapshot_at")
	var untilTick time.Duration
	recovered := false
	s.fault(t, s10Fault{
		check:   "datastore",
		reasons: []string{"DATASTORE_SNAPSHOT_FAILED"},
		inject: func(t *testing.T) {
			t.Cleanup(func() {
				if err := s.patch(s.server, secret, bucket(original)); err != nil {
					t.Errorf("restoring the snapshot bucket: %v", err)
					return
				}
				if !recovered {
					if err := s.snapshot(); err != nil {
						t.Errorf("recovering the datastore snapshot after an interrupted arm: %v", err)
					}
				}
			})
			if err := s.patch(s.server, secret, bucket(broken)); err != nil {
				t.Fatalf("breaking the snapshot upload: %v", err)
			}
			res, err := s.server.Run(context.Background(), "date +%M:%S")
			if err != nil {
				t.Fatalf("reading the snapshot host's clock: %v", err)
			}
			if res.ExitCode != 0 {
				t.Fatalf("reading the snapshot host's clock: exit %d: %s", res.ExitCode, firstLineOfE2E(res.Stderr))
			}
			untilTick, err = s10NextScheduledSnapshot(res.Stdout)
			if err != nil {
				t.Fatalf("parsing the snapshot host's clock: %v", err)
			}
			t.Logf("the snapshot bucket is broken at host clock %s; the next hourly scheduled snapshot is in %s", strings.TrimSpace(res.Stdout), untilTick)
		},
		during: func(t *testing.T, _ s10Event) {
			c, _ := s10CheckIn(s.detail(t), "datastore")
			local := s10DetailTime(c.Detail, "local_snapshot_at")
			off := s10DetailTime(c.Detail, "offcluster_snapshot_at")
			t.Logf("datastore %s: local snapshot %s, off-cluster %s", c.ReasonCode, local.Format(time.RFC3339), off.Format(time.RFC3339))
			if local.IsZero() || !local.After(previousLocal) || time.Since(local) > s10SnapshotMaxAge {
				t.Errorf("the local snapshot (%s) is not fresh and newer than the pre-fault snapshot (%s)", local.Format(time.RFC3339), previousLocal.Format(time.RFC3339))
			}
			if off.IsZero() || !off.Before(local) {
				t.Errorf("the off-cluster snapshot (%s) is not older than the local one (%s)", off.Format(time.RFC3339), local.Format(time.RFC3339))
			}
			failure, _ := c.Detail["last_failure"].(map[string]any)
			// k3s records a failed upload with no location, so the operator names the side.
			if side, _ := failure["side"].(string); side != "offcluster" {
				t.Errorf("the recorded failure's side is %q, want offcluster (failure: %v)", side, failure)
			}
		},
		recover: func(t *testing.T) {
			if err := s.patch(s.server, secret, bucket(original)); err != nil {
				t.Fatalf("restoring the snapshot bucket: %v", err)
			}
			if err := s.snapshot(); err != nil {
				t.Fatal(err)
			}
			recovered = true
		},
		fireWithinAfterInject: func() time.Duration {
			return untilTick + 5*time.Second + s10ReportInterval + s10SweepInterval + s10DeliveryLimit + time.Minute
		},
		resolveWithin: 2*time.Minute + s10ReportInterval + s10DeliveryLimit + time.Minute,
	})
}

// failTheReceiver raises an alert while the receiver refuses everything (the
// aged drill), restarts the backend with the receiver still failing, then
// restores the receiver: the alert must arrive afterwards, with its resolve.
func (s *s10Run) failTheReceiver(t *testing.T) {
	s.requireHealthy(t)
	s.requireEmptyOutbox(t)
	original := s.drillResult(t)
	aged, err := s10BackdateDrill(original, time.Now().Add(-s10DrillMaxAge-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	from := len(s.recv.eventsFrom(0))
	failingSince := time.Now()
	s.recv.setFailing(true)
	t.Cleanup(func() { s.recv.setFailing(false) })
	s.swapDrillResult(t, original, aged)

	var first api.InstanceAlerting
	s10WaitFor(t, s10ReportInterval+s10DeliveryLimit+2*time.Minute, "the undelivered alert to show in the backlog", func() (bool, string) {
		a, err := s.client.InstanceAlerting(context.Background())
		if err != nil {
			return false, err.Error()
		}
		first = a
		return a.Backlog.Pending > 0 && a.Backlog.OldestAgeSeconds != nil && a.FailedDestinations > 0, fmt.Sprintf("%+v", a)
	})
	time.Sleep(2 * s10DeliveryInterval)
	second := s.alerting(t)
	if second.Backlog.Pending == 0 || second.Backlog.OldestAgeSeconds == nil || *second.Backlog.OldestAgeSeconds <= *first.Backlog.OldestAgeSeconds {
		t.Errorf("the backlog's age did not grow while the receiver failed: %+v, then %+v", first.Backlog, second.Backlog)
	} else {
		t.Logf("while the receiver fails: %d pending, oldest %ds, %d failed destination(s)", second.Backlog.Pending, *second.Backlog.OldestAgeSeconds, second.FailedDestinations)
	}
	for _, e := range s.recv.eventsFrom(from) {
		if e.Delivered {
			t.Errorf("a request was accepted while the receiver was failing: %+v", e)
		}
	}

	// The outbox is the database's, not the process's: it survives the restart.
	s.kubectl(t, s.cp, "-n "+s10Namespace+" rollout restart deployment/"+s10Backend)
	s.backendSettled(t)
	if a := s.alerting(t); a.Backlog.Pending == 0 {
		t.Errorf("the backlog is empty after the backend restarted with the receiver still failing: %+v", a.Backlog)
	}
	if failedFor := time.Since(failingSince); failedFor > s10BacklogCritical {
		t.Logf("the receiver failed for %s, past the backlog's critical age of %s, so the heartbeat was withheld meanwhile", failedFor.Round(time.Second), s10BacklogCritical)
	}
	s.recv.setFailing(false)
	restoredAt := time.Now()

	firing, _ := s.awaitAlert(t, from, s10Firing, "", "backup", s10BackoffCeiling+3*s10DeliveryInterval+time.Minute)
	if firing.Check != "backup" || firing.Reason != "RESTORE_DRILL_STALE" {
		t.Errorf("the alert raised while the receiver failed is %s/%s, want backup/RESTORE_DRILL_STALE", firing.Check, firing.Reason)
	}
	t.Logf("the alert raised while the receiver failed arrived %s after it recovered", firing.At.Sub(restoredAt).Round(time.Second))
	s.restoreDrillResult(t, original)
	resolved, resolvedAt := s.awaitAlert(t, from, s10Resolved, firing.Incident, "backup", s10ReportInterval+s10DeliveryLimit+2*time.Minute)
	s10DeliveredWithinLimit(t, "the resolve", resolved, resolvedAt)
	s.requireHealthy(t)
	if _, err := s10OneAlertOneResolveForCheck(s.recv.eventsFrom(from), s.clusterID, "backup"); err != nil {
		t.Error(err)
	}
	s.requireEmptyOutbox(t)
}

// drainMissed discards notifications about silences that came before the
// branch, such as a restart's gap.
func (s *s10Run) drainMissed() {
	for {
		select {
		case <-s.missed:
		default:
			return
		}
	}
}

func (s *s10Run) awaitMissedSince(t *testing.T, since time.Time, within time.Duration) s10Missed {
	t.Helper()
	timeout := time.After(within)
	for {
		select {
		case m := <-s.missed:
			if !m.LastBeat.Before(since) {
				return m
			}
			t.Logf("ignoring a missed beat from before the fault (last beat %s)", m.LastBeat.Format(time.RFC3339))
		case <-timeout:
			t.Fatalf("the heartbeat receiver's switch sent no missed-beat notification within %s", within)
		}
	}
}

func (s *s10Run) lastSweep(t *testing.T) time.Time {
	t.Helper()
	fleet, err := s.client.FleetHealth(context.Background(), s.orgID)
	if err != nil {
		return time.Time{}
	}
	return s10DetailTime(fleet.Sweeper, "last_sweep_at")
}

func (s *s10Run) firingsFrom(from int) []s10Event {
	var fired []s10Event
	for _, e := range s.recv.eventsFrom(from) {
		if e.Delivered && e.Cluster == s.clusterID && e.State == s10Firing {
			fired = append(fired, e)
		}
	}
	return fired
}

func (s *s10Run) stopTheIdleWorker(t *testing.T) {
	s.requireHealthy(t)
	s.requireEmptyOutbox(t)
	from := len(s.recv.eventsFrom(0))
	// Undone in a cleanup only while it is still set: removing a variable the
	// Deployment no longer has is not a step this gate needs to depend on.
	workerStopped := true
	t.Cleanup(func() {
		if workerStopped {
			s.undo(t, s.cp, "-n "+s10Namespace+" set env deployment/"+s10Backend+" "+s10DeliveryEnv+"-")
		}
	})
	restartedAt := s.setBackendEnv(t, s10DeliveryEnv, "86400")
	s.drainMissed()
	s10WaitFor(t, 2*s10SweepInterval+time.Minute, "a beat from the restarted backend while its one delivery cycle is fresh", func() (bool, string) {
		n := s.recv.beatsAfter(restartedAt)
		return n > 0, fmt.Sprintf("%d beat(s) since the restart", n)
	})
	missed := s.awaitMissedSince(t, restartedAt, s10CycleDeadline+s10SweepInterval+s10DeadManDeadline+time.Minute)
	gap := missed.LastBeat.Sub(restartedAt)
	if gap > s10CycleDeadline+20*time.Second {
		t.Errorf("the last beat came %s after the backend restarted with its worker stopped, past the %s delivery-cycle deadline", gap.Round(time.Second), s10CycleDeadline)
	}
	// The sweeper is alive the whole time: the beats stopped because the worker did.
	s10WaitFor(t, 2*s10SweepInterval+time.Minute, "a sweep after the last beat", func() (bool, string) {
		at := s.lastSweep(t)
		return at.After(missed.LastBeat.Add(s10SweepInterval / 2)), "last sweep " + at.Format(time.RFC3339)
	})
	t.Logf("with only the delivery worker stopped, the last beat came %s after the restart and the switch noticed at %s, while the sweeper kept sweeping",
		gap.Round(time.Second), missed.Noticed.Format(time.RFC3339))

	backAt := s.setBackendEnv(t, s10DeliveryEnv, "")
	workerStopped = false
	s10WaitFor(t, 3*s10SweepInterval+time.Minute, "beats to resume with the worker back", func() (bool, string) {
		n := s.recv.beatsAfter(backAt)
		return n >= 2, fmt.Sprintf("%d beat(s) since the worker came back", n)
	})
	s.requireHealthy(t)
	if fired := s.firingsFrom(from); len(fired) > 0 {
		t.Errorf("stopping the idle worker raised %d alert(s) for the cluster: %+v", len(fired), fired)
	}
}

func (s *s10Run) stopTheBackend(t *testing.T) {
	s.requireHealthy(t)
	from := len(s.recv.eventsFrom(0))
	scale := "-n " + s10Namespace + " scale deployment/" + s10Backend + " --replicas="
	replicas := s.kubectl(t, s.cp, "-n "+s10Namespace+" get deployment/"+s10Backend+" -o jsonpath={.spec.replicas}")
	if replicas == "" || replicas == "0" {
		t.Fatalf("the backend has %q replicas before the fault", replicas)
	}
	t.Cleanup(func() { s.undo(t, s.cp, scale+replicas) })
	s.drainMissed()
	s.kubectl(t, s.cp, scale+"0")
	s10WaitFor(t, 5*time.Minute, "the backend's pods to be gone", func() (bool, string) {
		out, err := k3s.Kubectl(context.Background(), s.cp, "-n "+s10Namespace+" get deployment/"+s10Backend+" -o jsonpath={.status.replicas}")
		out = strings.TrimSpace(out)
		return err == nil && (out == "" || out == "0"), fmt.Sprintf("status.replicas %q, %v", out, err)
	})
	stoppedAt := time.Now()
	missed := s.awaitMissedSince(t, stoppedAt.Add(-2*s10SweepInterval), 2*s10SweepInterval+s10DeadManDeadline+time.Minute)
	if n := s.recv.beatsAfter(stoppedAt.Add(5 * time.Second)); n > 0 {
		t.Errorf("%d beat(s) arrived after the backend stopped", n)
	}
	t.Logf("the backend stopped at %s; the last beat came at %s, and the switch noticed at %s",
		stoppedAt.Format(time.RFC3339), missed.LastBeat.Format(time.RFC3339), missed.Noticed.Format(time.RFC3339))

	s.kubectl(t, s.cp, scale+replicas)
	backAt := s.backendSettled(t)
	s10WaitFor(t, 2*s10SweepInterval+time.Minute, "beats to resume with the backend back", func() (bool, string) {
		n := s.recv.beatsAfter(backAt)
		return n > 0, fmt.Sprintf("%d beat(s) since the backend came back", n)
	})
	// Every incident the outage raised must resolve; requireHealthy waits for that.
	s.requireHealthy(t)
	for _, e := range s.firingsFrom(from) {
		t.Logf("after a %s backend outage: %s/%s incident %s fired (the silence sweep has no start-up grace) and resolved",
			backAt.Sub(stoppedAt).Round(time.Second), e.Check, e.Reason, e.Incident)
	}
}

// ---------------------------------------------------------------- unit tests
//
// The gate's own logic, runnable anywhere with `go test -tags e2e -run TestS10 ./e2e/`.

func s10Ev(incident, state, cluster string, delivered bool) s10Event {
	return s10Event{Incident: incident, State: state, Cluster: cluster, Delivered: delivered}
}

func s10Post(t *testing.T, r *s10Receiver, path, body string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec.Code
}

const s10FiringBody = `{"kind":"kubenest.fleet_health.alert","event":"firing","incident_id":"inc-a",` +
	`"cluster_id":"c1","org_id":"o1","check":"reporting","reason_code":"CLUSTER_SILENT",` +
	`"status":"critical","message":"[lab-a] No health report","first_seen_at":"2026-11-12T10:00:00+00:00","resolved_at":null}`

func TestS10ReceiverRecordsADeliveredAlert(t *testing.T) {
	r := newS10Receiver(time.Minute)
	if code := s10Post(t, r, "/alert", s10FiringBody); code != http.StatusOK {
		t.Fatalf("a healthy receiver answered %d", code)
	}
	got := r.eventsFrom(0)
	if len(got) != 1 {
		t.Fatalf("recorded %d events, want 1", len(got))
	}
	e := got[0]
	if e.Incident != "inc-a" || e.State != s10Firing || e.Cluster != "c1" || e.Check != "reporting" ||
		e.Reason != "CLUSTER_SILENT" || !e.Delivered || e.At.IsZero() {
		t.Fatalf("recorded %+v", e)
	}
}

func TestS10AFailingReceiverAnswers5xxAndRecordsNoDelivery(t *testing.T) {
	r := newS10Receiver(time.Minute)
	r.setFailing(true)
	if code := s10Post(t, r, "/alert", s10FiringBody); code < 500 {
		t.Fatalf("a failing receiver answered %d, want a 5xx the backend must retry", code)
	}
	r.setFailing(false)
	if code := s10Post(t, r, "/alert", s10FiringBody); code != http.StatusOK {
		t.Fatalf("the restored receiver answered %d", code)
	}
	got := r.eventsFrom(0)
	if len(got) != 2 || got[0].Delivered || !got[1].Delivered {
		t.Fatalf("want one refused request then one delivery, recorded %+v", got)
	}
}

func TestS10ReceiverCountsHeartbeatsOnTheirOwnPath(t *testing.T) {
	r := newS10Receiver(time.Minute)
	before := time.Now().Add(-time.Second)
	if code := s10Post(t, r, "/heartbeat", `{"kind":"kubenest.control_plane.heartbeat"}`); code != http.StatusOK {
		t.Fatalf("the heartbeat receiver answered %d", code)
	}
	if n := r.beatsAfter(before); n != 1 {
		t.Fatalf("counted %d beats, want 1", n)
	}
	if n := len(r.eventsFrom(0)); n != 0 {
		t.Fatalf("a heartbeat was recorded as %d alert(s)", n)
	}
}

func TestS10ReceiverRefusesAndRecordsAMalformedRequest(t *testing.T) {
	r := newS10Receiver(time.Minute)
	for _, body := range []string{
		`not json`,
		`{"kind":"something.else","event":"firing","incident_id":"x","cluster_id":"c1"}`,
		`{"kind":"kubenest.fleet_health.alert","event":"firing","cluster_id":"c1"}`,
	} {
		if code := s10Post(t, r, "/alert", body); code != http.StatusBadRequest {
			t.Errorf("%q was answered %d, want 400", body, code)
		}
	}
	if code := s10Post(t, r, "/heartbeat", `{"kind":"kubenest.fleet_health.alert"}`); code != http.StatusBadRequest {
		t.Errorf("an alert sent to the heartbeat path was answered %d, want 400", code)
	}
	if n := len(r.malformedRequests()); n != 4 {
		t.Fatalf("recorded %d malformed requests, want 4", n)
	}
	if n := len(r.eventsFrom(0)); n != 0 {
		t.Fatalf("a malformed request became %d event(s)", n)
	}
}

func TestS10CPHostComesFromTheSslipName(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.5.75.1.2.sslip.io":   "5.75.1.2",
		"https://api.5.75.1.2.sslip.io/":  "5.75.1.2",
		"https://api.example.com":         "",
		"http://localhost:8000":           "",
		"https://api.999.75.1.2.sslip.io": "",
	} {
		if got := s10CPHost(in); got != want {
			t.Errorf("s10CPHost(%q) = %q, want %q", in, got, want)
		}
	}
}

const s10PassedDrill = `{"status":"passed","completed_at":"2026-11-12T09:00:00Z","backup":"daily-1",` +
	`"duration_seconds":42,"verification":{"mode":"full","objects":{"restored":3,"matched":3},"pvc_data":{"restored":1,"matched":1}}}`

func TestS10BackdatedDrillMovesOnlyItsCompletionTime(t *testing.T) {
	old := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	out, err := s10BackdateDrill(s10PassedDrill, old)
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the backdated result is not JSON: %v", err)
	}
	_ = json.Unmarshal([]byte(s10PassedDrill), &want)
	want["completed_at"] = "2026-10-27T09:00:00Z"
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if _, err := s10BackdateDrill(`{"status":"failed"}`, old); err == nil {
		t.Fatal("only a passed drill can be aged: a failed one alerts for a different reason")
	}
}

func TestS10InvalidDrillIsAPassedResultWithoutItsVerification(t *testing.T) {
	out, err := s10InvalidateDrill(s10PassedDrill)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the invalid result must still be JSON, so the operator's validation is what fails: %v", err)
	}
	if _, ok := got["verification"]; ok || got["status"] != "passed" || got["backup"] != "daily-1" {
		t.Fatalf("got %v, want the passed result with verification removed", got)
	}
}

// s10Check builds one check as the control plane returns it: measured is when
// its evidence was measured, received when the report carrying it arrived.
func s10Check(name, status, reason, message string, measured, received time.Time) api.HealthCheck {
	c := api.HealthCheck{Check: name, Status: status, ReasonCode: reason, Message: message, Evidence: map[string]any{}}
	if !measured.IsZero() {
		c.Evidence["measured_at"] = measured.Format(time.RFC3339)
	}
	if !received.IsZero() {
		c.Evidence["received_at"] = received.Format(time.RFC3339)
	}
	return c
}

func TestS10HealthProblems(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	fresh := now.Add(-30 * time.Second)
	// A backup group's measurement is its last backup or drill, which a healthy
	// daily schedule leaves hours old (evaluate.py measurement_time_for).
	lastNightsBackup := now.Add(-20 * time.Hour)
	healthy := []api.HealthCheck{
		s10Check("backup", "ok", "", "Backups current", lastNightsBackup, fresh),
		s10Check("os_patching", "ok", "", "Up to date", fresh, fresh),
		s10Check("certificates", "not_applicable", "NOT_APPLICABLE", "No cert-manager certificates on this cluster", time.Time{}, fresh),
	}
	for _, tc := range []struct {
		name   string
		checks []api.HealthCheck
		want   string
	}{
		{"healthy", healthy, ""},
		{"stale report", append(healthy[:1:1], s10Check("os_patching", "ok", "", "Up to date", now.Add(-10*time.Minute), now.Add(-10*time.Minute))), "os_patching"},
		{"unmeasured ok", append(healthy[:1:1], s10Check("os_patching", "ok", "", "Up to date", time.Time{}, fresh)), "os_patching"},
		{"silent not-applicable", append(healthy[:2:2], s10Check("certificates", "not_applicable", "NOT_APPLICABLE", "", time.Time{}, fresh)), "certificates"},
		{"required protection not applicable", []api.HealthCheck{healthy[1], s10Check("backup", "not_applicable", "NOT_APPLICABLE", "no target", time.Time{}, fresh)}, "backup"},
		{"a warning", append(healthy[:2:2], s10Check("datastore", "warning", "SNAPSHOT_STALE", "old", fresh, fresh)), "SNAPSHOT_STALE"},
		{"no backup group", healthy[1:], "backup"},
	} {
		problems := s10HealthProblems(api.ClusterHealthDetail{Checks: tc.checks}, now, 3*time.Minute)
		joined := strings.Join(problems, "; ")
		switch {
		case tc.want == "" && len(problems) != 0:
			t.Errorf("%s: want no problems, got %s", tc.name, joined)
		case tc.want != "" && !strings.Contains(joined, tc.want):
			t.Errorf("%s: want a problem naming %s, got %q", tc.name, tc.want, joined)
		}
	}
}

func TestS10LedgerAcceptsOneAlertThenOneResolve(t *testing.T) {
	got, err := s10OneAlertOneResolve([]s10Event{
		s10Ev("inc-a", s10Firing, "c1", true),
		s10Ev("inc-a", s10Resolved, "c1", true),
	}, "c1")
	if err != nil || got != "inc-a" {
		t.Fatalf("got (%q, %v), want (inc-a, nil)", got, err)
	}
}

func TestS10AlertSelectionUsesTheFaultsCheck(t *testing.T) {
	events := []s10Event{
		{Incident: "agent-a", Check: "agent", State: s10Firing, Cluster: "c1", Delivered: true},
		{Incident: "backup-a", Check: "backup", State: s10Firing, Cluster: "c1", Delivered: true},
	}
	got, ok := s10SelectDelivered(events, "c1", s10Firing, "", "backup")
	if !ok || got.Incident != "backup-a" {
		t.Fatalf("selected (%+v, %t), want the backup incident", got, ok)
	}
	if _, ok := s10SelectDelivered(events, "c1", s10Firing, "", "datastore"); ok {
		t.Fatal("an alert for another check cannot satisfy the datastore wait")
	}
	events = append(events,
		s10Event{Incident: "backup-a", Check: "agent", State: s10Resolved, Cluster: "c1", Delivered: true},
		s10Event{Incident: "backup-a", Check: "backup", State: s10Resolved, Cluster: "c1", Delivered: true})
	if got, ok := s10SelectDelivered(events, "c1", s10Resolved, "backup-a", "backup"); !ok || got.Check != "backup" {
		t.Fatalf("selected (%+v, %t), want the backup resolve", got, ok)
	}
}

func TestS10LedgerCountsTheFaultCheckOnly(t *testing.T) {
	events := []s10Event{
		{Incident: "agent-a", Check: "agent", State: s10Firing, Cluster: "c1", Delivered: true},
		{Incident: "backup-a", Check: "backup", State: s10Firing, Cluster: "c1", Delivered: true},
		{Incident: "agent-a", Check: "agent", State: s10Resolved, Cluster: "c1", Delivered: true},
		{Incident: "backup-a", Check: "backup", State: s10Resolved, Cluster: "c1", Delivered: true},
	}
	got, err := s10OneAlertOneResolveForCheck(events, "c1", "backup")
	if err != nil || got != "backup-a" {
		t.Fatalf("got (%q, %v), want backup-a and no error", got, err)
	}
	events = append(events,
		s10Event{Incident: "backup-b", Check: "backup", State: s10Firing, Cluster: "c1", Delivered: true},
		s10Event{Incident: "backup-b", Check: "backup", State: s10Resolved, Cluster: "c1", Delivered: true})
	if _, err := s10OneAlertOneResolveForCheck(events, "c1", "backup"); err == nil {
		t.Fatal("a second incident on the fault check must be refused")
	}
}

func TestS10NextScheduledSnapshot(t *testing.T) {
	for _, tc := range []struct {
		clock string
		want  time.Duration
	}{
		{"57:30", 2*time.Minute + 30*time.Second},
		{"00:03", 59*time.Minute + 57*time.Second},
	} {
		got, err := s10NextScheduledSnapshot(tc.clock)
		if err != nil || got != tc.want {
			t.Errorf("host clock %s: got (%s, %v), want %s", tc.clock, got, err, tc.want)
		}
	}
	if _, err := s10NextScheduledSnapshot("not-a-clock"); err == nil {
		t.Fatal("a malformed host clock must not become an immediate snapshot wait")
	}
}

func TestS10LedgerTreatsARetryAsOneDelivery(t *testing.T) {
	got, err := s10OneAlertOneResolve([]s10Event{
		s10Ev("inc-a", s10Firing, "c1", true),
		s10Ev("inc-a", s10Firing, "c1", true),
		s10Ev("inc-a", s10Resolved, "c1", true),
		s10Ev("inc-a", s10Resolved, "c1", true),
	}, "c1")
	if err != nil || got != "inc-a" {
		t.Fatalf("a retry of the same intent is allowed (exactly-once is not promised): got (%q, %v)", got, err)
	}
}

func TestS10LedgerIgnoresOtherClustersAndUndeliveredRequests(t *testing.T) {
	got, err := s10OneAlertOneResolve([]s10Event{
		s10Ev("inc-x", s10Firing, "other", true),
		s10Ev("inc-a", s10Firing, "c1", false),
		s10Ev("inc-a", s10Firing, "c1", true),
		s10Ev("inc-a", s10Resolved, "c1", true),
	}, "c1")
	if err != nil || got != "inc-a" {
		t.Fatalf("got (%q, %v), want (inc-a, nil)", got, err)
	}
}

func TestS10LedgerRefusesAResolveBeforeItsFiring(t *testing.T) {
	_, err := s10OneAlertOneResolve([]s10Event{
		s10Ev("inc-a", s10Resolved, "c1", true),
		s10Ev("inc-a", s10Firing, "c1", true),
	}, "c1")
	if err == nil || !strings.Contains(err.Error(), "before") {
		t.Fatalf("a resolve that arrives before its firing must be refused, got %v", err)
	}
}

func TestS10LedgerRefusesASecondIncident(t *testing.T) {
	_, err := s10OneAlertOneResolve([]s10Event{
		s10Ev("inc-a", s10Firing, "c1", true),
		s10Ev("inc-b", s10Firing, "c1", true),
		s10Ev("inc-a", s10Resolved, "c1", true),
		s10Ev("inc-b", s10Resolved, "c1", true),
	}, "c1")
	if err == nil || !strings.Contains(err.Error(), "inc-b") {
		t.Fatalf("one fault must produce one incident, got %v", err)
	}
}

func TestS10LedgerRefusesAMissingResolve(t *testing.T) {
	_, err := s10OneAlertOneResolve([]s10Event{s10Ev("inc-a", s10Firing, "c1", true)}, "c1")
	if err == nil || !strings.Contains(err.Error(), "resolve") {
		t.Fatalf("a firing with no resolve must be refused, got %v", err)
	}
}

func TestS10LedgerRefusesNoAlertAtAll(t *testing.T) {
	_, err := s10OneAlertOneResolve(nil, "c1")
	if err == nil {
		t.Fatal("a fault that produced no alert must be refused")
	}
}

func TestS10LedgerRefusesARefireAfterTheResolve(t *testing.T) {
	_, err := s10OneAlertOneResolve([]s10Event{
		s10Ev("inc-a", s10Firing, "c1", true),
		s10Ev("inc-a", s10Resolved, "c1", true),
		s10Ev("inc-a", s10Firing, "c1", true),
	}, "c1")
	if err == nil || !strings.Contains(err.Error(), "after") {
		t.Fatalf("a second firing after the resolve is a second alert, got %v", err)
	}
}

func TestS10UnresolvedNamesAFiringWithoutItsResolve(t *testing.T) {
	got := s10Unresolved([]s10Event{
		s10Ev("inc-a", s10Firing, "c1", true),
		s10Ev("inc-a", s10Resolved, "c1", true),
		s10Ev("inc-b", s10Firing, "c1", true),
		s10Ev("inc-c", s10Firing, "c1", false),
		s10Ev("inc-d", s10Firing, "other", true),
		s10Ev("inc-e", s10Resolved, "c1", true),
	}, "c1")
	if len(got) != 1 || got[0] != "inc-b" {
		t.Fatalf("got %v, want [inc-b]: only a delivered firing with no delivered resolve is still open", got)
	}
}

func TestS10DeadManNotifiesOnceWhenBeatsStop(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	d := &s10DeadMan{deadline: 2 * time.Minute}
	if _, ok := d.check(t0.Add(10 * time.Minute)); ok {
		t.Fatal("a receiver that never heard a beat cannot say the beats stopped")
	}
	d.beat(t0)
	if _, ok := d.check(t0.Add(2 * time.Minute)); ok {
		t.Fatal("notified at the deadline, not after it")
	}
	missed, ok := d.check(t0.Add(2*time.Minute + time.Second))
	if !ok || !missed.LastBeat.Equal(t0) {
		t.Fatalf("got (%+v, %t), want a notification naming the last beat %s", missed, ok, t0)
	}
	if _, ok := d.check(t0.Add(10 * time.Minute)); ok {
		t.Fatal("one silence must notify once")
	}
}

func TestS10DeadManRearmsWhenBeatsResume(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	d := &s10DeadMan{deadline: time.Minute}
	d.beat(t0)
	if _, ok := d.check(t0.Add(2 * time.Minute)); !ok {
		t.Fatal("the first silence was not noticed")
	}
	t1 := t0.Add(5 * time.Minute)
	d.beat(t1)
	if _, ok := d.check(t1.Add(30 * time.Second)); ok {
		t.Fatal("beats resumed, so this is not a silence")
	}
	missed, ok := d.check(t1.Add(2 * time.Minute))
	if !ok || !missed.LastBeat.Equal(t1) {
		t.Fatalf("got (%+v, %t), want a second notification naming %s", missed, ok, t1)
	}
}
