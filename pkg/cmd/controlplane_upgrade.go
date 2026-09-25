package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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
	"kubenest.io/cli/pkg/sshx"
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

	// THE NODE CONNECTION COMES FIRST, before any control-plane read.
	//
	// EVERY API READ THIS COMMAND MAKES CAN BE BEHIND THE FENCE THIS CLI RAISED
	// (kn-t70-control-plane-version-identity-4xso.1): the public route is the
	// fence, so the version check, the cluster's record, the maintenance window
	// and the bundle manifest all answer 503 while an upgrade of this very
	// control plane is in flight. The node is the only way to the backend, and
	// on a resume it is the only thing the CLI can reach before it has decided
	// anything.
	nodeRunner, err := controlPlaneNodeRunner(ctx, f)
	if err != nil {
		return err
	}
	nodeClient := func(ctx context.Context) (*api.Client, error) {
		return controlplane.NodeClient(ctx, nodeRunner, controlPlaneOpener())
	}
	// What the operation recorded when it began, when this run continues one.
	// It is the last resort: a failed migration leaves the backend at zero
	// replicas, so neither the public route nor the tunnel answers.
	recorded, recordedWindow := recordedOperationFacts(ctx, nodeRunner, f.Resume)

	client, before, err := controlPlaneForUpgrade(ctx, nodeClient, recorded)
	if err != nil {
		return err
	}

	// AN INTERRUPT IS NOT A FAILURE. SIGINT/SIGTERM cancels the run and leaves
	// the operation record STOPPED rather than terminal-failed, because the
	// whole point of a recorded step is that a second laptop can continue it —
	// and a terminal record cannot be continued, only restarted. That is the
	// difference between "my laptop died mid-migration" and "the migration
	// failed".
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	session, err := buildUpgradeSessionWith(ctx, out, f, client)
	if err != nil {
		return err
	}
	defer session.Close()
	// A WINDOW THAT COULD NOT BE READ IS STILL A WINDOW THE OPERATION KNOWS:
	// while the fence is up the read goes through the node, and if the backend
	// is at zero replicas there is nothing to read it from. The record names the
	// window this upgrade was started inside, and refusing on "unreadable" would
	// refuse the resume the CLI promised.
	if session.Window == nil && recordedWindow != "" {
		if spec, ok := parseRecordedWindow(recordedWindow); ok {
			session.Window, session.WindowErr = &spec, nil
			fmt.Fprintf(out, "  the maintenance window came from the operation record: %s\n", spec)
		}
	}
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

	// WHAT THE CONTROL PLANE SAYS IT IS NOW was read by controlPlaneForUpgrade,
	// behind the fence if the fence is up: it is the floor the validation
	// measures against, because the contract counter never goes down, so a lower
	// era after the upgrade means an older build is still the one serving.

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
	store, handle, skip, err := controlPlaneLock(ctx, server, f, session, out, before)
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
	if err := finishControlPlaneJournal(out, journal, nil); err != nil {
		// Reported, never fatal: the upgrade succeeded, and a warning about a
		// file is not a reason to report it as failed.
		_ = err
	}
	fmt.Fprintf(out, "\nUpgraded the control plane to %s in %s.\n", opts.To, result.Elapsed.Round(time.Second))
	return nil
}

// finishControlPlaneJournal removes the journal of a control-plane upgrade that
// COMPLETED, and keeps the one of a run that did not.
//
// THE JOURNAL HAS SERVED ITS PURPOSE ONCE THE RUN SUCCEEDED, as the workload
// upgrade's does after a rollback. Kept, it would make the next upgrade to the
// same bundle skip the fence, the migration and the chart BY NAME and report
// success over a control plane that has moved since — measured on hardware
// (2026-09-25): after the control plane was put back on the previous candidate,
// a second run printed "Upgraded the control plane to 1.1 in 14s" having
// changed nothing at all.
//
// A RUN THAT DID NOT COMPLETE KEEPS ITS JOURNAL, and that is the other half:
// the journal is where an interrupted or failed attempt stopped, and the
// identical command is meant to continue it rather than start over.
func finishControlPlaneJournal(out io.Writer, journal *stages.Journal, runErr error) error {
	if journal == nil || runErr != nil {
		return nil
	}
	if err := journal.Remove(); err != nil {
		fmt.Fprintf(out, "\nWarning: the upgrade journal %s could not be removed (%v); delete it before the next upgrade, or that upgrade will skip the stages it names.\n",
			journal.Path(), err)
		return err
	}
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
	// THE RECORD IS CLOSED WITH A CONTEXT OF ITS OWN, and it has to be: an
	// interrupt is a CANCELLED CONTEXT, and closing the record must not be
	// cancelled with it. Hardware (2026-09-25) printed
	// `the operation record … could not be closed: reading the operation record
	// kube-system/kubenest-operation: context canceled`, left the record at
	// `executor.state: running`, and the second laptop's take-over — which
	// requires the previous executor STOPPED — was refused. The operator was
	// left with an operation nobody could continue.
	ctx, cancel := recordCloseContext(ctx)
	defer cancel()

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

// recordCloseTimeout bounds how long closing the record may take. It is short
// because it is one ConfigMap write on the way out of a command whose run has
// already stopped.
const recordCloseTimeout = 30 * time.Second

// recordCloseContext detaches the record's closing from the run's cancellation
// and bounds it.
//
// The VALUES still travel: a caller that put something in the context (a
// request id, a logger) must not lose it just because the run was interrupted.
func recordCloseContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), recordCloseTimeout)
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
	before api.ControlPlaneVersion,
) (*operation.Store, *operation.Handle, map[string]bool, error) {
	store := &operation.Store{Runner: server}
	req := operation.Request{
		Kind:    operation.KindControlPlaneUpgrade,
		Cluster: f.Cluster,
		Versions: map[string]string{
			"bundle":            session.From.Bundle + " -> " + f.To,
			recordedContractKey: strconv.Itoa(before.Contract),
			recordedBuildKey:    before.Build,
		},
	}
	// THE WINDOW IS RECORDED WITH THEM, and it has to be: the resume of THIS
	// operation may find the public route fenced and the backend at zero
	// replicas, and the window gate refuses a window it cannot read. The record
	// is in the cluster and needs no backend.
	if session.Window != nil {
		if spec, err := json.Marshal(session.Window.Spec()); err == nil {
			req.Versions[recordedWindowKey] = string(spec)
		}
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

// controlPlaneNodeRunner dials the management cluster's first server node.
//
// It is the same connection the upgrade's stages use (buildUpgradeSession dials
// them again; one extra session on a resume path is cheaper than making every
// read wait for a dialect that a fenced control plane cannot answer).
func controlPlaneNodeRunner(ctx context.Context, f UpgradeFlags) (k3s.Runner, error) {
	servers, _, err := upgradeNodes(f)
	if err != nil {
		return nil, err
	}
	endpoint, err := sshx.Resolve(servers[0], sshx.Options{User: f.SSHUser, KeyPath: f.SSHKey})
	if err != nil {
		return nil, err
	}
	return sshx.Dial(ctx, endpoint, sshx.Options{KeyPath: f.SSHKey})
}

// recordedOperationFacts is what the operation recorded when it began: the
// control plane's version and the maintenance window.
//
// IT READS THE OPERATION RECORD, which lives in the CLUSTER and needs no
// backend: that is the whole point. A resume whose migration failed has a
// backend at zero replicas, so neither the public route nor the tunnel answers,
// and the record is the only thing left that knows which control plane is being
// upgraded and inside which window.
func recordedOperationFacts(ctx context.Context, runner k3s.Runner, operationID string) (api.ControlPlaneVersion, string) {
	if operationID == "" {
		return api.ControlPlaneVersion{}, ""
	}
	stored, err := (&operation.Store{Runner: runner}).Find(ctx, operationID)
	if err != nil || stored == nil || stored.Record == nil {
		return api.ControlPlaneVersion{}, ""
	}
	versions := stored.Record.Request.Versions
	contract, _ := strconv.Atoi(versions[recordedContractKey])
	return api.ControlPlaneVersion{Contract: contract, Build: versions[recordedBuildKey]}, versions[recordedWindowKey]
}

// The keys the operation record carries the control plane's own identity and
// the window under. They are part of the record's wire shape: a second laptop
// reads what the first one wrote.
const (
	recordedContractKey = "control-plane contract"
	recordedBuildKey    = "control-plane build"
	recordedWindowKey   = "maintenance window"
)

// parseRecordedWindow turns the recorded window back into a Window.
func parseRecordedWindow(recorded string) (window.Window, bool) {
	var spec window.Spec
	if err := json.Unmarshal([]byte(recorded), &spec); err != nil {
		return window.Window{}, false
	}
	parsed, err := window.Parse(spec)
	if err != nil {
		return window.Window{}, false
	}
	return parsed, true
}

// controlPlaneForUpgrade chooses the client every read of this command uses, and
// reads the control plane's version with it.
//
// THE CHOICE IS MADE ONCE, deliberately. A per-call fallback would mean every
// future read remembering to ask; one decision point means a read added later
// cannot forget. The order:
//
//	the public route answers        use it, which is the ordinary case;
//	it answers 404 (no counter)     use it: nothing is fenced and the other
//	                                routes exist;
//	it answers the FENCE's 503      use the node's tunnel — the fence is this
//	                                CLI's own state, and the backend is behind
//	                                the node while the route is the fence;
//	the node cannot either          fall back for the VERSION to what the
//	                                operation recorded, and let each read that
//	                                fails say so.
func controlPlaneForUpgrade(ctx context.Context, node func(context.Context) (*api.Client, error), recorded api.ControlPlaneVersion) (*api.Client, api.ControlPlaneVersion, error) {
	public, err := controlPlaneClient()
	if err != nil {
		return nil, api.ControlPlaneVersion{}, err
	}
	source := fencedVersionSource{Public: public, Node: node, Recorded: recorded}
	reported, known, err := source.version(ctx)
	if err != nil {
		return nil, api.ControlPlaneVersion{}, err
	}
	if err := requireControlPlaneForUpgrade(reported, known); err != nil {
		return nil, reported, err
	}
	if _, err := public.ControlPlaneVersion(ctx); err == nil || api.IsVersionEndpointAbsent(err) {
		return public, reported, nil
	}
	if node != nil {
		if viaNode, nerr := node(ctx); nerr == nil {
			return viaNode, reported, nil
		}
	}
	return public, reported, nil
}
