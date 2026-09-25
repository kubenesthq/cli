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
// complete, running, or failed.
func jobJSON(t *testing.T, condition string) string {
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
		"metadata": map[string]any{"name": MigrationJobName, "namespace": Namespace},
		"status":   status,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
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
	r = &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case command == postgresCmd:
			return sshx.Result{Stdout: postgresReadyJSON()}, nil
		case command == migrationJobCmd:
			jobRead++
			// The Job is applied by the helm-install job asynchronously: the
			// first observation is "not there yet", which is not a failure.
			if jobRead == 1 {
				return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): jobs.batch "kubenest-cp-migrate" not found`}, nil
			}
			return sshx.Result{Stdout: jobJSON(t, "Complete")}, nil
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
			return sshx.Result{Stdout: jobJSON(t, "Failed")}, nil
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
	// The failure was reached without waiting for the deadline, so the probe
	// ran once.
	if got := len(r.Commands()); got > 4 {
		t.Errorf("made %d calls before failing, want the database read, the apply and one Job read", got)
	}
}

// The wait itself is exercised directly too: "not found yet" is not a failure,
// Complete is a pass, and neither is inferred from a pod count.
func TestWaitForMigrationTreatsAnAbsentJobAsConvergingAndFailedAsFatal(t *testing.T) {
	ctx := context.Background()

	t.Run("absent then complete", func(t *testing.T) {
		var calls int
		r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
			if command != migrationJobCmd {
				t.Fatalf("unscripted command: %q", command)
			}
			calls++
			if calls == 1 {
				return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): jobs.batch "kubenest-cp-migrate" not found`}, nil
			}
			return sshx.Result{Stdout: jobJSON(t, "Complete")}, nil
		}}
		done := make(chan error, 1)
		go func() { done <- WaitForMigration(ctx, r, 10*time.Second, nil) }()
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
		r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
			if command != migrationJobCmd {
				t.Fatalf("unscripted command: %q", command)
			}
			return sshx.Result{Stdout: jobJSON(t, "Failed")}, nil
		}}
		var events []converge.Event
		done := make(chan error, 1)
		go func() {
			done <- WaitForMigration(ctx, r, 10*time.Second, converge.ReporterFunc(func(e converge.Event) {
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
	r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		if command != migrationJobCmd {
			t.Fatalf("unscripted command: %q", command)
		}
		return sshx.Result{Stdout: jobJSON(t, "")}, nil
	}}
	var events []converge.Event
	err := WaitForMigration(context.Background(), r, time.Millisecond, converge.ReporterFunc(func(e converge.Event) {
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
