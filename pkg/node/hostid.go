// Package node owns the cluster's host inventory: which machines a cluster
// is, and what each one is for.
//
// It was created by kn-t50 (the inventory in the control-plane record) and is
// extended by the node verbs that read and write it (T5.2–T5.4). The
// knowledge used to live only in the install journal on the laptop that ran
// the install, which meant a second operator — or the same one after a lost
// laptop — could not run a node verb at all.
//
// What belongs here is the vocabulary shared by every writer of the
// inventory: the host ID, the roles and lifecycle states the contract
// defines, and the entry helper that renders one host into the record's
// shape. What does NOT belong here is anything about a specific verb: the
// lock, the plans, the SSH work.
package node

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"kubenest.io/cli/pkg/api"
)

// Role is what a host is for, in the record's vocabulary.
type Role string

const (
	// RoleServer is a k3s control-plane node.
	RoleServer Role = "server"
	// RoleAgent is a worker node.
	RoleAgent Role = "agent"
)

// LifecycleState is where a host is in its life in this cluster, in the
// record's vocabulary.
type LifecycleState string

const (
	// StateJoining covers a host whose join is still in progress — including
	// one whose inventory write was interrupted. That is why an entry exists
	// before the join finishes: an interrupted join must leave a host that is
	// identifiable rather than one that is invisible.
	StateJoining LifecycleState = "joining"
	// StateActive is a host that is fully in the cluster.
	StateActive LifecycleState = "active"
	// StateRemoving is a host an operation is taking out of the cluster.
	StateRemoving LifecycleState = "removing"
	// StateRemoved keeps the entry — and the host ID — as the record that the
	// host was here. The ID is never reused for another machine.
	StateRemoved LifecycleState = "removed"
)

// NewHostID mints the KubeNest host ID for one host as it enters a cluster's
// inventory.
//
// It is opaque and derived from NOTHING about the machine — no hostname, no
// address, no MAC, no Node UID — and that is the point: those all change. A
// host that is rebuilt, renamed or re-addressed is a NEW host with a new ID,
// and the ID the old one had is never handed out again, so an old operation's
// record can never be read as being about the new machine.
//
// crypto/rand, not math/rand or a counter: IDs are minted on different
// laptops with no shared state, and two operators adding a host at the same
// moment must not collide.
func NewHostID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice, and a host that cannot be
		// named must not be recorded under a guessed or reused ID.
		return "", fmt.Errorf("minting a host ID: %w", err)
	}
	return "h-" + hex.EncodeToString(b[:]), nil
}

// Entry is one host's inventory entry while it is being assembled, before it
// becomes the contract's shape (api.HostRecord).
//
// The ID is deliberately absent: it is minted by Record, at the one moment
// the host enters the inventory, so no caller can hold an entry that has no
// identity or hand the same ID to two hosts.
type Entry struct {
	// Role is required: it decides what an operation may do to the host.
	Role Role
	// SSHAddress, SSHPort and SSHUser are how the CLI reaches the host.
	SSHAddress string
	SSHPort    int
	SSHUser    string
	// HostKeyFingerprint is the SHA-256 of the host key seen when this host
	// was reached, in OpenSSH's SHA256:<base64> form.
	HostKeyFingerprint string
	// JoinAddress is the address the host joined the cluster through.
	JoinAddress string
	// NodeUID is the Node's metadata.uid, empty until the node exists.
	NodeUID string
	// StorageDevice is the stable /dev/disk/by-id/... path, empty for a host
	// with no KubeNest-managed volume group.
	StorageDevice string
	// VolumeGroupOwnership is "customer-created" or "installer-created".
	VolumeGroupOwnership string
	// LifecycleState defaults to joining: a host is written into the
	// inventory before its work is finished, never after.
	LifecycleState LifecycleState
}

// Record mints the host's ID and renders the entry in the record's shape.
//
// Call it once per host: the ID it mints is that host's identity for the rest
// of its life in this cluster, and a second call is a second host.
func (e Entry) Record() (api.HostRecord, error) {
	if e.Role != RoleServer && e.Role != RoleAgent {
		return api.HostRecord{}, fmt.Errorf("a host entry with no role cannot be recorded: %q is neither %q nor %q",
			string(e.Role), RoleServer, RoleAgent)
	}
	id, err := NewHostID()
	if err != nil {
		return api.HostRecord{}, err
	}
	state := e.LifecycleState
	if state == "" {
		state = StateJoining
	}
	return api.HostRecord{
		HostID:               id,
		NodeUID:              e.NodeUID,
		SSHAddress:           e.SSHAddress,
		SSHPort:              e.SSHPort,
		SSHUser:              e.SSHUser,
		HostKeyFingerprint:   e.HostKeyFingerprint,
		JoinAddress:          e.JoinAddress,
		Role:                 string(e.Role),
		StorageDevice:        e.StorageDevice,
		VolumeGroupOwnership: e.VolumeGroupOwnership,
		LifecycleState:       string(state),
	}, nil
}
