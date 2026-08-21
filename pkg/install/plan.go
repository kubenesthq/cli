package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/component/certmanager"
	"kubenest.io/cli/pkg/component/day2"
	"kubenest.io/cli/pkg/component/gatewayapi"
	"kubenest.io/cli/pkg/component/traefik"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/preflight"
	"kubenest.io/cli/pkg/register"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/storage"
)

// Options is the install request — `kubenest platform install`'s flag surface
// resolved into the engine's terms.
type Options struct {
	Bundle        string
	Name          string
	Org           string
	Servers       []string
	Agents        []string
	HATier        string
	Profiles      []string
	SSHUser       string
	SSHKey        string
	StorageDevice string
	BackupTarget  string
}

// Identity is the part of the request a resume must match exactly.
func (o Options) Identity() Identity {
	return Identity{
		Kind:    Kind,
		Cluster: o.Name,
		Fields: map[string]string{
			"bundle":           o.Bundle,
			"HA tier":          o.HATier,
			"--storage-device": o.StorageDevice,
			"servers":          stages.List(o.Servers),
			"agents":           stages.List(o.Agents),
			"profiles":         stages.List(o.Profiles),
		},
	}
}

// NodeRole is what a node is for.
type NodeRole string

const (
	RoleServer NodeRole = "server"
	RoleAgent  NodeRole = "agent"
)

// Node is one target host with an open connection to it.
type Node struct {
	Address string
	Role    NodeRole
	Runner  k3s.Runner
}

// Credentials is stage 2's output, opaque to the engine.
//
// The engine can hold it and hand it to stage 10 and to nothing else. It
// cannot inspect it, print it or write it, and the journal has no field that
// could accept it. Keeping it `any` here is the type system carrying the rule
// that key material never leaves this process except onto the target hosts.
type Credentials any

// Record is what this install must remember across a resume, beyond the
// entries themselves. It is journalled; nothing in it is a secret, and there
// is deliberately no field a credential would fit into.
type Record struct {
	TokenVersion int               `json:"token_version,omitempty"`
	RepoURL      string            `json:"repo_url,omitempty"`
	Adopted      bool              `json:"adopted,omitempty"`
	Device       string            `json:"storage_device,omitempty"`
	Ownership    storage.Ownership `json:"volume_group_ownership,omitempty"`
}

// Session is one install run's state.
type Session struct {
	// ID identifies this process across its stages.
	ID   string
	Opts Options
	// Bundle is the manifest fetched from the control plane. Every version
	// and every deadline comes from here.
	Bundle   *manifest.Manifest
	Jnl      *Journal
	Emit     Emitter
	Reporter converge.Reporter
	Out      io.Writer
	// API is the control plane. Stages 2, 12 and 13 need it; nothing else
	// does, and no stage may hold a credential in it beyond the CLI token
	// it was built with.
	API *api.Client

	// Nodes is filled by stage 1 (preflight), which is why preflight always
	// runs: every later stage needs these connections.
	Nodes []Node
	// Creds is filled by stage 2 (register) and consumed by stage 10. In
	// memory only, for the life of this process.
	Creds Credentials
	// Record is the journalled non-secret record.
	Record Record

	closers []io.Closer
}

// The engine's Controller, implemented by this session.
func (s *Session) RunID() string         { return s.ID }
func (s *Session) Journal() *Journal     { return s.Jnl }
func (s *Session) Emitter() Emitter      { return s.Emit }
func (s *Session) BundleVersion() string { return s.Bundle.Bundle }

// TotalDeadline bounds the whole install. It is NOT the fifteen-minute
// budget: the budget is a target the release tests assert and an overrun is a
// defect to fix, while this is when an install that is going nowhere gives up
// and says which stage was still running.
func (s *Session) TotalDeadline() (time.Duration, error) {
	return s.Bundle.Limits.Timeouts.For("install-total")
}

// ResumeAdvice is empty: an install has no pause path — it runs to
// completion or it fails.
func (s *Session) ResumeAdvice() string { return "" }

// Exits are the two supported ways on from a failed install (install.mdx,
// "When it fails").
func (s *Session) Exits() []string { return exits }

// Logf writes narrative to the session's output.
func (s *Session) Logf(format string, args ...any) {
	if s.Out == nil {
		return
	}
	fmt.Fprintf(s.Out, format+"\n", args...)
}

// Close releases every connection stage 1 opened. Safe to call twice.
func (s *Session) Close() {
	for _, c := range s.closers {
		_ = c.Close()
	}
	s.closers = nil
}

// saveRecord persists the non-secret record to the journal.
func (s *Session) saveRecord() error { return s.Jnl.SetState(s.Record) }

// Recorded reads an install's non-secret record back out of its journal.
// Uninstall uses it: the volume-group ownership recorded here is what decides
// whether a volume group may ever be removed.
func Recorded(j *Journal) (Record, error) {
	var r Record
	if j == nil {
		return r, nil
	}
	err := j.DecodeState(&r)
	return r, err
}

// NodesFromJournal reads the node lists an install recorded, for uninstall to
// clean without being told them again.
func NodesFromJournal(j *Journal) (servers, agents []string) {
	if j == nil {
		return nil, nil
	}
	return strings.Fields(j.Identity.Fields["servers"]), strings.Fields(j.Identity.Fields["agents"])
}

// Server returns the primary control-plane node — the one that runs kubectl
// and holds the k3s auto-deploy directory.
func (s *Session) Server() (k3s.Runner, error) {
	for _, n := range s.Nodes {
		if n.Role == RoleServer {
			return n.Runner, nil
		}
	}
	return nil, errors.New("no server node connection: stage 1 (preflight) opens these, so this is an engine bug, not a host problem")
}

// NodesWithRole returns every node of one role, in the order given on the
// command line.
func (s *Session) NodesWithRole(role NodeRole) []Node {
	var out []Node
	for _, n := range s.Nodes {
		if n.Role == role {
			out = append(out, n)
		}
	}
	return out
}

// Plan is the thirteen stages wired to what actually does the work.
//
// The engine (engine.go) owns order, journalling and failure reporting; this
// file owns which function each stage calls. They are separate so the
// sequencing is readable without the plumbing, and so a test can exercise
// resume against a table of fakes.
func Plan(s *Session) []Stage {
	bind := func(f func(context.Context, *Session) error) stages.StageFunc {
		return func(ctx context.Context) error { return f(ctx, s) }
	}
	return []Stage{
		{Name: StagePreflight, AlwaysRun: true, Run: bind(stagePreflight)},
		{Name: StageRegister, AlwaysRun: true, Run: bind(stageRegister)},
		{Name: StageK3sServer, Component: "k3s", Run: bind(stageK3sServer)},
		{Name: StageK3sAgents, Component: "k3s", Run: bind(stageK3sAgents)},
		{Name: StageNetworking, Component: "traefik", Run: bind(stageNetworking)},
		{Name: StageCerts, Component: "cert-manager", Run: bind(stageCerts)},
		{Name: StageStorage, Component: "openebs-lvm-localpv", Run: bind(stageStorage)},
		{Name: StageBackup, Component: "velero", Run: bind(stageBackup)},
		{Name: StageDay2, Component: "system-upgrade-controller", Run: bind(stageDay2)},
		{Name: StageAgent, Component: "kubenest-agent", Run: bind(stageAgent)},
		{Name: StageProfiles, Run: bind(stageProfiles)},
		{Name: StageRecord, Run: bind(stageRecord)},
		{Name: StageVerify, AlwaysRun: true, Run: bind(Verify)},
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
	_, serversDone := s.Jnl.Completed(StageK3sServer)
	_, agentsDone := s.Jnl.Completed(StageK3sAgents)
	_, storageDone := s.Jnl.Completed(StageStorage)
	for i := range nodes {
		if nodes[i].Role == string(RoleServer) {
			nodes[i].ExistingK3sIsOurs = serversDone
		} else {
			nodes[i].ExistingK3sIsOurs = agentsDone
		}
		nodes[i].StorageIsOurs = storageDone && s.Record.Ownership == storage.InstallerCreated
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
	s.Jnl.ClusterID = cluster.ID
	s.Record.Adopted = adopted

	if _, agentInstalled := s.Jnl.Completed(StageAgent); agentInstalled {
		s.Logf("  the agent is already installed and holding credentials from an earlier run; not re-minting")
		return s.saveRecord()
	}

	creds, err := register.MintCredentials(ctx, s.API, cluster.ID)
	if err != nil {
		return err
	}
	// In memory, for this process only. Nothing here can reach the journal:
	// api.Secret refuses to marshal and ClusterRecord has no field for it.
	s.Creds = creds
	s.Record.TokenVersion = creds.AgentJWT.TokenVersion
	if creds.RepoCredential != nil {
		s.Record.RepoURL = creds.RepoCredential.RepoURL
	}
	return s.saveRecord()
}

// stageK3sServer installs k3s on the control-plane node, or all three for the
// ha tier: the first initialises the embedded-etcd cluster and the other two
// join it.
func stageK3sServer(ctx context.Context, s *Session) error {
	servers := s.NodesWithRole(RoleServer)
	if len(servers) == 0 {
		return fmt.Errorf("no server node")
	}
	if err := stages.NewComponentError("k3s", k3s.InstallServer(ctx, servers[0].Runner, s.Bundle, k3s.ServerOptions{}, s.Reporter)); err != nil {
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
	if err := stages.NewComponentError("gateway-api", gatewayapi.Install(ctx, server, s.Bundle, s.Reporter)); err != nil {
		return err
	}
	return stages.NewComponentError("traefik", traefik.Install(ctx, server, s.Bundle, s.Reporter))
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
	if err := stages.NewComponentError("cert-manager", certmanager.Install(ctx, server, s.Bundle, s.Reporter)); err != nil {
		return err
	}
	// The Gateway defaults need cert-manager to have issued the platform CA,
	// so a failure here is cert-manager's story far more often than
	// Traefik's — and the object the convergence state names says which.
	return stages.NewComponentError("cert-manager", traefik.InstallGatewayDefaults(ctx, server, s.Bundle, s.Reporter))
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
	s.Record.Device = s.Opts.StorageDevice
	s.Record.Ownership = ownership
	if err := s.saveRecord(); err != nil {
		return err
	}

	if err := stages.NewComponentError(storage.ComponentKey, storage.Install(ctx, server, s.Bundle, s.Reporter)); err != nil {
		return err
	}
	return stages.NewComponentError(storage.ComponentKey, storage.Verify(ctx, server, s.Bundle, s.Reporter))
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
	if err := stages.NewComponentError("velero", backup.Install(ctx, server, s.Bundle, s.Reporter)); err != nil {
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
	if err := stages.NewComponentError("velero", backup.Configure(ctx, server, s.Bundle, target, s.Reporter)); err != nil {
		return err
	}
	// Every tier uses embedded etcd (decision A), so every control-plane
	// server gets the same manifest-owned snapshot schedule and S3 target.
	// Restart serially and prove one upload per server; a half-configured HA
	// cluster would report backups while leaving two members unprotected.
	for _, node := range s.Nodes {
		if node.Role != RoleServer {
			continue
		}
		if err := backup.ConfigureDatastoreSnapshots(ctx, node.Runner, s.Bundle, target, s.Reporter); err != nil {
			return stages.NewComponentError("k3s", fmt.Errorf("datastore snapshots on %s: %w", node.Address, err))
		}
	}
	return nil
}

// stageDay2 places system-upgrade-controller and kured.
func stageDay2(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	if err := stages.NewComponentError("system-upgrade-controller", day2.InstallUpgradeController(ctx, server, s.Bundle, s.Reporter)); err != nil {
		return err
	}
	return stages.NewComponentError("kured", day2.InstallKured(ctx, server, s.Bundle, s.Reporter))
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
	return stages.NewComponentError("kubenest-agent", agent.Install(ctx, server, s.Bundle, creds, s.Reporter))
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
	if s.API == nil || s.Jnl.ClusterID == "" {
		return fmt.Errorf("no registered cluster to record against")
	}
	ownership := s.Record.Ownership
	if ownership == "" {
		ownership = storage.CustomerCreated
	}
	profiles := s.Opts.Profiles
	if profiles == nil {
		profiles = []string{}
	}
	return s.API.PutBundleRecord(ctx, s.Jnl.ClusterID, api.BundleRecord{
		BundleVersion:        s.Opts.Bundle,
		Profiles:             profiles,
		HATier:               s.Opts.HATier,
		VolumeGroupOwnership: string(ownership),
		InstallJournal:       terminalEntries(s.Jnl),
	})
}

// terminalEntries is the journal in the control plane's shape. Only terminal
// transitions are persisted server-side; `started` exists to make a killed
// run legible locally and to drive live progress, not to fill a permanent
// record with noise.
func terminalEntries(j *Journal) []api.InstallJournalEntry {
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
