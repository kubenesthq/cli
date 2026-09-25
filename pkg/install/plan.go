package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/component/certmanager"
	"kubenest.io/cli/pkg/component/day2"
	"kubenest.io/cli/pkg/component/gatewayapi"
	"kubenest.io/cli/pkg/component/traefik"
	"kubenest.io/cli/pkg/config"
	"kubenest.io/cli/pkg/controlplane"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/node"
	"kubenest.io/cli/pkg/preflight"
	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/register"
	"kubenest.io/cli/pkg/sshx"
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
	// ControlPlaneInstall is --control-plane: install the KubeNest control
	// plane into the first cluster and register that cluster through it,
	// instead of registering with a control plane that already exists.
	ControlPlaneInstall bool
	// Domain is the DNS suffix the control plane serves — api.<domain> for the
	// API, app.<domain> for the console. Defaults to the first server's
	// address as an sslip.io name.
	Domain string
	// AdminEmail is the control plane's first administrator, admin@<domain>
	// by default.
	AdminEmail string
	// ControlPlaneCA is the PEM of the authority the control plane's serving
	// certificate chains to. A registered install reads it from this machine's
	// config, where `kubenest login --ca-file` or a --control-plane install
	// stored it, so the agent trusts the API it reports to; a --control-plane
	// install MINTS that authority itself (kn-t47) and fills this in during
	// the control-plane stage, which is what lets the management cluster's own
	// agent trust the control plane beside it.
	ControlPlaneCA []byte
	// FleetRecipient is the PUBLIC age recipient of the instance's fleet
	// recovery key, and InstanceID is the instance's immutable id. A
	// --control-plane install generates the key and records both on the
	// cluster; every later install reads them from this machine's config
	// (written by that install) or is handed them with the matching flags.
	//
	// They are not secrets — a recipient can only encrypt — but they must not
	// change: a kit sealed to a different recipient is a kit the fleet key
	// cannot open.
	FleetRecipient string
	InstanceID     string
}

// Identity is the part of the request a resume must match exactly. The install
// MODE is part of it: a journal started as a registered install must not be
// resumed as a control-plane one, because the stages that already ran would
// have been aiming at a different control plane.
func (o Options) Identity() Identity {
	mode := "registered"
	if o.ControlPlaneInstall {
		mode = "control-plane"
	}
	return Identity{
		Kind:    Kind,
		Cluster: o.Name,
		Fields: map[string]string{
			"mode":             mode,
			"bundle":           o.Bundle,
			"HA tier":          o.HATier,
			"--storage-device": o.StorageDevice,
			"domain":           o.Domain,
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
	// AdminPasswordShown records that a --control-plane install has printed
	// its administrator password, so it is printed exactly once per install
	// however many runs that install takes.
	AdminPasswordShown bool `json:"admin_password_shown,omitempty"`
	// ControlPlaneMigration is the migration step a --control-plane install
	// ran, as "<job>@<install revision>", and is empty when the install had
	// nothing to migrate (a first install's database is empty).
	//
	// It is journalled because the step's completion is what a resume must be
	// able to read without re-deriving it: the Job object expires from the
	// cluster (ttlSecondsAfterFinished), so after a day nothing on the cluster
	// says whether the schema was brought forward or the database was simply
	// never behind. No credential fits into it — it names a Job and a hash.
	ControlPlaneMigration string `json:"control_plane_migration,omitempty"`
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
	// API is the control plane. Stages 2, 12 and 13 need it, and in a
	// --control-plane install stage 9 builds it and logs the CLI in to the
	// control plane it installed; nothing else does, and no stage may hold a
	// credential in it beyond the CLI token it was built with.
	API *api.Client

	// Nodes is filled by stage 1 (preflight), which is why preflight always
	// runs: every later stage needs these connections.
	Nodes []Node
	// Creds is filled by stage 2 (register) and consumed by stage 10. In
	// memory only, for the life of this process.
	Creds Credentials
	// Record is the journalled non-secret record.
	Record Record

	// joinToken is the cluster token this install minted for its first server.
	// In memory only, for the life of this process: it is a credential, and
	// the journal has no field a credential fits into. It is empty on a resume
	// whose k3s stage already completed, which is why the kit stage falls back
	// to reading the token file back off the server.
	joinToken string
	// orgID and instanceID are the immutable ids a recovery kit is bound to,
	// filled by the register and control-plane stages.
	orgID      string
	instanceID string
	// cpMaterial is the control plane's own key material (ENCRYPTION_KEY,
	// AGENT_JWT_SECRET, the control plane's CA), held in memory by the stage
	// that installed the control plane so the kit stage can seal it. It is not
	// journalled and not logged: Record has no field a credential fits into.
	cpMaterial map[string]string
	// kit is the kit this run wrote, held so the stage that first allows an
	// upload can refuse to configure anything without it.
	kit *recoverykit.Kit
	// fleetKey is the fleet recovery identity, present ONLY on the process
	// that generated it (the first control-plane install). Every other install
	// is handed the public recipient and has no private half to hold.
	fleetKey *recoverykit.FleetKey

	closers []io.Closer
}

// fleetRecipient is the public recipient this install is handed: the one it
// generated, the one recorded on the cluster it is installing, or the one in
// this machine's config.
func (s *Session) fleetRecipient() string {
	if s.fleetKey != nil {
		return s.fleetKey.Recipient()
	}
	if s.Opts.FleetRecipient != "" {
		return s.Opts.FleetRecipient
	}
	if cfg, err := config.Load(); err == nil {
		return cfg.FleetRecipient
	}
	return ""
}

// instanceIdentity is the immutable instance id kits are bound to: the one
// this install recorded, or the one in this machine's config.
func (s *Session) instanceIdentity() string {
	if s.instanceID != "" {
		return s.instanceID
	}
	if s.Opts.InstanceID != "" {
		return s.Opts.InstanceID
	}
	if cfg, err := config.Load(); err == nil {
		return cfg.InstanceID
	}
	return ""
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

// Plan is the stages wired to what actually does the work.
//
// The engine (engine.go) owns order, journalling and failure reporting; this
// file owns which function each stage calls. They are separate so the
// sequencing is readable without the plumbing, and so a test can exercise
// resume against a table of fakes.
//
// A REGISTERED install runs the thirteen stages install.mdx names. A
// --control-plane install runs fourteen: the control-plane stage sits between
// platform-day2 and register, so the cluster that hosts the control plane is
// registered through the control plane it now hosts, by the normal path.
func Plan(s *Session) []Stage {
	bind := func(f func(context.Context, *Session) error) stages.StageFunc {
		return func(ctx context.Context) error { return f(ctx, s) }
	}
	if s.Opts.ControlPlaneInstall {
		return []Stage{
			{Name: StagePreflight, AlwaysRun: true, Run: bind(stagePreflight)},
			{Name: StageK3sServer, Component: "k3s", Run: bind(stageK3sServer)},
			{Name: StageK3sAgents, Component: "k3s", Run: bind(stageK3sAgents)},
			{Name: StageNetworking, Component: "traefik", Run: bind(stageNetworking)},
			{Name: StageCerts, Component: "cert-manager", Run: bind(stageCerts)},
			{Name: StageStorage, Component: "openebs-lvm-localpv", Run: bind(stageStorage)},
			// Velero only, unconfigured: it creates the repository password and
			// can upload nothing, so it is safe before the kit.
			{Name: StageBackup, Component: "velero", Run: bind(stageBackup)},
			{Name: StageDay2, Component: "system-upgrade-controller", Run: bind(stageDay2)},
			// AlwaysRun: a resume has to re-establish API access before the
			// register stage can use it, and the chart apply it performs is
			// idempotent.
			{Name: StageControlPlane, AlwaysRun: true, Run: bind(stageControlPlane)},
			{Name: StageRegister, AlwaysRun: true, Run: bind(stageRegister)},
			// The kit is written and verified here, before anything can
			// produce a backup (StageBackupTarget), and after both the
			// control-plane material (StageControlPlane) and the immutable
			// ids (StageRegister) exist.
			{Name: StageRecoveryKit, Run: bind(stageRecoveryKit)},
			{Name: StageBackupTarget, Component: "velero", Run: bind(stageBackupTarget)},
			{Name: StageAgent, Component: "kubenest-agent", Run: bind(stageAgent)},
			{Name: StageProfiles, Run: bind(stageProfiles)},
			{Name: StageRecord, Run: bind(stageRecord)},
			{Name: StageVerify, AlwaysRun: true, Run: bind(Verify)},
		}
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
		{Name: StageRecoveryKit, Run: bind(stageRecoveryKit)},
		{Name: StageBackupTarget, Component: "velero", Run: bind(stageBackupTarget)},
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
		Catalog:       s.catalog(),
		// The control plane is what this run is installing, so there is no
		// control-plane connectivity to check yet — the bundle request is
		// still checked, against the catalog built into this binary.
		ControlPlaneInstall: s.Opts.ControlPlaneInstall,
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

// catalog is where stage 1 reads the offered bundles from: the control plane
// when this cluster is being registered with one that already exists, the
// versions built into this binary when this run is INSTALLING the control
// plane — it does not exist yet to be asked. Same check either way — a
// request for a tier or a profile the bundle does not offer is refused before
// anything is written to a machine.
func (s *Session) catalog() preflight.Catalog {
	if s.Opts.ControlPlaneInstall {
		return EmbeddedCatalog{}
	}
	return bundleCatalog{s.API}
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
	org, err := register.ResolveOrg(ctx, s.API, s.Opts.Org)
	if err != nil {
		return err
	}
	cluster, adopted, err := register.EnsureCluster(ctx, s.API, org.ID, s.Opts.Name, "")
	if err != nil {
		return err
	}
	s.Jnl.ClusterID = cluster.ID
	// The organisation the cluster belongs to is part of every recovery kit's
	// immutable binding, so it is kept for the duration of the run. It is an
	// id, not a credential.
	s.orgID = org.ID
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
	// api.Secret refuses to marshal and Record has no field for it.
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
	// The CLI generates the cluster token's password itself and hands the
	// first server the file to read it from. Once that server is up, the full
	// K10<CA-HASH>::server:<password> token is read back from it: every join
	// and the recovery kit use that form, so joins pin the cluster CA. A full
	// token cannot be minted beforehand, because the CA it hashes does not
	// exist until k3s starts (k3s refuses it: "failed to normalize server
	// token"; found on real hardware 2026-09-25).
	//
	// It is held in this process's memory and nowhere else: not in the
	// session's journal, not in a log line, not in the install Record.
	password, err := k3s.GenerateToken()
	if err != nil {
		return err
	}

	if err := stages.NewComponentError("k3s", k3s.InstallServer(ctx, servers[0].Runner, s.Bundle, k3s.ServerOptions{Token: password}, s.Reporter)); err != nil {
		return err
	}
	token, err := k3s.NodeToken(ctx, servers[0].Runner)
	if err != nil {
		return fmt.Errorf("reading the cluster token back from %s: %w", servers[0].Address, err)
	}
	s.joinToken = token
	if len(servers) == 1 {
		return nil
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
	// The token this install minted for its first server, so every agent joins
	// with the same value the kit will carry. A resume skips the k3s stage, so
	// fall back to the read path rather than minting a second token that no
	// node would accept.
	token := s.joinToken
	if token == "" {
		var err error
		token, err = k3s.NodeToken(ctx, servers[0].Runner)
		if err != nil {
			return err
		}
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

// stageBackup installs Velero and nothing else.
//
// A backup target is optional and its absence is VISIBLE, not silent: the
// cluster reports backup: unconfigured in every heartbeat until one is set,
// because a cluster that has never taken a backup is exactly the quiet
// failure this product exists to prevent.
//
// It is deliberately no longer where the target, the schedule and the
// datastore snapshots are configured. Per the backup-restore page an
// installed-but-unconfigured Velero is a legitimate, VISIBLE state, and it can
// produce no upload, which is exactly why it may run before the recovery kit
// exists. Everything that CAN produce an upload moved to stageBackupTarget,
// which is ordered after the kit.
func stageBackup(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	return stages.NewComponentError("velero", backup.Install(ctx, server, s.Bundle, s.Reporter))
}

// stageBackupTarget points the cluster at the backup target and proves it
// works: the scope of its credential, the bucket's protection, the credentials
// Secret, the BackupStorageLocation, convergence until Velero validates the
// location Available, the default workload Schedule, and the datastore
// snapshots on every control-plane server.
//
// This is the stage that first allows a backup to exist, so it refuses to do
// any of it unless the recovery kit is in place. The guard is not decorative:
// it is checked at run time, against the kit this run wrote or the one written
// locally by an earlier run, because a resume skips the kit stage.
func stageBackupTarget(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	if s.Opts.BackupTarget == "" {
		s.Logf("  no --backup-target given: Velero is installed unconfigured and this cluster will report backup: unconfigured in every heartbeat until one is set. No backup can exist, so no kit is needed yet either")
		return nil
	}
	if _, err := s.recoveryKitInPlace(); err != nil {
		return fmt.Errorf("refusing to configure a backup target: %w. The %s stage writes and verifies the kit first, and a backup whose repository password exists only on a host that may be gone is the state that ordering removes", err, StageRecoveryKit)
	}
	target, err := parseBackupTarget(s.Opts.BackupTarget)
	if err != nil {
		return err
	}
	// Before anything depends on the target: what the bucket does and does not
	// protect. Warns, never refuses — an unknown Crest storage choice must not
	// block an install.
	for _, warning := range target.Preflight(ctx, target.Client) {
		s.Logf("  warning: %s", warning)
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

// stageRecoveryKit writes, uploads and verifies this cluster's recovery kit —
// and, on a control-plane install, the instance's own kit — before any backup
// can exist.
//
// What the bucket exposes, stated once, in the open:
//
//   - with --secrets-encryption (every server, every tier), the Secret VALUES
//     in a datastore snapshot are sealed by k3s's bootstrap data under the
//     cluster token — which is in this kit;
//   - volume data (kopia repositories) is encrypted by the per-cluster
//     repository password — which is in this kit;
//   - Velero's RESOURCE archives hold workload Secrets in PLAINTEXT and are
//     protected by the credential's prefix scope alone, which is why
//     `backup set-target` refuses a credential that reaches another prefix.
//
// The verification is honest about what it can do. The first control-plane
// install still holds the freshly generated fleet identity, so it decrypts
// both the local copy and the uploaded copy and only then continues. Every
// later install holds only the public recipient: it uploads, fetches the
// uploaded ciphertext back and compares its digest with the local file. It
// does not claim to have decrypted anything, and it never asks for the private
// key. Only `kubenest recovery-kit check`, run with the fleet key, can say the
// kit opens.
func stageRecoveryKit(ctx context.Context, s *Session) error {
	if s.Opts.BackupTarget == "" {
		s.Logf("  no --backup-target given: there is no S3 location for a recovery kit, and no backup can exist for one to open. Pass --backup-target (or `kubenest backup set-target`) and the kit is written and verified before anything is uploaded")
		return nil
	}
	server, err := s.Server()
	if err != nil {
		return err
	}
	target, err := parseBackupTarget(s.Opts.BackupTarget)
	if err != nil {
		return err
	}
	recipient := s.fleetRecipient()
	if recipient == "" {
		return errors.New("this install was not handed a fleet recovery recipient, so it cannot seal a kit that the fleet key would open. A --control-plane install records one; a cluster added to a fleet reads it from this machine's config, or takes --fleet-recipient (and --instance-id) from the machine that installed the control plane")
	}
	instanceID := s.instanceIdentity()
	if instanceID == "" {
		return errors.New("this install does not know its instance id, which every kit and recovery set is bound to. A --control-plane install records one; a cluster added to a fleet reads it from this machine's config, or takes --instance-id from the machine that installed the control plane")
	}
	if s.Jnl.ClusterID == "" || s.orgID == "" {
		return errors.New("this install has no recorded cluster or organisation id, so a kit written now could not be bound to anything: stage 2 (register) fills both")
	}

	token := s.joinToken
	if token == "" {
		// A resume skips the k3s stage, so the token it minted is not in this
		// process. The read path is the same one `node add` uses against a
		// cluster installed before the CLI minted tokens.
		token, err = k3s.NodeToken(ctx, server)
		if err != nil {
			return fmt.Errorf("reading back the cluster token the kit must carry: %w", err)
		}
	}
	repoPassword, err := backup.RepositoryPassword(ctx, server)
	if err != nil {
		return fmt.Errorf("reading the cluster's Velero repository password, which the kit must carry: %w", err)
	}

	now := time.Now().UTC()
	location := recoverykit.Location{Endpoint: target.Endpoint, Bucket: target.Bucket, Region: target.Region, Prefix: target.Prefix}
	scope := strings.Trim(target.Prefix, "/")
	client := target.Client

	binding := recoverykit.Binding{Kind: recoverykit.KindCluster, InstanceID: instanceID, OrganisationID: s.orgID, ClusterID: s.Jnl.ClusterID}
	// The bucket's own credentials are NOT in here: the operator keeps them
	// separately, offline, with the fleet key. A kit that carried them would
	// put the key to the bucket inside the bucket. What the kit records is the
	// non-secret S3 location above.
	secrets := map[string]string{
		recoverykit.KeyVeleroRepoPassword: repoPassword,
		recoverykit.KeyK3sJoinToken:       token,
	}
	kit, err := s.sealAndUpload(ctx, client, scope, binding, recoverykit.KindCluster, s.Jnl.ClusterID,
		location, veleroRepositoryID(), secrets, now)
	if err != nil {
		return err
	}
	s.kit = kit

	// The instance's own kit, on a control-plane install only: the control
	// plane's key material, which no cluster kit may carry. Only the instance
	// administrator may export it, which the control plane enforces; here it
	// simply must exist before the control plane's own backup can.
	if s.Opts.ControlPlaneInstall {
		if len(s.cpMaterial) == 0 {
			return errors.New("this is a --control-plane install but the control plane's key material is not in hand, so the instance kit cannot be written: stage 9 (control-plane) fills it")
		}
		// It is stored under the MANAGEMENT cluster's id — the cluster this
		// control plane runs in — so the instance's own artifacts sit beside
		// that cluster's and the control plane's separate copy of the set
		// lands at the same relative path.
		cpBinding := recoverykit.Binding{Kind: recoverykit.KindControlPlane, InstanceID: instanceID, ClusterID: s.Jnl.ClusterID}
		if _, err := s.sealAndUpload(ctx, client, scope, cpBinding, recoverykit.KindControlPlane, s.Jnl.ClusterID,
			location, "", s.cpMaterial, now); err != nil {
			return err
		}
	}
	return nil
}

// veleroRepositoryID is the Velero backup repository identity this cluster's
// volumes are written to. It is derived from the managed storage location and
// the repository type, not read from a live repository: the kit has to exist
// BEFORE the first repository does.
func veleroRepositoryID() string {
	return "velero-" + backup.StorageLocationName + "-kopia"
}

// sealAndUpload is the whole write-verify path for one kit: seal, write
// locally, upload, fetch back, verify. Nothing else in the install path writes
// a kit, so the ordering guarantee has one implementation.
func (s *Session) sealAndUpload(ctx context.Context, client backup.Bucket, scope string, binding recoverykit.Binding, kind recoverykit.Kind, pathSegment string, location recoverykit.Location, repoID string, secrets map[string]string, now time.Time) (*recoverykit.Kit, error) {
	artifactID, err := recoverykit.ArtifactID(now)
	if err != nil {
		return nil, err
	}
	kit, err := recoverykit.New(binding, artifactID, location, repoID, secrets, now)
	if err != nil {
		return nil, err
	}
	if err := kit.SealTo(s.fleetRecipient()); err != nil {
		return nil, err
	}
	doc, err := kit.Document()
	if err != nil {
		return nil, err
	}
	local, err := recoverykit.LocalPath(pathSegment, kind, artifactID)
	if err != nil {
		return nil, err
	}
	if err := writeLocalKit(local, doc); err != nil {
		return nil, err
	}

	key := recoverykit.KitKey(scope, pathSegment, kind, artifactID)
	if err := client.Put(ctx, key, doc); err != nil {
		return nil, fmt.Errorf("uploading the %s recovery kit to %s: %w. Nothing else may be created until the kit is in the bucket: a cluster whose repository password exists only on this laptop is the state this stage removes", kind, key, err)
	}

	// Verify. What can be verified depends on which half of the fleet key this
	// process holds, and the report says which it did.
	uploaded, err := client.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("fetching the uploaded %s recovery kit back from %s to verify it: %w", kind, key, err)
	}
	switch {
	case s.fleetKey != nil:
		// The first control-plane install: the freshly generated identity is
		// in hand, so both copies are opened, not merely digested.
		for _, copyOf := range []struct {
			what string
			doc  []byte
		}{{"local", doc}, {"uploaded", uploaded}} {
			back, err := recoverykit.Load(copyOf.doc)
			if err != nil {
				return nil, fmt.Errorf("the %s copy of the %s recovery kit is unreadable: %w", copyOf.what, kind, err)
			}
			opened, err := back.Secrets(s.fleetKey.SecretKeyString())
			if err != nil {
				return nil, fmt.Errorf("the %s copy of the %s recovery kit did not open with the fleet key this install generated: %w", copyOf.what, kind, err)
			}
			if err := back.FingerprintsMatch(opened); err != nil {
				return nil, fmt.Errorf("the %s copy of the %s recovery kit: %w", copyOf.what, kind, err)
			}
		}
		s.Logf("  %s recovery kit %s: decrypted from both the local and the uploaded copy", kind, artifactID)
	default:
		if got, want := recoverykit.Digest(uploaded), recoverykit.Digest(doc); got != want {
			return nil, fmt.Errorf("the uploaded %s recovery kit at %s is not the one written (uploaded %s, local %s). This install holds only the public recipient, so it cannot open the kit; it can only prove the upload is intact", kind, key, got, want)
		}
		s.Logf("  %s recovery kit %s: uploaded and digest-verified against the local copy (this install holds only the public recipient, so it did not — and must not claim to — decrypt it)", kind, artifactID)
	}

	// The recovery set: the plaintext manifest a recovery selects a kit and a
	// backup by. Only a completed upload is eligible, so it is written after
	// the verification above.
	set, err := recoverykit.NewSet(kit, s.versions(), recoverykit.Digest(doc), now)
	if err != nil {
		return nil, err
	}
	set.Complete = true
	setDoc, err := set.Document()
	if err != nil {
		return nil, err
	}
	if err := client.Put(ctx, recoverykit.SetKey(scope, pathSegment, kind, artifactID), setDoc); err != nil {
		return nil, fmt.Errorf("writing the %s recovery set: %w", kind, err)
	}

	// And the control plane records that the kit exists — by fingerprint, never
	// by key. It cannot re-create the private material (it never holds any), so
	// its job is to know which kit is whose, which is what makes an export
	// authorisable. A failure here fails the stage: a kit the control plane
	// does not know about is a kit nothing will offer to export on the day the
	// cluster is gone.
	if err := s.recordKit(ctx, kind, binding, kit); err != nil {
		return nil, err
	}
	return kit, nil
}

// recordKit posts the kit's record: when it was written, its artifact id and a
// fingerprint of each key it carries.
func (s *Session) recordKit(ctx context.Context, kind recoverykit.Kind, binding recoverykit.Binding, kit *recoverykit.Kit) error {
	if s.API == nil || s.orgID == "" {
		return fmt.Errorf("cannot record the %s recovery kit with the control plane: this install has no authenticated control-plane client or no organisation id (stage 2 fills the latter). The control plane has to know the kit exists, or nothing will offer to export it", kind)
	}
	location := map[string]any{
		"endpoint": kit.S3Location.Endpoint,
		"bucket":   kit.S3Location.Bucket,
		"region":   kit.S3Location.Region,
		"prefix":   kit.S3Location.Prefix,
	}
	record := api.RecoveryKitRecord{
		ArtifactID:   kit.ArtifactID,
		Fingerprints: kit.Fingerprints,
		WrittenAt:    kit.WrittenAt,
		ClusterID:    binding.ClusterID,
		InstanceID:   binding.InstanceID,
		S3Location:   location,
	}
	if kind == recoverykit.KindCluster {
		// The route's path carries a cluster kit's cluster, and the backend
		// refuses a body that names a DIFFERENT one — so the body names the
		// same one rather than naming none and leaving a gap.
		record.VeleroRepositoryID = kit.VeleroRepositoryID
	}
	if err := s.API.RecordRecoveryKit(ctx, s.orgID, binding.ClusterID, string(kind), record); err != nil {
		return fmt.Errorf("recording the %s recovery kit %s with the control plane: %w", kind, kit.ArtifactID, err)
	}
	return nil
}

func writeLocalKit(path string, doc []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating the recovery-kit directory: %w", err)
	}
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		return fmt.Errorf("writing the recovery kit to %s: %w", path, err)
	}
	return nil
}

// recoveryKitInPlace returns the kit this cluster's backups must be opened by:
// the one this run wrote, or — on a resume, which skips the kit stage — the
// newest one written locally for this cluster.
func (s *Session) recoveryKitInPlace() (*recoverykit.Kit, error) {
	if s.kit != nil {
		return s.kit, nil
	}
	if s.Jnl.ClusterID == "" {
		return nil, errors.New("no cluster id is recorded")
	}
	kit, err := recoverykit.NewestLocal(s.Jnl.ClusterID, recoverykit.KindCluster)
	if err != nil {
		return nil, err
	}
	return kit, nil
}

// versions is the exact set of versions a recovery of this cluster needs, from
// the manifest that installed it.
func (s *Session) versions() map[string]string {
	versions := map[string]string{"bundle": s.Bundle.Bundle}
	for _, key := range []string{"velero", "k3s"} {
		if v, err := s.Bundle.Core.Version(key); err == nil {
			versions[key] = v
		}
	}
	return versions
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

// stageControlPlane installs the KubeNest control plane into this cluster and
// leaves the CLI logged in to it, so this cluster — the first one, the
// management cluster — is registered through the control plane it now hosts
// by the ordinary register stage that follows.
//
// It runs only for --control-plane, and it saves the CLI's login itself: that
// is what lets a resumed install (whose earlier stages are skipped) still
// reach the API, and it is why the stage is AlwaysRun — re-establishing
// access is the work, and the Helm apply it performs is idempotent.
func stageControlPlane(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	// The instance's identity comes first, because everything the fleet keeps
	// off a host is bound to it: the immutable instance id, and the PUBLIC
	// recipient of the fleet recovery key.
	//
	// On the first install there is nothing recorded, so this process mints
	// the fleet recovery identity. Its private half NEVER leaves this process:
	// it is not written to the host, not to the journal, not to a log line and
	// not to the install Record, which deliberately has no field one fits
	// into. It is printed once, after the recipient is durably recorded, so a
	// run that fails before that leaves no stale key in an operator's notes.
	//
	// Every later install holds only the recipient it reads back here.
	inst, _, err := controlplane.EnsureInstance(ctx, server, s.Opts.FleetRecipient)
	if errors.Is(err, controlplane.ErrNoInstance) {
		key, keyErr := recoverykit.GenerateFleetKey()
		if keyErr != nil {
			return keyErr
		}
		var created bool
		inst, created, err = controlplane.EnsureInstance(ctx, server, key.Recipient())
		if err != nil {
			return err
		}
		s.fleetKey = key
		if created {
			s.Logf(""+
				"  fleet recovery key — the only copy that will ever exist:\n"+
				"\n"+
				"    %s\n"+
				"\n"+
				"  Write it down and keep at least two offline copies apart from each other. It is not\n"+
				"  stored on this machine, on any host, or in the control plane: losing every copy means\n"+
				"  no recovery kit ever opens, and nothing can recreate it. Every kit and every\n"+
				"  checkpoint is encrypted to this key's public half.", key.SecretKeyString())
		}
	} else if err != nil {
		return err
	}
	s.instanceID = inst.ID
	sec, created, err := controlplane.EnsureSecrets(ctx, server)
	if err != nil {
		return err
	}
	// THE CONTROL PLANE'S OWN RECOVERY POINT. Built before the values, because
	// the chart's checkpoint CronJob and the rest of the control plane are one
	// apply, and because the fleet recipient this install holds is right here.
	checkpoint, err := s.checkpointTarget(ctx, server, inst)
	if err != nil {
		return err
	}
	values, err := controlplane.Values(controlplane.Settings{
		Domain:     s.Opts.Domain,
		AdminEmail: s.Opts.AdminEmail,
		Checkpoint: checkpoint,
	}, sec)
	if err != nil {
		return err
	}
	// The control plane's OWN certificate authority, minted by this install
	// and handed to the chart (gatewayCA): its Gateway certificate is signed by
	// this authority rather than by the host cluster's, so a control plane
	// restored or moved onto another cluster keeps the identity every CLI and
	// agent pins. The CLI itself trusts it next, and so does every agent this
	// machine installs (Options.ControlPlaneCA, consumed by the agent stage).
	ca := []byte(sec.GatewayCACertificate)
	revision, err := controlplane.Apply(ctx, server, values)
	if err != nil {
		return err
	}
	// THE MIGRATION STEP, before the control plane is allowed to serve.
	//
	// It runs only when this cluster ALREADY HAD this control plane's install
	// Secret — that is, when a database may already hold a schema. A first
	// install's database is empty: the backend builds that schema from its
	// models, and migrating an empty database is exactly what the plan forbids
	// (it would create a schema a restored checkpoint could not then load).
	// Any later run of this stage is the opposite case: the schema may predate
	// this build, the backend refuses to start on a schema it does not match,
	// and the Job is what brings the database to the code's revision.
	if !created {
		revision, err = controlplane.Migrate(ctx, server, values, s.Bundle, s.Reporter)
		if err != nil {
			return err
		}
		// Recorded, because "did the migration run, and which revision did the
		// control plane come up at" is what a resume has to answer without
		// re-deriving it: the Job object expires from the cluster.
		s.Record.ControlPlaneMigration = controlplane.MigrationJobName + "@" + revision
		if err := s.saveRecord(); err != nil {
			return err
		}
	}
	if err := controlplane.WaitReady(ctx, server, revision, s.Bundle, s.Reporter); err != nil {
		return err
	}
	addr, err := controlplane.BackendAddr(ctx, server)
	if err != nil {
		return err
	}
	// The control plane's own key material, held in this process only until
	// the kit stage can seal it. A CLUSTER kit must never carry any of it, so
	// it is kept apart from everything else the session holds and is never
	// journalled.
	//
	// The CA travels as one PEM stream, certificate and key: the kit's entry
	// for the control plane's authority has to be able to renew the
	// certificate every CLI and agent pinned, and a certificate without its
	// key cannot.
	s.cpMaterial = map[string]string{
		recoverykit.KeyEncryptionKey:  sec.EncryptionKey,
		recoverykit.KeyAgentJWTSecret: sec.AgentJWTSecret,
		recoverykit.KeyControlPlaneCA: sec.CABundle(),
	}
	// The agent stage trusts this CA: it is the authority of the control plane
	// the management cluster's own operator talks to, and of the endpoint
	// every cluster added later reports through.
	s.Opts.ControlPlaneCA = ca

	// The backend is a ClusterIP that only the node can route to, so the CLI
	// reaches it through a TCP connection opened FROM the node over the SSH
	// connection this install already holds. Nothing else in the CLI can
	// reach this control plane until DNS exists for it.
	tunnel, ok := server.(interface {
		DialTCP(ctx context.Context, addr string) (net.Conn, error)
	})
	if !ok {
		return fmt.Errorf("the SSH connection to the server cannot open a tunnel to the control plane backend (%s), so the CLI cannot log in to the control plane it just installed", addr)
	}
	open := func(opts ...api.Option) (*api.Client, error) {
		dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
			return tunnel.DialTCP(ctx, addr)
		}
		return api.New("http://"+addr, append([]api.Option{api.WithDialContext(dial)}, opts...)...)
	}
	base := "https://api." + s.Opts.Domain

	token, err := s.controlPlaneToken(ctx, open, base, sec.AdminPassword)
	if err != nil {
		return err
	}
	client, err := open(api.WithToken(token))
	if err != nil {
		return err
	}
	s.API = client

	// Stored exactly as `kubenest login` stores it, so every later command
	// finds this control plane without being told its URL or handed its CA.
	creds, err := config.LoadCredentials()
	if err != nil {
		return err
	}
	creds.Set(base, token)
	if err := config.SaveCredentials(creds); err != nil {
		return fmt.Errorf("the CLI token was obtained but could not be stored: %w", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.ControlPlaneURL = base
	cfg.ControlPlaneCA = string(ca)
	cfg.UserEmail = s.Opts.AdminEmail
	cfg.Token = "" // no legacy password JWT lives in config.json any more
	// This instance's identity, stored exactly as the control-plane URL and CA
	// are, so every later install on this machine encrypts each kit to the
	// fleet's recipient without being told it — and without ever being asked
	// for the private key, which this machine does not have.
	cfg.InstanceID = inst.ID
	cfg.FleetRecipient = inst.FleetRecipient
	if err := config.Save(cfg); err != nil {
		return err
	}

	s.Logf("  control plane: https://api.%s, console https://app.%s", s.Opts.Domain, s.Opts.Domain)
	s.Logf("  logged in as %s; the CLI token is stored in credentials.json and the control plane's CA in config.json", s.Opts.AdminEmail)
	return s.showAdminPasswordOnce(sec.AdminPassword)
}

// checkpointTarget is the control plane's own recovery target: where its
// checkpoints are written, what they are sealed to, and who may upload them.
//
// WHY THE CONTROL PLANE'S STAGE OWNS THIS, even though the target it derives
// from is the BACKUP target, which is configured later (StageBackupTarget runs
// after this stage, because a cluster's kit has to exist before a backup may).
// The control plane's Helm release is one apply: the checkpoint CronJob and the
// backend it runs in are rendered together, and the fleet recipient the
// checkpoint is sealed to is only in hand here.
//
// NOTHING IS ENABLED UNLESS THE PRINCIPAL IS PROVEN. The checkpoints land in
// the same bucket as the clusters' backups, so the credential that writes them
// must be REFUSED on every cluster prefix this install knows about — read
// access to a cluster's backups is cluster-admin on that cluster, and one
// credential that reaches both is one leak away from the fleet's recovery
// history. VerifyScope is that proof: a refusal there fails the install rather
// than leaving a CronJob that will upload the fleet's database with the wrong
// key.
//
// AND THIS IS WHY IT MAY RUN BEFORE THE KIT STAGE. A cluster's backup may not
// be configured until its recovery kit exists, because the kit carries the
// repository password that makes those backups readable. A control-plane
// checkpoint does not depend on the kit at all: its two keys are the fleet
// recipient (this install holds it) and the control plane's own object-store
// principal (verified above), and its content is the control plane's database.
// The control-plane kit is written later in the same install, and nothing about
// the checkpoint waits for it.
func (s *Session) checkpointTarget(ctx context.Context, server k3s.Runner, inst controlplane.Instance) (*controlplane.CheckpointTarget, error) {
	if s.Opts.BackupTarget == "" {
		// Said out loud rather than left out of the values document: the
		// chart's checkpoint CronJob is the control plane's only supported
		// recovery path, and an install that omits it has to report that
		// plainly. It is reported where it matters too — the management
		// cluster's `backup` verdict says the control plane is unprotected
		// until a target exists.
		s.Logf("  the control-plane checkpoint CronJob is NOT installed: this install was given no --backup-target, so there is nowhere to upload a checkpoint, and the management cluster will report the control plane as unprotected until there is")
		return nil, nil
	}
	if inst.FleetRecipient == "" {
		return nil, errors.New("this control plane has no fleet recovery recipient, so no checkpoint could be sealed to a key the fleet holds. A first --control-plane install records one; a later one is handed it with --fleet-recipient (and --instance-id) from the machine that installed the control plane")
	}
	target, err := parseBackupTarget(s.Opts.BackupTarget)
	if err != nil {
		return nil, err
	}
	// The CONTROL PLANE's principal, from its own variables. The cluster's
	// KUBENEST_BACKUP_* pair is deliberately not consulted: it is the
	// credential the check below exists to refuse, and reading it here would
	// make "the checkpoints have their own principal" a claim rather than a
	// fact. AWS_* is the conventional fallback, as it is for the target.
	checkpoint := controlplane.NewCheckpointTarget(
		target,
		inst.FleetRecipient,
		envFirst(controlplane.EnvCheckpointAccessKeyID, "AWS_ACCESS_KEY_ID"),
		envFirst(controlplane.EnvCheckpointSecretAccessKey, "AWS_SECRET_ACCESS_KEY"),
	)
	if err := checkpoint.Validate(); err != nil {
		return nil, err
	}
	probe, err := checkpoint.S3Client()
	if err != nil {
		return nil, err
	}
	if err := checkpoint.VerifyScope(ctx, probe, []string{target.Prefix}); err != nil {
		return nil, fmt.Errorf("refusing to enable the control-plane checkpoint: %w", err)
	}
	if err := controlplane.EnsureCredentials(ctx, server, checkpoint); err != nil {
		return nil, err
	}
	s.Logf("  control-plane checkpoints: %s, sealed to the fleet recipient and uploaded to s3://%s/%s by the control plane's own principal (secret %s/%s)",
		"nightly pg_dump -Fc", checkpoint.Bucket, checkpoint.Prefix, controlplane.Namespace, checkpoint.CredentialsSecret)
	return &checkpoint, nil
}

// showAdminPasswordOnce prints the administrator password the first time this
// install's control-plane stage completes, and records that it did.
//
// Keyed to the journal, not to whether this run generated the password: on
// real hardware the first run generated it and then failed at this stage's
// readiness check, and every resumed run found the Secret already there, so
// an install keyed on "generated in this run" never showed the password at
// all. Only the flag is journalled; the password lives in the cluster Secret.
func (s *Session) showAdminPasswordOnce(password string) error {
	if s.Record.AdminPasswordShown {
		return nil
	}
	s.Logf("  administrator: %s", s.Opts.AdminEmail)
	s.Logf("  admin password: %s", password)
	s.Logf("  the password is stored in the cluster Secret %s/%s and will not be shown again",
		controlplane.Namespace, controlplane.SecretName)
	s.Record.AdminPasswordShown = true
	return s.saveRecord()
}

// controlPlaneToken returns a CLI token for the control plane this run just
// installed: the one already stored for it when that still authenticates (a
// resumed install), otherwise a fresh one minted from the administrator login.
//
// open is the tunneled client factory: every call here goes through the node,
// because nothing else can reach a control plane that has no DNS yet.
func (s *Session) controlPlaneToken(ctx context.Context, open func(...api.Option) (*api.Client, error), base, adminPassword string) (string, error) {
	if cfg, err := config.Load(); err == nil && cfg.ControlPlaneURL == base {
		creds, err := config.LoadCredentials()
		if err != nil {
			return "", err
		}
		if stored := creds.TokenFor(base); stored != "" {
			client, err := open(api.WithToken(stored))
			if err != nil {
				return "", err
			}
			// The cheapest authenticated call there is: proving the stored
			// token is still accepted is what makes reusing it safe.
			if _, err := client.ListOrgs(ctx); err == nil {
				s.Logf("  reusing the CLI token already stored for %s", base)
				return stored, nil
			}
			s.Logf("  the stored CLI token for %s is no longer accepted; minting a new one", base)
		}
	}

	session, err := open()
	if err != nil {
		return "", err
	}
	sessionToken, err := session.PasswordLogin(ctx, s.Opts.AdminEmail, adminPassword)
	if err != nil {
		return "", fmt.Errorf("logging in to the control plane this run installed: %w", err)
	}
	authed, err := open(api.WithToken(sessionToken))
	if err != nil {
		return "", err
	}
	token, err := authed.CreateCLIToken(ctx, "kubenest-cli", cliTokenScopes, 90)
	if err != nil {
		return "", fmt.Errorf("minting a CLI token on the control plane this run installed: %w", err)
	}
	return token, nil
}

// cliTokenScopes is what the installer's own token may do, and nothing else.
var cliTokenScopes = []string{"clusters:read", "clusters:register", "bundles:read", "install:report"}

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
	// A management cluster's operator talks to the hub running beside it,
	// inside the cluster, rather than to the public API the mint's hub URL
	// names. A registered cluster reaches the control plane it was registered
	// with, and carries the CA to trust it.
	//
	// Both shapes carry the CA (kn-t47), because in both the operator verifies
	// an endpoint whose certificate the control plane's own authority signed,
	// and no public trust store has that authority: for a --control-plane
	// install the control-plane stage just minted it and filled this in; for
	// every other cluster it is the CA this machine stored when it logged in.
	var opts agent.ValuesOptions
	if s.Opts.ControlPlaneInstall {
		opts.BackendURLOverride = controlplane.HubInClusterURL
	}
	opts.ControlPlaneCA = s.Opts.ControlPlaneCA
	return stages.NewComponentError("kubenest-agent",
		agent.Install(ctx, server, s.Bundle, creds, opts, s.Reporter))
}

// stageProfiles installs each selected profile, in the order given.
//
// Every requested profile has already been checked against the bundle by
// preflight, so anything reaching here is offered by the bundle — but the
// component profiles are not built yet (kn-sev5, kn-ynaq, wave 4).
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
	return fmt.Errorf("bundle %s offers %s, but this build of the CLI cannot install %s yet — the component profiles land after core (kn-sev5 observability, kn-ynaq secrets). Install core now and add the profile when it ships",
		s.Bundle.Bundle, strings.Join(s.Bundle.Profiles.Names(), ", "), strings.Join(unbuilt, ", "))
}

// stageRecord writes what was installed against the cluster, in the control
// plane: bundle version, profile set, HA tier and volume-group ownership. It
// is the same call in both modes — a --control-plane install set s.API in the
// control-plane stage, so by here there is always a control plane to record
// against.
//
// The ownership value is not bookkeeping — it is what uninstall reads to
// decide whether it may remove a volume group, which is the difference
// between a clean teardown and destroying a customer's data.
//
// sshNodeInfo is the part of a node's connection the record stage reads: the
// resolved endpoint (the SSH user and port actually in use, which may come
// from ssh_config rather than a flag) and the SHA-256 of the host key the
// handshake negotiated. It is declared before stageRecord only because Go
// reads top to bottom; the stage is below.
//
// An interface rather than a concrete type because the runner is what a test
// fakes: a fake has neither, and the entry then records what was observed —
// which is nothing — instead of inventing a port.
type sshNodeInfo interface {
	Endpoint() *sshx.Endpoint
	HostKeyFingerprint() string
}

// hostInventory is the record's host list: one entry per node this install
// opened a connection to.
//
// The host ID is minted here, at the one moment a host enters the cluster's
// inventory. The Node UID is read back from the CLUSTER rather than derived
// from the address: the node object knows itself by its hostname and internal
// IP, and a UID that is guessed is worse than one that is absent, because a
// later operation uses it to tell a rebuilt node from the node it recorded.
func (s *Session) hostInventory(ctx context.Context, ownership storage.Ownership) ([]api.HostRecord, error) {
	servers := s.NodesWithRole(RoleServer)
	if len(servers) == 0 {
		return nil, fmt.Errorf("no server node, so the address the cluster's nodes joined through is unknown")
	}
	joinAddress := serverURL(servers[0].Address)

	uids := map[string]string{}
	if byAddress, err := k3s.NodeUIDsByAddress(ctx, servers[0].Runner); err != nil {
		// The install has already succeeded on the machines; a kubectl read
		// that fails here must not fail the install, the same way a failed
		// install-stage report must not (see ReportInstallStage). An entry
		// with no Node UID is the honest "not read", and the first node
		// operation that needs one reads it from the cluster it is about to
		// act on anyway.
		s.Logf("  warning: the cluster's Node UIDs could not be read (%v); the inventory records the hosts without them", err)
	} else {
		uids = byAddress
	}

	// The storage stage records the device in the journal and this stage runs
	// after it; the flag is the same value for a run that has not reached that
	// stage yet.
	device := s.Record.Device
	if device == "" {
		device = s.Opts.StorageDevice
	}

	hosts := make([]api.HostRecord, 0, len(s.Nodes))
	for _, n := range s.Nodes {
		port, user, fingerprint := 22, s.Opts.SSHUser, ""
		if info, ok := n.Runner.(sshNodeInfo); ok {
			fingerprint = info.HostKeyFingerprint()
			if ep := info.Endpoint(); ep != nil {
				if ep.Port != 0 {
					port = ep.Port
				}
				if user == "" {
					user = ep.User
				}
			}
		}
		entry, err := node.Entry{
			Role:                 node.Role(n.Role),
			SSHAddress:           n.Address,
			SSHPort:              port,
			SSHUser:              user,
			HostKeyFingerprint:   fingerprint,
			JoinAddress:          joinAddress,
			NodeUID:              uids[n.Address],
			StorageDevice:        device,
			VolumeGroupOwnership: string(ownership),
			LifecycleState:       node.StateActive,
		}.Record()
		if err != nil {
			return nil, fmt.Errorf("recording host %s: %w", n.Address, err)
		}
		hosts = append(hosts, entry)
	}
	return hosts, nil
}

// stageRecord writes the cluster's record: what was installed, which
// profiles, which tier, who owns the volume groups — and, since kn-t50,
// WHICH MACHINES this cluster is.
//
// The host list is what makes a node verb possible from a laptop other than
// the one that ran the install, so it is written by the stage that already
// knows every host it just touched rather than by a later command that would
// have to be told.
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
	// The revision this write carries is the one the record is at NOW, read
	// here rather than assumed to be 0. The write is a compare-and-swap: a
	// body carrying a stale revision is refused (409) by design, and an
	// install that failed at its last stage over a number nobody typed would
	// be the wrong way to learn that someone else has moved the inventory.
	current, err := s.API.BundleRecord(ctx, s.Jnl.ClusterID)
	if err != nil {
		return fmt.Errorf("reading the cluster's record, which this stage's write is based on: %w", err)
	}
	hosts, err := s.hostInventory(ctx, ownership)
	if err != nil {
		return err
	}
	return s.API.PutBundleRecord(ctx, s.Jnl.ClusterID, api.BundleRecord{
		BundleVersion:        s.Opts.Bundle,
		Profiles:             profiles,
		HATier:               s.Opts.HATier,
		VolumeGroupOwnership: string(ownership),
		InstallJournal:       terminalEntries(s.Jnl),
		Hosts:                hosts,
		Revision:             current.Revision,
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
