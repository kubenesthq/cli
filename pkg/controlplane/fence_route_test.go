package controlplane

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/sshx"
)

// THE FENCE IS ONLY A FENCE ONCE THE ROUTE POINTS AT IT (hardware, 2026-09-25).
//
// Apply writes the HelmChart; helm-controller re-renders the route afterwards.
// A stage that returns as soon as the apply returned therefore reports "the
// fence is up" while api.<domain> still reaches the backend, and — worse — the
// unfence stage deletes the fence objects before the route points back at the
// backend, leaving the public hostname pointing at a Service that is gone. The
// gate observed exactly that: straight after `[7/7] control-plane-unfence ok`
// the route's backendRef was still `kubenest-cp-fence`.
//
// The route is a fact about a DIFFERENT object than the one the apply wrote, so
// it is observed, not assumed.

// routeLagRunner answers the reads the fence's stages make, and models the ONE
// thing that makes them necessary: the HTTPRoute is re-rendered by
// helm-controller AFTER the apply returns, so it lags by a configurable number
// of reads. What it reports is derived from the fence value the last apply
// carried, which is what the real chart does — a fake that flipped on a timer
// would prove nothing about the route.
type routeLagRunner struct {
	*componenttest.FakeRunner
	// lag is how many reads still answer with the PREVIOUS ref after an apply
	// changed the fence value.
	lag int
	// frozen makes the route ignore the applies entirely, for the arm that
	// asserts a fence whose route never switches is a failure rather than a
	// pass.
	frozen bool
	// current is the ref the route answers with now; pending is the ref the
	// last apply asked for.
	current string
	pending string
	count   int
	// routeReads records every answer the route gave, in order.
	routeReads []string
	// events is the ordered log of what the fake was asked, for ordering
	// assertions that span commands.
	events []string
	// fenceAvailable is whether the fence Deployment reports available.
	fenceAvailable bool
	// migrationRevision, when set, is the install revision the migration Job
	// answers with.
	migrationRevision string
	// revisions is the install revision each workload Deployment reports, taken
	// from the apply that stamped it.
	revisions map[string]string
}

// lastInput is the document most recently streamed to the host.
func (r *routeLagRunner) lastInput(t *testing.T) []byte {
	t.Helper()
	inputs := r.Inputs()
	if len(inputs) == 0 {
		t.Fatal("a manifest was written without streaming a document")
	}
	return inputs[len(inputs)-1]
}

// newRouteLagRunner starts with the route on ref, and a lag of `lag` reads
// before an apply's fence value is visible.
func newRouteLagRunner(t *testing.T, ref string, lag int) *routeLagRunner {
	t.Helper()
	r := &routeLagRunner{lag: lag, current: ref, fenceAvailable: true}
	r.FakeRunner = &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.HasPrefix(command, "sudo -n k3s kubectl get httproute "):
			answer := r.current
			if !r.frozen && r.pending != "" && r.pending != r.current {
				if r.count > 0 {
					r.count--
				} else {
					r.current = r.pending
					r.pending = ""
					answer = r.current
				}
			}
			r.routeReads = append(r.routeReads, answer)
			r.events = append(r.events, "route:"+answer)
			return sshx.Result{Stdout: `{"spec":{"rules":[{"backendRefs":[{"name":"` + answer + `"}]}]}}`}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl get service "+FenceService):
			return sshx.Result{Stdout: FenceService}, nil
		case strings.Contains(command, "get deployment/"+fenceName):
			available := 0
			if r.fenceAvailable {
				available = 1
			}
			return sshx.Result{Stdout: `{"metadata":{"generation":1},"spec":{"replicas":1},"status":{"observedGeneration":1,"availableReplicas":` + strconv.Itoa(available) + `}}`}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl delete deployment/"+fenceName):
			r.events = append(r.events, "delete-fence")
			return sshx.Result{}, nil
		case strings.Contains(command, "rm -f") && strings.Contains(command, fenceName+".yaml"):
			r.events = append(r.events, "rm-fence-manifest")
			return sshx.Result{}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl get job "+MigrationJobName):
			if r.migrationRevision == "" {
				return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): jobs.batch "kubenest-cp-migrate" not found`}, nil
			}
			return sshx.Result{Stdout: jobJSON(t, "Complete", r.migrationRevision)}, nil
		case strings.Contains(command, backendDeploymentImageCmd):
			return sshx.Result{Stdout: "ghcr.io/kubenesthq/kubenest-backend:132b7ea"}, nil
		case strings.Contains(command, "get certificate/") || strings.Contains(command, "get gateway/"):
			// Both conditions Ready and Programmed, so whichever the caller
			// asked for is answered: the fake must not have to know which.
			return sshx.Result{Stdout: `{"status":{"conditions":[{"type":"Ready","status":"True"},{"type":"Programmed","status":"True"}]}}`}, nil
		case strings.HasPrefix(command, "sudo -n install -m 0600 "):
			stdin := r.lastInput(t)
			switch {
			case strings.Contains(command, fenceName+".yaml"):
				return sshx.Result{}, nil
			case strings.Contains(command, ReleaseName+".yaml"):
				applied := decodeApplied(t, stdin)
				r.recordRevision(t, stdin)
				// THE ROUTE FOLLOWS THE FENCE VALUE THIS APPLY CARRIED, after
				// the lag: that is exactly what helm-controller does with
				// routes.yaml once the HelmChart lands.
				want := fenceDesiredRef(applied)
				if want != "" && want != r.current {
					r.pending, r.count = want, r.lag
				}
				return sshx.Result{}, nil
			}
			return sshx.Result{}, nil
		case strings.Contains(command, backendReplicasCmd):
			return sshx.Result{Stdout: "1"}, nil
		case command == checkpointStatusCmd:
			return sshx.Result{Stdout: checkpointMarkerJSON(t, checkpointMarker("cp/x.dump", 1))}, nil
		case command == postgresCmd:
			return sshx.Result{Stdout: postgresReadyJSON()}, nil
		case strings.Contains(command, "get deployment/") && strings.Contains(command, " -o json -n "):
			// The rolled-out check (WaitReady): the Deployment is at whatever
			// revision the last apply wrote.
			fields := strings.Fields(command)
			if len(fields) < 6 {
				t.Fatalf("unparsable read: %q", command)
			}
			name := strings.TrimPrefix(fields[5], "deployment/")
			revision, ok := r.revisions[name]
			if !ok {
				return sshx.Result{ExitCode: 1, Stderr: "not found"}, nil
			}
			return sshx.Result{Stdout: `{"metadata":{"generation":1},"spec":{"replicas":1,"template":{"metadata":{"annotations":{"kubenest.io/install-revision":"` + revision + `"}}}},"status":{"observedGeneration":1,"replicas":1,"updatedReplicas":1,"availableReplicas":1}}`}, nil
		default:
			return sshx.Result{}, nil
		}
	}}
	return r
}

// fenceDesiredRef is the Service the chart's api route names for these values.
func fenceDesiredRef(applied appliedValues) string {
	if applied.fence {
		return FenceService
	}
	return backendService
}

// recordRevision remembers the install revision the applied values carry, so a
// rollout read can answer about it rather than about a number the test chose.
func (r *routeLagRunner) recordRevision(t *testing.T, manifest []byte) {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(manifest, &doc); err != nil {
		t.Fatal(err)
	}
	spec, _ := doc["spec"].(map[string]any)
	values, _ := spec["valuesContent"].(string)
	var applied map[string]any
	if err := yaml.Unmarshal([]byte(values), &applied); err != nil {
		t.Fatal(err)
	}
	revision, ok := applied["installRevision"].(string)
	if !ok {
		return
	}
	if r.revisions == nil {
		r.revisions = map[string]string{}
	}
	for _, name := range []string{"backend", "hub", "ui"} {
		r.revisions[ReleaseName+"-"+name] = revision
	}
}

func (r *routeLagRunner) appliedValues(t *testing.T) []appliedValues {
	t.Helper()
	var out []appliedValues
	for _, run := range r.Executions() {
		if !strings.Contains(run.Command, ReleaseName+".yaml") {
			continue
		}
		out = append(out, decodeApplied(t, run.Stdin))
	}
	return out
}

func (r *routeLagRunner) eventIndex(t *testing.T, want string) int {
	t.Helper()
	for i, event := range r.events {
		if event == want {
			return i
		}
	}
	return -1
}

// The fence stage does not return until the ROUTE says the fence is up.
func TestTheFenceStageWaitsForTheRouteToReachTheFence(t *testing.T) {
	values := "domain: kn.example.com\njwtSecret: s\n"
	// The route keeps answering with the backend for three reads after the
	// apply, and only then reports the fence: exactly what helm-controller's
	// own latency looks like from here.
	runner := newRouteLagRunner(t, backendService, 3)
	s := testUpgradeSession(t, runner, values)
	s.PollInterval = time.Millisecond
	ctx := context.Background()

	if err := stageFence(ctx, s); err != nil {
		t.Fatal(err)
	}
	if report := cpStatus(t, ctx, runner); report.State != FenceUp {
		t.Fatalf("the fence stage returned with the route pointing at %q, so from the moment it said the fence was up every customer request still reached the backend", report.BackendRef)
	}
	if len(runner.routeReads) < 2 {
		t.Errorf("the route was read %d time(s): the stage did not wait for it, it happened to read it once the apply had landed", len(runner.routeReads))
	}
	if !runner.fenceAvailable {
		t.Fatal("the test drove the fence Deployment unavailable without asserting the consequence")
	}

	// And it fails rather than passing when the route NEVER reaches the fence,
	// naming the ref it saw: "the fence is up" must not be an assumption.
	none := newRouteLagRunner(t, backendService, 0)
	none.frozen = true
	s2 := testUpgradeSession(t, none, values)
	s2.Opts.PollInterval = time.Millisecond
	// A WAIT THAT IS ASSERTED TO FAIL MUST NOT SPEND THE MANIFEST'S REAL
	// MINUTE. The deadline is otherwise the bundle's `component-ready`, and it
	// still is everywhere in production: this only makes the arm that proves
	// the refusal cheap enough to sit in every `go test ./...`.
	s2.Opts.WaitDeadline = 200 * time.Millisecond
	s2.PollInterval = s2.Opts.PollInterval
	s2.WaitDeadline = s2.Opts.WaitDeadline
	err := stageFence(ctx, s2)
	if err == nil {
		t.Fatal("a fence whose route never switched was reported as up")
	}
	if !strings.Contains(err.Error(), backendService) {
		t.Errorf("the failure does not name the backendRef it observed:\n%v", err)
	}
}

// The fence's objects are deleted only once the route points BACK at the
// backend: between the unfence apply and the re-render, the route names a
// Service that must still exist.
func TestTheFenceObjectsAreDeletedOnlyAfterTheRouteIsBackOnTheBackend(t *testing.T) {
	values := "domain: kn.example.com\njwtSecret: s\n"
	runner := newRouteLagRunner(t, FenceService, 3)
	s := testUpgradeSession(t, runner, values)
	s.PollInterval = time.Millisecond

	if err := stageUnfence(context.Background(), s); err != nil {
		t.Fatal(err)
	}

	back := -1
	for i, event := range runner.events {
		if event == "route:"+backendService {
			back = i
			break
		}
	}
	if back < 0 {
		t.Fatalf("the route never reported the backend; events were %v", runner.events)
	}
	deleted := runner.eventIndex(t, "delete-fence")
	if deleted < 0 {
		t.Fatalf("the fence objects were never deleted; events were %v", runner.events)
	}
	if deleted < back {
		t.Errorf("the fence objects were deleted (event %d) before the route pointed back at the backend (event %d), so api.<domain> named a Service that was gone:\n%v",
			deleted, back, runner.events)
	}
	if removed := runner.eventIndex(t, "rm-fence-manifest"); removed >= 0 && removed < back {
		t.Errorf("the fence's durable manifest was removed (event %d) before the route pointed back at the backend (event %d)", removed, back)
	}
}

// THE MIGRATION JOB IS THE OPERATOR-VISIBLE RECORD OF THE SCHEMA STEP, and a
// chart apply that renders it off makes helm DELETE it: after the upgrade there
// was no `kubenest-cp-migrate` Job and `kubectl logs job/kubenest-cp-migrate`
// had nothing to read (hardware, 2026-09-25).
//
// The Job's pod template depends only on inputs that do not change across the
// later applies, so keeping it enabled cannot hit the immutable-field rule —
// and that is asserted here rather than assumed.
func TestTheMigrationJobOutlivesTheUpgrade(t *testing.T) {
	values := "domain: kn.example.com\njwtSecret: s\nagentJwtSecret: a\nencryptionKey: e\n" +
		"postgresql:\n  auth:\n    username: kubenest\n    database: kubenest\n"
	stopped, err := FenceValues(values, FenceOptions{Up: true, BackendReplicas: fenceInt32Value(0)})
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := MigrationValues(stopped)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := Revision(migrated)
	if err != nil {
		t.Fatal(err)
	}
	runner := newRouteLagRunner(t, backendService, 0)
	runner.migrationRevision = revision
	s := testUpgradeSession(t, runner, values)
	s.PollInterval = time.Millisecond

	ctx := context.Background()
	if err := stageFence(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := stageMigration(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := stageChart(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := stageUnfence(ctx, s); err != nil {
		t.Fatal(err)
	}

	applies := runner.appliedValues(t)
	migrationAt := -1
	for i, apply := range applies {
		if apply.migration {
			migrationAt = i
			break
		}
	}
	if migrationAt < 0 {
		t.Fatalf("no apply enabled the migration Job, so there is nothing to keep alive: %+v", applies)
	}
	base := podTemplateInputs(t, applies[migrationAt])
	for i := migrationAt; i < len(applies); i++ {
		if !applies[i].migration {
			t.Errorf("apply %d after the migration apply renders the Job OFF, so helm deletes the Job that is the operator's record of the schema step: %+v", i, applies[i])
			continue
		}
		if got := podTemplateInputs(t, applies[i]); got != base {
			t.Errorf("apply %d would change the migration Job's pod template, which is IMMUTABLE and would fail the chart apply:\n got %s\nwant %s", i, got, base)
		}
	}
	// And the fence apply BEFORE the migration must not have it on: that Job
	// would run against the old backend's live database.
	if applies[0].migration {
		t.Error("the fence apply enabled the migration Job, which runs it before anything was fenced off deliberately")
	}
}

// podTemplateInputs is the part of a values document the migration Job's pod
// template is rendered from. Everything else in the document (the fence, the
// replica count, the install revision) is metadata and is deliberately excluded:
// a change there is exactly what must be allowed.
func podTemplateInputs(t *testing.T, apply appliedValues) string {
	t.Helper()
	doc := map[string]any{}
	if err := yaml.Unmarshal([]byte(apply.raw), &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"installRevision", "fence", "backend", "migration"} {
		delete(doc, key)
	}
	body, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// fenceInt32Value is the replica count pointer the fence options take.
func fenceInt32Value(v int32) *int32 { return &v }

func cpStatus(t *testing.T, ctx context.Context, r k3s.Runner) FenceReport {
	t.Helper()
	report, err := Status(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	return report
}
