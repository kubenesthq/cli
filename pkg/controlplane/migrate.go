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
	return migrationValues(valuesYAML, true)
}

// migrationValues sets the chart's migration Job on or off.
//
// OFF IS NOT THE ABSENCE OF THE KEY: the values a release runs with may have it
// ON (a previous upgrade's applies keep it on so helm does not delete the Job
// that records the schema step), and re-applying a release whose Job has a
// different pod template would fail on the immutable field.
func migrationValues(valuesYAML string, enabled bool) (string, error) {
	doc := map[string]any{}
	if err := yaml.Unmarshal([]byte(valuesYAML), &doc); err != nil {
		return "", fmt.Errorf("reading the control-plane values: %w", err)
	}
	doc["migration"] = map[string]any{"enabled": enabled}
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
	// THE JOB IS REMOVED BEFORE THE CHART IS APPLIED, and both halves of that
	// sentence are load-bearing (hardware, 2026-09-25):
	//
	//	Job.spec.template is IMMUTABLE. The Job is a chart resource, so a chart
	//	upgrade that changes the backend image tries to patch the pod template
	//	of the release's existing Job and `helm upgrade` FAILS outright. With
	//	failurePolicy: abort the release is then left failed and waits for an
	//	operator. Deleting the Job first makes the upgrade a CREATE, which
	//	helm will do.
	//
	//	A deleted Job is never recreated while the chart content is unchanged,
	//	and a FAILED Job stays failed for ever (backoffLimit is 0, by design:
	//	a retried migration is a migration nobody watched). So a resume after
	//	a failed migration would otherwise find the same Failed Job and report
	//	its failure again with no way forward. Removing it makes the resume
	//	create a new Job for the same revision, which is what "run the
	//	identical command again" has to mean.
	//
	// A Job already at the revision about to be applied AND not failed is left
	// alone: it is this same step's outcome, and re-running `alembic upgrade
	// head` under it would be a second writer of the schema that the Job's own
	// log does not explain.
	wantRevision, err := Revision(values)
	if err != nil {
		return "", err
	}
	if err := clearStaleMigrationJob(ctx, r, wantRevision); err != nil {
		return "", err
	}
	revision, err := Apply(ctx, r, values)
	if err != nil {
		return "", err
	}
	if revision != wantRevision {
		return "", fmt.Errorf("the migration applied install revision %s but computed %s: the revision must be a function of the values alone", revision, wantRevision)
	}
	if err := WaitForMigration(ctx, r, revision, deadline, rep); err != nil {
		return "", err
	}
	return revision, nil
}

// migrationJobPrefix is what `kubectl get job` is filtered by when the step
// looks for the Job it owns.
//
// The Job's name is fixed by the chart (MigrationJobName), so a stale one from
// an earlier revision wears the same name as the one this step will create.
// What separates them is the install-revision annotation the chart stamps —
// which is why the wait reads it and why clearing the stale Job reads it too.
const migrationJobRevisionAnnotation = "kubenest.io/install-revision"

// clearStaleMigrationJob removes the release's migration Job when it is not
// this step's to keep: a different install revision, or a failed one.
//
// It is a no-op when the Job is absent, which is the ordinary case on a first
// migration and on every resume that already removed it.
func clearStaleMigrationJob(ctx context.Context, r k3s.Runner, wantRevision string) error {
	job, found, err := readMigrationJob(ctx, r)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	revision := job.Metadata.Annotations[migrationJobRevisionAnnotation]
	failed := jobFailed(job)
	if revision == wantRevision && !failed {
		return nil
	}
	if _, err := k3s.Kubectl(ctx, r, "delete job "+MigrationJobName+" -n "+Namespace+" --ignore-not-found"); err != nil {
		return fmt.Errorf("the previous migration Job %s/%s could not be removed, so the chart cannot re-create it: %w",
			Namespace, MigrationJobName, err)
	}
	return nil
}

// migrationJob is the slice of a Job this file reads: its install revision,
// and whether it has failed for good.
type migrationJob struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
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

// readMigrationJob reads the Job once. Not found is an observation, not a
// failure: the helm-install job applies the chart asynchronously, and on a
// first apply the Job does not exist yet.
func readMigrationJob(ctx context.Context, r k3s.Runner) (migrationJob, bool, error) {
	var job migrationJob
	out, err := k3s.Kubectl(ctx, r, "get job "+MigrationJobName+" -n "+Namespace+" -o json")
	if err != nil {
		return job, false, nil
	}
	if err := json.Unmarshal([]byte(out), &job); err != nil {
		return job, false, fmt.Errorf("reading the migration Job %s/%s: %w", Namespace, MigrationJobName, err)
	}
	return job, true, nil
}

// jobFailed reports whether the Job has ended in failure.
func jobFailed(job migrationJob) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Status == "True" && condition.Type == "Failed" {
			return true
		}
	}
	return false
}

// WaitForMigration waits until the chart's migration Job has completed for
// THE INSTALL REVISION THIS STEP APPLIED, and returns an error naming the Job
// and how to read it when it failed.
//
// THE REVISION IS NOT DECORATION. It used to be omitted, and on hardware on
// 2026-09-25 the wait passed in one second against the PREVIOUS revision's
// Completed Job while helm-controller was still installing; the new revision's
// Job then ran against a restarting PostgreSQL, failed with "connection
// refused", and the install still reported the control plane ready because the
// schema happened to be at head already. A Completed Job is only an answer
// about the revision it was stamped for.
func WaitForMigration(ctx context.Context, r k3s.Runner, revision string, deadline time.Duration, rep converge.Reporter) error {
	// A failed Job is a verdict, not a state to converge out of: the Job has
	// backoffLimit 0, so Failed is final. converge has no fail-fast — a probe
	// error is an observation, and only the deadline is a verdict — so the
	// probe reports the failure as SETTLED and the verdict is read from the
	// observation afterwards. Burning the whole deadline on a Job that already
	// said why would hide the reason for ten minutes.
	var failure string
	probe := func(ctx context.Context) (bool, converge.State, error) {
		return migrationState(ctx, r, revision, &failure)
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
		return fmt.Errorf("%s: the migration Job %s did not complete at install revision %s: %w (read it with `kubectl logs job/%s -n %s`)",
			migrationCheckName, MigrationJobName, revision, err, MigrationJobName, Namespace)
	}
	return nil
}

// postgresProbe adapts postgresReady to a converge.Probe.
func postgresProbe(r k3s.Runner) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		return postgresReady(ctx, r)
	}
}

// migrationState observes the migration Job once: done when it completed AT
// THIS INSTALL REVISION, and done with failure set when it failed.
//
// The Job may not exist yet — the helm-install job applies the chart
// asynchronously — which is an observation ("not found yet"), not a failure.
// So is a Job stamped for a different revision: it is the previous release's,
// and this step's Job has not arrived.
func migrationState(ctx context.Context, r k3s.Runner, revision string, failure *string) (bool, converge.State, error) {
	object := "job " + MigrationJobName + " in " + Namespace
	job, found, err := readMigrationJob(ctx, r)
	if err != nil {
		return false, converge.State{Object: object, Status: "unparsable"}, err
	}
	if !found {
		return false, converge.State{Object: object, Status: "not found yet"}, nil
	}
	if got := job.Metadata.Annotations[migrationJobRevisionAnnotation]; got != revision {
		// NOT this step's Job. A Completed Job from an earlier revision is
		// exactly the false pass the revision stamp exists to prevent, so it
		// is reported as an observation and never as this step's outcome.
		status := "belongs to install revision " + got
		if got == "" {
			status = "carries no install revision"
		}
		return false, converge.State{
			Object: object,
			Status: status + ", not " + revision + " which this step applied",
			Detail: "the previous revision's migration Job is not this step's result",
		}, nil
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
				"the migration Job %s failed: %s. The database is at whatever revision alembic left it at; read the failure with `kubectl logs job/%s -n %s`, then run the identical command again — the failed Job is removed and a new one is created for the same install revision, because the Job has backoffLimit 0 and would otherwise stay Failed for ever",
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
