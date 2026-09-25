package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/sshx"
)

// A PostgreSQL major change is a database migration, not a version bump, and
// this upgrade does not carry one.
func TestAChartWhosePostgresMajorDiffersIsRefused(t *testing.T) {
	running := parsePostgresImage("docker.io/bitnami/postgresql:17.2.0-debian-12-r0")
	declared := parsePostgresImage("docker.io/bitnami/postgresql:18.1.0-debian-12-r0")

	err := comparePostgres(running, declared)
	if err == nil {
		t.Fatal("a chart that would move the control plane's database by a major version was accepted")
	}
	var refusal *PostgresRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v (%T), want a *PostgresRefusal", err, err)
	}
	message := err.Error()
	for _, want := range []string{"18", "17", "PostgreSQL"} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, message)
		}
	}

	// A PATCH change is not a database migration: the same major from the same
	// builder is the same database.
	patch := parsePostgresImage("docker.io/bitnami/postgresql:17.5.0-debian-12-r1")
	if err := comparePostgres(running, patch); err != nil {
		t.Errorf("a patch-level PostgreSQL change was refused: %v", err)
	}
}

// A different DISTRIBUTION is the same refusal, for a stronger reason: the same
// major from another builder is a different database with a different upgrade
// story, and the tag would not tell anyone.
func TestAChartWhosePostgresDistributionDiffersIsRefused(t *testing.T) {
	running := parsePostgresImage("docker.io/bitnami/postgresql:17.2.0-debian-12-r0")
	declared := parsePostgresImage("docker.io/library/postgres:17.2")

	err := comparePostgres(running, declared)
	if err == nil {
		t.Fatal("a chart that would change the PostgreSQL distribution was accepted")
	}
	if !strings.Contains(err.Error(), "distribution") {
		t.Errorf("the refusal does not say the distribution changed:\n%s", err)
	}
	if !strings.Contains(err.Error(), "bitnami/postgresql") || !strings.Contains(err.Error(), "library/postgres") {
		t.Errorf("the refusal does not name both repositories:\n%s", err)
	}
}

// An image whose major cannot be read is REFUSED, not passed: "I could not
// tell" is not "they are the same", and this gate stands between an upgrade and
// the installation's database.
func TestAMajorThatCannotBeReadIsRefusedRatherThanPassed(t *testing.T) {
	running := parsePostgresImage("docker.io/bitnami/postgresql@sha256:aaaa")
	declared := parsePostgresImage("docker.io/bitnami/postgresql:17.2.0-debian-12-r0")
	if err := comparePostgres(running, declared); err == nil {
		t.Fatal("a database whose major could not be established was accepted")
	}
}

// The chart declares its PostgreSQL twice over, and both halves are read from
// the chart rather than guessed: the reference from its values, the major from
// the Bitnami subchart's appVersion (because the reference is pinned by digest
// and carries no tag).
func TestTheChartsPostgresIsReadFromTheChartItself(t *testing.T) {
	chart, err := ChartPostgresImage("")
	if err != nil {
		t.Fatal(err)
	}
	if chart.repository == "" {
		t.Error("the chart's PostgreSQL repository was not read")
	}
	if chart.digest == "" {
		t.Error("the chart's PostgreSQL digest was not read: this chart pins the database by digest")
	}
	if chart.major == "" {
		t.Error("the chart's PostgreSQL major was not read from the subchart's appVersion; without it every upgrade would be refused, because a digest-pinned image carries no version of its own")
	}
	if !strings.Contains(chart.repository, "postgresql") {
		t.Errorf("the chart's PostgreSQL repository is %q", chart.repository)
	}
}

// A values document that redirects the database is a chart whose PostgreSQL IS
// that image, and it is what the check must judge.
func TestAValuesOverrideRedirectingPostgresIsWhatIsJudged(t *testing.T) {
	values := "postgresql:\n  image:\n    repository: docker.io/library/postgres\n    tag: \"16.4\"\n"
	chart, err := ChartPostgresImage(values)
	if err != nil {
		t.Fatal(err)
	}
	if chart.repository != "docker.io/library/postgres" || chart.major != "16" {
		t.Fatalf("the override was not read: %+v", chart)
	}
}

// The running side is read from the cluster, and its major falls back to the
// checkpoint when the reference is digest-pinned.
func TestTheRunningPostgresComesFromTheCluster(t *testing.T) {
	r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.Contains(command, postgresStatefulSetImageCmd):
			return sshx.Result{Stdout: "docker.io/bitnami/postgresql@sha256:bbbb"}, nil
		case strings.Contains(command, postgresVersionCmd):
			// The server cannot be asked, so the FALLBACK is what this test is
			// about. When it can be asked, the server's own answer wins — see
			// TestTheRunningMajorIsReadFromTheRunningServerWhenTheImageIsPinnedByDigest.
			return sshx.Result{ExitCode: 1, Stderr: "error: unable to upgrade connection"}, nil
		case strings.Contains(command, "get configmap "+CheckpointStatusConfigMap):
			return sshx.Result{Stdout: checkpointMarkerJSON(t, checkpointMarker("cp/x.dump", 1))}, nil
		default:
			t.Fatalf("unscripted command: %q", command)
		}
		return sshx.Result{}, nil
	}}
	image, err := RunningPostgresImage(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if image.repository != "docker.io/bitnami/postgresql" {
		t.Errorf("the running repository is %q", image.repository)
	}
	if image.major == "" {
		t.Error("the running major was not established from the checkpoint the runner published")
	}
}

// And the two together: a cluster running what the chart declares passes, so
// the gate is not a refusal of every upgrade.
func TestTheChartAndTheClusterAgreeingPasses(t *testing.T) {
	chart, err := ChartPostgresImage("")
	if err != nil {
		t.Fatal(err)
	}
	running := chart
	running.digest = "sha256:different-bytes-same-database"
	if err := comparePostgres(running, chart); err != nil {
		t.Errorf("a control plane already running the chart's PostgreSQL was refused: %v", err)
	}
}

// THE FIRST UPGRADE OF A FRESHLY INSTALLED CONTROL PLANE MUST NOT BE REFUSED.
//
// The chart pins PostgreSQL by DIGEST, so the StatefulSet's reference carries no
// tag; and the eligible checkpoint does not exist before the first upgrade,
// because the checkpoint stage runs after the gates. A gate whose only two
// sources were "the image tag" and "the checkpoint" therefore refused every
// first upgrade — observed on a fresh host on 2026-09-25.
//
// The running SERVER is the third source, and it is the authoritative one: it
// is asked directly with `postgres --version` inside the running pod.
func TestTheRunningMajorIsReadFromTheRunningServerWhenTheImageIsPinnedByDigest(t *testing.T) {
	t.Run("the server answers", func(t *testing.T) {
		r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
			switch {
			case strings.Contains(command, postgresStatefulSetImageCmd):
				return sshx.Result{Stdout: "registry-1.docker.io/bitnami/postgresql@sha256:7d77a46bbf200709237318c27dec0eaeae2520e403dd18fdebde2be7a6b04993"}, nil
			case strings.Contains(command, postgresVersionCmd):
				return sshx.Result{Stdout: "postgres (PostgreSQL) 17.6"}, nil
			case strings.Contains(command, "get configmap "+CheckpointStatusConfigMap):
				// NO ELIGIBLE CHECKPOINT YET: this is the first upgrade, so the
				// fallback has nothing to offer and the server has to answer.
				return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): configmaps "control-plane-checkpoint-status" not found`}, nil
			default:
				t.Fatalf("unscripted command: %q", command)
			}
			return sshx.Result{}, nil
		}}

		image, err := RunningPostgresImage(context.Background(), r)
		if err != nil {
			t.Fatalf("the running PostgreSQL could not be established from the server itself: %v", err)
		}
		if image.major != "17" {
			t.Fatalf("the running major is %q, want 17 from `postgres --version`", image.major)
		}
		if image.repository != "registry-1.docker.io/bitnami/postgresql" {
			t.Errorf("the distribution is %q, want the running image's repository", image.repository)
		}

		// And the gate itself: the same chart passes, a different major refuses.
		same := parsePostgresImage("bitnami/postgresql:17.2.0-debian-12-r0")
		if err := comparePostgres(image, same); err != nil {
			t.Errorf("an upgrade onto the PostgreSQL the server already runs was refused: %v", err)
		}
		other := parsePostgresImage("bitnami/postgresql:18.1.0-debian-12-r0")
		if err := comparePostgres(image, other); err == nil {
			t.Error("an upgrade that would change the PostgreSQL major was accepted once the major came from the server")
		}
	})

	t.Run("the server cannot be asked", func(t *testing.T) {
		r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
			switch {
			case strings.Contains(command, postgresStatefulSetImageCmd):
				return sshx.Result{Stdout: "registry-1.docker.io/bitnami/postgresql@sha256:7d77a46b"}, nil
			case strings.Contains(command, postgresVersionCmd):
				return sshx.Result{ExitCode: 1, Stderr: "error: unable to upgrade connection"}, nil
			case strings.Contains(command, "get configmap "+CheckpointStatusConfigMap):
				return sshx.Result{ExitCode: 1, Stderr: "not found"}, nil
			default:
				t.Fatalf("unscripted command: %q", command)
			}
			return sshx.Result{}, nil
		}}
		if _, err := RunningPostgresImage(context.Background(), r); err == nil {
			t.Fatal("a database whose major could not be established from ANY source was accepted: this gate fails closed")
		}
	})

	t.Run("the tag still wins when the image carries one", func(t *testing.T) {
		r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
			switch {
			case strings.Contains(command, postgresStatefulSetImageCmd):
				return sshx.Result{Stdout: "docker.io/bitnami/postgresql:17.2.0-debian-12-r0"}, nil
			default:
				// The running server is NOT asked when the reference already
				// names its major: one read, not two.
				t.Fatalf("the running server was asked although the image carries a tag: %q", command)
			}
			return sshx.Result{}, nil
		}}
		image, err := RunningPostgresImage(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		if image.major != "17" {
			t.Errorf("the running major is %q, want 17 from the image tag", image.major)
		}
	})

	t.Run("the checkpoint is the last resort", func(t *testing.T) {
		// The server cannot be asked AND the image carries no tag, but a
		// checkpoint records the server's own major: that answer is better than
		// a refusal, and it is what the runner derived from `server_version_num`.
		r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
			switch {
			case strings.Contains(command, postgresStatefulSetImageCmd):
				return sshx.Result{Stdout: "registry-1.docker.io/bitnami/postgresql@sha256:7d77a46b"}, nil
			case strings.Contains(command, postgresVersionCmd):
				return sshx.Result{ExitCode: 1, Stderr: "error: unable to upgrade connection"}, nil
			case strings.Contains(command, "get configmap "+CheckpointStatusConfigMap):
				return sshx.Result{Stdout: checkpointMarkerJSON(t, checkpointMarker("cp/x.dump", 1))}, nil
			default:
				t.Fatalf("unscripted command: %q", command)
			}
			return sshx.Result{}, nil
		}}
		image, err := RunningPostgresImage(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		if image.major != "16" {
			t.Errorf("the running major is %q, want 16 from the checkpoint's postgres_major", image.major)
		}
	})
}
