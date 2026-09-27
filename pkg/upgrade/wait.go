package upgrade

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/window"
)

// WaitForWindow holds the run until the cluster's maintenance window is open.
//
// IT HOLDS NOTHING WHILE IT WAITS (PLAN 7.2). No operation record is acquired
// and no cluster state is touched, so an interrupted wait is simply started
// again and a second operator is free to work while this one waits. That is
// also why the gates and the lock come AFTER the wait rather than before it:
// every pre-flight check has to read the cluster at the moment the upgrade
// will actually act, not the state hours earlier when the operator typed the
// command.
//
// A missing or unreadable window is a refusal with the fix, never an opening
// to wait for and never "any time": the same rule checkWindow applies, stated
// here so a --wait run cannot begin by assuming it is inside a window that is
// not there.
func (s *Session) WaitForWindow(ctx context.Context, out io.Writer) error {
	if s.Opts.BypassWindow {
		return nil
	}
	if s.WindowErr != nil {
		return fmt.Errorf("the cluster's maintenance window could not be read, so there is no opening to wait for: %w", s.WindowErr)
	}
	if s.Window == nil {
		return fmt.Errorf("%s: %s", window.NoWindow, window.NoWindowFix)
	}
	if s.Window.Contains(s.now()) {
		s.windowf(out, "inside %s (now %s).\n", s.Window, s.Window.Moment(s.now()))
		return nil
	}
	at, ok := s.Window.NextOpen(s.now())
	if !ok {
		return fmt.Errorf("the maintenance window %s has no opening within the next week, so waiting would never finish: change it with `kubenest cluster set-window`", s.Window)
	}
	s.windowf(out, "%s is closed (now %s); it next opens %s.\nWaiting. Nothing is held while this waits — no operation record is created and the cluster is not touched, so an interrupted wait can simply be re-run.\n",
		s.Window, s.Window.Moment(s.now()), s.Window.Moment(at))

	// The wait is bounded by the opening itself: if the clock reaches it and
	// the window still is not open — a stored window that cannot be represented
	// in the zone the host is using, a clock that jumped — the wait says so
	// instead of spinning a second at a time for a week.
	deadline := at.Add(5 * time.Minute)
	for !s.Window.Contains(s.now()) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("stopped waiting for the maintenance window: %w", err)
		}
		if s.now().After(deadline) {
			return fmt.Errorf("waited past %s and the maintenance window %s still has not opened, so waiting would never finish: check the window's timezone against this host's clock, or change it with `kubenest cluster set-window`",
				s.Window.Moment(deadline), s.Window)
		}
		// Waiting in bounded steps rather than one long sleep: a window that
		// opens EARLIER than the computed opening — a clock correction, a
		// changed window, a DST transition — is then noticed instead of slept
		// through.
		step := time.Minute
		if remaining := at.Sub(s.now()); remaining < step {
			step = remaining
		}
		if step <= 0 {
			step = time.Second
		}
		if err := s.sleep(ctx, step); err != nil {
			return err
		}
	}
	s.windowf(out, "inside %s (now %s). Every gate re-runs from here, against the cluster as it is now, before anything is changed.\n",
		s.Window, s.Window.Moment(s.now()))
	return nil
}

// sleep waits for d, on the session's clock if a test supplied one.
func (s *Session) sleep(ctx context.Context, d time.Duration) error {
	if s.Opts.Sleep != nil {
		return s.Opts.Sleep(ctx, d)
	}
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// windowf writes the wait's narrative to out, falling back to the session's
// own output when the caller has none.
func (s *Session) windowf(out io.Writer, format string, args ...any) {
	if out == nil {
		out = s.Out
	}
	if out == nil {
		return
	}
	fmt.Fprintf(out, format, args...)
}

// LockOperation takes the record-as-lock for this upgrade.
//
// IT IS CALLED AFTER THE WINDOW OPENS, NEVER BEFORE. The record being the lock
// means a run that took it and then waited would hold the whole cluster for
// hours while it held nothing but a clock — the opposite of what --wait is for
// (PLAN 7.2). The Request is derived from the bundle transition and the nodes,
// so a resume can tell this record from a different move wearing the same
// cluster's name.
func (s *Session) LockOperation(ctx context.Context) (*operation.Store, *operation.Handle, error) {
	store, err := s.operationStore()
	if err != nil {
		return nil, nil, err
	}
	req, err := s.operationRequest()
	if err != nil {
		return nil, nil, err
	}
	handle, err := store.Acquire(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	return store, handle, nil
}

// TakeOverOperation reconciles the upgrade this run continues and claims its
// record on the operator's assertion that the previous executor and its
// outstanding actions have stopped.
//
// IT IS WHAT `--take-over <operation-id> --confirm` DOES FOR AN UPGRADE. The
// reconcile runs first — it submits nothing, it establishes what the recorded
// actions did from their postconditions, and it stops naming the reconciliation
// step when it cannot — and only then does the assertion change hands, which is
// the order the plan fixes (7.2). The assertion is recorded in the record, so
// the history says who said the dead executor was gone.
func (s *Session) TakeOverOperation(ctx context.Context, out io.Writer, recovery operation.Recovery) (*operation.Store, *operation.Handle, error) {
	store, err := s.operationStore()
	if err != nil {
		return nil, nil, err
	}
	req, err := s.operationRequest()
	if err != nil {
		return nil, nil, err
	}
	plan, err := operation.Resume(ctx, store, recovery.ID())
	if err != nil {
		return nil, nil, err
	}
	if err := plan.Verify(req); err != nil {
		return nil, nil, err
	}
	s.windowf(out, "%s\n", recovery.Progress(plan))
	for _, step := range plan.Steps {
		s.windowf(out, "  %s %s (%s): %s\n", step.Decision, step.ActionID, step.Stage, step.Reason)
	}
	handle, err := recovery.Claim(ctx, store)
	if err != nil {
		return nil, nil, err
	}
	return store, handle, nil
}

// operationStore is the record-as-lock store for this session: one ConfigMap
// in kube-system, reached over the connection to a server.
func (s *Session) operationStore() (*operation.Store, error) {
	server, err := s.Server()
	if err != nil {
		return nil, err
	}
	return &operation.Store{
		Runner:   server,
		Operator: OperatorName(),
		// The control plane's copy is display and check_upgrade: the record in
		// the cluster is the lock, and it works while the control plane does
		// not. A mirror that fails is reported on the handle rather than
		// failing the operation.
		Mirror:          s.API,
		MirrorClusterID: s.journalClusterID(),
	}, nil
}

// operationRequest is the immutable request this upgrade is about: the bundle
// transition and the nodes, so a resume can tell this record from a different
// move wearing the same cluster's name.
func (s *Session) operationRequest() (operation.Request, error) {
	if s.From == nil || s.To == nil {
		return operation.Request{}, fmt.Errorf("this session has no bundle transition to record, so the operation record could not name what it is about")
	}
	req := operation.Request{
		Kind:     operation.KindUpgrade,
		Cluster:  s.Opts.Cluster,
		Versions: map[string]string{"bundle": s.From.Bundle + " -> " + s.To.Bundle},
	}
	for _, n := range s.Nodes {
		// The address is this CLI's identity for a host; the Node UID is read
		// by the stages that act on the node and is deliberately not guessed
		// here.
		req.Targets = append(req.Targets, operation.Target{HostID: n.Address})
	}
	return req, nil
}

// journalClusterID is the control plane's id for this cluster, when the
// session's journal carries it. Empty means this CLI does not know it, and the
// mirror then records that rather than posting to the wrong cluster.
func (s *Session) journalClusterID() string {
	if s.Jnl == nil {
		return ""
	}
	return s.Jnl.ClusterID
}

// OperatorName names whoever holds a new record, e.g. "ana@laptop". It is what
// a refused second laptop is told, so it identifies a person and a machine and
// never a credential.
//
// It is exported because every disruptive verb names its executor in one place:
// an operator refused by `node reboot` and an operator refused by `platform
// upgrade` must read the same shape, and two functions would drift.
func OperatorName() string {
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		if user == "" {
			return "someone@somewhere"
		}
		return user
	}
	if user == "" {
		return host
	}
	return user + "@" + host
}
