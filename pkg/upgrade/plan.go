package upgrade

import (
	"context"
	"fmt"
	"strings"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/component/certmanager"
	"kubenest.io/cli/pkg/component/day2"
	"kubenest.io/cli/pkg/component/gatewayapi"
	"kubenest.io/cli/pkg/component/traefik"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/deprecation"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/storage"
)

// Plan is the eight stages, wired.
//
// Components first, Kubernetes last: everything before StageKubernetes is a
// Helm release that reverts in seconds, and StageKubernetes is the point of
// no return. The ordering is the argument — five of the seven ways this can
// fail cost seconds because of where the irreversible step sits, not because
// of any recovery machinery.
func Plan(s *Session) []stages.Stage {
	// Every stage checks the maintenance window before it starts. THE RULE:
	// no new stage starts once the window has closed, but the stage in
	// progress finishes — abandoning a half-completed stage to respect a
	// clock leaves the cluster in a worse state than the overrun does.
	// windowExempt says which stages are never paused and why.
	bind := func(name string, f func(context.Context, *Session) error) stages.StageFunc {
		return func(ctx context.Context) error {
			if err := s.windowStillOpen(name); err != nil {
				return err
			}
			return f(ctx, s)
		}
	}
	return []stages.Stage{
		{Name: StagePreflight, AlwaysRun: true, Run: bind(StagePreflight, stagePreflight)},
		{Name: StageBackup, Component: "velero", AlwaysRun: true, Run: bind(StageBackup, stageBackup)},
		{Name: StageComponents, Run: bind(StageComponents, stageComponents)},
		{Name: StageProfiles, Run: bind(StageProfiles, stageProfiles)},
		{Name: StageAgent, Component: "kubenest-agent", Run: bind(StageAgent, stageAgent)},
		{Name: StageKubernetes, Component: "k3s", Run: bind(StageKubernetes, stageKubernetes)},
		{Name: StageVerify, AlwaysRun: true, Run: bind(StageVerify, stageVerify)},
		{Name: StageRecord, Run: bind(StageRecord, stageRecord)},
	}
}

// stagePreflight runs every gate. Nothing is changed by it, which is what
// makes abandoning an upgrade here free.
func stagePreflight(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	var report GateReport

	report.add(checkBundlePath(s.From, s.To, s.installedProfiles(), s.haTier()))
	report.add(checkWindow(s))

	dwell, err := s.To.Limits.Timeouts.For("node-ready")
	if err != nil {
		return err
	}
	report.add(checkNodesReady(ctx, server, dwell, s.now()))

	headroom := s.To.Limits.Resources.UpgradeHeadroom.Disk
	if headroom <= 0 {
		return fmt.Errorf("bundle manifest has no limits.resources.upgrade-headroom.disk: running out of disk mid-upgrade is a hard failure at the worst moment, so the threshold comes from the bundle rather than a default here")
	}
	report.add(checkDiskHeadroom(ctx, s.Nodes, headroom))
	report.add(checkDisruptionBudgets(ctx, server))

	maxAge, err := s.To.Health.MaxRestoreDrillAge()
	if err != nil {
		return err
	}
	drill, drillErr := s.drill(ctx)
	report.add(checkDrill(drill, drillErr, maxAge, s.now()))

	// The scan last, because it is the slowest and the one most likely to
	// need the operator to go and change something: reporting the cheap
	// refusals alongside it saves a round trip.
	report.add(s.scanDeprecatedAPIs(ctx, server, &report))

	for _, g := range report.Results {
		if g.Passed {
			s.Logf("  ok   %s: %s", g.Gate, g.Detail)
		}
	}
	for _, w := range report.Deprecations.Warnings {
		s.Logf("  warning: %s uses %s, deprecated in %s and removed in %s",
			w.Ref(), w.APIVersion, w.DeprecatedIn, w.RemovedIn)
	}
	return report.Err()
}

// scanDeprecatedAPIs is the gate that matters most, and the one that makes
// this an upgrade product rather than a cron job.
func (s *Session) scanDeprecatedAPIs(ctx context.Context, server k3s.Runner, report *GateReport) GateResult {
	targetK3s, err := s.To.Core.Version("k3s")
	if err != nil {
		return GateResult{Gate: GateDeprecatedAPIs, Passed: false, Detail: err.Error(),
			Fix: "the target bundle must pin a Kubernetes version to scan against"}
	}
	scan, err := deprecation.Scan(ctx, server, s.To, s.Opts.Acknowledge, targetK3s)
	if err != nil {
		return GateResult{Gate: GateDeprecatedAPIs, Passed: false, Detail: err.Error(),
			Fix: "this gate fails closed: a scan that could not run is not a cluster with nothing to find"}
	}
	report.Deprecations = scan
	if err := scan.Err(); err != nil {
		return GateResult{Gate: GateDeprecatedAPIs, Passed: false, Detail: err.Error(),
			Fix: "fix the resources named above in your own manifests and redeploy them, then re-run. We cannot rewrite your application for you"}
	}
	detail := fmt.Sprintf("no workload uses an API removed in Kubernetes %s", deprecation.KubernetesVersion(targetK3s))
	if len(scan.Warnings) > 0 {
		detail += fmt.Sprintf(" (%d deprecated-but-present, listed above)", len(scan.Warnings))
	}
	if len(scan.Acknowledged) > 0 {
		detail += fmt.Sprintf("; %d finding(s) accepted by name", len(scan.Acknowledged))
	}
	return GateResult{Gate: GateDeprecatedAPIs, Passed: true, Detail: detail}
}

// stageBackup takes the datastore snapshot and the workload backup that a
// rollback returns to. It is the last moment before anything changes.
//
// It ALWAYS runs, even on a resume: the state you may need to return to must
// be from now, not from the attempt that failed yesterday.
func stageBackup(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	name := fmt.Sprintf("pre-upgrade-%s-%s", strings.ReplaceAll(s.Opts.To, ".", "-"), s.now().UTC().Format("20060102t150405"))

	// The datastore snapshot is the one that matters for rollback: it is
	// what returns Kubernetes objects to their pre-upgrade state after the
	// point of no return.
	res, err := server.Run(ctx, "sudo -n k3s etcd-snapshot save --name "+shellQuote(name))
	if err != nil {
		return stages.NewComponentError("k3s", err)
	}
	if res.ExitCode != 0 {
		return stages.NewComponentError("k3s", fmt.Errorf("taking the datastore snapshot: exit %d: %s", res.ExitCode, firstLine(res.Stderr)))
	}
	s.Record.Snapshot = name
	s.Record.SnapshotAt = s.now().UTC()

	// The workload backup is Velero's, and it is optional in exactly one
	// case: a cluster with no backup target configured. That is a warning
	// rather than a refusal, because the datastore snapshot — which is what
	// rollback actually needs — is local and was just taken.
	unconfigured, err := backup.Unconfigured(ctx, server)
	if err != nil {
		return stages.NewComponentError("velero", err)
	}
	if unconfigured {
		s.Logf("  no backup target is configured, so no workload backup was taken. The datastore snapshot %s was, and that is what a rollback restores", name)
		return s.saveRecord()
	}
	if err := backup.TakeBackup(ctx, server, s.To, name, s.Reporter); err != nil {
		return stages.NewComponentError("velero", err)
	}
	s.Record.BackupName = name
	return s.saveRecord()
}

// coreComponent is one platform component in dependency order, with the
// manifest key that pins it and the call that moves it.
type coreComponent struct {
	key     string
	install func(context.Context, k3s.Runner, *manifest.Manifest, converge.Reporter) error
}

// stageComponents moves every core component to the target bundle's pins, in
// dependency order.
//
// Each is a Helm release expressed as a HelmChart resource, so "upgrade" here
// is rewriting the resource at the new pinned version and letting the
// cluster's own helm-controller perform the upgrade — the same mechanism that
// installed it, which is also why the revert is a rewrite at the old version
// and takes seconds.
func stageComponents(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	for _, c := range coreComponents() {
		from, _ := s.From.Core.Version(c.key)
		to, err := s.To.Core.Version(c.key)
		if err != nil {
			return err
		}
		if from == to {
			s.Logf("  %s unchanged at %s", c.key, to)
			continue
		}
		s.Logf("  %s %s → %s", c.key, from, to)
		if err := stages.NewComponentError(c.key, c.install(ctx, server, s.To, s.Reporter)); err != nil {
			return err
		}
	}
	return nil
}

// coreComponents is the upgrade order, and it is dependency order rather than
// the install order: the Gateway API CRDs move before the controller that
// serves them, and cert-manager before anything that asks it for a
// certificate.
func coreComponents() []coreComponent {
	return []coreComponent{
		{"gateway-api", gatewayapi.Install},
		{"traefik", traefik.Install},
		{"cert-manager", certmanager.Install},
		{"openebs-lvm-localpv", storage.Install},
		{"velero", backup.Install},
		{"system-upgrade-controller", day2.InstallUpgradeController},
		{"kured", day2.InstallKured},
	}
}

// stageProfiles moves each enabled profile's components.
//
// A cluster's profile set does not change during an upgrade — adding or
// removing one is a separate operation — so this moves what is already there
// and nothing else.
func stageProfiles(ctx context.Context, s *Session) error {
	installed := s.installedProfiles()
	var components []string
	for _, name := range installed {
		if name == "ha" {
			continue // a topology, not components
		}
		components = append(components, name)
	}
	if len(components) == 0 {
		s.Logf("  core only, no component profiles installed")
		return nil
	}
	return fmt.Errorf("this cluster has profile(s) %s installed, and this build of the CLI cannot move them yet — the component profiles land after core (kn-sev5, kn-ynaq, kn-54ni). Upgrading core alone would leave the cluster not matching its own record, so it is refused rather than done partially",
		strings.Join(components, ", "))
}

// stageAgent moves the KubeNest agent.
//
// The agent is upgraded with the identity it already holds: an upgrade does
// not re-mint credentials, because rotating a live cluster's identity is a
// separate operation with its own consequences and no part of moving a
// version forward.
func stageAgent(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	from, _ := s.From.Core.Version("kubenest-agent")
	to, err := s.To.Core.Version("kubenest-agent")
	if err != nil {
		return err
	}
	if from == to {
		s.Logf("  kubenest-agent unchanged at %s", to)
		return nil
	}
	s.Logf("  kubenest-agent %s → %s", from, to)
	return stages.NewComponentError("kubenest-agent", upgradeAgentChart(ctx, server, s.To, to, s.Reporter))
}

// stageRecord updates the cluster's recorded bundle version.
//
// It runs last, after verify, so a cluster is never recorded as successfully
// upgraded before the checks that say it was have passed.
func stageRecord(ctx context.Context, s *Session) error {
	if s.API == nil || s.Jnl.ClusterID == "" {
		return fmt.Errorf("no registered cluster to record against")
	}
	profiles := s.installedProfiles()
	if profiles == nil {
		profiles = []string{}
	}
	return s.API.PutBundleRecord(ctx, s.Jnl.ClusterID, api.BundleRecord{
		BundleVersion:        s.Opts.To,
		Profiles:             profiles,
		HATier:               s.haTier(),
		VolumeGroupOwnership: s.volumeGroupOwnership(),
		InstallJournal:       terminalEntries(s.Jnl),
	})
}

// terminalEntries is the journal in the control plane's shape. Only terminal
// transitions are persisted server-side.
func terminalEntries(j *stages.Journal) []api.InstallJournalEntry {
	var out []api.InstallJournalEntry
	for _, e := range j.Entries {
		if e.Status == stages.StatusStarted {
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
		if e.Status == stages.StatusFailed {
			entry.ReasonCode = stages.ReasonCode(e.Stage)
		}
		out = append(out, entry)
	}
	return out
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
