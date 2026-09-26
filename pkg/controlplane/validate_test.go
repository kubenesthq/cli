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
)

// VALIDATION MUST BE ABLE TO TELL A NEW IMAGE FROM THE OLD ONE STILL SERVING.
//
// The validation compares a contract era and a build stamp, and the era alone
// is not enough: hardware (2026-09-25) ran an upgrade whose every stage was
// skipped — it changed nothing — and `control-plane-validation` passed in 0 s
// against the PREVIOUS candidate's image, because the only checks were "the era
// is not below the one before" and "the build is non-empty".
//
// The discriminator is the BUILD, and the rule does not read the running image at
// all: the answering build must start with the tag the chart pins, and it must not
// be the build this operation started from — unless that IS the chart's build, in
// which case a genuine no-op run reports it and refusing it would refuse a correct
// re-run or resume. Keying the rule on the running image instead demanded nothing
// on a resume whose live control plane already runs the new code, which is exactly
// the run that has most to answer for (hardware, 2026-09-26, run 26).
const (
	validationOldBuild = "c121ed887750b1d3196d54fe9fd8368791a1bf03"
	validationNewTag   = "9e9698d"
	validationNewBuild = "9e9698dcafe4d1a3c25e29f9fd8368791a1bf03"
)

// validationValues is a values document pinning the NEW backend image, which is
// what an upgrade's values carry: the chart's own pin, carried through.
func validationValues() string {
	return "domain: kn.example.com\n" +
		"backend:\n  image:\n    repository: ghcr.io/kubenesthq/kubenest-backend\n" +
		"    tag: \"" + validationNewTag + "\"\n" +
		"    digest: \"sha256:1111111111111111111111111111111111111111111111111111111111111111\"\n"
}

// The build this operation started from must be REFUSED: otherwise an upgrade
// whose chart roll silently failed reports success.
func TestValidationRefusesTheBuildThisOperationStartedFrom(t *testing.T) {
	opts, err := ValidationExpectations(api.ControlPlaneVersion{Contract: 3, Build: validationOldBuild}, validationValues())
	if err != nil {
		t.Fatal(err)
	}
	if opts.StaleBuild != validationOldBuild {
		t.Fatalf("StaleBuild = %q, want the build this operation started from (%s): answering with it means the old code is still the one serving", opts.StaleBuild, validationOldBuild)
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

// A RESUME THAT FINDS THE NEW CODE ALREADY RUNNING STILL DEMANDS THE NEW BUILD
// (the follow-up to kn-t70-control-plane-version-identity-4xso.7, found in run 26).
//
// The .3 recovery brings the new code back behind the fence, so such a resume's
// live control plane reports the NEW build and the Deployment already runs the new
// image: declared and running are THE SAME IMAGE. The rule that keyed on the
// images differing demanded nothing at all there — on hardware the resumed
// validation read `build not "", build prefix ""` — while the operation had moved
// the code and had to answer for it. The rule now reads no running image: what
// must not answer is the build this operation STARTED from, and what must is the
// chart's own tag.
func TestAResumedRunAfterTheNewCodeRolledStillDemandsTheNewBuild(t *testing.T) {
	// declared == running: the values carry the chart's pin, which is the image
	// the Deployment already runs after the recovery brought it back.
	opts, err := ValidationExpectations(api.ControlPlaneVersion{Contract: 3, Build: validationOldBuild}, validationValues())
	if err != nil {
		t.Fatal(err)
	}
	if opts.StaleBuild != validationOldBuild {
		t.Errorf("StaleBuild = %q, want the build this operation started from (%s): the new code serving is what the run moved to, and the code it came from must not answer", opts.StaleBuild, validationOldBuild)
	}
	if opts.WantBuild != validationNewTag {
		t.Errorf("WantBuild = %q, want the chart's tag %q: the answering build has to be tied to the image this run applied", opts.WantBuild, validationNewTag)
	}
	if err := checkReportedVersion(opts, api.ControlPlaneVersion{Contract: 3, Build: validationNewBuild}, "10.0.0.1:8000"); err != nil {
		t.Errorf("the new code answering was refused: %v", err)
	}
	if err := checkReportedVersion(opts, api.ControlPlaneVersion{Contract: 3, Build: validationOldBuild}, "10.0.0.1:8000"); err == nil {
		t.Error("the build this operation started from was accepted, although the run moved the code away from it")
	}
}

// A NO-OP RUN IS ACCEPTED: when the build this operation started from IS the
// build the chart pins, the same build is exactly what a correct run reports, and
// refusing it would refuse every re-run and every resume.
func TestValidationAcceptsTheSameBuildWhenTheChartPinsIt(t *testing.T) {
	opts, err := ValidationExpectations(api.ControlPlaneVersion{Contract: 3, Build: validationNewBuild}, validationValues())
	if err != nil {
		t.Fatal(err)
	}
	// The chart's tag is still DEMANDED — the image serving must be tied to the
	// artefact — but it is not called stale.
	if opts.WantBuild != validationNewTag {
		t.Errorf("WantBuild = %q, want the chart's tag %q: it is what ties the answering build to the image this run applied", opts.WantBuild, validationNewTag)
	}
	if opts.StaleBuild != "" {
		t.Fatalf("StaleBuild = %q although the build this operation started from is the chart's own: this would refuse a correct no-op run", opts.StaleBuild)
	}
	if err := checkReportedVersion(opts, api.ControlPlaneVersion{Contract: 3, Build: validationNewBuild}, "10.0.0.1:8000"); err != nil {
		t.Errorf("the chart's own build was refused: %v", err)
	}
	// The era floor still applies: the counter never goes down.
	if err := checkReportedVersion(opts, api.ControlPlaneVersion{Contract: 2, Build: validationNewBuild}, "10.0.0.1:8000"); err == nil {
		t.Error("a reported era below the one the control plane was on before the upgrade was accepted")
	}
	// And a build the chart does not pin is still refused: the prefix is what ties
	// the answering image to this run's artefact, no-op or not.
	if err := checkReportedVersion(opts, api.ControlPlaneVersion{Contract: 3, Build: "deadbeef1234"}, "10.0.0.1:8000"); err == nil {
		t.Error("a build that is not the chart's tag was accepted, so the check cannot tie the serving image to the artefact")
	}
}

// A DECLARED IMAGE WITH NO TAG NAMES NO BUILD PREFIX, so nothing can be demanded
// of the build stamp and no build can be called stale against it. The chart this
// binary carries always declares a tag; this arm is here so a values document
// that does not is not turned into a refusal nobody could satisfy.
func TestADigestOnlyPinDemandsNoBuildPrefix(t *testing.T) {
	digest := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	values := "backend:\n  image:\n    repository: ghcr.io/kubenesthq/kubenest-backend\n    digest: \"" + digest + "\"\n"

	opts, err := ValidationExpectations(api.ControlPlaneVersion{Contract: 3, Build: validationOldBuild}, values)
	if err != nil {
		t.Fatal(err)
	}
	if opts.WantBuild != "" {
		t.Errorf("WantBuild = %q for an image pinned by digest alone: there is no tag to demand as a prefix", opts.WantBuild)
	}
	if opts.StaleBuild != "" {
		t.Errorf("StaleBuild = %q for an image pinned by digest alone: a digest cannot be compared with a build stamp", opts.StaleBuild)
	}
	// The era floor is all that is left, and it still holds.
	if err := checkReportedVersion(opts, api.ControlPlaneVersion{Contract: 3, Build: validationOldBuild}, "10.0.0.1:8000"); err != nil {
		t.Errorf("a digests-only declaration refused a build: %v", err)
	}
	if err := checkReportedVersion(opts, api.ControlPlaneVersion{Contract: 2, Build: validationOldBuild}, "10.0.0.1:8000"); err == nil {
		t.Error("a reported era below the one the control plane was on before the upgrade was accepted")
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
