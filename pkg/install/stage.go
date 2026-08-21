// Package install is the thirteen-stage platform installer (kn-7k8).
//
// The engine owns exactly three things: the ORDER, the JOURNAL, and what a
// FAILURE says. It does not own what any stage does — the component
// installers are separate packages that predate it, and stage 2 belongs to
// the credential seat. That separation is deliberate: sequencing is the part
// that must be understandable in one file.
//
// Every stage names itself in any failure and emits a progress event, because
// "install failed" is worse than no installer (install.mdx, "When it fails").
//
// The three properties the engine exists to give:
//
//	Stage 1 writes nothing anywhere. Stage 2 writes only to the control
//	plane. The first change to a customer machine happens in stage 3 — so
//	abandoning an install before then costs nothing, which is what makes
//	preflight worth being thorough.
//
//	There is no automatic rollback. A failed stage stops the install and
//	leaves completed stages in place, because automatic teardown destroys
//	the evidence needed to diagnose the failure and can itself fail.
//
//	Resume is deterministic. It reads the journal rather than relying on
//	every component happening to be idempotent.
package install

import (
	"context"
	"strings"
)

// The thirteen stage names, exactly as install.mdx names them. These strings
// are the journal's vocabulary and the wire's payload.stage (kn-w051) — one
// vocabulary, two views. They are API: renaming one is a coordinated change.
const (
	StagePreflight  = "preflight"
	StageRegister   = "register"
	StageK3sServer  = "k3s-server"
	StageK3sAgents  = "k3s-agents"
	StageNetworking = "platform-networking"
	StageCerts      = "platform-certs"
	StageStorage    = "platform-storage"
	StageBackup     = "platform-backup"
	StageDay2       = "platform-day2"
	StageAgent      = "kubenest-agent"
	StageProfiles   = "profiles"
	StageRecord     = "record"
	StageVerify     = "verify"
)

// StageNames is the order. The order is not arbitrary — every stage depends
// on the ones above it.
var StageNames = []string{
	StagePreflight,
	StageRegister,
	StageK3sServer,
	StageK3sAgents,
	StageNetworking,
	StageCerts,
	StageStorage,
	StageBackup,
	StageDay2,
	StageAgent,
	StageProfiles,
	StageRecord,
	StageVerify,
}

// Stage is one step of the install.
type Stage struct {
	// Name is one of the thirteen constants above.
	Name string
	// Component is the bundle-manifest core key this stage installs, where
	// there is exactly one — traefik, openebs-lvm-localpv, velero… It is the
	// field a failure-injection run reads to learn WHICH component broke, so
	// it must match a manifest key, not a prose name. Empty for stages that
	// install no single component.
	Component string
	// AlwaysRun marks a stage the journal may never skip. Three qualify, for
	// three different reasons: preflight writes nothing and the hosts may
	// have drifted; register mints credentials that a fresh process cannot
	// recover, so it must re-run to have them; and a skipped verify is not
	// an install.
	AlwaysRun bool
	// Run does the work. A nil Run is a stage that is not wired yet and the
	// engine refuses to pretend otherwise.
	Run StageFunc
}

// StageFunc does one stage's work against the session.
type StageFunc func(ctx context.Context, s *Session) error

// ReasonCode is the machine-readable failure reason for a stage, matching the
// contract's ^[A-Z0-9_]+$ taxonomy: "platform-storage" -> PLATFORM_STORAGE_FAILED.
// Derived rather than enumerated so adding a stage cannot forget one.
func ReasonCode(stage string) string {
	return strings.ToUpper(strings.ReplaceAll(stage, "-", "_")) + "_FAILED"
}

// Index returns the 1-based position of a stage name, or 0 if unknown.
func Index(stage string) int {
	for i, name := range StageNames {
		if name == stage {
			return i + 1
		}
	}
	return 0
}
