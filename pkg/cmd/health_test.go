package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
)

// `kubenest health` (T2.9).
//
// These drive the REAL command tree against an httptest control plane, with the
// machine pointed at it through a temp HOME (login_test.go's isolation), so what
// they pin is the whole path: the flags, the two reads, the view, and the exit
// code main maps (ExitCode, which cmd/kubenest/main.go calls).
//
// The two negatives the bead names are here and not in prose: a cluster that
// never reported is shown as such rather than as healthy, and an unknown check
// is never rendered as `ok`.

const (
	healthOrgID   = "0192f0c4-0000-7000-8000-00000000a001"
	healthOrgName = "acme"
)

// --- the control plane `kubenest health` reads ---------------------------------

// healthControlPlane serves the four routes the command calls. Nothing else:
// a route this test did not expect answers 404 and shows up as a failure.
type healthControlPlane struct {
	fleet    api.FleetHealth
	details  map[string]api.ClusterHealthDetail
	alerting api.InstanceAlerting

	// fleetStatus, when set, makes the fleet route refuse with fleetBody.
	fleetStatus int
	fleetBody   string
	// alertingStatus, when set, makes the alerting route refuse with alertingBody.
	alertingStatus int
	alertingBody   string

	mu    sync.Mutex
	paths []string
}

// requestedPaths is every path the command asked for, in order.
func (h *healthControlPlane) requestedPaths() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.paths...)
}

// healthControlPlaneFor builds a control plane holding these rows, each with a
// detail answer that repeats the row's own checks.
func healthControlPlaneFor(rows ...api.FleetClusterHealth) *healthControlPlane {
	h := &healthControlPlane{
		fleet: api.FleetHealth{
			OrgID:    healthOrgID,
			Total:    len(rows),
			Counts:   map[string]int{},
			Clusters: rows,
		},
		details:  map[string]api.ClusterHealthDetail{},
		alerting: api.InstanceAlerting{Destinations: 1, HeartbeatConfigured: true},
	}
	for _, row := range rows {
		h.fleet.Counts[row.Status]++
		h.details[row.ClusterID] = api.ClusterHealthDetail{
			ClusterID:     row.ClusterID,
			ClusterName:   row.ClusterName,
			Status:        row.Status,
			ExitCode:      row.ExitCode,
			Checks:        append([]api.HealthCheck{}, row.Problems...),
			BundleVersion: row.BundleVersion,
			ReceivedAt:    row.LastReportedAt,
		}
	}
	return h
}

// withDetail replaces one cluster's detail answer, which is where the extras and
// the full group list come from.
func (h *healthControlPlane) withDetail(clusterID string, checks ...api.HealthCheck) *healthControlPlane {
	detail := h.details[clusterID]
	detail.Checks = checks
	h.details[clusterID] = detail
	return h
}

func (h *healthControlPlane) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.paths = append(h.paths, r.URL.Path)
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/orgs":
			json.NewEncoder(w).Encode([]api.Org{{ID: healthOrgID, Name: healthOrgName, Slug: healthOrgName}})
		case strings.HasSuffix(r.URL.Path, "/clusters"):
			// `--cluster C` resolves the name through the org's cluster list.
			type clusterRecord struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				Status string `json:"status"`
				OrgID  string `json:"org_id"`
			}
			data := make([]clusterRecord, 0, len(h.fleet.Clusters))
			for _, row := range h.fleet.Clusters {
				data = append(data, clusterRecord{ID: row.ClusterID, Name: row.ClusterName, Status: row.Status, OrgID: healthOrgID})
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data, "has_more": false, "total_count": len(data), "page": 1})
		case strings.HasSuffix(r.URL.Path, "/fleet-health"):
			if h.fleetStatus != 0 {
				w.WriteHeader(h.fleetStatus)
				w.Write([]byte(h.fleetBody))
				return
			}
			json.NewEncoder(w).Encode(h.fleet)
		case strings.HasSuffix(r.URL.Path, "/health"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/clusters/"), "/health")
			detail, ok := h.details[id]
			if !ok {
				http.Error(w, `{"detail": "no such cluster"}`, http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(detail)
		case r.URL.Path == "/api/v1/instance/alerting":
			if h.alertingStatus != 0 {
				w.WriteHeader(h.alertingStatus)
				w.Write([]byte(h.alertingBody))
				return
			}
			json.NewEncoder(w).Encode(h.alerting)
		default:
			http.Error(w, `{"detail": "no route"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// healthFixtureCheck is one check as the control plane sends it. A zero
// measuredAt means the check carries no evidence envelope at all.
func healthFixtureCheck(name, status, reasonCode, message string, detail map[string]any, measuredAt time.Time) api.HealthCheck {
	check := api.HealthCheck{Check: name, Status: status, ReasonCode: reasonCode, Message: message, Detail: detail}
	if check.Detail == nil {
		check.Detail = map[string]any{}
	}
	if !measuredAt.IsZero() {
		stamp := measuredAt.UTC().Format(time.RFC3339)
		check.Evidence = map[string]any{
			"measured_at": stamp, "received_at": stamp, "source": "operator", "check_set_version": "2",
		}
	}
	return check
}

// healthFixtureRow is one fleet row.
func healthFixtureRow(name, status string, exitCode int, problems []api.HealthCheck, lastReported *time.Time) api.FleetClusterHealth {
	return api.FleetClusterHealth{
		ClusterID:      name + "-id",
		ClusterName:    name,
		Status:         status,
		ExitCode:       exitCode,
		Problems:       problems,
		LastReportedAt: lastReported,
		BundleVersion:  "1.1",
		HATier:         "single-server",
	}
}

// healthTwoMinutesAgo is the receipt time the fixtures use, so an age renders
// as "2m ago" and stays there for the life of the test.
func healthTwoMinutesAgo() *time.Time {
	at := time.Now().Add(-2 * time.Minute)
	return &at
}

// --- running the command tree --------------------------------------------------

// healthTree runs the real command tree and returns what it printed, the exit
// code the verdict asks for (the same errors.As mapping main uses), and any
// error that was NOT a verdict.
func healthTree(t *testing.T, args ...string) (string, int, error) {
	t.Helper()
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return out.String(), 0, nil
	}
	var exit *ExitError
	if errors.As(err, &exit) {
		return out.String(), exit.Code, nil
	}
	return out.String(), 1, err
}

// healthViewAt points this machine at the stub and runs `kubenest health`.
func healthViewAt(t *testing.T, controlPlane *healthControlPlane, args ...string) (string, int, error) {
	t.Helper()
	loggedIn(t, controlPlane.start(t))
	return healthTree(t, append([]string{"health"}, args...)...)
}

// --- parsing what it printed ---------------------------------------------------

// healthColumnSplit splits a table line into columns. The renderer pads with
// two or more spaces, so a single space never separates two columns.
var healthColumnSplit = regexp.MustCompile(`\s{2,}`)

// healthTableRows parses the fleet table: every line that carries its five
// columns, as name, bundle, tier, state, reason.
func healthTableRows(t *testing.T, output string) [][]string {
	t.Helper()
	var rows [][]string
	for _, line := range strings.Split(output, "\n") {
		fields := healthColumnSplit.Split(strings.TrimSpace(line), -1)
		if len(fields) < 5 {
			continue
		}
		rows = append(rows, fields[:5])
	}
	return rows
}

func healthTableNames(t *testing.T, output string) []string {
	t.Helper()
	var names []string
	for _, row := range healthTableRows(t, output) {
		names = append(names, row[0])
	}
	return names
}

// healthGroupRow is one row of the single-cluster group table.
type healthGroupRow struct {
	name       string
	state      string
	reasonCode string
	evidence   string
	message    string
}

// healthGroupRows parses the group table, in the order it was printed.
func healthGroupRows(t *testing.T, output string) []healthGroupRow {
	t.Helper()
	lines := strings.Split(output, "\n")
	start := -1
	for i, line := range lines {
		if fields := healthColumnSplit.Split(strings.TrimSpace(line), -1); len(fields) >= 5 && fields[0] == "check" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("no group table in:\n%s", output)
	}
	var rows []healthGroupRow
	for _, line := range lines[start:] {
		fields := healthColumnSplit.Split(strings.TrimSpace(line), -1)
		if len(fields) < 5 {
			break
		}
		rows = append(rows, healthGroupRow{
			name:       fields[0],
			state:      fields[1],
			reasonCode: fields[2],
			evidence:   fields[3],
			message:    strings.Join(fields[4:], "  "),
		})
	}
	return rows
}

// healthExtraLine returns the extras line for one key, e.g. "pending reboots".
func healthExtraLine(output, key string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, key+":") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// healthJSON decodes a `--json` document into generic maps, so the key sets are
// the document's own rather than the struct's.
func healthJSON(t *testing.T, output string) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal([]byte(output), &document); err != nil {
		t.Fatalf("--json is not a JSON document: %v\n%s", err, output)
	}
	return document
}

func healthJSONClusters(t *testing.T, document map[string]any) []map[string]any {
	t.Helper()
	raw, ok := document["clusters"].([]any)
	if !ok {
		t.Fatalf("no clusters array in the document: %v", document)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		cluster, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("a cluster is not an object: %v", item)
		}
		out = append(out, cluster)
	}
	return out
}

// healthKeys is the exact key set of an object, which is what a consumer of the
// document is written against.
func healthKeys(t *testing.T, object map[string]any) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	for key := range object {
		keys[key] = true
	}
	return keys
}

func healthWantsKeys(t *testing.T, what string, object map[string]any, want ...string) {
	t.Helper()
	got := healthKeys(t, object)
	for _, key := range want {
		if !got[key] {
			t.Errorf("%s is missing the %q field", what, key)
		}
		delete(got, key)
	}
	for key := range got {
		t.Errorf("%s carries an undocumented field %q", what, key)
	}
}

// --- fixtures ------------------------------------------------------------------

// healthFourStateFleet is the fleet the ordering, reason, table, JSON and
// exit-code tests share: one cluster per verdict, with the reasons and the
// bundle/tier values the published day-2 example uses.
func healthFourStateFleet(t *testing.T, now time.Time) *healthControlPlane {
	t.Helper()
	recent := healthTwoMinutesAgo()
	silent := func() *time.Time { at := now.Add(-14 * time.Minute); return &at }()

	staging := healthFixtureRow("staging", "critical", 2, []api.HealthCheck{
		healthFixtureCheck("backup", "critical", "NO_BACKUP_TARGET", "no backup target configured", nil, *recent),
	}, silent)
	staging.BundleVersion, staging.HATier = "1.0", "single-server"

	euWest := healthFixtureRow("eu-west-1", "unknown", 3, []api.HealthCheck{
		healthFixtureCheck("clock", "unknown", "SILENT", "silent for 14m", nil, *silent),
	}, silent)
	euWest.HATier = "ha"

	prod2 := healthFixtureRow("prod-2", "warning", 1, []api.HealthCheck{
		healthFixtureCheck("certificates", "warning", "CERT_EXPIRING", "certificate expires in 19d", nil, *recent),
	}, recent)
	prod2.HATier = "ha"

	prod1 := healthFixtureRow("prod-1", "ok", 0, nil, recent)

	h := healthControlPlaneFor(staging, euWest, prod2, prod1)
	// The recovery fact behind an `ok` line: the drill the cluster last passed.
	drill := now.Add(-49 * time.Hour)
	h.withDetail(prod1.ClusterID,
		healthFixtureCheck("backup", "ok", "OK", "Backups current; last restore drill 2 days ago.",
			map[string]any{"last_restore_drill_at": drill.UTC().Format(time.RFC3339)}, *recent),
		healthFixtureCheck("os_patching", "ok", "OK", "Hosts are current on OS patches.",
			map[string]any{"pending_reboot_nodes": 0}, *recent),
	)
	return h
}

// --- the fleet view ------------------------------------------------------------

// The order is the plan's, and the second half of it is the point: `unknown`
// sorts ABOVE `ok`, because forty green and two grey is the truth while
// forty-two green is the failure this view exists to prevent.
func TestFleetViewIsWorstFirstWithUnknownAboveOK(t *testing.T) {
	h := healthFourStateFleet(t, time.Now())
	out, code, err := healthViewAt(t, h)
	if err != nil {
		t.Fatalf("health: %v\n%s", err, out)
	}
	if code != 2 {
		t.Errorf("exit code = %d, want 2 for a fleet holding a critical cluster", code)
	}

	names := healthTableNames(t, out)
	want := []string{"staging", "eu-west-1", "prod-2", "prod-1"}
	if len(names) != len(want) {
		t.Fatalf("rows = %v, want %v\n%s", names, want, out)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("row order = %v, want critical, unknown, warning, ok (%v)\n%s", names, want, out)
		}
	}
	unknown, ok := -1, -1
	for i, name := range names {
		if name == "eu-west-1" {
			unknown = i
		}
		if name == "prod-1" {
			ok = i
		}
	}
	if unknown > ok {
		t.Errorf("the unknown cluster (%d) is sorted below the ok one (%d)\n%s", unknown, ok, out)
	}
}

// The reason, not a colour: the message the control plane wrote, and no ANSI.
func TestTheLineNamesTheReasonNotAColour(t *testing.T) {
	h := healthFourStateFleet(t, time.Now())
	out, _, err := healthViewAt(t, h)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("the view uses colours:\n%q", out)
	}
	for _, row := range healthTableRows(t, out) {
		if row[4] == "" {
			t.Errorf("%s carries no reason", row[0])
		}
	}
	for name, reason := range map[string]string{
		"staging":   "no backup target configured",
		"eu-west-1": "silent for 14m",
		"prod-2":    "certificate expires in 19d",
	} {
		if !strings.Contains(out, reason) {
			t.Errorf("the reason %q for %s is not in the view:\n%s", reason, name, out)
		}
	}
}

// The published day-2 example, in the order the plan requires (worst first),
// field for field: name, bundle, tier, state, reason.
func TestTheTableMatchesThePublishedDayTwoExample(t *testing.T) {
	h := healthFourStateFleet(t, time.Now())
	out, _, err := healthViewAt(t, h)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	want := [][]string{
		{"staging", "Platform 1.0", "single-server", "critical", "no backup target configured"},
		{"eu-west-1", "Platform 1.1", "ha", "unknown", "silent for 14m"},
		{"prod-2", "Platform 1.1", "ha", "warning", "certificate expires in 19d"},
		{"prod-1", "Platform 1.1", "single-server", "ok", "drill passed 2d ago"},
	}
	got := healthTableRows(t, out)
	if len(got) != len(want) {
		t.Fatalf("rows = %d, want %d:\n%s", len(got), len(want), out)
	}
	for i := range want {
		for column := range want[i] {
			if got[i][column] != want[i][column] {
				t.Errorf("row %d column %d = %q, want %q\n%s", i, column, got[i][column], want[i][column], out)
			}
		}
	}
}

// S10's planted negative: with nothing to deliver to and nothing beating, the
// first line says so, before any verdict that could be mistaken for an alarm.
func TestTheFirstLineNamesAMissingAlertDestinationAndHeartbeat(t *testing.T) {
	h := healthFourStateFleet(t, time.Now())
	h.alerting = api.InstanceAlerting{Destinations: 0, HeartbeatConfigured: false}

	out, _, err := healthViewAt(t, h)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	first := healthFirstLine(out)
	lower := strings.ToLower(first)
	if !strings.Contains(lower, "destination") {
		t.Errorf("the first line does not name the missing destination: %q", first)
	}
	if !strings.Contains(lower, "heartbeat") {
		t.Errorf("the first line does not name the missing heartbeat: %q", first)
	}
	if strings.Contains(first, "staging") {
		t.Errorf("the first line is a cluster row, not the notice: %q", first)
	}
	if len(healthTableNames(t, out)) != 4 {
		t.Errorf("the fleet did not follow the notice:\n%s", out)
	}
}

func TestFirstLineIsAbsentWhenBothAreConfigured(t *testing.T) {
	h := healthFourStateFleet(t, time.Now())
	out, _, err := healthViewAt(t, h)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	first := healthFirstLine(out)
	if !healthLooksLikeRow(first) {
		t.Errorf("the view opens with a notice even though both are configured: %q", first)
	}
	if names := healthTableNames(t, out); names[0] != "staging" {
		t.Errorf("the first row is %q, want the worst cluster; the view opened with:\n%s", names[0], first)
	}
}

// healthLooksLikeRow reports whether a line carries the fleet table's columns.
func healthLooksLikeRow(line string) bool {
	return len(healthColumnSplit.Split(line, -1)) >= 5
}

// A view that cannot say whether alerts have anywhere to go must say THAT,
// rather than open as if everything were in order.
func TestTheFirstLineSaysWhenTheAlertingStatusCouldNotBeRead(t *testing.T) {
	h := healthFourStateFleet(t, time.Now())
	h.alertingStatus = http.StatusInternalServerError
	h.alertingBody = `{"detail": "alerting unavailable"}`

	out, code, err := healthViewAt(t, h)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	first := strings.ToLower(healthFirstLine(out))
	if !strings.Contains(first, "alerting") {
		t.Errorf("the first line does not mention the alerting status: %q", first)
	}
	// The fleet is still the answer, and its verdict still decides the code.
	if code != 2 || len(healthTableNames(t, out)) != 4 {
		t.Errorf("code = %d, rows = %v; the fleet view did not survive an unreadable alerting status:\n%s",
			code, healthTableNames(t, out), out)
	}
}

// healthFirstLine is the first line with anything on it.
func healthFirstLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// --- the exit codes ------------------------------------------------------------

// Four fixtures, four exact codes. The numbers are NOT in severity order, which
// is why this is a mapping and never a maximum.
func TestExitCodeIsZeroOneTwoThreeForHealthyWarningCriticalUnknown(t *testing.T) {
	now := time.Now()
	cases := []struct {
		state    string
		code     int
		problems []api.HealthCheck
	}{
		{"ok", 0, nil},
		{"warning", 1, []api.HealthCheck{healthFixtureCheck("certificates", "warning", "CERT_EXPIRING", "certificate expires in 19d", nil, now)}},
		{"critical", 2, []api.HealthCheck{healthFixtureCheck("backup", "critical", "NO_BACKUP_TARGET", "no backup target configured", nil, now)}},
		{"unknown", 3, []api.HealthCheck{healthFixtureCheck("clock", "unknown", "SILENT", "silent for 14m", nil, now)}},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			row := healthFixtureRow("prod-1", tc.state, tc.code, tc.problems, healthTwoMinutesAgo())
			h := healthControlPlaneFor(row)
			_, code, err := healthViewAt(t, h)
			if err != nil {
				t.Fatalf("health: %v", err)
			}
			if code != tc.code {
				t.Errorf("exit code for %s = %d, want %d", tc.state, code, tc.code)
			}
		})
	}
}

// The CLI's derived fleet code and the control plane's own number are one
// contract (T2.6). This is the test that keeps them from drifting: the expected
// codes below are written out, not taken from the implementation.
func TestTheCLIsExitCodeAgreesWithTheAPICodeForAllFourStates(t *testing.T) {
	now := time.Now()
	apiCodes := map[string]int{"ok": 0, "warning": 1, "critical": 2, "unknown": 3}
	for state, apiCode := range apiCodes {
		t.Run(state, func(t *testing.T) {
			problems := []api.HealthCheck{}
			if state != "ok" {
				problems = append(problems, healthFixtureCheck("nodes", state, "CHECK", "the check's own reason", nil, now))
			}
			row := healthFixtureRow("prod-1", state, apiCode, problems, healthTwoMinutesAgo())
			h := healthControlPlaneFor(row)
			h.fleet.ExitCode = apiCode

			// The fleet view derives its code from the worst row state.
			out, derived, err := healthViewAt(t, h)
			if err != nil {
				t.Fatalf("health: %v", err)
			}
			if derived != apiCode {
				t.Errorf("the CLI derived %d for a %s fleet, and the control plane says %d\n%s", derived, state, apiCode, out)
			}

			// The single-cluster view reports the control plane's number.
			single, singleCode, err := healthViewAt(t, h, "--cluster", "prod-1")
			if err != nil {
				t.Fatalf("health --cluster: %v", err)
			}
			if singleCode != apiCode {
				t.Errorf("--cluster reported %d, and the control plane says %d\n%s", singleCode, apiCode, single)
			}
		})
	}
}

// An unsupported check on required protection is not a health finding, but it is
// an inability to judge: the plan folds it to unknown, and so does this.
func TestAnUnsupportedApplicableCheckCountsAsUnknownForTheExitCode(t *testing.T) {
	now := time.Now()
	unsupported := healthFixtureCheck("backup", "unsupported", "CHECK_NOT_DECLARED",
		"This cluster runs bundle 1.0, whose manifest does not declare the backup check, so no "+
			"threshold for it exists and it was not evaluated on this report. Run `kubenest platform "+
			"upgrade --to 1.2` to evaluate it.", map[string]any{"bundle_version": "1.0"}, now)

	cases := []struct {
		name     string
		status   string
		problems []api.HealthCheck
		want     int
	}{
		{"a required check the bundle does not declare", "ok", []api.HealthCheck{unsupported}, 3},
		{"an unsupported state arriving as the cluster's own", "unsupported", nil, 3},
		{"a check this cluster's bundle never declared is left out", "ok",
			[]api.HealthCheck{healthFixtureCheck("bundle", "unsupported", "CHECK_NOT_DECLARED",
				"This cluster's bundle does not declare the bundle check.", nil, now)}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := healthFixtureRow("prod-1", tc.status, 0, tc.problems, healthTwoMinutesAgo())
			out, code, err := healthViewAt(t, healthControlPlaneFor(row))
			if err != nil {
				t.Fatalf("health: %v", err)
			}
			if code != tc.want {
				t.Errorf("exit code = %d, want %d\n%s", code, tc.want, out)
			}
		})
	}
}

// A check that cannot apply is left out of the verdict, and still rendered with
// the reason the producer gave. Anything else would either hide it or turn it
// into a fault.
func TestANotApplicableGroupIsRenderedWithItsReasonAndLeftOutOfTheVerdict(t *testing.T) {
	row := healthFixtureRow("prod-1", "ok", 0, []api.HealthCheck{
		healthFixtureCheck("certificates", "not_applicable", "NOT_APPLICABLE",
			"this cluster runs no workloads of its own, so there are no certificates to watch", nil, time.Now()),
	}, healthTwoMinutesAgo())
	h := healthControlPlaneFor(row)

	out, code, err := healthViewAt(t, h)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0: a not-applicable check is left out of the verdict\n%s", code, out)
	}

	single, _, err := healthViewAt(t, h, "--cluster", "prod-1")
	if err != nil {
		t.Fatalf("health --cluster: %v", err)
	}
	var found *healthGroupRow
	groups := healthGroupRows(t, single)
	for i := range groups {
		if groups[i].name == "certificates" {
			found = &groups[i]
		}
	}
	if found == nil {
		t.Fatalf("the not-applicable group is not rendered:\n%s", single)
	}
	if found.state != "not_applicable" {
		t.Errorf("state = %q, want not_applicable", found.state)
	}
	if !strings.Contains(found.message, "no certificates to watch") {
		t.Errorf("the group's own reason is not rendered: %q", found.message)
	}
}

// Required protection can never be ruled out by the thing being judged. A
// `backup` or `os_patching` group arriving not_applicable is a defect the view
// says out loud, and never a clean line.
func TestRequiredProtectionIsNeverAcceptedAsNotApplicable(t *testing.T) {
	for _, name := range []string{"backup", "os_patching"} {
		t.Run(name, func(t *testing.T) {
			claim := healthFixtureCheck(name, "not_applicable", "NOT_APPLICABLE",
				"the operator says this does not apply here", nil, time.Now())
			row := healthFixtureRow("prod-1", "ok", 0, []api.HealthCheck{claim}, healthTwoMinutesAgo())
			h := healthControlPlaneFor(row)

			out, code, err := healthViewAt(t, h)
			if err != nil {
				t.Fatalf("health: %v", err)
			}
			if code != 3 {
				t.Errorf("exit code = %d, want 3: %s cannot be ruled out by the cluster it protects\n%s", code, name, out)
			}
			if !strings.Contains(strings.ToLower(out), "defect") {
				t.Errorf("the view does not say the answer is a defect:\n%s", out)
			}

			single, _, err := healthViewAt(t, h, "--cluster", "prod-1")
			if err != nil {
				t.Fatalf("health --cluster: %v", err)
			}
			rows := healthGroupRows(t, single)
			if len(rows) != 1 {
				t.Fatalf("groups = %d, want the one claimed check:\n%s", len(rows), single)
			}
			if rows[0].state != "not_applicable" {
				t.Errorf("state = %q, want the word the control plane sent", rows[0].state)
			}
			if !strings.Contains(strings.ToLower(rows[0].message), "defect") {
				t.Errorf("the group line reads as a clean answer: %q", rows[0].message)
			}
		})
	}
}

// `--cluster C` reports the control plane's own number, even when the checks in
// the same answer would fold to something else: two derivations of one number is
// two answers waiting to disagree.
func TestTheSingleClusterExitCodeComesFromTheAPIAndIsNotReDerived(t *testing.T) {
	now := time.Now()
	row := healthFixtureRow("prod-2", "warning", 1, []api.HealthCheck{
		healthFixtureCheck("certificates", "warning", "CERT_EXPIRING", "certificate expires in 19d", nil, now),
	}, healthTwoMinutesAgo())
	// The detail disagrees with the row on purpose: these checks fold to critical.
	critical := healthFixtureCheck("backup", "critical", "NO_BACKUP_TARGET", "no backup target configured", nil, now)
	h := healthControlPlaneFor(row).withDetail(row.ClusterID, critical)

	out, code, err := healthViewAt(t, h, "--cluster", "prod-2")
	if err != nil {
		t.Fatalf("health --cluster: %v", err)
	}
	if code != 1 {
		t.Errorf("exit code = %d, want the control plane's own 1\n%s", code, out)
	}
	if !strings.Contains(out, "exit code 1") {
		t.Errorf("the view does not print the control plane's code:\n%s", out)
	}

	document := healthJSON(t, mustHealthJSON(t, h, "--cluster", "prod-2"))
	if got := document["exit_code"]; got != float64(1) {
		t.Errorf("the document's exit_code = %v, want the control plane's 1", got)
	}
}

// The codes are 2, 3, 1, 0 and the order is critical, unknown, warning, ok, so a
// fleet verdict taken as the numeric maximum would report a fleet holding a
// critical cluster as merely unknown — which the reader has no way to notice.
func TestTheFleetCodeIsTheWorstStateNotTheNumericMaximumOfTheCodes(t *testing.T) {
	now := time.Now()
	critical := healthFixtureRow("prod-1", "critical", 2, []api.HealthCheck{
		healthFixtureCheck("backup", "critical", "NO_BACKUP_TARGET", "no backup target configured", nil, now),
	}, healthTwoMinutesAgo())
	unknown := healthFixtureRow("prod-2", "unknown", 3, []api.HealthCheck{
		healthFixtureCheck("clock", "unknown", "SILENT", "silent for 14m", nil, now),
	}, healthTwoMinutesAgo())

	out, code, err := healthViewAt(t, healthControlPlaneFor(critical, unknown))
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if code != 2 {
		t.Errorf("exit code = %d, want 2: 3 is unknown, not worse than critical\n%s", code, out)
	}
}

// A fleet nobody has assessed has not been found healthy.
func TestAnEmptyFleetIsUnknownAndSaysSo(t *testing.T) {
	out, code, err := healthViewAt(t, healthControlPlaneFor())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if code != 3 {
		t.Errorf("exit code = %d, want 3 for a fleet with nothing in it\n%s", code, out)
	}
	if !strings.Contains(strings.ToLower(out), "no clusters") {
		t.Errorf("the view does not say there is nothing to assess:\n%s", out)
	}
}

// --- the machine-readable document ---------------------------------------------

func mustHealthJSON(t *testing.T, h *healthControlPlane, args ...string) string {
	t.Helper()
	out, _, err := healthViewAt(t, h, append([]string{"--json"}, args...)...)
	if err != nil {
		t.Fatalf("health --json: %v\n%s", err, out)
	}
	return out
}

// The document is the table's data, not a second opinion about it.
func TestJSONOutputMatchesTheTable(t *testing.T) {
	h := healthFourStateFleet(t, time.Now())

	table, tableCode, err := healthViewAt(t, h)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	documentOutput := mustHealthJSON(t, h)
	document := healthJSON(t, documentOutput)
	clusters := healthJSONClusters(t, document)

	rows := healthTableRows(t, table)
	if len(rows) != len(clusters) {
		t.Fatalf("the table has %d rows and the document %d clusters:\n%s", len(rows), len(clusters), table)
	}
	for i, row := range rows {
		if name := clusters[i]["cluster_name"]; name != row[0] {
			t.Errorf("cluster %d is %v in the document and %q in the table", i, name, row[0])
		}
		if status := clusters[i]["status"]; status != row[3] {
			t.Errorf("cluster %q is %v in the document and %q in the table", row[0], status, row[3])
		}
		if reason := clusters[i]["reason"]; reason != row[4] {
			t.Errorf("cluster %q's reason is %v in the document and %q in the table", row[0], reason, row[4])
		}
	}
	if code := document["exit_code"]; code != float64(tableCode) {
		t.Errorf("the document's exit_code = %v and the table's run exited %d", code, tableCode)
	}
}

// The field names ARE the contract: Crest's monitoring reads this document with
// no mapping pass, so a renamed tag is a broken integration, not a refactor.
func TestJSONIsStableAndDocumented(t *testing.T) {
	h := healthFourStateFleet(t, time.Now())
	document := healthJSON(t, mustHealthJSON(t, h))

	healthWantsKeys(t, "the document", document,
		"org_id", "exit_code", "total", "counts", "alerting", "alerting_error", "clusters")

	alerting, ok := document["alerting"].(map[string]any)
	if !ok {
		t.Fatalf("alerting is not an object: %v", document["alerting"])
	}
	healthWantsKeys(t, "alerting", alerting, "destinations", "heartbeat_configured", "backlog", "failed_destinations")

	clusters := healthJSONClusters(t, document)
	if len(clusters) == 0 {
		t.Fatal("no clusters in the document")
	}
	healthWantsKeys(t, "a cluster", clusters[0],
		"cluster_id", "cluster_name", "status", "exit_code", "reason",
		"bundle_version", "ha_tier", "last_reported_at", "groups", "extras", "detail_read_error")

	extras, ok := clusters[0]["extras"].(map[string]any)
	if !ok {
		t.Fatalf("extras is not an object: %v", clusters[0]["extras"])
	}
	healthWantsKeys(t, "the extras", extras,
		"last_restore_drill_at", "pending_reboot_nodes", "paused_projects", "operation")

	groups, ok := clusters[0]["groups"].([]any)
	if !ok || len(groups) == 0 {
		t.Fatalf("groups is not a non-empty array: %v", clusters[0]["groups"])
	}
	group, ok := groups[0].(map[string]any)
	if !ok {
		t.Fatalf("a group is not an object: %v", groups[0])
	}
	healthWantsKeys(t, "a group", group, "check", "status", "reason_code", "message", "detail", "evidence")

	if exitCode, ok := clusters[0]["exit_code"].(float64); !ok || exitCode != 2 {
		t.Errorf("the critical cluster's exit_code = %v, want the control plane's 2", clusters[0]["exit_code"])
	}
}

// The extras the plan requires ride along per cluster, with a null — not a zero
// — for anything the control plane did not report.
func TestTheMachineReadableExtrasCarryTheDrillRebootsOperationAndPausedProjects(t *testing.T) {
	now := time.Now()
	row := healthFixtureRow("prod-1", "ok", 0, nil, healthTwoMinutesAgo())
	h := healthControlPlaneFor(row).withDetail(row.ClusterID,
		healthFixtureCheck("backup", "ok", "OK", "Backups current.", map[string]any{
			"last_restore_drill_at": now.Add(-49 * time.Hour).UTC().Format(time.RFC3339),
			"paused_projects":       3,
		}, now),
		healthFixtureCheck("os_patching", "ok", "OK", "Hosts are current on OS patches.",
			map[string]any{"pending_reboot_nodes": 1}, now),
		healthFixtureCheck("upgrade", "ok", "OK", "No upgrade in progress.", map[string]any{
			"operation_id":   "0192f0c4-0000-7000-8000-00000000b001",
			"stage":          "apply",
			"state":          "in-progress",
			"resume_command": "kubenest platform upgrade --resume",
		}, now),
	)

	document := healthJSON(t, mustHealthJSON(t, h))
	clusters := healthJSONClusters(t, document)
	extras, ok := clusters[0]["extras"].(map[string]any)
	if !ok {
		t.Fatalf("extras is not an object: %v", clusters[0]["extras"])
	}
	if paused := extras["paused_projects"]; paused != float64(3) {
		t.Errorf("paused_projects = %v, want the reported 3", paused)
	}
	if reboots := extras["pending_reboot_nodes"]; reboots != float64(1) {
		t.Errorf("pending_reboot_nodes = %v, want the reported 1", reboots)
	}
	if drill := extras["last_restore_drill_at"]; drill == nil {
		t.Error("last_restore_drill_at is null, and the control plane reported it")
	}
	operation, ok := extras["operation"].(map[string]any)
	if !ok {
		t.Fatalf("operation = %v, want the reported record", extras["operation"])
	}
	if operation["operation_id"] != "0192f0c4-0000-7000-8000-00000000b001" || operation["stage"] != "apply" {
		t.Errorf("operation = %v, want the record's own fields", operation)
	}

	// The single-cluster view reads the same facts, so the two cannot disagree.
	single, _, err := healthViewAt(t, h, "--cluster", "prod-1")
	if err != nil {
		t.Fatalf("health --cluster: %v", err)
	}
	for key, want := range map[string]string{
		"last restore drill": "2d ago",
		"pending reboots":    "1",
		"paused projects":    "3",
		"operation":          "0192f0c4-0000-7000-8000-00000000b001",
	} {
		if line := healthExtraLine(single, key); !strings.Contains(line, want) {
			t.Errorf("the %s line is %q, want it to carry %q", key, line, want)
		}
	}
}

// --- one cluster ---------------------------------------------------------------

// Every group, with its own state, its reason, how old its evidence is and the
// fix the control plane attached to it.
func TestOneClusterExpandsEveryGroupWithReasonEvidenceAgeAndFix(t *testing.T) {
	now := time.Now()
	measured := now.Add(-2 * time.Minute)
	row := healthFixtureRow("prod-1", "warning", 1, nil, healthTwoMinutesAgo())
	h := healthControlPlaneFor(row).withDetail(row.ClusterID,
		healthFixtureCheck("nodes", "ok", "OK", "Every node is Ready.", nil, measured),
		healthFixtureCheck("certificates", "warning", "CERT_EXPIRING",
			"certificate expires in 19d; renew it before it does.", nil, measured),
		healthFixtureCheck("backup", "unsupported", "CHECK_NOT_DECLARED",
			"This cluster runs bundle 1.0, whose manifest does not declare the backup check. "+
				"Run `kubenest platform upgrade --to 1.2` to evaluate it.",
			map[string]any{"bundle_version": "1.0"}, measured),
		healthFixtureCheck("clock", "unknown", "CLOCK_UNKNOWN",
			"the clock comparison could not be made on this report", nil, measured),
	)

	out, _, err := healthViewAt(t, h, "--cluster", "prod-1")
	if err != nil {
		t.Fatalf("health --cluster: %v", err)
	}
	groups := healthGroupRows(t, out)
	if len(groups) != 4 {
		t.Fatalf("groups = %d, want the four reported:\n%s", len(groups), out)
	}
	byName := map[string]healthGroupRow{}
	for _, group := range groups {
		byName[group.name] = group
	}
	for name, state := range map[string]string{
		"nodes": "ok", "certificates": "warning", "backup": "unsupported", "clock": "unknown",
	} {
		group, ok := byName[name]
		if !ok {
			t.Fatalf("group %s is not expanded:\n%s", name, out)
		}
		if group.state != state {
			t.Errorf("%s's state = %q, want %q", name, group.state, state)
		}
		if group.reasonCode == "" || group.reasonCode == healthNotReported {
			t.Errorf("%s carries no reason code: %q", name, group.reasonCode)
		}
		if !strings.Contains(group.evidence, "2m ago") {
			t.Errorf("%s's evidence age = %q, want the measurement time", name, group.evidence)
		}
		if !strings.Contains(group.message, "renew it") && name == "certificates" {
			t.Errorf("certificates' line does not carry its reason and fix: %q", group.message)
		}
	}
	if !strings.Contains(byName["backup"].message, "kubenest platform upgrade --to 1.2") {
		t.Errorf("the unsupported group does not carry the upgrade as its fix: %q", byName["backup"].message)
	}
}

// Worst first, with the two answers that are not verdicts after the verdicts.
func TestASingleClusterOrdersItsGroupsWorstFirst(t *testing.T) {
	now := time.Now()
	row := healthFixtureRow("prod-1", "warning", 1, nil, healthTwoMinutesAgo())
	h := healthControlPlaneFor(row).withDetail(row.ClusterID,
		healthFixtureCheck("planned_reboot", "not_applicable", "NOT_APPLICABLE", "no reboot is planned", nil, now),
		healthFixtureCheck("backup", "ok", "OK", "Backups current.", nil, now),
		healthFixtureCheck("bundle", "unsupported", "CHECK_NOT_DECLARED", "the bundle does not declare this check", nil, now),
		healthFixtureCheck("nodes", "critical", "NODE_NOT_READY", "one node is not Ready", nil, now),
		healthFixtureCheck("certificates", "warning", "CERT_EXPIRING", "certificate expires in 19d", nil, now),
		healthFixtureCheck("clock", "unknown", "CLOCK_UNKNOWN", "the clock comparison could not be made", nil, now),
	)

	out, _, err := healthViewAt(t, h, "--cluster", "prod-1")
	if err != nil {
		t.Fatalf("health --cluster: %v", err)
	}
	var got []string
	for _, group := range healthGroupRows(t, out) {
		got = append(got, group.name)
	}
	want := []string{"nodes", "clock", "certificates", "backup", "bundle", "planned_reboot"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("group order = %v, want %v (critical, unknown, warning, ok, then the two that are not verdicts)\n%s",
			got, want, out)
	}
}

// A key the control plane did not send is not a zero and not an absence of
// problems: it is "not reported".
func TestAnAbsentDetailKeyRendersUnknownWithAReasonAndNeverNone(t *testing.T) {
	now := time.Now()
	row := healthFixtureRow("prod-1", "ok", 0, nil, healthTwoMinutesAgo())
	h := healthControlPlaneFor(row).withDetail(row.ClusterID,
		healthFixtureCheck("backup", "ok", "OK", "Backups current.", map[string]any{}, now),
		healthFixtureCheck("os_patching", "ok", "OK", "Hosts are current on OS patches.", map[string]any{}, now),
		healthFixtureCheck("upgrade", "ok", "OK", "No upgrade in progress.", map[string]any{}, now),
	)

	out, _, err := healthViewAt(t, h, "--cluster", "prod-1")
	if err != nil {
		t.Fatalf("health --cluster: %v", err)
	}
	for _, key := range []string{"last restore drill", "pending reboots", "paused projects", "operation"} {
		line := healthExtraLine(out, key)
		if line == "" {
			t.Fatalf("no %s line in:\n%s", key, out)
		}
		if !strings.Contains(line, healthNotReported) {
			t.Errorf("the %s line is %q, want %q rather than a value the control plane never sent",
				key, line, healthNotReported)
		}
	}
	if strings.Contains(out, "none") {
		t.Errorf("the view renders an absent fact as \"none\":\n%s", out)
	}

	// The document says the same thing, with a null rather than a zero.
	clusters := healthJSONClusters(t, healthJSON(t, mustHealthJSON(t, h, "--cluster", "prod-1")))
	extras, ok := clusters[0]["extras"].(map[string]any)
	if !ok {
		t.Fatalf("extras is not an object: %v", clusters[0]["extras"])
	}
	for _, key := range []string{"last_restore_drill_at", "pending_reboots", "paused_projects", "operation"} {
		if key == "pending_reboots" {
			key = "pending_reboot_nodes"
		}
		if extras[key] != nil {
			t.Errorf("%s = %v, want null for a fact that was not reported", key, extras[key])
		}
	}
}

// An unknown check is never rendered as ok, and a group with no message carries
// a reason rather than a blank.
func TestAnUnknownGroupIsNeverShownAsOk(t *testing.T) {
	row := healthFixtureRow("prod-1", "unknown", 3, nil, healthTwoMinutesAgo())
	h := healthControlPlaneFor(row).withDetail(row.ClusterID,
		healthFixtureCheck("clock", "unknown", "", "", nil, time.Time{}),
		healthFixtureCheck("agent", "", "", "", nil, time.Time{}),
	)

	out, _, err := healthViewAt(t, h, "--cluster", "prod-1")
	if err != nil {
		t.Fatalf("health --cluster: %v", err)
	}
	groups := healthGroupRows(t, out)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want the two reported:\n%s", len(groups), out)
	}
	for _, group := range groups {
		if group.state == "ok" {
			t.Errorf("the group %s reads as ok although it carries no ok: %q", group.name, group.message)
		}
		if group.state != "unknown" {
			t.Errorf("the group %s reads %q, want unknown", group.name, group.state)
		}
		if !strings.Contains(group.message, healthNotReported) {
			t.Errorf("the group %s with no message reads as a blank one: %q", group.name, group.message)
		}
		if !strings.Contains(group.reasonCode, healthNotReported) {
			t.Errorf("the group %s with no reason code reads as a blank one: %q", group.name, group.reasonCode)
		}
	}
}

// The negative the bead names: a cluster that never reported is shown as such.
func TestAClusterThatNeverReportedIsShownAsSuchNotAsHealthy(t *testing.T) {
	row := healthFixtureRow("new-cluster", "unknown", 3, nil, nil)
	h := healthControlPlaneFor(row).withDetail(row.ClusterID)

	out, code, err := healthViewAt(t, h)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	rows := healthTableRows(t, out)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the one cluster:\n%s", len(rows), out)
	}
	if rows[0][3] != "unknown" {
		t.Errorf("state = %q, want unknown for a cluster that has never reported", rows[0][3])
	}
	if !strings.Contains(strings.ToLower(rows[0][4]), "never reported") {
		t.Errorf("the reason is %q, want it to say the cluster has never reported", rows[0][4])
	}
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

// The control plane's own cluster is a cluster record like any other.
func TestTheControlPlanesOwnClusterAppearsLikeAnyOther(t *testing.T) {
	now := time.Now()
	controlPlaneCluster := healthFixtureRow("kubenest-control-plane", "critical", 2, []api.HealthCheck{
		healthFixtureCheck("backup", "critical", "NO_BACKUP_TARGET", "no backup target configured", nil, now),
	}, healthTwoMinutesAgo())
	prod := healthFixtureRow("prod-1", "ok", 0, nil, healthTwoMinutesAgo())

	out, code, err := healthViewAt(t, healthControlPlaneFor(controlPlaneCluster, prod))
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	names := healthTableNames(t, out)
	if len(names) != 2 || names[0] != "kubenest-control-plane" {
		t.Errorf("rows = %v, want the control plane's own cluster first, like any other", names)
	}
	if !strings.Contains(out, "no backup target configured") {
		t.Errorf("the control plane's own cluster was not rendered like the rest:\n%s", out)
	}
	if code != 2 {
		t.Errorf("exit code = %d, want the fleet's 2", code)
	}
}

// --- refusals ------------------------------------------------------------------

func TestAnUnknownClusterNameIsRefusedByName(t *testing.T) {
	h := healthControlPlaneFor(healthFixtureRow("prod-1", "ok", 0, nil, healthTwoMinutesAgo()))
	out, _, err := healthViewAt(t, h, "--cluster", "nosuch-cluster")
	if err == nil {
		t.Fatalf("an unknown cluster name was accepted:\n%s", out)
	}
	if !strings.Contains(err.Error(), "nosuch-cluster") {
		t.Errorf("the refusal does not name the cluster: %v", err)
	}
	if strings.Contains(out, "check") && strings.Contains(out, "state") {
		t.Errorf("a group table was printed for a cluster that does not exist:\n%s", out)
	}
	// Resolved by NAME, so no cluster id was invented and asked about: the
	// control plane never saw the name in a path.
	for _, path := range h.requestedPaths() {
		if strings.Contains(path, "nosuch-cluster") {
			t.Errorf("the CLI asked the control plane about %q", path)
		}
	}
	// A refusal is a failure, and a failure is printed once. Only the verdict's
	// own exit code is silent.
	if !strings.Contains(out, "nosuch-cluster") {
		t.Errorf("the refusal was not printed:\n%s", out)
	}
}

func TestALoginProblemSaysHowToRecover(t *testing.T) {
	isolateHome(t)
	out, _, err := healthTree(t, "health")
	if err == nil {
		t.Fatal("health ran with no control plane configured")
	}
	if !strings.Contains(err.Error(), "kubenest login") {
		t.Errorf("the error %q does not say how to log in", err.Error())
	}
	if !strings.Contains(out, "kubenest login") {
		t.Errorf("the login problem was not printed:\n%s", out)
	}
}

// --- the exit code contract with main ------------------------------------------

// A verdict is not a failure: the view is printed once, and the warning is not
// repeated as an "Error:" line. The code travels as an ExitError, which is what
// main maps.
func TestAVerdictExitPrintsTheViewAndNoErrorLine(t *testing.T) {
	row := healthFixtureRow("prod-1", "critical", 2, []api.HealthCheck{
		healthFixtureCheck("backup", "critical", "NO_BACKUP_TARGET", "no backup target configured", nil, time.Now()),
	}, healthTwoMinutesAgo())
	h := healthControlPlaneFor(row)
	loggedIn(t, h.start(t))

	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"health"})
	err := root.Execute()

	var exit *ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("error = %v, want an ExitError carrying the verdict", err)
	}
	if exit.Code != 2 {
		t.Errorf("ExitError.Code = %d, want 2", exit.Code)
	}
	if !strings.Contains(out.String(), "no backup target configured") {
		t.Errorf("the view was not printed:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Error:") {
		t.Errorf("the verdict was repeated as an error line:\n%s", out.String())
	}
}

// ExitCode is the mapping cmd/kubenest/main.go uses: the verdict's own code, and
// 1 for everything else, exactly as every other command exited before this one.
func TestExitCodeIsTheVerdictForAHealthErrorAndOneForEveryOtherError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"no error", nil, 0},
		{"a critical verdict", &ExitError{Code: 2, Verdict: "critical"}, 2},
		{"an unknown verdict", &ExitError{Code: 3, Verdict: "unknown"}, 3},
		{"a wrapped verdict", fmt.Errorf("command failed: %w", &ExitError{Code: 1, Verdict: "warning"}), 1},
		{"any other error", errors.New("connection refused"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(tc.err); got != tc.want {
				t.Errorf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
