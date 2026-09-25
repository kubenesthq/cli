package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/sshx"
)

// VALIDATION MUST BE ABLE TO TELL A NEW IMAGE FROM THE OLD ONE STILL SERVING.
//
// The validation compares a contract era and a build stamp, and the era alone
// is not enough: hardware (2026-09-25) ran an upgrade whose every stage was
// skipped — it changed nothing — and `control-plane-validation` passed in 0 s
// against the PREVIOUS candidate's image, because the only checks were "the era
// is not below the one before" and "the build is non-empty".
//
// The discriminator is a comparison of IMAGES: when the chart's backend image
// differs from the one the Deployment runs now, the build the old image reports
// is STALE and must not be accepted; when the image is unchanged, the same build
// is exactly what a correct upgrade reports.
const (
	validationOldBuild = "c121ed887750b1d3196d54fe9fd8368791a1bf03"
	validationNewTag   = "9e9698d"
	validationNewBuild = "9e9698dcafe4d1a3c25e29f9fd8368791a1bf03"
)

// validationRunner answers the reads the expectations make: the backend
// Deployment's image, and nothing else.
func validationRunner(running string) *componenttest.FakeRunner {
	return &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		if strings.Contains(command, backendDeploymentImageCmd) {
			return sshx.Result{Stdout: running}, nil
		}
		return sshx.Result{}, nil
	}}
}

// validationValues is a values document pinning the NEW backend image, which is
// what an upgrade's values carry: the chart's own pin, carried through.
func validationValues() string {
	return "domain: kn.example.com\n" +
		"backend:\n  image:\n    repository: ghcr.io/kubenesthq/kubenest-backend\n" +
		"    tag: \"" + validationNewTag + "\"\n" +
		"    digest: \"sha256:1111111111111111111111111111111111111111111111111111111111111111\"\n"
}

// The old build must be REFUSED when the chart moves the image: otherwise an
// upgrade whose chart roll silently failed reports success.
func TestValidationRefusesTheOldBuildWhenTheImageChanged(t *testing.T) {
	ctx := context.Background()
	// The Deployment runs a DIFFERENT image from the one the values pin.
	runner := validationRunner("ghcr.io/kubenesthq/kubenest-backend:" + validationOldBuild)

	opts, err := ValidationExpectations(ctx, runner, api.ControlPlaneVersion{Contract: 3, Build: validationOldBuild}, validationValues())
	if err != nil {
		t.Fatal(err)
	}
	if opts.StaleBuild != validationOldBuild {
		t.Fatalf("StaleBuild = %q, want the build the running image reported (%s): the chart's image differs, so answering with the old build means the old code is still serving", opts.StaleBuild, validationOldBuild)
	}
	if opts.WantBuild != validationNewTag {
		t.Errorf("WantBuild = %q, want the chart's tag %q: the image reports the FULL sha and the chart pins a short one, so the check is a prefix", opts.WantBuild, validationNewTag)
	}

	// And the check itself: the backend answering with the OLD build is refused.
	err = checkReportedVersion(opts, api.ControlPlaneVersion{Contract: 3, Build: validationOldBuild}, "10.0.0.1:8000")
	if err == nil {
		t.Fatal("a backend still reporting the build that was serving before the upgrade was accepted")
	}
	if !strings.Contains(err.Error(), validationOldBuild) {
		t.Errorf("the refusal does not name the stale build it saw:\n%v", err)
	}

	// The new image's own report is accepted: the full sha starts with the tag
	// the chart pins.
	if err := checkReportedVersion(opts, api.ControlPlaneVersion{Contract: 3, Build: validationNewBuild}, "10.0.0.1:8000"); err != nil {
		t.Errorf("the upgraded backend's own build was refused: %v", err)
	}
	// And an image that rolled but reports something unrelated is refused too:
	// the point of the tag is that the build can be tied to the artefact.
	if err := checkReportedVersion(opts, api.ControlPlaneVersion{Contract: 3, Build: "deadbeef1234"}, "10.0.0.1:8000"); err == nil {
		t.Error("a build that is not the chart's tag was accepted, so the check cannot tie the running image to the artefact")
	}
}

// The same build is CORRECT when the image did not change: refusing it would
// refuse every re-run and every resume, which is the opposite failure and just
// as wrong.
func TestValidationAcceptsTheSameBuildWhenTheImageDidNotChange(t *testing.T) {
	ctx := context.Background()
	// The Deployment already runs exactly what the values pin.
	runner := validationRunner("ghcr.io/kubenesthq/kubenest-backend:" + validationNewTag)

	opts, err := ValidationExpectations(ctx, runner, api.ControlPlaneVersion{Contract: 3, Build: validationOldBuild}, validationValues())
	if err != nil {
		t.Fatal(err)
	}
	if opts.StaleBuild != "" {
		t.Fatalf("StaleBuild = %q with the image unchanged: an unchanged image legitimately reports the same build, so this would refuse a correct re-run", opts.StaleBuild)
	}
	if err := checkReportedVersion(opts, api.ControlPlaneVersion{Contract: 3, Build: validationOldBuild}, "10.0.0.1:8000"); err != nil {
		t.Errorf("the same build was refused although the image did not change: %v", err)
	}
	// The era floor still applies: the counter never goes down.
	if err := checkReportedVersion(opts, api.ControlPlaneVersion{Contract: 2, Build: validationOldBuild}, "10.0.0.1:8000"); err == nil {
		t.Error("a reported era below the one the control plane was on before the upgrade was accepted")
	}
}

// A digest-only pin is the same image when the digest matches, whatever tag the
// Deployment's reference happens to carry: that is the chart's normal shape.
func TestAnImagePinnedByDigestIsRecognisedAsUnchanged(t *testing.T) {
	ctx := context.Background()
	digest := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	runner := validationRunner("ghcr.io/kubenesthq/kubenest-backend@" + digest)
	values := "backend:\n  image:\n    repository: ghcr.io/kubenesthq/kubenest-backend\n    digest: \"" + digest + "\"\n"

	opts, err := ValidationExpectations(ctx, runner, api.ControlPlaneVersion{Contract: 3, Build: validationOldBuild}, values)
	if err != nil {
		t.Fatal(err)
	}
	if opts.StaleBuild != "" {
		t.Errorf("StaleBuild = %q although the digests match, so the image is the same one", opts.StaleBuild)
	}
}

// THE GATES STAGE ESTABLISHES IT, WHILE THE OLD BACKEND IS STILL THE ONE
// RUNNING. After the fence stage the Deployments are the chart's, so a
// comparison made later would answer "unchanged" for every upgrade — which is
// exactly the shape of the defect hardware found.
func TestTheGatesStageEstablishesWhatTheValidationMustSee(t *testing.T) {
	values := upgradeTestValues(t, map[string]any{
		"repository": runningBackendRepository,
		"tag":        recordedPinTag,
		"digest":     recordedPinDigest,
	})
	// The Deployment runs a DIFFERENT build from the one the values pin.
	runner := newStageRunner(t, values)
	runner.runningImage = runningBackendRepository + ":" + runningBackendTag
	s := testUpgradeSession(t, runner, values)
	s.Opts.Before = api.ControlPlaneVersion{Contract: 3, Build: validationOldBuild}

	if err := stageGates(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if s.Validation.MinContract != 3 {
		t.Errorf("MinContract = %d, want the era the control plane reported before the upgrade", s.Validation.MinContract)
	}
	if s.Validation.StaleBuild != validationOldBuild {
		t.Errorf("StaleBuild = %q, want the pre-upgrade build: the chart moves the image, so answering with it means the old code is still serving", s.Validation.StaleBuild)
	}
	if s.Validation.WantBuild != recordedPinTag {
		t.Errorf("WantBuild = %q, want the chart's backend tag %q", s.Validation.WantBuild, recordedPinTag)
	}
}

// The new backend's pod can be rolled out and not yet listening when the
// validation first dials it: on hardware (2026-09-26) a re-run's chart stage
// passed in 3 s and the validation's single attempt was refused with
// "connection refused". A backend that is not answering YET is waited for,
// within the deadline; a backend that answers with the wrong build is refused
// at once.
func TestValidationWaitsForABackendThatIsNotListeningYet(t *testing.T) {
	refusals := 2
	calls := 0
	check := func() error {
		calls++
		if calls <= refusals {
			return fmt.Errorf("reading the upgraded backend's version through 10.43.0.1:8000: %w",
				&url.Error{Op: "Get", URL: "http://kubenest-backend/api/v1/version", Err: errors.New("connect failed (\"Connection refused\")")})
		}
		return nil
	}
	if err := retryUnreachable(context.Background(), time.Second, time.Millisecond, check); err != nil {
		t.Fatalf("a backend that answered on the third dial was refused: %v", err)
	}
	if calls != refusals+1 {
		t.Errorf("the validation dialled %d time(s), want %d", calls, refusals+1)
	}

	calls = 0
	wrong := func() error {
		calls++
		return errors.New("the backend behind 10.43.0.1:8000 still reports the build that was serving before this upgrade")
	}
	if err := retryUnreachable(context.Background(), time.Second, time.Millisecond, wrong); err == nil || calls != 1 {
		t.Errorf("a wrong build was retried (%d call(s), err %v): only an unreachable backend is waited for", calls, err)
	}
}
