// Package stages is the ONE staging engine: an ordered sequence of named
// stages, a durable journal, deterministic resume, and a failure that names
// what broke and what to do about it.
//
// It exists in its own package because there are two operations that need
// exactly this and must not each grow their own — the thirteen-stage
// installer (kn-7k8) and the bundle upgrade (kn-fuo). A second staging
// engine would mean a second journal format, a second resume rule and a
// second definition of a failed stage, and an operator would have to learn
// which one they were looking at.
//
// The engine owns exactly three things: the ORDER, the JOURNAL, and what a
// FAILURE says. It does not own what any stage does. That separation is
// deliberate: sequencing is the part that must be understandable in one file.
//
// Every stage names itself in any failure and emits a progress event, because
// "install failed" is worse than no installer (install.mdx, "When it fails").
//
// Two properties the engine gives both operations:
//
//	Nothing is automatically undone. A failed stage stops the sequence and
//	leaves completed stages in place, because automatic teardown destroys
//	the evidence needed to diagnose the failure and can itself fail. What
//	recovery means afterwards is the caller's to decide and to offer.
//
//	Resume is deterministic. It reads the journal rather than relying on
//	every step happening to be idempotent.
package stages

import (
	"context"
	"strings"
)

// Stage is one step of an operation.
type Stage struct {
	// Name is the stage's machine name, and it is API: it is what the
	// journal records and what install_journal_entry.stage carries, so the
	// caller's constants and the contract's enum have to agree.
	Name string
	// Component is the bundle-manifest core key this stage acts on, where
	// there is exactly one — traefik, openebs-lvm-localpv, velero… It is the
	// field a failure-injection run reads to learn WHICH component broke, so
	// it must match a manifest key, not a prose name. Empty for stages that
	// install no single component.
	Component string
	// AlwaysRun marks a stage the journal may never skip. Both operations
	// have some: a check that writes nothing and must see current reality,
	// a step whose output cannot be recovered from a previous process, and
	// a verification that would not be one if it were skipped.
	AlwaysRun bool
	// Run does the work. A nil Run is a stage that is not wired yet and the
	// engine refuses to pretend otherwise.
	Run StageFunc
}

// StageFunc does one stage's work. It is a closure over whatever session the
// operation needs, which is how one engine drives two very different
// sequences without knowing anything about either.
type StageFunc func(ctx context.Context) error

// ReasonCode is the machine-readable failure reason for a stage, matching the
// contract's ^[A-Z0-9_]+$ taxonomy: "platform-storage" -> PLATFORM_STORAGE_FAILED.
// Derived rather than enumerated so adding a stage cannot forget one.
func ReasonCode(stage string) string {
	return strings.ToUpper(strings.ReplaceAll(stage, "-", "_")) + "_FAILED"
}

// Index returns the 1-based position of a stage within a sequence, or 0 if it
// is not in it.
func Index(sequence []Stage, name string) int {
	for i, s := range sequence {
		if s.Name == name {
			return i + 1
		}
	}
	return 0
}
