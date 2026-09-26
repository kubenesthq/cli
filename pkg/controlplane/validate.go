package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
)

// VALIDATION THROUGH THE SSH PORT-FORWARD (T7.0 item 5).
//
// The fence is up while the new backend is validated: api.<domain> is served by
// the 503 page, so the ONLY way to reach the backend the upgrade just rolled is
// from the server node itself. The precedent already exists and is reused
// rather than reinvented — pkg/install reaches the backend the same way at the
// end of a --control-plane install (BackendAddr + the SSH connection's DialTCP
// + api.New with WithDialContext), because the backend is a ClusterIP that only
// the node can route to and DNS for api.<domain> does not point anywhere yet.
//
// The validation asserts TWO things, and the second is the one that catches a
// backend that started but cannot serve: the control plane REPORTS the era and
// build the upgrade applied, and a real unauthenticated route answers.
const (
	// smokePath is the control plane's own liveness route. It is
	// unauthenticated by contract (era 1 removed the version from it), so it
	// is a request that exercises routing, the ASGI app and the response
	// pipeline without depending on a token being right.
	smokePath = "/api/v1/health"
)

// portDialer is the capability the SSH connection must offer to open the
// tunnel. A named interface rather than an inline assertion so the failure
// message can say which capability is missing.
type portDialer interface {
	DialTCP(ctx context.Context, addr string) (net.Conn, error)
}

// ClientOpener builds a client for the backend at an address only the node can
// route to. The caller owns the credential and the CA, because both live in the
// caller's configuration and neither belongs in this package.
type ClientOpener func(dial func(ctx context.Context, network, addr string) (net.Conn, error)) (*api.Client, error)

// ValidationOptions is what the validation is asked to prove.
//
// THE CLI CANNOT KNOW THE BUILD IT IS ABOUT TO GET, and pretending otherwise
// would make this check a tautology. What it CAN know is what it is upgrading
// FROM and what the counter's own rule forbids:
//
//	MinContract  the contract era never goes DOWN. An era below the one the
//	             control plane reported before the upgrade means an older image
//	             is still serving, whatever the chart said it applied.
//	StaleBuild   the build stamp the OLD backend reported, when the chart's
//	             backend image has changed. The new one must not answer with
//	             it: that is exactly "the old code is still the one serving".
//	WantBuild    a PREFIX the build must start with: the chart's backend tag,
//	             which is short while the image reports the full sha.
type ValidationOptions struct {
	// MinContract is the lowest contract era this may report.
	MinContract int
	// StaleBuild must NOT be what it reports, when non-empty.
	StaleBuild string
	// WantBuild must be exactly what it reports, when non-empty.
	WantBuild string
}

// ValidateThroughThePortForward asserts the control plane now serving reports
// the identity this upgrade applied, and answers a real request.
//
// IT RUNS WHILE THE FENCE IS UP, which is the point: the public route is the
// 503 page, so a validation that went through it would be validating the fence.
func ValidateThroughThePortForward(ctx context.Context, server k3s.Runner, open ClientOpener, o ValidationOptions) error {
	addr, err := BackendAddr(ctx, server)
	if err != nil {
		return err
	}
	tunnel, ok := server.(portDialer)
	if !ok {
		return fmt.Errorf("the SSH connection to the server cannot open a tunnel to the control plane backend (%s), so the new backend cannot be validated while the fence is up", addr)
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return tunnel.DialTCP(ctx, addr)
	}
	client, err := open(dial)
	if err != nil {
		return err
	}

	got, err := client.ControlPlaneVersion(ctx)
	if err != nil {
		if api.IsVersionEndpointAbsent(err) {
			return fmt.Errorf("the backend behind %s does not serve GET /api/v1/version, so the upgrade cannot tell which build it just rolled — that is an older backend still serving, and the fence must not be lifted: %w", addr, err)
		}
		return fmt.Errorf("reading the upgraded backend's version through %s: %w", addr, err)
	}
	if err := checkReportedVersion(o, got, addr); err != nil {
		return err
	}

	// A REAL REQUEST. The version route answers from a constant, so it proves
	// the process is up and nothing more; this exercises the request pipeline
	// the customers' API calls will take when the fence comes down.
	status, body, err := client.Get(ctx, smokePath)
	if err != nil {
		return fmt.Errorf("the smoke request %s against the upgraded backend through %s failed: %w", smokePath, addr, err)
	}
	if status != 200 {
		return fmt.Errorf("the smoke request %s against the upgraded backend through %s answered HTTP %d, so the backend is up but not serving: the fence stays up", smokePath, addr, status)
	}
	if len(body) == 0 {
		return fmt.Errorf("the smoke request %s against the upgraded backend through %s answered no body, which is not a control plane answering its own health route", smokePath, addr)
	}
	return nil
}

// ValidationReporter wraps the validation in the converge vocabulary, so the
// upgrade stage's log says what was checked rather than only what failed.
func ValidationReporter(ctx context.Context, server k3s.Runner, open ClientOpener, o ValidationOptions, rep converge.Reporter) error {
	err := ValidateThroughThePortForward(ctx, server, open, o)
	if rep != nil {
		event := converge.Event{
			Check:   "kubenest-control-plane-validation",
			Outcome: converge.Pass,
			State: converge.State{
				Object: "backend through the node's port-forward",
				Status: fmt.Sprintf("contract era >= %d, build not %q, %s answered", o.MinContract, o.StaleBuild, smokePath),
			},
		}
		if err != nil {
			event.Outcome = converge.Fail
			event.State.Status = err.Error()
		}
		rep.Report(event)
	}
	return err
}

// checkReportedVersion is the identity half of the validation, on its own so its
// terms can be tested without a tunnel to a live backend.
func checkReportedVersion(o ValidationOptions, got api.ControlPlaneVersion, addr string) error {
	if got.Contract < o.MinContract {
		return fmt.Errorf("the backend behind %s reports contract era %d, below the era %d the control plane reported before this upgrade: the counter never goes down, so an older build is still the one serving and the fence stays up", addr, got.Contract, o.MinContract)
	}
	if o.StaleBuild != "" && got.Build == o.StaleBuild {
		return fmt.Errorf("the backend behind %s still reports the build that was serving before this upgrade (%q): the new image did not roll, so the fence stays up", addr, got.Build)
	}
	if o.WantBuild != "" && !strings.HasPrefix(got.Build, o.WantBuild) {
		return fmt.Errorf("the backend behind %s reports build %q, which is not the chart's backend tag %q: the image this upgrade applied cannot be tied to the build that is serving, so the fence stays up", addr, got.Build, o.WantBuild)
	}
	if got.Build == "" {
		return fmt.Errorf("the backend behind %s reports no build stamp, so nothing can say which image is serving; the upgrade pins the backend by digest and an unstamped image cannot be shown to be the one it applied", addr)
	}
	return nil
}

// ValidationExpectations works out what the upgraded backend must report, from
// the build this operation started from and the image the chart declares.
//
// THE ERA ALONE IS NOT A DISCRIMINATOR. Hardware (2026-09-25) ran an upgrade
// whose every stage was skipped — it changed nothing — and the validation passed
// in 0 s against the PREVIOUS candidate's image, because the only checks were
// "the era is not below the one before" and "the build is not empty". Both held.
//
// IT DOES NOT READ THE RUNNING IMAGE AT ALL, and that is deliberate. The question
// the validation answers is "is the code this run moved to the one serving?", and
// the answer has to hold on a resume whose live control plane ALREADY runs the
// new code — the .3 recovery brings it back behind the fence, so declared and
// running are the same image there, and a rule keyed on "the images differ"
// demands nothing exactly when the run has most to answer for. So:
//
//	WantBuild   the chart's own tag, which the answering build must start with;
//	StaleBuild  the build this operation started from, unless the chart's pin IS
//	            that build: a genuine no-op run reports the same build and
//	            refusing it would refuse a correct run.
//	            (See the body for the two cases, and hardware, 2026-09-26, run
//	            26, for what the previous rule cost.)
func ValidationExpectations(before api.ControlPlaneVersion, valuesYAML string) (ValidationOptions, error) {
	out := ValidationOptions{MinContract: before.Contract}
	declared, ok, err := backendImageFromValues(valuesYAML)
	if err != nil {
		return out, err
	}
	if !ok {
		body, err := ChartFile("values.yaml")
		if err != nil {
			return out, fmt.Errorf("reading the control-plane chart's values: %w", err)
		}
		declared, ok, err = backendImageFromValues(string(body))
		if err != nil {
			return out, fmt.Errorf("reading the control-plane chart's values: %w", err)
		}
		if !ok {
			return out, fmt.Errorf("the control-plane chart's values.yaml declares no backend.image, so which build the upgrade applies cannot be established; a validation that cannot say what it is looking for is not one")
		}
	}
	// THE RULE IS ABOUT THE TWO BUILDS, NOT ABOUT THE TWO IMAGES.
	//
	// It used to compare the declared image with the one the Deployment runs and
	// demand nothing when they matched. On a resume that is wrong: the .3 recovery
	// brings the NEW code back behind the fence, so the resume finds declared ==
	// running, and a run that had already moved to the new code still has to be
	// held to it — "the same image is answering" is also what a run that rolled
	// NOTHING produces (hardware, 2026-09-26, run 26: the resumed validation
	// demanded no build at all, `build not "", build prefix ""`).
	//
	//	WantBuild   the tag the chart declares, when it declares one: the
	//	            answering build must start with it, so the image serving can be
	//	            tied to the one this run applied. An image pinned by digest
	//	            alone names no build prefix, so nothing is demanded of the
	//	            build stamp.
	//	StaleBuild  the build THIS OPERATION started from, unless that build is
	//	            the one the chart declares: a genuine no-op run — every resume
	//	            of an already-upgraded control plane — reports exactly that
	//	            build, and refusing it would refuse a correct run.
	if declared.tag == "" {
		return out, nil
	}
	out.WantBuild = declared.tag
	if strings.HasPrefix(before.Build, declared.tag) {
		return out, nil
	}
	out.StaleBuild = before.Build
	return out, nil
}

// RunningBackendImageRef reads the image reference the backend Deployment runs,
// verbatim.
func RunningBackendImageRef(ctx context.Context, r k3s.Runner) (string, error) {
	out, err := k3s.Kubectl(ctx, r, backendDeploymentImageCmd)
	if err != nil {
		return "", fmt.Errorf("reading the backend Deployment %s/%s: %w", Namespace, backendService, err)
	}
	ref := strings.TrimSpace(out)
	if ref == "" {
		return "", fmt.Errorf("the backend Deployment %s/%s names no image, so which build is serving cannot be established", Namespace, backendService)
	}
	return ref, nil
}

// backendImageFromValues extracts backend.image from a values document. It uses
// the same splitter the PostgreSQL pin uses — both are container references and
// one parser is enough — and the same precedence: a values override wins over
// the chart's own defaults, because a document that sets backend.image IS the
// chart this upgrade applies.
//
// IT IS ALWAYS backend.image AND NEVER backend.heldImage. heldImage is the pin
// the fence and the restore hold the backend DEPLOYMENT on, so it names the code
// this upgrade is moving AWAY from; the declared image is the code the chart is
// moving to, and a values document read by a run no longer carries either key
// (ValuesWithoutTheFencePin), which is why the chart's own values.yaml is what
// normally answers here.
func backendImageFromValues(valuesYAML string) (postgresImage, bool, error) {
	if strings.TrimSpace(valuesYAML) == "" {
		return postgresImage{}, false, nil
	}
	doc := map[string]any{}
	if err := yaml.Unmarshal([]byte(valuesYAML), &doc); err != nil {
		return postgresImage{}, false, err
	}
	backend, _ := doc["backend"].(map[string]any)
	if backend == nil {
		return postgresImage{}, false, nil
	}
	image, _ := backend["image"].(map[string]any)
	if image == nil {
		return postgresImage{}, false, nil
	}
	repository, _ := image["repository"].(string)
	tag, _ := image["tag"].(string)
	digest, _ := image["digest"].(string)
	if repository == "" && tag == "" && digest == "" {
		return postgresImage{}, false, nil
	}
	ref := repository
	if tag != "" {
		ref += ":" + tag
	}
	if digest != "" {
		ref += "@" + digest
	}
	return parsePostgresImage(ref), true, nil
}

// retryUnreachable runs check until it passes, returns an error that is not a
// transport failure, or the deadline passes.
//
// ONLY AN UNREACHABLE BACKEND IS WAITED FOR. The new backend's pod can be rolled
// out and not yet listening when the validation first dials it (hardware,
// 2026-09-26: a re-run's chart stage passed in 3 s and the single attempt was
// refused with "connection refused"). A backend that answers with the wrong
// build or era is a verdict, not a delay, and is returned at once.
func retryUnreachable(ctx context.Context, within, every time.Duration, check func() error) error {
	deadline := time.Now().Add(within)
	for {
		err := check()
		var transport *url.Error
		if err == nil || !errors.As(err, &transport) || !time.Now().Before(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(every):
		}
	}
}
