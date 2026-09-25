package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

// MigrationJobName is the Job the chart renders when migration.enabled is set
// (kubenest-helm/kubenest/templates/migration-job.yaml). It is deterministic —
// the release name plus a suffix — because the CLI has to observe it, and
// because a re-run has to recognize the Job an earlier attempt ran rather than
// create a second one.
const MigrationJobName = ReleaseName + "-migrate"

// migrationCheckName is what the wait reports itself as.
const migrationCheckName = "kubenest-control-plane-migrate"

// MigrationValues returns the values document with the chart's migration Job
// enabled.
//
// THE JOB IS OFF BY DEFAULT AND THE CALLER TURNS IT ON, because whether a
// migration is needed is not a fact about the chart: it is a fact about the
// database the chart is being installed over. A database that has just been
// created is empty, and `alembic upgrade head` there would race the backend
// building the same schema from its models. A database that already holds a
// schema is the case the Job exists for: the backend refuses to start on a
// schema its code does not match, and the Job is what brings the database to
// the code's revision.
func MigrationValues(valuesYAML string) (string, error) {
	doc := map[string]any{}
	if err := yaml.Unmarshal([]byte(valuesYAML), &doc); err != nil {
		return "", fmt.Errorf("reading the control-plane values: %w", err)
	}
	doc["migration"] = map[string]any{"enabled": true}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("rendering the control-plane values: %w", err)
	}
	return string(out), nil
}

// Migrate runs the control plane's schema migration as ONE step of an install
// or an upgrade, and returns the install revision it applied.
//
// ORDER: PostgreSQL ready, then the Job, then the rest of the control plane.
// The chart's Job runs `alembic upgrade head` with backoffLimit 0 — a retried
// migration is a migration nobody watched — so a Job created before the
// database accepts connections fails permanently. The chart is therefore
// applied without it first (Install's Apply), and this step re-applies the
// chart with migration.enabled once the database is up. That second apply is
// the same chart with one more value, so a re-run is idempotent: the same
// revision, the same Job, the same outcome.
//
// It WAITS FOR THE JOB, and the caller converges on the revision it returns
// afterwards: the backend refuses to start until the schema matches, so
// "installed" has to mean "migrated and then rolled out", never "the migration
// was started".
func Migrate(ctx context.Context, r k3s.Runner, valuesYAML string, bundle *manifest.Manifest, rep converge.Reporter) (string, error) {
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return "", err
	}
	// The database first. postgresReady is the same observation the readiness
	// check uses, so "the migration started on a Ready database" and "the
	// control plane is up" cannot disagree about what Ready means.
	res, err := converge.Wait(ctx, postgresProbe(r), converge.Options{
		Name:     "kubenest-control-plane-database-ready",
		Deadline: deadline,
		Reporter: rep,
	})
	if err != nil {
		return "", err
	}
	if err := res.Err(); err != nil {
		return "", fmt.Errorf("the migration step needs the database: %w", err)
	}

	values, err := MigrationValues(valuesYAML)
	if err != nil {
		return "", err
	}
	revision, err := Apply(ctx, r, values)
	if err != nil {
		return "", err
	}
	if err := WaitForMigration(ctx, r, deadline, rep); err != nil {
		return "", err
	}
	return revision, nil
}

// WaitForMigration waits until the chart's migration Job has completed, and
// returns an error naming the Job and how to read it when it failed.
func WaitForMigration(ctx context.Context, r k3s.Runner, deadline time.Duration, rep converge.Reporter) error {
	// A failed Job is a verdict, not a state to converge out of: the Job has
	// backoffLimit 0, so Failed is final. converge has no fail-fast — a probe
	// error is an observation, and only the deadline is a verdict — so the
	// probe reports the failure as SETTLED and the verdict is read from the
	// observation afterwards. Burning the whole deadline on a Job that already
	// said why would hide the reason for ten minutes.
	var failure string
	probe := func(ctx context.Context) (bool, converge.State, error) {
		return migrationState(ctx, r, &failure)
	}
	res, err := converge.Wait(ctx, probe, converge.Options{
		Name:     migrationCheckName,
		Deadline: deadline,
		Reporter: rep,
	})
	if err != nil {
		return err
	}
	if failure != "" {
		// The wait saw the Job SETTLE, so it reports a pass; tell the terminal
		// the truth as well, because a green check followed by a failed
		// install is a log nobody can read.
		if rep != nil {
			rep.Report(converge.Event{
				Check:    migrationCheckName,
				Outcome:  converge.Fail,
				State:    res.Last,
				Elapsed:  res.Elapsed,
				Deadline: deadline,
			})
		}
		return fmt.Errorf("%s: %s", migrationCheckName, failure)
	}
	if err := res.Err(); err != nil {
		return fmt.Errorf("%s: the migration Job %s did not complete: %w (read it with `kubectl logs job/%s -n %s`)",
			migrationCheckName, MigrationJobName, err, MigrationJobName, Namespace)
	}
	return nil
}

// postgresProbe adapts postgresReady to a converge.Probe.
func postgresProbe(r k3s.Runner) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		return postgresReady(ctx, r)
	}
}

// migrationState observes the migration Job once: done when it completed, and
// done with failure set when it failed.
//
// The Job may not exist yet — the helm-install job applies the chart
// asynchronously — which is an observation ("not found yet"), not a failure.
func migrationState(ctx context.Context, r k3s.Runner, failure *string) (bool, converge.State, error) {
	object := "job " + MigrationJobName + " in " + Namespace
	var job struct {
		Status struct {
			Succeeded  int32 `json:"succeeded"`
			Failed     int32 `json:"failed"`
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	out, err := k3s.Kubectl(ctx, r, "get job "+MigrationJobName+" -n "+Namespace+" -o json")
	if err != nil {
		return false, converge.State{Object: object, Status: "not found yet"}, err
	}
	if err := json.Unmarshal([]byte(out), &job); err != nil {
		return false, converge.State{Object: object, Status: "unparsable"}, err
	}
	for _, condition := range job.Status.Conditions {
		if condition.Status != "True" {
			continue
		}
		switch condition.Type {
		case "Complete":
			return true, converge.State{Object: object, Status: "Complete"}, nil
		case "Failed":
			// The migration ended on an error the operator has to read: the
			// schema is now in whatever state alembic left it, and the message
			// names the object to read rather than guessing which statement
			// failed.
			reason := condition.Message
			if reason == "" {
				reason = condition.Reason
			}
			*failure = fmt.Sprintf(
				"the migration Job %s failed: %s. The database is at whatever revision alembic left it at; read the failure with `kubectl logs job/%s -n %s` before re-running this install",
				MigrationJobName, reason, MigrationJobName, Namespace)
			return true, converge.State{Object: object, Status: "Failed", Detail: reason}, nil
		}
	}
	if job.Status.Failed > 0 && job.Status.Succeeded == 0 {
		// A pod failed but no Failed condition has been written yet; still not
		// a verdict, because the Job controller is the one that decides.
		return false, converge.State{
			Object: object,
			Status: fmt.Sprintf("%d pod(s) failed, no verdict yet", job.Status.Failed),
		}, nil
	}
	return false, converge.State{Object: object, Status: "running"}, nil
}
