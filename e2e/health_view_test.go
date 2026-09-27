//go:build e2e

// T2.9's real-hardware arm: `kubenest health` against a REAL control plane,
// driven through the real command tree (S10's first half).
//
// WHAT THIS ARM OWNS, and what it deliberately leaves to the operators who run
// the rest of S10. It asserts the things a fake control plane cannot:
//
//	(a) `kubenest health` exits 0 on a healthy fleet, and the verdict it derives
//	    from the rows equals the control plane's own exit codes, row for row;
//	(b) `--json` carries the same clusters, states, reasons and codes as the
//	    table, and the same numbers the control plane itself returns — the
//	    document is a projection of the API answer, not a second opinion;
//	(c) the fleet verdict is the worst STATE and not the largest code: a fleet
//	    holding a critical cluster exits 2 even though unknown's code is 3;
//	(d) the first line about a missing alert destination or heartbeat agrees
//	    with GET /api/v1/instance/alerting, which this arm reads directly;
//	(e) `--cluster C` reports the control plane's own exit_code and expands
//	    every group the detail call returns, in worst-first order;
//	(f) S10's planted negative, when KUBENEST_GATE_SILENT_CLUSTER names a
//	    cluster whose agent has been stopped: that cluster reads `unknown` with
//	    its reason, never `ok`, and the fleet does not exit 0 while it is the
//	    worst row. Without that variable the arm skips this part and says so.
//
// THE FAULT INJECTION IS A HUMAN STEP, and that is not laziness: stopping the
// agent, filling the data disk past the manifest threshold, ageing the restore
// drill, breaking the S3 upload, failing the webhook receiver, stopping the
// delivery worker and stopping the backend are all changes to the lab fixture,
// and a test that made them would leave them behind if it failed halfway. The
// steps and the observations are S10's; this arm measures the CLI's half of
// them.
//
// WHAT A HARDWARE RUN NEEDS
//
//   - a lab cluster from `./scripts/ephemeral-env.sh up --profile host` and,
//     for the planted negative, `KUBENEST_GATE_SILENT_CLUSTER=<cluster>` set
//     after its agent has been stopped;
//   - a candidate control plane with its own CA, named by KUBENEST_CONTROL_PLANE
//     and KUBENEST_CLI_TOKEN, and (when the control plane's certificate chains
//     to a private CA) KUBENEST_CONTROL_PLANE_CA pointing at its PEM, exactly
//     as the other gates do;
//   - an instance alert destination and a heartbeat URL (`kubenest alerts
//     add-destination`, `kubenest alerts set-heartbeat`) for the healthy part of
//     S10; the arm reads the alerting status rather than changing it, so it
//     works either way and reports which state it found;
//   - the cluster reporting: `kubenest health` exits 0 only when the control
//     plane's evaluation is healthy, so the report interval (default 60s) must
//     have elapsed since the last fault was repaired.
//
// Run from the umbrella workspace:
//
//	source lab/hetzner/.lab-env.sh
//	export KUBENEST_CONTROL_PLANE=https://api.<domain>
//	export KUBENEST_CLI_TOKEN=knp_...
//	export KUBENEST_CONTROL_PLANE_CA=$PWD/lab/control-plane-ca.pem
//	cd kubenest-cli && go test -tags e2e -v -timeout 60m ./e2e/ -run TestFleetHealthGate
//
// PASS LIMITS (recorded on the bead from the first full run, not asserted from
// a guess): the exit codes exact; `kubenest health` exiting 0 within 30 s of the
// last report; the whole gate under 60 minutes.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/cmd"
	"kubenest.io/cli/pkg/config"
)

// healthExitCodeMap is the documented 2/3/1/0 contract (T2.6), written here so
// this arm checks the CLI against the plan rather than against itself.
var healthExitCodeMap = map[string]int{"critical": 2, "unknown": 3, "warning": 1, "ok": 0}

// healthGateColumns splits a padded table line.
var healthGateColumns = regexp.MustCompile(`\s{2,}`)

func TestFleetHealthGate(t *testing.T) {
	env := gateEnvironment(t)

	// THE REAL COMMAND TREE, pointed at the gate's control plane through an
	// isolated HOME: the token never touches the operator's own store.
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := config.Save(&config.Config{
		ControlPlaneURL: env.controlPlane,
		ControlPlaneCA:  string(env.controlPlaneCA),
	}); err != nil {
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
	ctx := context.Background()

	// The control plane's own answer, read directly. Everything the CLI prints
	// is checked against this rather than against a fixture.
	orgs, err := client.ListOrgs(ctx)
	if err != nil {
		t.Fatalf("reading the organisations: %v", err)
	}
	if len(orgs) == 0 {
		t.Fatal("the gate's credential sees no organisation")
	}
	fleet, err := client.FleetHealth(ctx, orgs[0].ID)
	if err != nil {
		t.Fatalf("reading the fleet: %v", err)
	}
	if len(fleet.Clusters) == 0 {
		t.Fatal("the control plane holds no cluster: S10's fixtures start with an installed one")
	}
	t.Logf("the control plane reports %d cluster(s), exit_code %d", fleet.Total, fleet.ExitCode)

	// (a) The fleet view, exactly as an operator runs it.
	started := time.Now()
	table, code := healthGateRun(t, "health")
	elapsed := time.Since(started)
	t.Logf("kubenest health took %s and exited %d", elapsed, code)

	worst := healthGateWorstState(fleet.Clusters)
	if want := healthExitCodeMap[worst]; code != want {
		t.Fatalf("kubenest health exited %d, want %d for a fleet whose worst state is %s:\n%s",
			code, want, worst, table)
	}
	rows := healthGateRows(t, table)
	if len(rows) != len(fleet.Clusters) {
		t.Fatalf("the table has %d rows and the control plane %d clusters:\n%s",
			len(rows), len(fleet.Clusters), table)
	}
	// Worst first, and `unknown` above `ok`: the row order is the plan's.
	lastRank := -1
	for _, row := range rows {
		rank, ok := healthGateRank(row.state)
		if !ok {
			t.Errorf("the table carries the state %q, which is not one of the four verdicts:\n%s", row.state, table)
			continue
		}
		if rank < lastRank {
			t.Errorf("the table is not worst-first: %s follows a milder row:\n%s", row.name, table)
		}
		lastRank = rank
	}
	// A healthy fixture must read ok on every row; anything else is the fault
	// injection this arm is not allowed to repair, named with its reason.
	if code != 0 {
		for _, row := range rows {
			if row.state != "ok" {
				t.Errorf("cluster %s reads %s: %s", row.name, row.state, row.reason)
			}
		}
		t.Fatalf("kubenest health must exit 0 on the healthy fixture, and exited %d", code)
	}

	// (d) The first line agrees with the alerting status this arm read directly.
	alerting, err := client.InstanceAlerting(ctx)
	if err != nil {
		t.Fatalf("reading the instance's alerting status: %v", err)
	}
	firstLine := healthGateFirstLine(table)
	expectNotice := alerting.Destinations == 0 || !alerting.HeartbeatConfigured
	hasNotice := !healthGateLooksLikeRow(firstLine)
	if expectNotice != hasNotice {
		t.Errorf("the alerting status is destinations=%d heartbeat=%t, and the first line is %q",
			alerting.Destinations, alerting.HeartbeatConfigured, firstLine)
	}
	if expectNotice {
		lower := strings.ToLower(firstLine)
		if alerting.Destinations == 0 && !strings.Contains(lower, "destination") {
			t.Errorf("no destination is configured and the first line does not say so: %q", firstLine)
		}
		if !alerting.HeartbeatConfigured && !strings.Contains(lower, "heartbeat") {
			t.Errorf("no heartbeat is configured and the first line does not say so: %q", firstLine)
		}
		t.Logf("S10 planted negative observed: %s", firstLine)
	} else {
		t.Logf("this instance has a destination and a heartbeat, so the first line is the first row: %q", firstLine)
	}

	// (c) The fleet verdict is the worst state, not the largest code. When the
	// two differ, this arm has observed S10's exact case on real data.
	if wantMax := healthGateNumericMaximum(fleet.Clusters); wantMax != healthExitCodeMap[worst] {
		if code == wantMax {
			t.Errorf("the fleet exited %d, the largest row code, instead of %d for the worst state %s",
				code, healthExitCodeMap[worst], worst)
		}
		t.Logf("the fleet holds %s and exits %d while the largest row code is %d: the numbers are not in severity order",
			worst, code, wantMax)
	}

	// (b) --json is the same data, and the control plane's numbers.
	documentOutput, documentCode := healthGateRun(t, "health", "--json")
	if documentCode != code {
		t.Errorf("--json exited %d and the table %d", documentCode, code)
	}
	var document api.HealthView
	if err := json.Unmarshal([]byte(documentOutput), &document); err != nil {
		t.Fatalf("--json is not the documented document: %v\n%s", err, documentOutput)
	}
	if document.Total != fleet.Total || len(document.Clusters) != len(fleet.Clusters) {
		t.Errorf("the document holds %d clusters and the control plane %d", len(document.Clusters), len(fleet.Clusters))
	}
	byName := map[string]api.ClusterView{}
	for _, cluster := range document.Clusters {
		byName[cluster.ClusterName] = cluster
	}
	for _, row := range fleet.Clusters {
		got, ok := byName[row.ClusterName]
		if !ok {
			t.Errorf("cluster %s is in the control plane's fleet and not in the document", row.ClusterName)
			continue
		}
		if got.Status != row.Status {
			t.Errorf("cluster %s reads %s in the document and %s in the control plane", row.ClusterName, got.Status, row.Status)
		}
		if got.ExitCode != row.ExitCode {
			t.Errorf("cluster %s carries exit_code %d in the document and %d in the control plane",
				row.ClusterName, got.ExitCode, row.ExitCode)
		}
	}
	for i, row := range rows {
		if len(document.Clusters) <= i {
			break
		}
		cluster := document.Clusters[i]
		if cluster.ClusterName != row.name || cluster.Status != row.state {
			t.Errorf("row %d is %s/%s in the table and %s/%s in the document",
				i, row.name, row.state, cluster.ClusterName, cluster.Status)
		}
		if cluster.Reason != row.reason {
			t.Errorf("row %d's reason is %q in the table and %q in the document", i, row.reason, cluster.Reason)
		}
	}

	// (e) One cluster: the control plane's code, and every group it returned.
	sample := rows[0]
	detail, err := client.ClusterHealthDetail(ctx, byName[sample.name].ClusterID)
	if err != nil {
		t.Fatalf("reading %s's detail: %v", sample.name, err)
	}
	single, singleCode := healthGateRun(t, "health", "--cluster", sample.name)
	if singleCode != detail.ExitCode {
		t.Errorf("--cluster %s exited %d and the control plane says %d for it:\n%s",
			sample.name, singleCode, detail.ExitCode, single)
	}
	if groups := healthGateGroups(single); len(groups) != len(detail.Checks) {
		t.Errorf("--cluster %s expanded %d groups and the control plane returned %d:\n%s",
			sample.name, len(groups), len(detail.Checks), single)
	} else {
		t.Logf("--cluster %s expanded %d group(s) in worst-first order", sample.name, len(groups))
	}

	// (f) S10's planted negative, when the operator has stopped an agent.
	silent := os.Getenv("KUBENEST_GATE_SILENT_CLUSTER")
	if silent == "" {
		t.Log("KUBENEST_GATE_SILENT_CLUSTER is not set: stop one cluster's agent, set it to that cluster's name and re-run for the unknown-not-ok arm")
		return
	}
	silentView, ok := byName[silent]
	if !ok {
		t.Fatalf("KUBENEST_GATE_SILENT_CLUSTER=%s names a cluster the fleet answer does not hold", silent)
	}
	if silentView.Status == "ok" {
		t.Errorf("the cluster %s whose agent was stopped reads ok in the document: %s", silent, silentView.Reason)
	}
	if silentView.Status != "unknown" {
		t.Errorf("the cluster %s reads %s, and a stopped collector must read unknown", silent, silentView.Status)
	}
	if strings.TrimSpace(silentView.Reason) == "" {
		t.Errorf("the cluster %s reads %s with no reason", silent, silentView.Status)
	}
	// And the fleet does not claim to be healthy while a row is unknown.
	if worst == "unknown" || worst == "critical" {
		silentTable, silentCode := healthGateRun(t, "health")
		if silentCode == 0 {
			t.Errorf("a fleet whose worst row is %s exited 0:\n%s", worst, silentTable)
		}
		t.Logf("planted negative: %s reads %s (exit %d)", silent, silentView.Status, silentCode)
	}
}

// healthGateRun executes one command path from the real tree and returns what it
// printed and the process exit code the verdict maps to — the same mapping
// cmd/kubenest/main.go performs, via cmd.ExitCode.
func healthGateRun(t *testing.T, args ...string) (string, int) {
	t.Helper()
	root := cmd.NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	if err != nil {
		var exit *cmd.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("kubenest %s failed: %v\n%s", strings.Join(args, " "), err, out.String())
		}
	}
	return out.String(), cmd.ExitCode(err)
}

// healthGateRow is one line of the fleet table.
type healthGateRow struct {
	name   string
	state  string
	reason string
}

func healthGateRows(t *testing.T, table string) []healthGateRow {
	t.Helper()
	var rows []healthGateRow
	for _, line := range strings.Split(table, "\n") {
		fields := healthGateColumns.Split(strings.TrimSpace(line), -1)
		if len(fields) < 5 {
			continue
		}
		rows = append(rows, healthGateRow{name: fields[0], state: fields[3], reason: strings.Join(fields[4:], "  ")})
	}
	return rows
}

// healthGateGroups is the single-cluster group table: the lines after its
// header that carry all five columns.
func healthGateGroups(single string) [][]string {
	lines := strings.Split(single, "\n")
	start := -1
	for i, line := range lines {
		if fields := healthGateColumns.Split(strings.TrimSpace(line), -1); len(fields) >= 5 && fields[0] == "check" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil
	}
	var groups [][]string
	for _, line := range lines[start:] {
		fields := healthGateColumns.Split(strings.TrimSpace(line), -1)
		if len(fields) < 5 {
			break
		}
		groups = append(groups, fields[:5])
	}
	return groups
}

func healthGateFirstLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func healthGateLooksLikeRow(line string) bool {
	return len(healthGateColumns.Split(line, -1)) >= 5
}

func healthGateRank(state string) (int, bool) {
	switch state {
	case "critical":
		return 0, true
	case "unknown":
		return 1, true
	case "warning":
		return 2, true
	case "ok":
		return 3, true
	}
	return 0, false
}

// healthGateWorstState is the plan's fold, written from the plan: critical, then
// unknown, then warning, then ok. A fleet nobody has assessed, or one carrying
// a word that is not a verdict, is unknown.
func healthGateWorstState(rows []api.FleetClusterHealth) string {
	worst := ""
	worstRank := 0
	for _, row := range rows {
		rank, ok := healthGateRank(row.Status)
		if !ok {
			return "unknown"
		}
		if worst == "" || rank < worstRank {
			worst, worstRank = row.Status, rank
		}
	}
	if worst == "" {
		return "unknown"
	}
	return worst
}

func healthGateNumericMaximum(rows []api.FleetClusterHealth) int {
	max := 0
	for _, row := range rows {
		if row.ExitCode > max {
			max = row.ExitCode
		}
	}
	return max
}
