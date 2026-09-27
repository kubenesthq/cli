package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"kubenest.io/cli/pkg/api"
)

// `kubenest health` (T2.9): the fleet view on a terminal.
//
// THE ONE RULE THIS FILE EXISTS TO KEEP. A fleet view that renders "we do not
// know" as "no problems found" is the failure the whole health system exists to
// prevent. So `unknown` is a verdict in its own right, it sorts above `ok`, and
// nothing absent is ever rendered as a happy zero: a detail key the control
// plane did not send reads "not reported", never a blank and never 0.
//
// THE EXIT CODES ARE A CONTRACT SHARED WITH THE CONTROL PLANE (T2.6): critical
// 2, unknown 3, warning 1, healthy 0. They are NOT in severity order — unknown
// outranks warning in the plan's verdict order (§7.7) while its number is larger
// than critical's — so the fleet verdict is a fold over STATES that maps once at
// the end, never a maximum over the rows' numbers (max(2, 3) would report a
// fleet holding a critical cluster as merely unknown).
//
// `--cluster C` reports the control plane's own exit_code unchanged: two
// derivations of one number is two answers waiting to disagree. The fleet view
// derives its own from the worst row state, and a test asserts the two agree for
// every state.

// HealthFlags are `kubenest health`'s two flags.
type HealthFlags struct {
	// Cluster expands one cluster (by name) instead of the whole fleet.
	Cluster string
	// JSON writes the documented machine-readable document instead of the table.
	JSON bool
}

// ExitError is the process exit code a command's verdict asks for.
//
// A verdict is not a failure, so it must not read like one: the command prints
// the view itself and returns this type with its error printing silenced, and
// `ExitCode` maps it to the process status. Every other error still exits 1,
// which is the behaviour main.go had before this command existed.
type ExitError struct {
	Code int
	// Verdict is the worst state the code came from, for the message.
	Verdict string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("health: the worst verdict is %s (exit code %d)", e.Verdict, e.Code)
}

// ExitCode is the process exit code for an error from the command tree: 0 for
// none, the verdict's own code for an ExitError, and 1 for everything else.
//
// It is exported and separate from main so the rule "any other command's error
// still exits 1" is testable without forking a process.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *ExitError
	if errors.As(err, &exit) {
		return exit.Code
	}
	return 1
}

// NewHealthCommand builds `kubenest health`.
func NewHealthCommand() *cobra.Command {
	var f HealthFlags
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Show every cluster's health, worst verdict first",
		Long: `Show the fleet's health: one line per cluster, worst verdict first, with the
reason rather than a colour.

The verdicts are the control plane's own (it evaluates the reports the agents
send), and they include what is NOT known. A cluster that has gone silent, or a
check an agent could not collect, reads ` + "`unknown`" + ` and sorts ABOVE ` + "`ok`" + `: a fleet
view showing forty green and two grey is the truth, while one showing forty-two
green is the failure this exists to prevent.

The first line says so when no alert destination or heartbeat is configured:
without either, this view is a record of what nobody was told.

--json writes the same data as the table as a documented, stable document.

The exit code is the worst verdict: critical 2, unknown 3, warning 1, healthy 0.
Other control-plane and argument errors exit 1.`,
		Example: `  kubenest health
  kubenest health --cluster prod-1
  kubenest health --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := controlPlaneClient()
			if err != nil {
				return err
			}
			code, err := runHealth(cmd.Context(), client, cmd.OutOrStdout(), f)
			if err != nil {
				return err
			}
			if code != 0 {
				// The verdict IS the exit code and the view IS the output, so a
				// warning the table has already stated must not be printed again
				// as an "Error:" line. Only the verdict is silenced, and only
				// here: a real failure — a control plane that cannot be reached,
				// a cluster name that does not exist — still reaches stderr once,
				// exactly as every other command's does.
				cmd.SilenceErrors = true
				return &ExitError{Code: code, Verdict: healthVerdictForCode(code)}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&f.Cluster, "cluster", "", "expand one cluster, by name, instead of the whole fleet")
	cmd.Flags().BoolVar(&f.JSON, "json", false, "write the documented machine-readable document instead of the table")
	return cmd
}

// runHealth is the whole command behind the flag parsing, so a test (and S10's
// hardware gate) can drive it against a control plane with an injected client.
// It returns the exit code the verdict asks for.
func runHealth(ctx context.Context, client *api.Client, out io.Writer, f HealthFlags) (int, error) {
	view, err := buildHealthView(ctx, client, f)
	if err != nil {
		return 1, err
	}
	if f.JSON {
		blob, err := json.MarshalIndent(view, "", "  ")
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(out, "%s\n", blob)
		return view.ExitCode, nil
	}
	renderHealth(out, view, f.Cluster != "")
	return view.ExitCode, nil
}

// The exit-code map, and the plan's verdict order it is applied to.
//
// worst-first: critical, then unknown, then warning, then ok. Deliberately not
// sorted by the codes, which are 2, 3, 1, 0.
var (
	healthVerdictOrder = []string{"critical", "unknown", "warning", "ok"}
	healthExitCodes    = map[string]int{
		"critical": 2, "unknown": 3, "warning": 1, "ok": 0,
		// `unsupported` and `not_applicable` are per-check answers and never a
		// cluster's own state, but the map is kept total exactly as the control
		// plane's is, so no caller can receive a missing code for one.
		"unsupported": 3, "not_applicable": 0,
	}
	// The two checks whose protection can never be ruled out: backups and
	// patching. A producer that claims either is not applicable is ignored, not
	// obeyed.
	healthRequiredChecks = map[string]bool{"backup": true, "os_patching": true}
)

// healthNotReported is what an absent fact reads as. Never a blank and never a
// zero: a key nobody sent and a key that says zero are different answers.
const healthNotReported = "not reported"

// healthVerdictForCode names the state a code came from. The four codes are
// distinct, so this is a rename for the message and not a second derivation.
func healthVerdictForCode(code int) string {
	for _, state := range healthVerdictOrder {
		if healthExitCodes[state] == code {
			return state
		}
	}
	return "unknown"
}

// healthRank is where a state sits in the plan's verdict order, worst first. An
// unrecognised word ranks as `unknown`: an inability to judge is never health.
func healthRank(state string) int {
	for i, s := range healthVerdictOrder {
		if s == state {
			return i
		}
	}
	return 1
}

// healthGroupRank is where a check sits in the single-cluster view. The four
// verdicts keep the plan's order; `unsupported` and `not_applicable` follow them
// because neither is a finding about the cluster's health.
func healthGroupRank(state string) int {
	switch state {
	case "critical":
		return 0
	case "unknown":
		return 1
	case "warning":
		return 2
	case "ok":
		return 3
	case "unsupported":
		return 4
	case "not_applicable":
		return 5
	default:
		// An unrecognised word is read as unknown, so it sorts with it.
		return 1
	}
}

// healthWorstState folds states to the worst one, in the plan's order. No states
// at all is `unknown`: a fleet nobody has assessed has not been found healthy.
func healthWorstState(states []string) string {
	worst := -1
	for _, state := range states {
		if rank := healthRank(state); worst < 0 || rank < worst {
			worst = rank
		}
	}
	if worst < 0 {
		return "unknown"
	}
	return healthVerdictOrder[worst]
}

// healthCheckDefect names the one thing a check's own answer can be that the
// renderer must say out loud rather than pass on as a verdict.
//
// REQUIRED PROTECTION CAN NEVER BE RULED OUT BY THE THING BEING JUDGED. `backup`
// and `os_patching` arriving `not_applicable` is not "this cluster has nothing
// to protect"; it is a statement the control plane's own fold refuses, and a
// view that printed it as a clean line would be the failure this feature exists
// to prevent. An unrecognised word is the same kind of problem.
func healthCheckDefect(check api.HealthCheck) string {
	if healthRequiredChecks[check.Check] && check.Status == "not_applicable" {
		return "defect: " + check.Check + " is required protection and can never be not_applicable, so it cannot be read as clean"
	}
	if !healthStates[check.Status] {
		return "defect: " + strconv.Quote(check.Status) + " is not a check state, so it cannot be read as clean"
	}
	return ""
}

// healthStates is the six-state vocabulary (T2.6).
var healthStates = map[string]bool{
	"ok": true, "warning": true, "critical": true,
	"unknown": true, "unsupported": true, "not_applicable": true,
}

// foldHealthState folds one cluster's own state together with the checks it
// carries into the single verdict word the exit code is mapped from, and
// collects the defects it found on the way.
//
// WHY IT FOLDS THE CHECKS AT ALL, given the control plane already folded them:
// the fold is the contract, and this is the same fold, so the CLI cannot be
// talked into a clean verdict by an answer that contradicts it. Every state it
// considers is one the control plane's own fold considers: an unsupported
// check the cluster's bundle never declared is not a health finding unless the
// check is required, and a not-applicable answer is not one either. Where the
// two can only differ is where the answer is self-contradictory, and there the
// safe reading is the one that does not say "fine".
func foldHealthState(status string, checks []api.HealthCheck) (string, []string) {
	considered := []string{status}
	var defects []string

	switch {
	case status == "unsupported" || status == "not_applicable":
		defects = append(defects, "defect: "+strconv.Quote(status)+
			" is not a cluster verdict, so this cluster is read as unknown")
	case !healthStates[status] && status != "unknown":
		defects = append(defects, "defect: the control plane's state "+strconv.Quote(status)+
			" is not one this CLI knows, so this cluster is read as unknown")
	}

	for _, check := range checks {
		switch check.Status {
		case "not_applicable":
			if healthRequiredChecks[check.Check] {
				defects = append(defects, healthCheckDefect(check))
				considered = append(considered, "unknown")
			}
		case "unsupported":
			if healthRequiredChecks[check.Check] {
				// The cluster's bundle cannot answer the one question we are not
				// allowed to leave open, so "we could not judge it" is the
				// honest verdict rather than a green fleet.
				considered = append(considered, "unknown")
			}
		case "ok", "warning", "critical", "unknown":
			considered = append(considered, check.Status)
		default:
			defects = append(defects, healthCheckDefect(check))
			considered = append(considered, "unknown")
		}
	}
	return healthWorstState(considered), defects
}

// buildHealthView reads the control plane and folds its answer into the one
// document both renderers draw from.
func buildHealthView(ctx context.Context, client *api.Client, f HealthFlags) (api.HealthView, error) {
	if f.Cluster == "" {
		return buildFleetView(ctx, client)
	}
	return buildClusterView(ctx, client, f.Cluster)
}

// buildFleetView reads every cluster the credential can see.
//
// ORGS ARE ALL READ, not the first one. A credential is usually bound to one
// organisation, and one that is not should see its whole fleet rather than an
// arbitrary slice of it; the document names the organisation only when there is
// exactly one, because naming one of several would misdescribe the rest.
func buildFleetView(ctx context.Context, client *api.Client) (api.HealthView, error) {
	orgs, err := client.ListOrgs(ctx)
	if err != nil {
		return api.HealthView{}, err
	}

	view := api.HealthView{Counts: map[string]int{}}
	var rows []api.FleetClusterHealth
	for _, org := range orgs {
		fleet, err := client.FleetHealth(ctx, org.ID)
		if err != nil {
			return api.HealthView{}, err
		}
		if len(orgs) == 1 {
			view.OrgID = fleet.OrgID
		}
		rows = append(rows, fleet.Clusters...)
		for state, count := range fleet.Counts {
			view.Counts[state] += count
		}
	}
	view.Total = len(rows)
	view.Alerting, view.AlertingError = readInstanceAlerting(ctx, client)

	type fleetRow struct {
		view  api.ClusterView
		state string
	}
	folded := make([]fleetRow, 0, len(rows))
	states := make([]string, 0, len(rows))
	now := time.Now()
	for _, row := range rows {
		state, defects := foldHealthState(row.Status, row.Problems)
		states = append(states, state)

		cv := api.ClusterView{
			ClusterID:   row.ClusterID,
			ClusterName: row.ClusterName,
			Status:      row.Status,
			// The control plane's own number, unchanged: the document reports
			// one code per cluster and the CLI does not re-derive it.
			ExitCode:       row.ExitCode,
			BundleVersion:  row.BundleVersion,
			HATier:         row.HATier,
			LastReportedAt: row.LastReportedAt,
			Groups:         []api.HealthCheck{},
		}
		// The extras and the groups come from the detail call, because the
		// fleet route's `problems` list holds only the checks that are not ok.
		// A failure here is disclosed, never fatal: the row is the answer and
		// this is the colour around it.
		if detail, err := client.ClusterHealthDetail(ctx, row.ClusterID); err != nil {
			cv.DetailReadError = err.Error()
		} else {
			cv.Groups = sortHealthGroups(detail.Checks)
		}
		cv.Extras = healthExtras(cv.Groups)
		cv.Reason = healthClusterReason(row.Status, row.LastReportedAt, row.Problems,
			cv.Extras, cv.DetailReadError, defects, now)
		folded = append(folded, fleetRow{view: cv, state: state})
	}

	// Worst verdict first, then by name so two runs over one fleet agree.
	sort.SliceStable(folded, func(i, j int) bool {
		ri, rj := healthRank(folded[i].state), healthRank(folded[j].state)
		if ri != rj {
			return ri < rj
		}
		return folded[i].view.ClusterName < folded[j].view.ClusterName
	})
	view.Clusters = make([]api.ClusterView, 0, len(folded))
	for _, row := range folded {
		view.Clusters = append(view.Clusters, row.view)
	}
	view.ExitCode = healthExitCodes[healthWorstState(states)]
	return view, nil
}

// buildClusterView reads one cluster, resolved by name.
//
// THE EXIT CODE IS THE CONTROL PLANE'S, whatever the checks in the same answer
// look like: a reader who compares this command's status with the console's must
// get one number, and the number the control plane derived from the stored
// evaluation is the one it acts on.
func buildClusterView(ctx context.Context, client *api.Client, name string) (api.HealthView, error) {
	clusterID, err := resolveCluster(ctx, client, name)
	if err != nil {
		return api.HealthView{}, err
	}
	detail, err := client.ClusterHealthDetail(ctx, clusterID)
	if err != nil {
		return api.HealthView{}, err
	}

	groups := sortHealthGroups(detail.Checks)
	lastReported := detail.ReceivedAt
	if lastReported == nil {
		lastReported = detail.ReportedAt
	}
	_, defects := foldHealthState(detail.Status, groups)

	view := api.HealthView{
		ExitCode: detail.ExitCode,
		Total:    1,
		Counts:   map[string]int{},
	}
	if detail.Status != "" {
		view.Counts[detail.Status] = 1
	}
	cv := api.ClusterView{
		ClusterID:      detail.ClusterID,
		ClusterName:    detail.ClusterName,
		Status:         detail.Status,
		ExitCode:       detail.ExitCode,
		BundleVersion:  detail.BundleVersion,
		LastReportedAt: lastReported,
		Groups:         groups,
		Extras:         healthExtras(groups),
	}
	cv.Reason = healthClusterReason(detail.Status, lastReported, healthProblems(groups),
		cv.Extras, "", defects, time.Now())
	view.Clusters = []api.ClusterView{cv}
	view.Alerting, view.AlertingError = readInstanceAlerting(ctx, client)
	return view, nil
}

// readInstanceAlerting reads the T2.5 status behind the first line. A failure is
// carried, not swallowed: a view that cannot say whether alerts have anywhere to
// go must say THAT, rather than say nothing and look fine.
func readInstanceAlerting(ctx context.Context, client *api.Client) (*api.InstanceAlerting, string) {
	alerting, err := client.InstanceAlerting(ctx)
	if err != nil {
		return nil, err.Error()
	}
	return &alerting, ""
}

// sortHealthGroups orders checks worst-first for the single-cluster view, then
// by name so two runs over one cluster agree.
func sortHealthGroups(checks []api.HealthCheck) []api.HealthCheck {
	out := make([]api.HealthCheck, len(checks))
	copy(out, checks)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := healthGroupRank(out[i].Status), healthGroupRank(out[j].Status)
		if ri != rj {
			return ri < rj
		}
		return out[i].Check < out[j].Check
	})
	return out
}

// healthProblems is the checks that are not ok — the same list the fleet route
// sends as `problems`. The order is the view's own, worst first, so the reason
// column is the worst finding rather than whichever check the control plane
// happened to evaluate first.
func healthProblems(checks []api.HealthCheck) []api.HealthCheck {
	var out []api.HealthCheck
	for _, check := range checks {
		if check.Status != "ok" {
			out = append(out, check)
		}
	}
	return sortHealthGroups(out)
}

// healthClusterReason is the reason column: a verdict without a reason is a
// colour nobody can act on.
func healthClusterReason(status string, lastReported *time.Time, problems []api.HealthCheck,
	extras api.ClusterExtras, detailErr string, defects []string, now time.Time) string {
	problems = sortHealthGroups(problems)

	var reason string
	switch {
	case len(problems) > 0:
		problem := problems[0]
		reason = healthOrNotReported(problem.Message)
		if note := healthCheckDefect(problem); note != "" {
			reason += " [" + note + "]"
		}
	case status == "ok":
		// An `ok` cluster's reason is the recovery fact that makes it worth
		// calling ok. A drill nobody has restored is exactly the kind of thing
		// a green line must not hide, so its absence is stated.
		if extras.LastRestoreDrillAt != nil {
			reason = "drill passed " + healthAge(*extras.LastRestoreDrillAt, now)
		} else {
			reason = "restore drill " + healthNotReported
		}
	case lastReported == nil:
		// Never reported is its own answer, and never a healthy one.
		reason = "never reported"
	default:
		reason = "no check reported a problem, and the control plane's verdict is " + healthOrNotReported(status)
	}

	if detailErr != "" {
		reason += " (the cluster's health detail could not be read: " + detailErr + ")"
	}
	if len(defects) > 0 {
		reason = strings.Join(defects, " ") + " " + reason
	}
	return reason
}

// healthExtras reads the four machine-readable extras out of a cluster's checks.
//
// ONLY the keys the plan names are read (checks["backup"].detail's
// last_restore_drill_at and paused_projects, checks["os_patching"].detail's
// pending_reboot_nodes, and the upgrade group's operation record), and a key the
// control plane did not send stays nil: the renderer then says "not reported".
func healthExtras(checks []api.HealthCheck) api.ClusterExtras {
	var extras api.ClusterExtras
	for _, check := range checks {
		switch check.Check {
		case "backup":
			extras.LastRestoreDrillAt = healthDetailTime(check.Detail, "last_restore_drill_at")
			extras.PausedProjects = healthDetailCount(check.Detail, "paused_projects")
		case "os_patching":
			extras.PendingRebootNodes = healthDetailCount(check.Detail, "pending_reboot_nodes")
		case "upgrade":
			if id := healthDetailString(check.Detail, "operation_id"); id != "" {
				extras.Operation = &api.RunningOperation{
					OperationID:   id,
					Stage:         healthDetailString(check.Detail, "stage"),
					State:         healthDetailString(check.Detail, "state"),
					ResumeCommand: healthDetailString(check.Detail, "resume_command"),
				}
			}
		}
	}
	return extras
}

// healthDetailString reads a string out of a detail map. Anything else — a
// number, a bool, an absent key, JSON null — is not the fact we asked for.
func healthDetailString(detail map[string]any, key string) string {
	value, ok := detail[key]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return text
}

// healthDetailCount reads a count out of a detail map. A bool is never a count
// and neither is a float that is not whole, because a reader must not be handed
// `true` where a number was promised.
func healthDetailCount(detail map[string]any, key string) *int {
	value, ok := detail[key]
	if !ok || value == nil {
		return nil
	}
	var n int
	switch typed := value.(type) {
	case float64:
		if typed != float64(int(typed)) {
			return nil
		}
		n = int(typed)
	case int:
		n = typed
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return nil
		}
		n = parsed
	default:
		return nil
	}
	return &n
}

// healthDetailTime reads a timestamp out of a detail map. The control plane
// stores what the cluster reported, so this accepts the RFC 3339 family and
// says nothing rather than guessing when the value is anything else.
func healthDetailTime(detail map[string]any, key string) *time.Time {
	switch typed := detail[key].(type) {
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
			if when, err := time.Parse(layout, typed); err == nil {
				return &when
			}
		}
	case time.Time:
		when := typed
		return &when
	}
	return nil
}

// renderHealth draws the whole view. `single` is `--cluster C`, which is drawn
// as one cluster's groups rather than as one line of a table.
func renderHealth(out io.Writer, view api.HealthView, single bool) {
	if !single {
		renderAlertingLine(out, view)
	}

	if view.Total == 0 {
		fmt.Fprintln(out, "No clusters are registered with this control plane, so nothing has been assessed.")
		return
	}
	if single {
		renderClusterHealth(out, view.Clusters[0], time.Now())
		return
	}
	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	for _, cluster := range view.Clusters {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			cluster.ClusterName,
			healthPlatform(cluster.BundleVersion),
			healthOrNotReported(cluster.HATier),
			healthOrNotReported(cluster.Status),
			healthOrNotReported(cluster.Reason))
	}
	tw.Flush()
}

// renderAlertingLine is the first line a human reads, and it comes first for a
// reason: with nothing to deliver to and nothing beating, every verdict below it
// is a record of what nobody was told (S10's planted negative).
//
// It is one line, and only when something is missing — a fleet view that opens
// with a paragraph when nothing is wrong is a fleet view people stop reading.
func renderAlertingLine(out io.Writer, view api.HealthView) {
	if view.AlertingError != "" {
		fmt.Fprintf(out, "Warning: the instance's alerting status could not be read (%s), so whether alerts have anywhere to go is unknown.\n\n",
			view.AlertingError)
		return
	}
	if view.Alerting == nil {
		return
	}
	var missing []string
	if view.Alerting.Destinations == 0 {
		missing = append(missing, "no alert destination is configured, so alerts are recorded and never delivered")
	}
	if !view.Alerting.HeartbeatConfigured {
		missing = append(missing, "no heartbeat is configured, so a cluster that goes silent cannot be noticed")
	}
	if len(missing) > 0 {
		fmt.Fprintf(out, "Warning: %s.\n\n", strings.Join(missing, "; "))
	}
}

// renderClusterHealth is `--cluster C`: every group with its own state, its
// reason, how old its evidence is and the fix the control plane attached to it.
func renderClusterHealth(out io.Writer, cluster api.ClusterView, now time.Time) {
	// The tier is deliberately NOT on this line: the detail endpoint does not
	// carry it (the fleet route does), and printing "not reported" for a fact
	// this view never asked for would be the CLI inventing a gap.
	fmt.Fprintf(out, "%s  %s  %s  exit code %d\n",
		cluster.ClusterName,
		healthPlatform(cluster.BundleVersion),
		healthOrNotReported(cluster.Status),
		cluster.ExitCode)
	fmt.Fprintf(out, "reason: %s\n", healthOrNotReported(cluster.Reason))
	if cluster.LastReportedAt != nil {
		fmt.Fprintf(out, "last reported: %s (%s)\n",
			cluster.LastReportedAt.UTC().Format(time.RFC3339), healthAge(*cluster.LastReportedAt, now))
	} else {
		fmt.Fprintln(out, "last reported: never")
	}
	fmt.Fprintln(out)

	if len(cluster.Groups) == 0 {
		if cluster.DetailReadError != "" {
			fmt.Fprintf(out, "No groups were read: %s\n", cluster.DetailReadError)
		} else {
			fmt.Fprintln(out, "No group was reported for this cluster, so none of it has been assessed.")
		}
	} else {
		tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "check\tstate\treason_code\tevidence\tmessage")
		for _, group := range cluster.Groups {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				healthOrNotReported(group.Check),
				healthGroupState(group),
				healthOrNotReported(group.ReasonCode),
				healthEvidenceAge(group.Evidence, now),
				healthGroupMessage(group))
		}
		tw.Flush()
	}
	fmt.Fprintln(out)

	fmt.Fprintln(out, "extras")
	fmt.Fprintf(out, "  last restore drill: %s\n", healthTimeFact(cluster.Extras.LastRestoreDrillAt, now))
	fmt.Fprintf(out, "  pending reboots: %s\n", healthCountFact(cluster.Extras.PendingRebootNodes))
	fmt.Fprintf(out, "  paused projects: %s\n", healthCountFact(cluster.Extras.PausedProjects))
	fmt.Fprintf(out, "  operation: %s\n", healthOperationFact(cluster.Extras.Operation))
}

// healthGroupState is a group's state word. Absent is unknown, never ok.
func healthGroupState(group api.HealthCheck) string {
	if group.Status == "" {
		return "unknown"
	}
	return group.Status
}

// healthGroupMessage is the group's own sentence, with the one defect the
// renderer must not pass on silently appended where it applies.
func healthGroupMessage(group api.HealthCheck) string {
	message := healthOrNotReported(group.Message)
	if note := healthCheckDefect(group); note != "" {
		message += " [" + note + "]"
	}
	return message
}

// healthEvidenceAge is how old the evidence behind a verdict is. The measurement
// time is the one that matters (a restarted collector must not make a cached
// measurement look new); the receipt is the fallback and is labelled as such.
func healthEvidenceAge(evidence map[string]any, now time.Time) string {
	if measured := healthDetailTime(evidence, "measured_at"); measured != nil {
		return "measured " + healthAge(*measured, now)
	}
	if received := healthDetailTime(evidence, "received_at"); received != nil {
		return "received " + healthAge(*received, now)
	}
	return healthNotReported
}

// healthTimeFact renders a timestamp as its age, or says it was not reported.
func healthTimeFact(when *time.Time, now time.Time) string {
	if when == nil {
		return healthNotReported
	}
	return healthAge(*when, now)
}

// healthCountFact renders a count, or says it was not reported. A reported zero
// is a zero, which is a different answer and is printed as one.
func healthCountFact(count *int) string {
	if count == nil {
		return healthNotReported
	}
	return strconv.Itoa(*count)
}

// healthOperationFact renders the in-flight operation, or says none was
// reported. A missing field within a reported operation is said out loud too:
// an operation at "not reported" stage is not an operation at stage zero.
func healthOperationFact(operation *api.RunningOperation) string {
	if operation == nil {
		return healthNotReported
	}
	return fmt.Sprintf("%s at stage %s (%s), resume with %s",
		operation.OperationID,
		healthOrNotReported(operation.Stage),
		healthOrNotReported(operation.State),
		healthOrNotReported(operation.ResumeCommand))
}

// healthPlatform labels a bundle version the way the published table does.
func healthPlatform(version string) string {
	if version == "" {
		return healthNotReported
	}
	return "Platform " + version
}

// healthOrNotReported keeps an empty fact from rendering as a blank one.
func healthOrNotReported(text string) string {
	if text == "" {
		return healthNotReported
	}
	return text
}

// healthAge renders how long ago something was, rounded down to the largest
// unit that still reads as a duration.
func healthAge(when, now time.Time) string {
	age := now.Sub(when)
	switch {
	case age < 5*time.Second:
		return "just now"
	case age < 90*time.Second:
		return fmt.Sprintf("%ds ago", int(age.Seconds()))
	case age < 90*time.Minute:
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	case age < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(age.Hours()/24))
	}
}
