// Package install is the thirteen-stage platform installer (kn-7k8).
//
// The sequencing machinery — the ordered stages, the journal, resume, and
// what a failure says — lives in pkg/stages and is shared with the bundle
// upgrade (kn-fuo). There is one staging engine, one journal format and one
// notion of a converged stage in this CLI, deliberately: an operator should
// not have to learn which of two they are looking at.
//
// What lives HERE is what is specific to building a cluster from nothing:
// the thirteen stage names, what each one does, the eleven preflight checks,
// the five acceptance checks, and uninstall.
package install

import (
	"kubenest.io/cli/pkg/stages"
)

// The engine's vocabulary, re-exported so this package reads as one thing and
// callers do not have to know where the machinery lives.
type (
	// Stage is one of the thirteen.
	Stage = stages.Stage
	// Status is started, completed or failed.
	Status = stages.Status
	// Entry is one journalled transition.
	Entry = stages.Entry
	// Journal is the durable record of this install.
	Journal = stages.Journal
	// Identity is what a resume must match.
	Identity = stages.Identity
	// Event is one transition on the wire.
	Event = stages.Event
	// Emitter publishes transitions.
	Emitter = stages.Emitter
	// Emitters fans out to several.
	Emitters = stages.Emitters
	// TextEmitter prints transitions for a human.
	TextEmitter = stages.TextEmitter
	// NopEmitter drops them.
	NopEmitter = stages.NopEmitter
	// ControlPlaneEmitter publishes to the control plane.
	ControlPlaneEmitter = stages.ControlPlaneEmitter
	// Result is what a completed run reports.
	Result = stages.Result
	// StageError is a failed stage: which stage, which component, what next.
	StageError = stages.StageError
)

const (
	StatusStarted   = stages.StatusStarted
	StatusCompleted = stages.StatusCompleted
	StatusFailed    = stages.StatusFailed
)

// Kind names this operation in the journal, so an install journal and an
// upgrade journal can never be opened as each other.
const Kind = "install"

var (
	// Execute runs the stages in order. See pkg/stages.
	Execute = stages.Execute
	// OpenJournal loads or starts this cluster's install journal.
	OpenJournal = stages.OpenJournal
	// ReadJournal loads one without an identity to check against.
	ReadJournal = stages.ReadJournal
	// JournalDir is where journals live.
	JournalDir = stages.JournalDir
	// NewRunID mints one process's run id.
	NewRunID = stages.NewRunID
	// Sanitize strips credential-shaped runs from user-facing text.
	Sanitize = stages.Sanitize
	// ReasonCode derives the failure taxonomy code from a stage name.
	ReasonCode = stages.ReasonCode
	// NewComponentError tags an error with the component that produced it.
	NewComponentError = stages.NewComponentError
	// ComponentOf reads that tag back.
	ComponentOf = stages.ComponentOf
	// NewControlPlaneEmitter builds the queueing control-plane emitter.
	NewControlPlaneEmitter = stages.NewControlPlaneEmitter
)

// JournalPath is where this cluster's INSTALL journal lives.
func JournalPath(cluster string) (string, error) {
	return stages.JournalPath(Kind, cluster)
}

// The thirteen stage names, exactly as install.mdx names them. They are the
// journal's vocabulary and the wire's payload.stage (kn-w051) — one
// vocabulary, two views — so renaming one is a coordinated change.
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

// StageNames is the order. It is not arbitrary — every stage depends on the
// ones above it.
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

// exits are the two supported ways on from a failed install, printed by every
// failure message (install.mdx, "When it fails").
var exits = []string{
	"resume     fix what the error names, then run the identical command again\n             (completed stages are skipped)",
	"start over kubenest platform uninstall --confirm",
}

// taggedComponents is every manifest key the install plan can attach to a
// failure. Kept beside the call sites it mirrors so a test can assert each is
// a key the shipped bundle actually pins — a typo here would be a wrong
// record discovered on a customer's cluster rather than a failing test.
var taggedComponents = []string{
	"k3s",
	"gateway-api",
	"traefik",
	"cert-manager",
	"openebs-lvm-localpv",
	"velero",
	"system-upgrade-controller",
	"kured",
	"kubenest-agent",
}

// TaggedComponents returns the component keys the install plan can report.
func TaggedComponents() []string {
	return append([]string(nil), taggedComponents...)
}
