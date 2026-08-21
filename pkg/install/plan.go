package install

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/component/certmanager"
	"kubenest.io/cli/pkg/component/day2"
	"kubenest.io/cli/pkg/component/gatewayapi"
	"kubenest.io/cli/pkg/component/traefik"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/preflight"
	"kubenest.io/cli/pkg/register"
	"kubenest.io/cli/pkg/storage"
)

// Plan is the thirteen stages wired to what actually does the work.
//
// The engine (engine.go) owns order, journalling and failure reporting; this
// file owns which function each stage calls. They are separate so the
// sequencing is readable without the plumbing, and so a test can exercise
// resume against a table of fakes.
func Plan() []Stage {
	return []Stage{
		{Name: StagePreflight, AlwaysRun: true, Run: stagePreflight},
		{Name: StageRegister, AlwaysRun: true, Run: stageRegister},
		{Name: StageK3sServer, Component: "k3s", Run: stageK3sServer},
		{Name: StageK3sAgents, Component: "k3s", Run: stageK3sAgents},
		{Name: StageNetworking, Component: "traefik", Run: stageNetworking},
		{Name: StageCerts, Component: "cert-manager", Run: stageCerts},
		{Name: StageStorage, Component: "openebs-lvm-localpv", Run: stageStorage},
		{Name: StageBackup, Component: "velero", Run: stageBackup},
		{Name: StageDay2, Component: "system-upgrade-controller", Run: stageDay2},
		{Name: StageAgent, Component: "kubenest-agent", Run: stageAgent},
		{Name: StageProfiles, Run: stageProfiles},
		{Name: StageRecord, Run: stageRecord},
		{Name: StageVerify, AlwaysRun: true, Run: Verify},
	}
}

// stagePreflight opens a connection to every node and runs all eleven checks.
// It writes nothing anywhere, which is what makes abandoning an install here
// free, and it is also where the connections every later stage uses come from.
func stagePreflight(ctx context.Context, s *Session) error {
	nodes := s.dialAll(ctx)

	// A resumed install re-runs preflight after earlier stages already
	// installed k3s and possibly created the volume group. Two checks would
	// otherwise refuse the installer's own work.
	_, serversDone := s.Journal.Completed(StageK3sServer)
	_, agentsDone := s.Journal.Completed(StageK3sAgents)
	_, storageDone := s.Journal.Completed(StageStorage)
	for i := range nodes {
		if nodes[i].Role == string(RoleServer) {
			nodes[i].ExistingK3sIsOurs = serversDone
		} else {
			nodes[i].ExistingK3sIsOurs = agentsDone
		}
		nodes[i].StorageIsOurs = storageDone && s.Journal.Storage.Ownership == storage.InstallerCreated
	}

	report, err := preflight.Run(ctx, preflight.Options{
		Bundle:        s.Bundle,
		BundleVersion: s.Opts.Bundle,
		HATier:        s.Opts.HATier,
		Profiles:      s.Opts.Profiles,
		StorageDevice: s.Opts.StorageDevice,
		Nodes:         nodes,
		Egress:        EgressTargets(s),
		Catalog:       bundleCatalog{s.API},
	})
	for _, warning := range report.Warnings() {
		s.Logf("  warning: %s", warning)
	}
	if err != nil {
		return err
	}

	// Preflight passed: adopt its connections as the session's nodes.
	s.Nodes = s.Nodes[:0]
	for _, n := range nodes {
		s.Nodes = append(s.Nodes, Node{Address: n.Address, Role: NodeRole(n.Role), Runner: n.Runner})
	}
	return nil
}

// EgressTargets is what the nodes must be able to reach, assembled from the
// component installers' OWN chart repositories rather than a list copied here.
// A bundle that moves a repository cannot leave preflight checking the old one.
func EgressTargets(s *Session) []preflight.EgressTarget {
	targets := []preflight.EgressTarget{
		{Name: "k3s installer", URL: "https://get.k3s.io"},
		{Name: "container registry (docker.io)", URL: "https://registry-1.docker.io/v2/"},
		{Name: "container registry (ghcr.io)", URL: "https://ghcr.io/v2/"},
		{Name: "Gateway API release", URL: gatewayapi.ReleaseBaseURL},
		{Name: "system-upgrade-controller release", URL: day2.ReleaseBaseURL},
	}
	charts := []struct {
		name  string
		chart func() (k3s.HelmChart, error)
	}{
		{"Traefik charts", func() (k3s.HelmChart, error) { return traefik.Chart(s.Bundle) }},
		{"cert-manager charts", func() (k3s.HelmChart, error) { return certmanager.Chart(s.Bundle) }},
		{"Velero charts", func() (k3s.HelmChart, error) { return backup.Chart(s.Bundle) }},
		{"kured charts", func() (k3s.HelmChart, error) { return day2.Chart(s.Bundle) }},
	}
	for _, c := range charts {
		chart, err := c.chart()
		if err != nil || chart.Repo == "" {
			// A pin the manifest does not carry is preflight's bundle check
			// to report, not egress's.
			continue
		}
		targets = append(targets, preflight.EgressTarget{
			Name: c.name,
			URL:  strings.TrimRight(chart.Repo, "/") + "/index.yaml",
		})
	}
	return targets
}

// bundleCatalog adapts the API client to preflight's narrow view of it.
type bundleCatalog struct{ client *api.Client }

func (b bundleCatalog) ListBundles(ctx context.Context) ([]preflight.BundleEntry, error) {
	if b.client == nil {
		return nil, fmt.Errorf("no control plane configured")
	}
	entries, err := b.client.ListBundles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]preflight.BundleEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, preflight.BundleEntry{Version: e.Version, HATiers: e.HATiers, Profiles: e.Profiles})
	}
	return out, nil
}

// stageRegister creates or adopts the cluster record and mints the
// credentials the agent will carry. It writes only to the control plane; the
// first change to a customer machine is still stage 3.
//
// MINTING IS NOT IDEMPOTENT and cannot be: every mint rotates the agent JWT's
// token_version and issues a fresh deploy key. So the two halves are treated
// differently on a resume — the cluster record is always adopted, and the
// credentials are minted only when a stage that consumes them is still ahead.
// Re-minting on a resume whose agent is already installed and heartbeating
// would rotate a live cluster's identity to no purpose.
func stageRegister(ctx context.Context, s *Session) error {
	if s.API == nil {
		return fmt.Errorf("no control plane configured: run `kubenest login` first")
	}
	org, err := register.ResolveOrg(ctx, s.API, s.Opts.Org)
	if err != nil {
		return err
	}
	cluster, adopted, err := register.EnsureCluster(ctx, s.API, org.ID, s.Opts.Name, "")
	if err != nil {
		return err
	}
	s.Journal.Cluster = ClusterRecord{ClusterID: cluster.ID, Adopted: adopted}

	if _, agentInstalled := s.Journal.Completed(StageAgent); agentInstalled {
		s.Logf("  the agent is already installed and holding credentials from an earlier run; not re-minting")
		return s.Journal.Save()
	}

	creds, err := register.MintCredentials(ctx, s.API, cluster.ID)
	if err != nil {
		return err
	}
	// In memory, for this process only. Nothing here can reach the journal:
	// api.Secret refuses to marshal and ClusterRecord has no field for it.
	s.Creds = creds
	s.Journal.Cluster.TokenVersion = creds.AgentJWT.TokenVersion
	if creds.RepoCredential != nil {
		s.Journal.Cluster.RepoURL = creds.RepoCredential.RepoURL
	}
	return s.Journal.Save()
}

// stageK3sServer installs k3s on the control-plane node, or all three for the
// ha tier: the first initialises the embedded-etcd cluster and the other two
// join it.
func stageK3sServer(ctx context.Context, s *Session) error {
	servers := s.NodesWithRole(RoleServer)
	if len(servers) == 0 {
		return fmt.Errorf("no server node")
	}
	if err := failing("k3s", k3s.InstallServer(ctx, servers[0].Runner, s.Bundle, k3s.ServerOptions{}, s.Reporter)); err != nil {
		return err
	}
	if len(servers) == 1 {
		return nil
	}

	// The token is read for immediate use and never stored: not in the
	// session, not in the journal, not in a log line.
	token, err := k3s.NodeToken(ctx, servers[0].Runner)
	if err != nil {
		return err
	}
	joinURL := serverURL(servers[0].Address)
	for _, server := range servers[1:] {
		if err := k3s.InstallServer(ctx, server.Runner, s.Bundle,
			k3s.ServerOptions{JoinURL: joinURL, Token: token}, s.Reporter); err != nil {
			return fmt.Errorf("joining %s to the etcd cluster: %w", server.Address, err)
		}
	}
	return k3s.WaitNodesReady(ctx, servers[0].Runner, s.Bundle, len(servers), s.Reporter)
}

// stageK3sAgents joins the worker nodes.
func stageK3sAgents(ctx context.Context, s *Session) error {
	agents := s.NodesWithRole(RoleAgent)
	if len(agents) == 0 {
		return nil
	}
	servers := s.NodesWithRole(RoleServer)
	if len(servers) == 0 {
		return fmt.Errorf("no server node to join")
	}
	token, err := k3s.NodeToken(ctx, servers[0].Runner)
	if err != nil {
		return err
	}
	joinURL := serverURL(servers[0].Address)
	for _, node := range agents {
		if err := k3s.InstallAgent(ctx, node.Runner, s.Bundle, joinURL, token, s.Reporter); err != nil {
			return fmt.Errorf("joining agent %s: %w", node.Address, err)
		}
	}
	return k3s.WaitNodesReady(ctx, servers[0].Runner, s.Bundle, len(s.Nodes), s.Reporter)
}

// serverURL is the address other nodes join through. It is the address the
// operator gave on the command line — the installer does not guess at a
// different interface, because on a private network it would guess wrong.
func serverURL(address string) string {
	return "https://" + address + ":6443"
}

// stageNetworking installs the Gateway API CRDs and Traefik with the Gateway
// API provider. NOT ingress-nginx: it reached end of life on 24 March 2026 —
// read-only repository, no CVE patches — and Traefik is already the k3s
// default, so this is the lighter choice as well as the safe one.
func stageNetworking(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	if err := failing("gateway-api", gatewayapi.Install(ctx, server, s.Bundle, s.Reporter)); err != nil {
		return err
	}
	return failing("traefik", traefik.Install(ctx, server, s.Bundle, s.Reporter))
}

// stageCerts installs cert-manager, then the platform's Gateway defaults —
// the CA issuer chain, the default listener certificate and the Gateway the
// whole app layer attaches to. The defaults live here rather than in stage 5
// because they need cert-manager to exist first.
func stageCerts(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	if err := failing("cert-manager", certmanager.Install(ctx, server, s.Bundle, s.Reporter)); err != nil {
		return err
	}
	// The Gateway defaults need cert-manager to have issued the platform CA,
	// so a failure here is cert-manager's story far more often than
	// Traefik's — and the object the convergence state names says which.
	return failing("cert-manager", traefik.InstallGatewayDefaults(ctx, server, s.Bundle, s.Reporter))
}

// stageStorage verifies or creates the volume group on every node that can
// hold data, then installs OpenEBS Local PV LVM and the default StorageClass.
func stageStorage(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	ownership := storage.CustomerCreated
	for _, node := range s.Nodes {
		if s.Opts.StorageDevice != "" {
			ownership = storage.InstallerCreated
		}
		if err := storage.EnsureVolumeGroup(ctx, node.Runner, s.Opts.StorageDevice); err != nil {
			return fmt.Errorf("volume group on %s: %w", node.Address, err)
		}
	}
	// Recorded before the install proceeds, because it is what uninstall
	// reads to decide whether it may ever remove a volume group.
	s.Journal.Storage = StorageRecord{Device: s.Opts.StorageDevice, Ownership: ownership}
	if err := s.Journal.Save(); err != nil {
		return err
	}

	if err := failing(storage.ComponentKey, storage.Install(ctx, server, s.Bundle, s.Reporter)); err != nil {
		return err
	}
	return failing(storage.ComponentKey, storage.Verify(ctx, server, s.Bundle, s.Reporter))
}

// stageBackup installs Velero, configured if a target was supplied.
//
// A backup target is optional and its absence is VISIBLE, not silent: the
// cluster reports backup: unconfigured in every heartbeat until one is set,
// because a cluster that has never taken a backup is exactly the quiet
// failure this product exists to prevent.
func stageBackup(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	if err := failing("velero", backup.Install(ctx, server, s.Bundle, s.Reporter)); err != nil {
		return err
	}
	if s.Opts.BackupTarget == "" {
		s.Logf("  no --backup-target given: Velero is installed unconfigured and this cluster will report backup: unconfigured in every heartbeat until one is set")
		return nil
	}
	target, err := parseBackupTarget(s.Opts.BackupTarget)
	if err != nil {
		return err
	}
	if err := failing("velero", backup.Configure(ctx, server, s.Bundle, target, s.Reporter)); err != nil {
		return err
	}
	return failing("velero", backup.EnsureSchedule(ctx, server, s.Bundle, s.Reporter))
}

// stageDay2 places system-upgrade-controller and kured.
func stageDay2(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	if err := failing("system-upgrade-controller", day2.InstallUpgradeController(ctx, server, s.Bundle, s.Reporter)); err != nil {
		return err
	}
	return failing("kured", day2.InstallKured(ctx, server, s.Bundle, s.Reporter))
}

// stageAgent installs the KubeNest agent — which IS the operator (decision G)
// — with the identity minted in stage 2, and only ever with THIS process's
// credentials.
func stageAgent(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	creds, ok := s.Creds.(*api.AgentCredentials)
	if !ok || creds == nil {
		return fmt.Errorf("no credentials from stage 2: the mint returns them once per run, so this stage cannot be reached with credentials from an earlier process")
	}
	return failing("kubenest-agent", agent.Install(ctx, server, s.Bundle, creds, s.Reporter))
}

// stageProfiles installs each selected profile, in the order given.
//
// Every requested profile has already been checked against the bundle by
// preflight, so anything reaching here is offered by the bundle — but the
// component profiles are not built yet (kn-sev5, kn-ynaq, kn-54ni, wave 4).
// Saying so and failing is the only honest option: silently installing core
// when someone asked for observability produces a cluster that does not match
// its own record.
func stageProfiles(ctx context.Context, s *Session) error {
	var unbuilt []string
	for _, name := range s.Opts.Profiles {
		if name == "ha" {
			// The ha profile is a topology, not components: two more servers
			// joining the embedded-etcd cluster single-server already runs.
			// Stage 3 did that.
			continue
		}
		unbuilt = append(unbuilt, name)
	}
	if len(unbuilt) == 0 {
		s.Logf("  core only, no component profiles requested")
		return nil
	}
	return fmt.Errorf("bundle %s offers %s, but this build of the CLI cannot install %s yet — the component profiles land after core (kn-sev5 observability, kn-ynaq secrets, kn-54ni replicated-storage). Install core now and add the profile when it ships",
		s.Bundle.Bundle, strings.Join(s.Bundle.Profiles.Names(), ", "), strings.Join(unbuilt, ", "))
}

// stageRecord writes what was installed against the cluster: bundle version,
// profile set, HA tier and volume-group ownership.
//
// The ownership value is not bookkeeping — it is what uninstall reads to
// decide whether it may remove a volume group, which is the difference
// between a clean teardown and destroying a customer's data.
func stageRecord(ctx context.Context, s *Session) error {
	if s.API == nil || s.Journal.Cluster.ClusterID == "" {
		return fmt.Errorf("no registered cluster to record against")
	}
	ownership := s.Journal.Storage.Ownership
	if ownership == "" {
		ownership = storage.CustomerCreated
	}
	profiles := s.Opts.Profiles
	if profiles == nil {
		profiles = []string{}
	}
	return s.API.PutBundleRecord(ctx, s.Journal.Cluster.ClusterID, api.BundleRecord{
		BundleVersion:        s.Opts.Bundle,
		Profiles:             profiles,
		HATier:               s.Opts.HATier,
		VolumeGroupOwnership: string(ownership),
		InstallJournal:       s.Journal.terminalEntries(),
	})
}

// terminalEntries is the journal in the control plane's shape. Only terminal
// transitions are persisted server-side; `started` exists to make a killed
// run legible locally and to drive live progress, not to fill a permanent
// record with noise.
func (j *Journal) terminalEntries() []api.InstallJournalEntry {
	var out []api.InstallJournalEntry
	for _, e := range j.Entries {
		if e.Status == StatusStarted {
			continue
		}
		at := e.At
		entry := api.InstallJournalEntry{
			Stage:     e.Stage,
			Component: e.Component,
			Status:    api.InstallStageStatus(e.Status),
			At:        &at,
			Detail:    e.Detail,
		}
		if e.Status == StatusFailed {
			entry.ReasonCode = ReasonCode(e.Stage)
		}
		out = append(out, entry)
	}
	return out
}

// ControlPlaneEmitter publishes stage transitions to the control plane, where
// they become live SSE progress and the server-side install journal.
//
// THE QUEUE IS THE POINT. An event needs a cluster to belong to, and the
// cluster id does not exist until stage 2 registers it — so preflight's two
// transitions and register's `started` happen before there is anywhere to
// send them. Dropping them would cost more than a thin trace:
//
//   - the console's progress would begin at stage 2 completed, so the two
//     stages that run before any machine is touched would be invisible
//     exactly while an operator is deciding whether to walk away;
//   - and `started` is the ONLY signal that clears install_failed
//     (kubenest-backend 9c19e5e, deliberately sticky). A stage that never
//     emits `started` is a stage whose failure can never be cleared, so a
//     cluster that failed at preflight or register would stay install_failed
//     through every subsequent successful run.
//
// So they are queued, with the timestamp of when they actually happened, and
// flushed IN ORDER the moment registration produces the id. If registration
// never produces one — a preflight refusal — the queue is discarded, which is
// correct: nothing was written to any machine and no cluster record exists.
// Locking discipline: the mutex guards the queue slice ONLY, and is never
// held across a network call. Every critical section here is lock, swap or
// append the slice, unlock — deliberately without `defer`, because deferring
// to the end of send would hold the lock through every HTTP POST and make one
// slow control plane stall the install's next transition.
type ControlPlaneEmitter struct {
	Client  *api.Client
	Session *Session

	mu      sync.Mutex
	pending []api.InstallJournalEntry
}

// NewControlPlaneEmitter builds the emitter. It is a pointer because it holds
// the queue.
func NewControlPlaneEmitter(client *api.Client, session *Session) *ControlPlaneEmitter {
	return &ControlPlaneEmitter{Client: client, Session: session}
}

// Emit posts one transition, queueing it if the cluster is not registered yet.
func (e *ControlPlaneEmitter) Emit(ctx context.Context, ev Event) error {
	if e == nil || e.Client == nil || e.Session == nil {
		return nil
	}
	// Stamped when it HAPPENED, not when it is sent: a queued transition
	// flushed after registration must not claim to have occurred then.
	now := time.Now().UTC()
	entry := api.InstallJournalEntry{
		Stage:      ev.Stage,
		Component:  ev.Component,
		Status:     api.InstallStageStatus(ev.Status),
		At:         &now,
		ReasonCode: ev.ReasonCode,
		Detail:     Sanitize(ev.Message),
	}

	clusterID := e.Session.Journal.Cluster.ClusterID
	if clusterID == "" {
		e.mu.Lock()
		e.pending = append(e.pending, entry)
		e.mu.Unlock()
		return nil
	}
	return e.send(ctx, clusterID, entry)
}

// send flushes anything queued, in order, then the new entry. A flush that
// fails partway leaves the remainder queued so the next transition retries
// it — and returns the error, which the engine prints without failing the
// install.
func (e *ControlPlaneEmitter) send(ctx context.Context, clusterID string, entry api.InstallJournalEntry) error {
	e.mu.Lock()
	queued := e.pending
	e.pending = nil
	e.mu.Unlock()

	requeue := func(from int, err error) error {
		e.mu.Lock()
		// Order is preserved exactly: the unsent remainder, then the entry
		// that triggered this flush — which has not been sent either — then
		// anything a concurrent Emit queued behind us.
		remainder := append([]api.InstallJournalEntry{}, queued[from:]...)
		remainder = append(remainder, entry)
		e.pending = append(remainder, e.pending...)
		held := len(e.pending)
		e.mu.Unlock()
		return fmt.Errorf("holding %d stage transition(s) for the next attempt: %w", held, err)
	}

	for i, q := range queued {
		if err := e.Client.ReportInstallStage(ctx, clusterID, q); err != nil {
			return requeue(i, err)
		}
	}
	if err := e.Client.ReportInstallStage(ctx, clusterID, entry); err != nil {
		return requeue(len(queued), err)
	}
	return nil
}

// Pending reports how many transitions are still waiting for a cluster id.
// Used by tests and by the engine's end-of-run check.
func (e *ControlPlaneEmitter) Pending() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.pending)
}
