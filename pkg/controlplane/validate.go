package controlplane

import (
	"context"
	"fmt"
	"net"

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
//	WantBuild    an exact stamp, for a caller that does know it.
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
	if got.Contract < o.MinContract {
		return fmt.Errorf("the backend behind %s reports contract era %d, below the era %d the control plane reported before this upgrade: the counter never goes down, so an older build is still the one serving and the fence stays up", addr, got.Contract, o.MinContract)
	}
	if o.WantBuild != "" && got.Build != o.WantBuild {
		return fmt.Errorf("the backend behind %s reports build %q, but this upgrade applied %q: the fence stays up", addr, got.Build, o.WantBuild)
	}
	if o.StaleBuild != "" && got.Build == o.StaleBuild {
		return fmt.Errorf("the backend behind %s still reports build %q, the build that was serving before this upgrade: the new image did not roll, so the fence stays up", addr, got.Build)
	}
	if got.Build == "" {
		return fmt.Errorf("the backend behind %s reports no build stamp, so nothing can say which image is serving; the upgrade pins the backend by digest and an unstamped image cannot be shown to be the one it applied", addr)
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
