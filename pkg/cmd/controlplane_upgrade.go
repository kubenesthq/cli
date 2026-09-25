package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"
	"kubenest.io/cli/pkg/controlplane"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/upgrade"
	"kubenest.io/cli/pkg/version"
	"kubenest.io/cli/pkg/window"
)

// addControlPlaneUpgradeFlags adds the flags this path contributes to
// `platform upgrade`.
func addControlPlaneUpgradeFlags(cmd *cobra.Command, f *UpgradeFlags) {
	fs := cmd.Flags()
	fs.BoolVar(&f.ControlPlane, "control-plane", false, "upgrade the KubeNest control plane itself: fence the public API, take a checkpoint, run the migration Job, roll the new chart and validate it through the node (the management cluster)")
	fs.StringVar(&f.Resume, "resume", "", "continue an interrupted control-plane upgrade by operation id, as reported when it stopped. Every remote action is recorded before it is submitted, so this reconciles what happened instead of repeating it")
}

// runControlPlaneUpgrade is `kubenest platform upgrade --control-plane`.
//
// It runs pkg/controlplane's seven stages through the SAME staging engine and
// journal as every other operation, with the operation record as the lock — see
// pkg/controlplane/upgrade.go for the order and why it is that order.
//
// EVERY GATE RUNS BEFORE THE FENCE GOES UP. Four of them are assembled here
// because only this layer can: compatibility (the CLI's floor against the
// control plane's own report), the maintenance window (the cluster's stored
// one), whether a recovery point exists at all (the values the control plane
// runs with), and whether another operation holds the record. The two that need
// the cluster — the field-ownership assertion and the PostgreSQL pin — are run
// by the gates stage itself.
func runControlPlaneUpgrade(ctx context.Context, out io.Writer, f UpgradeFlags) error {
	client, err := controlPlaneClientChecked(ctx, "platform upgrade")
	if err != nil {
		return err
	}

	// The management cluster's name is needed to find its record, its window
	// and its journal. Without --cluster it is the only install journal on this
	// machine; several is a question rather than a guess, because upgrading the
	// wrong control plane is not recoverable.
	if f.Cluster == "" {
		name, err := onlyInstallCluster()
		if err != nil {
			return err
		}
		f.Cluster = name
	}

	// AN INTERRUPT IS NOT A FAILURE. SIGINT/SIGTERM cancels the run and leaves
	// the operation record STOPPED rather than terminal-failed, because the
	// whole point of a recorded step is that a second laptop can continue it —
	// and a terminal record cannot be continued, only restarted. That is the
	// difference between "my laptop died mid-migration" and "the migration
	// failed".
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	session, err := buildUpgradeSession(ctx, out, f)
	if err != nil {
		return err
	}
	defer session.Close()
	if err := session.Connect(ctx); err != nil {
		return err
	}
	server, err := session.Server()
	if err != nil {
		return err
	}

	// THE VALUES THE CONTROL PLANE RUNS WITH, read from the cluster. The
	// chart's archive carries the new pins; the settings, the generated secrets
	// and the control plane's CA are facts about this installation and are
	// never re-derived here — re-deriving them would rotate a signing key or
	// replace the CA every agent pins.
	values, err := currentControlPlaneValues(ctx, server)
	if err != nil {
		return err
	}

	// What the control plane says it is NOW. It is the floor the validation
	// measures against, because the contract counter never goes down: a lower
	// era after the upgrade means an older build is still the one serving.
	before, err := client.ControlPlaneVersion(ctx)
	if err != nil && !api.IsVersionEndpointAbsent(err) {
		return err
	}

	checkpointConfigured, err := valuesFlagEnabled(values, "checkpoint", "enabled")
	if err != nil {
		return err
	}

	opts := controlplane.UpgradeOptions{
		Cluster:      f.Cluster,
		From:         session.From.Bundle,
		To:           f.To,
		Values:       values,
		Bundle:       session.To,
		Server:       server,
		Before:       before,
		Open:         controlPlaneOpener(),
		Out:          out,
		Reporter:     converge.NewTextReporter(out),
		Window:       session.Window,
		WindowErr:    session.WindowErr,
		BypassWindow: f.Now,
		Gates: []controlplane.Gate{
			{
				Name:   "Control-plane compatibility",
				Passed: true,
				Detail: fmt.Sprintf("the control plane reports contract era %d, and this CLI's operations need era %d or later", before.Contract, requiredControlPlaneEra()),
			},
			windowGate(session, f.Now),
			{
				Name:   "Recovery point",
				Passed: checkpointConfigured,
				Detail: "the control plane's own checkpoint is enabled, so a state worth returning to can be published before anything changes",
				Fix:    "install the control plane with a backup target (`--backup-target`) before upgrading it. An upgrade with no recovery point must not start: the fence cannot be lifted onto a state nobody can return to",
			},
		},
	}

	journalPath, err := controlplane.JournalPath(f.Cluster)
	if err != nil {
		return err
	}
	journal, err := stages.OpenJournal(journalPath, opts.Identity())
	if err != nil {
		return err
	}
	cp := &controlplane.UpgradeSession{
		ID:   stages.NewRunID(),
		Opts: opts,
		Jnl:  journal,
		Emit: stages.Emitters{stages.TextEmitter{W: out}},
	}

	// THE RECORD IS THE LOCK. Taking it is the "no other operation" gate, and
	// it is what refuses a second laptop while this upgrade is unfinished.
	store, handle, skip, err := controlPlaneLock(ctx, server, f, session, out)
	if err != nil {
		return err
	}
	cp.WithOperation(handle, skip)

	fmt.Fprintf(out, "Upgrading the control plane %s from bundle %s to %s.\n", f.Cluster, opts.From, opts.To)
	fmt.Fprintf(out, "Fence first, migration second, validation last: the public API stops answering the backend\n"+
		"before anything changes and answers it again only once the new build is proven.\n\n")

	result, runErr := stages.Execute(ctx, cp, controlplane.Plan(cp))
	// A run the operator interrupted is STOPPED, not terminal: the record has
	// to stay continuable, or the second laptop has nothing to finish.
	endControlPlaneOperation(ctx, out, store, handle, runErr, ctx.Err() != nil)
	if runErr != nil {
		return runErr
	}
	fmt.Fprintf(out, "\nUpgraded the control plane to %s in %s.\n", opts.To, result.Elapsed.Round(time.Second))
	return nil
}

// endControlPlaneOperation closes the record the way the run ended.
//
// STOPPED IS NOT TERMINAL AND TERMINAL IS NOT STOPPED. An interrupted run
// leaves the record in place, stopped, so `--resume` can take it over; a failed
// run ends the record, so the operator's fix-then-re-run starts a new operation
// over a cluster whose state the record describes.
func endControlPlaneOperation(ctx context.Context, out io.Writer, store *operation.Store, handle *operation.Handle, runErr error, interrupted bool) {
	if store == nil || handle == nil {
		return
	}
	var err error
	switch {
	case runErr == nil:
		err = store.Complete(ctx, handle, operation.ResultSucceeded)
	case interrupted || errors.Is(runErr, stages.ErrPaused):
		err = store.Stop(ctx, handle)
		fmt.Fprintf(out, "\nInterrupted. The operation record %s is left in place: continue it with\n"+
			"  kubenest platform upgrade --control-plane --cluster %s --to <bundle> --resume %s\n",
			handle.OperationID(), clusterOf(handle), handle.OperationID())
	default:
		err = store.Complete(ctx, handle, operation.ResultFailed)
	}
	if err != nil {
		fmt.Fprintf(out, "warning: the operation record %s could not be closed: %v\n", handle.OperationID(), err)
	}
}

// clusterOf names the cluster a record is about, for the resume line.
func clusterOf(handle *operation.Handle) string {
	rec := handle.Record()
	if rec == nil {
		return "<cluster>"
	}
	return rec.Request.Cluster
}

// controlPlaneLock takes the operation record, or takes over the one named.
func controlPlaneLock(
	ctx context.Context,
	server k3s.Runner,
	f UpgradeFlags,
	session *upgrade.Session,
	out io.Writer,
) (*operation.Store, *operation.Handle, map[string]bool, error) {
	store := &operation.Store{Runner: server}
	req := operation.Request{
		Kind:     operation.KindControlPlaneUpgrade,
		Cluster:  f.Cluster,
		Versions: map[string]string{"bundle": session.From.Bundle + " -> " + f.To},
	}
	for _, node := range session.Nodes {
		req.Targets = append(req.Targets, operation.Target{HostID: node.Address})
	}
	if f.Resume == "" {
		handle, err := store.Acquire(ctx, req)
		if err != nil {
			return nil, nil, nil, err
		}
		return store, handle, nil, nil
	}

	// THE RECORD IS READ BEFORE ANYTHING IS SUBMITTED, and its probes are
	// read-only. A resume that reconciled by changing things would be a resume
	// that guesses.
	plan, err := operation.Resume(ctx, store, f.Resume)
	if err != nil {
		var blocked *operation.BlockedError
		if errors.As(err, &blocked) {
			return nil, nil, nil, err
		}
		return nil, nil, nil, err
	}
	if err := plan.Verify(req); err != nil {
		return nil, nil, nil, err
	}
	fmt.Fprintf(out, "Resuming operation %s, stopped at %s: %d step(s) established, %d to repeat.\n",
		f.Resume, plan.Record.Stage, len(plan.Skip()), len(plan.Steps)-len(plan.Skip()))
	for _, step := range plan.Steps {
		fmt.Fprintf(out, "  %s %s (%s): %s\n", step.Decision, step.ActionID, step.Stage, step.Reason)
	}
	handle, err := operation.TakeOver(ctx, store, f.Resume)
	if err != nil {
		return nil, nil, nil, err
	}
	return store, handle, resumableSkip(plan), nil
}

// resumableSkip is the skip set, with the checkpoint Job's creation removed.
//
// ITS EFFECT CANNOT BE ESTABLISHED BY OBSERVATION. Skipping it is not "it
// already happened": the checkpoint stage waits for a checkpoint that did not
// exist before the Job, so a skipped create is a run that waits out its whole
// deadline for a Job nobody was allowed to make. The stage reconciles that step
// itself, against the checkpoint the control plane publishes.
func resumableSkip(plan *operation.Plan) map[string]bool {
	skip := plan.Skip()
	for _, step := range plan.Steps {
		if step.Stage == controlplane.StageCheckpoint {
			delete(skip, step.ActionID)
		}
	}
	return skip
}

// controlPlaneOpener builds clients for the backend through the node's
// port-forward, carrying the operator's own token and CA.
func controlPlaneOpener() controlplane.ClientOpener {
	return func(dial func(ctx context.Context, network, addr string) (net.Conn, error)) (*api.Client, error) {
		cfg, err := config.Load()
		if err != nil {
			return nil, err
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			return nil, err
		}
		token := creds.TokenFor(cfg.ControlPlaneURL)
		if token == "" {
			return nil, fmt.Errorf("not logged in to %s: the validation reads GET /api/v1/version through the node's port-forward and that route is authenticated", cfg.ControlPlaneURL)
		}
		opts := []api.Option{api.WithToken(token), api.WithDialContext(dial)}
		if cfg.ControlPlaneCA != "" {
			opts = append(opts, api.WithCACert([]byte(cfg.ControlPlaneCA)))
		}
		return api.New("http://kubenest-backend", opts...)
	}
}

// windowGate is the maintenance-window gate: the same rule and the same
// refusals as every other operation's, because a control plane that goes
// unavailable for a migration is a bigger interruption than a component
// upgrade, not a smaller one.
func windowGate(session *upgrade.Session, bypass bool) controlplane.Gate {
	if bypass {
		return controlplane.Gate{Name: "Maintenance window", Passed: true,
			Detail: "--now: the window is bypassed for this run; every other gate still runs"}
	}
	if session.WindowErr != nil {
		return controlplane.Gate{Name: "Maintenance window", Passed: false,
			Detail: "the cluster's maintenance window could not be read, so whether now is inside it is unknown: " + session.WindowErr.Error(),
			Fix:    "an unread window is not a passed check; " + window.NoWindowFix}
	}
	if session.Window == nil {
		return controlplane.Gate{Name: "Maintenance window", Passed: false,
			Detail: window.NoWindow, Fix: window.NoWindowFix}
	}
	if err := session.Window.Outside(time.Now()); err != nil {
		return controlplane.Gate{Name: "Maintenance window", Passed: false, Detail: err.Error(), Fix: window.OutsideFix}
	}
	return controlplane.Gate{Name: "Maintenance window", Passed: true, Detail: "inside " + session.Window.String()}
}

// requiredControlPlaneEra is the era this CLI needs, for the gate's detail
// line. It comes from pkg/version, which is the one place the floor is
// recorded.
func requiredControlPlaneEra() int {
	required, ok := version.RequiredEra()
	if !ok {
		return 0
	}
	return required.Era
}

// onlyInstallCluster names the cluster whose install journal this machine
// holds, refusing when there is none or when there is more than one.
func onlyInstallCluster() (string, error) {
	_, path, err := findJournal("")
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("--cluster is required: this machine has no install journal naming the cluster that runs this control plane, so pass the name it is registered under")
	}
	return strings.TrimSuffix(filepath.Base(path), ".json"), nil
}

// currentControlPlaneValues reads the values document the control plane is
// running with, from the HelmChart the CLI applied.
func currentControlPlaneValues(ctx context.Context, server k3s.Runner) (string, error) {
	out, err := k3s.Kubectl(ctx, server,
		"get helmchart "+controlplane.ReleaseName+" -n kube-system -o jsonpath={.spec.valuesContent}")
	if err != nil {
		return "", fmt.Errorf("reading the control plane's applied values from HelmChart %s in kube-system: %w", controlplane.ReleaseName, err)
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("HelmChart %s in kube-system carries no values, so the settings, secrets and control-plane CA this installation runs with cannot be carried onto the new chart. A control-plane upgrade never renders them again: it would rotate a signing key and replace the CA every agent pins", controlplane.ReleaseName)
	}
	return out, nil
}

// valuesFlagEnabled reports whether a nested boolean in a values document is
// true. An absent group is false, which is what "not configured" means.
func valuesFlagEnabled(valuesYAML string, path ...string) (bool, error) {
	doc := map[string]any{}
	if err := yaml.Unmarshal([]byte(valuesYAML), &doc); err != nil {
		return false, fmt.Errorf("the control plane's applied values are not readable YAML: %w", err)
	}
	var current any = doc
	for _, key := range path {
		mapping, ok := current.(map[string]any)
		if !ok {
			return false, nil
		}
		current, ok = mapping[key]
		if !ok {
			return false, nil
		}
	}
	enabled, _ := current.(bool)
	return enabled, nil
}
