package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/sshx"
)

// migrationJobCmd is the read migrationState makes.
var migrationJobCmd = "sudo -n k3s kubectl get job " + MigrationJobName + " -n " + Namespace + " -o json"

// postgresCmd is the read postgresReady makes.
var postgresCmd = "sudo -n k3s kubectl get statefulset " + postgresStatefulSet + " -n " + Namespace + " -o json"

// jobJSON renders the answer `kubectl get job -o json` gives for a Job that is
// complete, running, or failed, stamped with the install revision it belongs
// to. THE STAMP IS NOT OPTIONAL: the wait matches the Job to the revision the
// step applied, so a Job without one is a Job that cannot be accepted.
func jobJSON(t *testing.T, condition, revision string) string {
	t.Helper()
	status := map[string]any{"succeeded": 0, "failed": 0}
	if condition != "" {
		status["conditions"] = []any{map[string]any{
			"type": condition, "status": "True", "reason": "BackoffLimitExceeded",
			"message": "Job has reached the specified backoff limit",
		}}
		if condition == "Complete" {
			status["succeeded"] = 1
		} else {
			status["failed"] = 1
		}
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{
			"name": MigrationJobName, "namespace": Namespace,
			"annotations": map[string]any{migrationJobRevisionAnnotation: revision},
		},
		"status": status,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// migrationRevision is the install revision the migration step will apply for
// these values — the same value the chart stamps on the Job.
func migrationRevision(t *testing.T, values string) string {
	t.Helper()
	migrated, err := MigrationValues(values)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := Revision(migrated)
	if err != nil {
		t.Fatal(err)
	}
	return revision
}

func postgresReadyJSON() string {
	return `{"spec":{"replicas":1},"status":{"readyReplicas":1}}`
}

// A migration is a step the CLI takes, and it is a step that needs a database:
// the chart's Job runs `alembic upgrade head` with backoffLimit 0, so a Job
// created before PostgreSQL accepts connections fails permanently and no
// second attempt happens. The step therefore waits for the database, applies
// the chart with the Job enabled (the chart value migration.enabled), waits for
// the Job itself, and returns the revision it applied — which is the revision
// the caller then converges on.
func TestMigrateWaitsForTheDatabaseAppliesTheJobAndReturnsItsRevision(t *testing.T) {
	ctx := context.Background()
	values, err := Values(Settings{Domain: "kn.example.com", AdminEmail: "admin@kn.example.com"}, testSecrets())
	if err != nil {
		t.Fatal(err)
	}

	var (
		posted  []byte
		jobRead int
	)
	var r *componenttest.FakeRunner
	wantRevision := migrationRevision(t, values)
	r = &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case command == postgresCmd:
			return sshx.Result{Stdout: postgresReadyJSON()}, nil
		case command == migrationJobCmd:
			jobRead++
			// The Job is applied by the helm-install job asynchronously: the
			// first observation is "not there yet", which is not a failure.
			if jobRead <= 2 {
				return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): jobs.batch "kubenest-cp-migrate" not found`}, nil
			}
			return sshx.Result{Stdout: jobJSON(t, "Complete", wantRevision)}, nil
		case strings.HasPrefix(command, "sudo -n install -m 0600 "):
			inputs := r.Inputs()
			if len(inputs) == 0 {
				t.Fatal("the chart was applied without streaming a document")
			}
			posted = inputs[len(inputs)-1]
			return sshx.Result{}, nil
		default:
			t.Fatalf("unscripted command: %q", command)
		}
		return sshx.Result{}, nil
	}}

	revision, err := Migrate(ctx, r, values, installBundle(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if revision == "" {
		t.Error("Migrate returned no revision: the caller would converge on the revision of the chart it applied BEFORE the migration")
	}
	if revision != wantRevision {
		t.Errorf("Migrate returned revision %s, want %s", revision, wantRevision)
	}

	// What was applied is the chart with the migration Job enabled, not the
	// values the caller passed — otherwise no Job exists to wait for.
	var doc map[string]any
	if err := yaml.Unmarshal(posted, &doc); err != nil {
		t.Fatalf("the applied manifest is not valid YAML: %v", err)
	}
	spec := section(t, doc, "spec")
	applied, ok := spec["valuesContent"].(string)
	if !ok {
		t.Fatalf("the applied HelmChart has no valuesContent: %v", spec)
	}
	var sent map[string]any
	if err := yaml.Unmarshal([]byte(applied), &sent); err != nil {
		t.Fatalf("the applied values are not valid YAML: %v", err)
	}
	migration := section(t, sent, "migration")
	if migration["enabled"] != true {
		t.Errorf("migration = %v, want enabled: true, or the chart renders no Job", migration)
	}
	if sent["installRevision"] != revision {
		t.Errorf("installRevision = %v, want the revision Migrate returned (%s)", sent["installRevision"], revision)
	}
	// The revision is a function of the chart and the values, so enabling the
	// Job is part of what the control plane rolls to.
	plain, plainRevision, err := withRevision(values)
	if err != nil {
		t.Fatal(err)
	}
	if plainRevision == revision {
		t.Error("enabling the migration Job did not change the install revision: a re-run against a migrated database would apply identical values and roll nothing")
	}
	if !strings.Contains(applied, "agentJwtSecret") || !strings.Contains(applied, "gatewayCA") {
		t.Errorf("the migration apply dropped values the chart requires (%s): %q", plain, applied)
	}

	// It waited for the Job rather than returning as soon as it was applied.
	if jobRead < 2 {
		t.Errorf("the Job was observed %d times, want it polled until it completed", jobRead)
	}
	// And it never read the database with a credential on the command line.
	for _, command := range r.Commands() {
		if strings.Contains(command, "postgres-password") || strings.Contains(command, base64.StdEncoding.EncodeToString([]byte("pg"))) {
			t.Errorf("command %q carries a credential", command)
		}
	}
}

// A failed migration is a verdict, not a state to converge out of: the Job has
// backoffLimit 0, so nothing is going to retry it. The step has to fail with
// the reason and how to read it, rather than waiting out the deadline.
func TestMigrateFailsAsSoonAsTheJobFails(t *testing.T) {
	ctx := context.Background()
	values, err := Values(Settings{Domain: "kn.example.com", AdminEmail: "admin@kn.example.com"}, testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case command == postgresCmd:
			return sshx.Result{Stdout: postgresReadyJSON()}, nil
		case command == migrationJobCmd:
			return sshx.Result{Stdout: jobJSON(t, "Failed", migrationRevision(t, values))}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl delete job "+MigrationJobName):
			// The failed Job is removed before the chart is applied, so a
			// resume can create a new one: backoffLimit 0 means this one will
			// never try again.
			return sshx.Result{}, nil
		case strings.HasPrefix(command, "sudo -n install -m 0600 "):
			return sshx.Result{}, nil
		default:
			t.Fatalf("unscripted command: %q", command)
		}
		return sshx.Result{}, nil
	}}

	_, err = Migrate(ctx, r, values, installBundle(t), nil)
	if err == nil {
		t.Fatal("Migrate reported success on a failed migration Job: the control plane would come up on a schema nobody brought forward")
	}
	for _, want := range []string{MigrationJobName, "kubectl logs job/" + MigrationJobName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name %q", err, want)
		}
	}
	if time.Since(start) > 20*time.Second {
		t.Errorf("the failure took %s: a Job that has already failed must not be waited out to the deadline", time.Since(start))
	}
	// The failure was reached without waiting for the deadline: the database
	// read, the stale-Job read that removes the failed Job, the apply, and the
	// one Job read that answers Failed.
	if got := len(r.Commands()); got > 5 {
		t.Errorf("made %d calls before failing, want the database read, the stale-Job read, the apply and one Job read", got)
	}
}

// The wait itself is exercised directly too: "not found yet" is not a failure,
// Complete is a pass, and neither is inferred from a pod count.
func TestWaitForMigrationTreatsAnAbsentJobAsConvergingAndFailedAsFatal(t *testing.T) {
	ctx := context.Background()

	t.Run("absent then complete", func(t *testing.T) {
		revision := "rev0000000000001"
		var calls int
		r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
			if command != migrationJobCmd {
				t.Fatalf("unscripted command: %q", command)
			}
			calls++
			if calls == 1 {
				return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): jobs.batch "kubenest-cp-migrate" not found`}, nil
			}
			return sshx.Result{Stdout: jobJSON(t, "Complete", revision)}, nil
		}}
		done := make(chan error, 1)
		go func() { done <- WaitForMigration(ctx, r, revision, 10*time.Second, nil) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("WaitForMigration = %v, want nil once the Job completed", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("WaitForMigration never returned")
		}
	})

	t.Run("failed", func(t *testing.T) {
		revision := "rev0000000000002"
		r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
			if command != migrationJobCmd {
				t.Fatalf("unscripted command: %q", command)
			}
			return sshx.Result{Stdout: jobJSON(t, "Failed", revision)}, nil
		}}
		var events []converge.Event
		done := make(chan error, 1)
		go func() {
			done <- WaitForMigration(ctx, r, revision, 10*time.Second, converge.ReporterFunc(func(e converge.Event) {
				events = append(events, e)
			}))
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "failed") {
				t.Fatalf("WaitForMigration = %v, want a failure naming the Job", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("WaitForMigration waited out the deadline on a Job that had already failed")
		}
		// The wait saw the Job settle, so it would report a pass; the failure
		// is what the operator has to see last.
		if len(events) == 0 || events[len(events)-1].Outcome != converge.Fail {
			t.Errorf("reported events = %+v, want a final fail for %s", events, migrationCheckName)
		}
	})
}

// A migration that never completes fails at the deadline with the object named,
// like every other converge check.
func TestWaitForMigrationReportsAJobThatNeverCompletes(t *testing.T) {
	revision := "rev0000000000003"
	r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		if command != migrationJobCmd {
			t.Fatalf("unscripted command: %q", command)
		}
		return sshx.Result{Stdout: jobJSON(t, "", revision)}, nil
	}}
	var events []converge.Event
	err := WaitForMigration(context.Background(), r, revision, time.Millisecond, converge.ReporterFunc(func(e converge.Event) {
		events = append(events, e)
	}))
	if err == nil {
		t.Fatal("a Job that never completed was reported as a successful migration")
	}
	if !strings.Contains(err.Error(), MigrationJobName) {
		t.Errorf("error %q must name %q", err, MigrationJobName)
	}
	if len(events) == 0 {
		t.Error("no progress was reported while waiting")
	}
}

// MigrationValues turns the Job on and changes nothing else: the values a
// caller renders are the values the chart is installed with.
func TestMigrationValuesOnlyEnablesTheJob(t *testing.T) {
	values, err := Values(Settings{Domain: "kn.example.com", AdminEmail: "admin@kn.example.com"}, testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	before := map[string]any{}
	if err := yaml.Unmarshal([]byte(values), &before); err != nil {
		t.Fatal(err)
	}
	out, err := MigrationValues(values)
	if err != nil {
		t.Fatal(err)
	}
	after := map[string]any{}
	if err := yaml.Unmarshal([]byte(out), &after); err != nil {
		t.Fatal(err)
	}
	migration, ok := after["migration"].(map[string]any)
	if !ok || migration["enabled"] != true {
		t.Fatalf("migration = %v, want {enabled: true}", after["migration"])
	}
	delete(after, "migration")
	if fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("MigrationValues changed more than the migration Job:\n after  %v\n before %v", after, before)
	}
}

// testSecrets is a complete generated set: Values renders all of it, and a
// missing half would come out of the chart's `required` instead of the test.
func testSecrets() Secrets {
	return Secrets{
		JWTSecret:            "jwt",
		AgentJWTSecret:       "agent",
		EncryptionKey:        "enc",
		PostgresPassword:     "pg",
		AdminPassword:        "adm",
		GatewayCACertificate: "ca-pem",
		GatewayCAPrivateKey:  "ca-key-pem",
	}
}

// ---------------------------------------------------------------------------
// The four defects hardware found on 2026-09-25. Each of these is a case the
// step used to get wrong, and each fails without the change it tests.
// ---------------------------------------------------------------------------

// A chart upgrade that changes the backend image must remove the release's
// previous migration Job BEFORE the chart is applied.
//
// Job.spec.template is immutable, so `helm upgrade` cannot patch the existing
// Job's pod template: the upgrade fails, and with failurePolicy: abort the
// whole release is left failed, waiting for an operator with helm on the host.
// Successive applies of the same chart with one value changed is EXACTLY what
// an upgrade is, so this is not an edge case — it is the ordinary path.
func TestAnImageChangeRemovesThePreviousMigrationJobBeforeTheChartIsApplied(t *testing.T) {
	ctx := context.Background()
	values, err := Values(Settings{Domain: "kn.example.com", AdminEmail: "admin@kn.example.com"}, testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	want := migrationRevision(t, values)
	previous := "rev00000000000ff"

	var jobReads int
	r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case command == postgresCmd:
			return sshx.Result{Stdout: postgresReadyJSON()}, nil
		case command == migrationJobCmd:
			jobReads++
			switch jobReads {
			case 1:
				// The previous revision's Job: Complete, and immutable.
				return sshx.Result{Stdout: jobJSON(t, "Complete", previous)}, nil
			case 2:
				// Removed by the step, and the helm-install job has not
				// created this revision's yet.
				return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): jobs.batch "kubenest-cp-migrate" not found`}, nil
			default:
				return sshx.Result{Stdout: jobJSON(t, "Complete", want)}, nil
			}
		case strings.HasPrefix(command, "sudo -n k3s kubectl delete job "+MigrationJobName):
			return sshx.Result{}, nil
		case strings.HasPrefix(command, "sudo -n install -m 0600 "):
			return sshx.Result{}, nil
		default:
			t.Fatalf("unscripted command: %q", command)
		}
		return sshx.Result{}, nil
	}}

	revision, err := Migrate(ctx, r, values, installBundle(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if revision != want {
		t.Fatalf("Migrate returned revision %s, want %s", revision, want)
	}

	// Order, not just occurrence: a delete AFTER the apply is the failure this
	// test exists for.
	var deletedAt, appliedAt = -1, -1
	for i, command := range r.Commands() {
		if strings.HasPrefix(command, "sudo -n k3s kubectl delete job "+MigrationJobName) {
			deletedAt = i
		}
		if appliedAt < 0 && strings.HasPrefix(command, "sudo -n install -m 0600 ") {
			appliedAt = i
		}
	}
	if deletedAt < 0 {
		t.Fatalf("the previous revision's migration Job was never removed, so the chart upgrade would fail on its immutable pod template; commands were %v", r.Commands())
	}
	if appliedAt < 0 {
		t.Fatal("the chart was never applied")
	}
	if deletedAt > appliedAt {
		t.Errorf("the migration Job was removed at call %d but the chart was applied at call %d: the apply would fail on the immutable pod template", deletedAt, appliedAt)
	}
	// And the Job the step waited on is the one it applied, not the previous
	// revision's: a Completed Job from the earlier revision is not this
	// revision's migration.
	if jobReads < 2 {
		t.Errorf("the Job was read %d time(s); the previous revision's Job was still what a single read would have answered", jobReads)
	}
}

// A Completed Job stamped for a different install revision is NOT this step's
// result.
//
// Observed on hardware 2026-09-25: the CLI printed
// `kubenest-control-plane-migrate: pass (1s)` against the previous revision's
// Completed Job while helm-controller was still installing, and the new
// revision's Job then failed against a restarting PostgreSQL. A pass that fast
// was the whole tell.
func TestWaitForMigrationRefusesAJobFromADifferentRevision(t *testing.T) {
	ctx := context.Background()
	applied := "rev00000000abcde"
	stale := "rev00000000f1234"

	r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		if command != migrationJobCmd {
			t.Fatalf("unscripted command: %q", command)
		}
		// Complete — which is exactly why it must not be accepted.
		return sshx.Result{Stdout: jobJSON(t, "Complete", stale)}, nil
	}}
	var events []converge.Event
	err := WaitForMigration(ctx, r, applied, time.Millisecond, converge.ReporterFunc(func(e converge.Event) {
		events = append(events, e)
	}))
	if err == nil {
		t.Fatal("a Completed Job from another install revision was accepted as this step's result")
	}
	if !strings.Contains(err.Error(), applied) {
		t.Errorf("the failure does not name the revision that was applied (%s): %v", applied, err)
	}
	if len(events) == 0 {
		t.Fatal("nothing was reported")
	}
	if last := events[len(events)-1]; last.Outcome == converge.Pass {
		t.Errorf("the wait reported a pass on another revision's Job: %+v", last)
	}
	// The observation says WHY it was not this step's Job, so the operator is
	// not left reading a timeout.
	found := false
	for _, event := range events {
		if strings.Contains(event.State.Status, stale) {
			found = true
		}
	}
	if !found {
		t.Errorf("no observation named the revision the Job carried (%s): %+v", stale, events)
	}
}

// A resume after a FAILED migration must create a new Job for the same
// revision.
//
// The Job has backoffLimit 0, so Failed is final; the chart content is
// unchanged by a resume, so a deleted Job is not recreated by helm-controller
// either. Without removing it, "fix what the error names, then run the
// identical command again" names a step that does not exist — which is what the
// controller on hardware had to work around by patching the chart's migration
// value off and on by hand.
func TestAResumeAfterAFailedMigrationJobCreatesANewOne(t *testing.T) {
	ctx := context.Background()
	values, err := Values(Settings{Domain: "kn.example.com", AdminEmail: "admin@kn.example.com"}, testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	want := migrationRevision(t, values)

	var jobReads int
	r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case command == postgresCmd:
			return sshx.Result{Stdout: postgresReadyJSON()}, nil
		case command == migrationJobCmd:
			jobReads++
			switch jobReads {
			case 1:
				// The failed attempt, at THIS revision. A resume must not stop here.
				return sshx.Result{Stdout: jobJSON(t, "Failed", want)}, nil
			case 2:
				return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): jobs.batch "kubenest-cp-migrate" not found`}, nil
			default:
				return sshx.Result{Stdout: jobJSON(t, "Complete", want)}, nil
			}
		case strings.HasPrefix(command, "sudo -n k3s kubectl delete job "+MigrationJobName):
			return sshx.Result{}, nil
		case strings.HasPrefix(command, "sudo -n install -m 0600 "):
			return sshx.Result{}, nil
		default:
			t.Fatalf("unscripted command: %q", command)
		}
		return sshx.Result{}, nil
	}}

	revision, err := Migrate(ctx, r, values, installBundle(t), nil)
	if err != nil {
		t.Fatalf("the resume did not get past the failed migration Job: %v", err)
	}
	if revision != want {
		t.Errorf("Migrate returned revision %s, want %s", revision, want)
	}
	deleted := false
	for _, command := range r.Commands() {
		if strings.HasPrefix(command, "sudo -n k3s kubectl delete job "+MigrationJobName) {
			deleted = true
		}
	}
	if !deleted {
		t.Errorf("the failed Job was left in place, so the resumed run waits on it for ever; commands were %v", r.Commands())
	}
	if jobReads < 3 {
		t.Errorf("the Job was read %d time(s), want the failed one, its absence, and the new one", jobReads)
	}
}
