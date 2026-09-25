package cmd

import (
	"context"
	"errors"
	"fmt"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/version"
)

// requireControlPlane refuses a control plane whose contract era is below the
// floor the CLI's operations need, naming the control-plane upgrade as the fix.
//
// IT IS THE ONE PLACE THE CHECK IS MADE, deliberately. A check written at each
// call site would be a check some call site forgets, and the failure it is
// meant to prevent — an operation that needs a behaviour a control plane does
// not implement — reads as an unexplained 404 several steps later.
//
// A 404 FROM THE VERSION ROUTE IS NOT A REFUSAL. Every control plane built
// before the contract counter answers 404, including ones that already carry
// the behaviour an era was minted for, so absence is "cannot tell" and the
// command proceeds. That is the difference between this and every other gate in
// the CLI: a gate that cannot see must not claim to have seen.
func requireControlPlane(ctx context.Context, client *api.Client) error {
	reported, err := client.ControlPlaneVersion(ctx)
	if err != nil {
		if api.IsVersionEndpointAbsent(err) {
			return nil
		}
		return err
	}
	return version.RequireControlPlane(version.Report{
		Era:   reported.Contract,
		Build: reported.Build,
	})
}

// controlPlaneVersionRefusal is the refusal in requireControlPlane's error,
// when there was one. A caller that wants to report rather than refuse — the
// diff command among them — uses it instead of matching on text.
func controlPlaneVersionRefusal(err error) (*version.Refusal, bool) {
	var refusal *version.Refusal
	if err != nil && errors.As(err, &refusal) {
		return refusal, true
	}
	return nil, false
}

// checkControlPlaneNow runs the check for a command path without handing back
// the client: an entry point that needs the verdict but builds its own
// transport (or none) uses this.
func checkControlPlaneNow(ctx context.Context, path string) error {
	_, err := controlPlaneClientChecked(ctx, path)
	return err
}

// commandsNeedingTheControlPlane lists the command paths whose operations need
// the control plane's API, in the order they appear in the command tree.
//
// SIGNING IN IS NOT ONE OF THEM, and neither are local inspection,
// `recovery-kit check`, recovery or resuming an operation: those check
// compatibility against the recovery set's manifest, which is a document the
// operator holds, not a live endpoint (PLAN 7.8). A login that required a
// version endpoint could not be the command an operator runs to reach a control
// plane whose era they are trying to establish.
var commandsNeedingTheControlPlane = []string{
	"login",
	"platform install",
	"platform upgrade",
	"node reboot",
	"backup",
}

// needsTheControlPlane reports whether a command path must check the control
// plane's contract era before doing its work.
//
// A path is matched as a whole word: "platform install" needs it, "platform
// installer" does not, and a subcommand of a needing path inherits it because
// its own path has the needing path as a prefix.
func needsTheControlPlane(path string) bool {
	for _, needing := range commandsNeedingTheControlPlane {
		if path == needing {
			return true
		}
		if len(path) > len(needing) && path[:len(needing)] == needing && path[len(needing)] == ' ' {
			return true
		}
	}
	return false
}

// checkControlPlaneForCommand runs the version check for a command path, and
// does nothing for a path that does not need it.
func checkControlPlaneForCommand(ctx context.Context, path string, client *api.Client) error {
	if !needsTheControlPlane(path) {
		return nil
	}
	return requireControlPlane(ctx, client)
}

// controlPlaneClientChecked is the CLI's control-plane client, after the CLI's
// floor has been checked against the control plane's own report.
//
// ONE PLACE, BECAUSE THE CHECK IS ONLY WORTH ANYTHING IF EVERY COMMAND THAT
// NEEDS IT MAKES IT. Each of these entry points calls this instead of
// controlPlaneClient, and a command that is not in
// commandsNeedingTheControlPlane is not touched at all — which is what keeps
// `recovery-kit check`, recovery and resuming an operation working against a
// control plane whose version endpoint cannot be read.
func controlPlaneClientChecked(ctx context.Context, path string) (*api.Client, error) {
	client, err := controlPlaneClient()
	if err != nil {
		return nil, err
	}
	if err := checkControlPlaneForCommand(ctx, path, client); err != nil {
		return nil, err
	}
	return client, nil
}

// fencedVersionSource is where a FENCED command gets the version it needs.
//
// THREE ANSWERS, IN ORDER, and the order is the argument:
//
//	the public route   answers unless the fence is up;
//	the node's tunnel  where the backend is while the fence is up, because the
//	                   public route is the fence;
//	the record         what the operation wrote down when it began, which is the
//	                   only source left once the chart has stopped the backend
//	                   (`backend.replicas: 0`) for a migration that then failed.
//
// A source is a value rather than three branches inside one function so the
// fallbacks can be exercised without a cluster (kn-t70...4xso.1).
type fencedVersionSource struct {
	// Public is the client for the configured control-plane URL.
	Public *api.Client
	// Node builds a client for the backend THROUGH the node's SSH connection.
	// Nil means this command holds no node connection, which is a different
	// answer from "the node could not reach the backend".
	Node func(ctx context.Context) (*api.Client, error)
	// Recorded is the version the operation recorded when it began: from the
	// operation record on a resume, and from the live read on a first run.
	Recorded api.ControlPlaneVersion
}

// version answers with the control plane's version, behind the fence if the
// fence is up.
// version answers with the control plane's version behind the fence, and says
// whether it could be established at all.
//
// KNOWN IS A THIRD OUTCOME, not a zero version: a control plane that does not
// serve /api/v1/version at all is "cannot tell", which is not a refusal, and a
// control plane that reports contract 0 is a malformed answer the floor check
// must refuse.
func (s fencedVersionSource) version(ctx context.Context) (reported api.ControlPlaneVersion, known bool, err error) {
	reported, err = s.Public.ControlPlaneVersion(ctx)
	if err == nil {
		return reported, true, nil
	}
	if api.IsVersionEndpointAbsent(err) {
		return api.ControlPlaneVersion{}, false, nil
	}
	if !api.IsControlPlaneFenced(err) {
		// ANY OTHER FAILURE IS A FAILURE. A 503 without the fence's header is a
		// load balancer, an ingress or a dead backend — none of them is
		// permission to proceed, and none of them says anything about the
		// control plane's era.
		return api.ControlPlaneVersion{}, false, err
	}
	if s.Node != nil {
		if node, nerr := s.Node(ctx); nerr == nil {
			if reported, nerr = node.ControlPlaneVersion(ctx); nerr == nil {
				return reported, true, nil
			}
		}
	}
	if s.Recorded != (api.ControlPlaneVersion{}) {
		return s.Recorded, true, nil
	}
	return api.ControlPlaneVersion{}, false, fmt.Errorf(
		"the control plane's public route is fenced (%w) and the backend behind the node does not answer, and this operation recorded no version when it began: a resume cannot establish which control plane it is resuming, and guessing an era is how a resume walks into a control plane it does not understand — run `kubenest platform upgrade --control-plane` from the machine that started it, whose journal records what was recorded",
		err)
}

// requireControlPlaneForUpgrade is requireControlPlane for a command that may be
// running BEHIND the fence it raised: it takes the version the source
// established, so the read happens once and the floor check is the only thing
// this adds.
//
// A version that could NOT be established is passed through as nil, exactly as
// a 404 is: absence is not a determination.
func requireControlPlaneForUpgrade(reported api.ControlPlaneVersion, known bool) error {
	if !known {
		return nil
	}
	return version.RequireControlPlane(version.Report{Era: reported.Contract, Build: reported.Build})
}
