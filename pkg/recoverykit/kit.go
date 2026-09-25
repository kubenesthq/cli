// Package recoverykit is the fleet recovery key and the sealed kits it opens.
//
// A recovery kit is the small set of secrets that no cluster can re-derive
// after the laptop that installed it is gone: a cluster's Velero repository
// password and its k3s join token, or the control plane's credential
// encryption key, agent signing secret and CA. Nothing else in the platform
// holds them off the host that installed them, which is why losing that host
// today makes a cluster unrecoverable even though every byte of its data is in
// the bucket.
//
// ONE identity opens every kit in the fleet. The installer generates it at
// control-plane install, prints it exactly once and keeps it in process memory
// only: it is never written to a host, a journal, a log line or the install
// Record (pkg/install deliberately has no field a credential fits into). Only
// the PUBLIC half — the age recipient — is kept, in the control plane and in
// the operator's own config, so every later install encrypts without ever
// asking for the private key. The first control-plane install is the only
// place that can ever decrypt a kit it just wrote, which is exactly why
// `kubenest recovery-kit check` exists: a later operator must be able to prove
// the upload is intact without being able to open it.
//
// Sealing is age (filippo.io/age), one dependency, chosen because it is a
// file format and a small library rather than a service: the decryption key is
// a string a human can write down, and nothing here depends on a network or a
// KMS the customer may not have.
package recoverykit

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
)

// Kind is a kit's scope, which is an authority boundary, not a label: what a
// kit may carry and who may export it both follow from it.
type Kind string

const (
	// KindCluster is one workload cluster's kit. An organisation admin may
	// export it; it holds nothing that opens another cluster.
	KindCluster Kind = "cluster"
	// KindControlPlane is the instance's kit: the control plane's own key
	// material. Only the instance administrator may export it, and a cluster
	// kit never contains any of it — that separation is what keeps read access
	// to one cluster's backups from being read access to the control plane.
	KindControlPlane Kind = "control-plane"
)

// The names of the things a kit carries. They are the vocabulary the control
// plane records fingerprints under and the recovery-set manifest repeats, so
// they are a wire contract, not local labels.
const (
	KeyVeleroRepoPassword = "velero-repo-password"
	KeyK3sJoinToken       = "k3s-join-token"
	KeyEncryptionKey      = "ENCRYPTION_KEY"
	KeyAgentJWTSecret     = "AGENT_JWT_SECRET"
	KeyControlPlaneCA     = "CONTROL_PLANE_CA"
)

// The bucket's own credentials are deliberately NOT here. They are one of the
// things the customer keeps separately and offline (with the fleet key, SSH
// access to the hosts and DNS control), and a kit that carried them would put
// the key to the bucket inside the bucket. What a kit records is the
// NON-SECRET S3 location, so a recovery knows where to point the credentials
// the operator holds.

// ControlPlaneSecrets are the keys only a control-plane kit may carry. A
// cluster kit that carried one of these would be a cluster-admin credential
// that reaches the control plane, which is the whole thing the two scopes
// exist to prevent.
var ControlPlaneSecrets = []string{KeyEncryptionKey, KeyAgentJWTSecret, KeyControlPlaneCA}

// ClusterSecrets are the keys a cluster kit must carry. Its immutable IDs, S3
// location and repository identifier are in the header (and are not secrets);
// these two are the material a recovery cannot re-derive from anywhere.
var ClusterSecrets = []string{KeyVeleroRepoPassword, KeyK3sJoinToken}

// ErrDoesNotDecrypt is a kit that the fleet key supplied does not open. It is
// its own error so a check can report "does not decrypt" rather than
// mislabelling it as a set that is incomplete or an upload that is damaged:
// those are three different afternoons.
var ErrDoesNotDecrypt = errors.New("the kit does not decrypt with the fleet key supplied")

// ErrForeign is an artifact that belongs to a different instance,
// organisation or cluster. It is detected from the plaintext header, so it is
// refused before anything destructive is attempted — and the message names
// where the artifact does belong, because "wrong cluster" without the name is
// the actionable half missing.
var ErrForeign = errors.New("the artifact belongs to another instance, organisation or cluster")

// Binding is the immutable identity a kit or a recovery set is bound to. An
// instance is one KubeNest control plane; an organisation and a cluster are
// the ones the control plane recorded at register time.
type Binding struct {
	Kind           Kind   `json:"kind"`
	InstanceID     string `json:"instance_id"`
	OrganisationID string `json:"organisation_id"`
	ClusterID      string `json:"cluster_id"`
}

func (b Binding) String() string {
	return fmt.Sprintf("instance %q, organisation %q, cluster %q (%s)", b.InstanceID, b.OrganisationID, b.ClusterID, b.Kind)
}

func (b Binding) validate() error {
	switch b.Kind {
	case KindCluster:
		if b.InstanceID == "" || b.OrganisationID == "" || b.ClusterID == "" {
			return fmt.Errorf("a cluster kit is bound to a cluster: instance, organisation and cluster are all required (got %s)", b)
		}
	case KindControlPlane:
		if b.InstanceID == "" {
			return fmt.Errorf("a control-plane kit is an instance resource and needs the instance id (got %s)", b)
		}
		// ClusterID is OPTIONAL and means something different here: it names
		// the MANAGEMENT cluster, the one the control plane runs in. It is
		// recorded because the instance's control plane is not an abstraction
		// to recover — it is a cluster — and because it is what lets the
		// control plane tell which cluster is an instance resource rather than
		// an organisation's. A control-plane kit written by a control plane
		// that does not know which cluster hosts it simply omits it.
	default:
		return fmt.Errorf("%q is not a kit scope: it is %q (a workload cluster) or %q (the instance)", b.Kind, KindCluster, KindControlPlane)
	}
	return nil
}

// Location is the non-secret S3 coordinate a kit records, so a recovery knows
// where to look before it can decrypt anything. Credentials are never part of
// it: the kit's own content is what opens the store, and a pointer that
// carried credentials would be a credential in every backup of the bucket.
type Location struct {
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket"`
	Region   string `json:"region"`
	Prefix   string `json:"prefix,omitempty"`
}

// FleetKey is the fleet recovery identity: one age private key. It exists in
// process memory of the install that generated it and nowhere else.
type FleetKey struct {
	id *age.X25519Identity
}

// GenerateFleetKey draws one fresh identity from the system CSPRNG.
func GenerateFleetKey() (*FleetKey, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("generating the fleet recovery key: %w", err)
	}
	return &FleetKey{id: id}, nil
}

// ParseFleetKey reads back the string the install printed once.
func ParseFleetKey(s string) (*FleetKey, error) {
	id, err := age.ParseX25519Identity(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("this is not a fleet recovery key: it must be the age identity the control-plane install printed once (AGE-SECRET-KEY-1...): %w", err)
	}
	return &FleetKey{id: id}, nil
}

// SecretKeyString is the whole private key, as the install prints it exactly
// once. Losing every copy means no kit ever opens, which is why the install
// says so in the same breath.
func (k *FleetKey) SecretKeyString() string { return k.id.String() }

// Recipient is the public half. It is what every later install is handed, and
// what a workload cluster encrypts with without ever holding the private key.
func (k *FleetKey) Recipient() string { return k.id.Recipient().String() }

// Seal encrypts to this identity's own recipient. It is the first install's
// path; every later one uses Sealer, which has no private half.
func (k *FleetKey) Seal(plaintext []byte) ([]byte, error) {
	return SealTo(k.Recipient(), plaintext)
}

// Sealer encrypts to the fleet recipient and can never open what it writes.
// This is the object a later install holds — it satisfies pkg/operation's
// Sealer without that package ever seeing a private key.
type Sealer struct {
	Recipient string
}

// Seal encrypts plaintext to the recipient.
func (s Sealer) Seal(plaintext []byte) ([]byte, error) {
	return SealTo(s.Recipient, plaintext)
}

// SealTo encrypts to an age recipient. The plaintext is the caller's; nothing
// here copies it anywhere.
func SealTo(recipient string, plaintext []byte) ([]byte, error) {
	r, err := parseRecipient(recipient)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r)
	if err != nil {
		return nil, fmt.Errorf("sealing to the fleet recipient: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, fmt.Errorf("sealing to the fleet recipient: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("sealing to the fleet recipient: %w", err)
	}
	return buf.Bytes(), nil
}

// parseRecipient accepts the age recipient form only. An identity string in
// its place would be a private key handed to something that has no business
// holding one, so it is refused rather than coerced.
func parseRecipient(s string) (age.Recipient, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "AGE-SECRET-KEY-") {
		return nil, errors.New("that is an age PRIVATE key, not a recipient: only the public age1... recipient may leave the install that generated the fleet key")
	}
	r, err := age.ParseX25519Recipient(s)
	if err != nil {
		return nil, fmt.Errorf("%q is not a fleet recovery recipient (it must start with age1): %w", s, err)
	}
	return r, nil
}

// Open decrypts ciphertext with the fleet key the operator supplied. A key
// that does not open it is ErrDoesNotDecrypt, never a generic failure.
func Open(fleetKey string, ciphertext []byte) ([]byte, error) {
	id, err := age.ParseX25519Identity(strings.TrimSpace(fleetKey))
	if err != nil {
		return nil, fmt.Errorf("this is not a fleet recovery key: it must be the age identity the control-plane install printed once (AGE-SECRET-KEY-1...): %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(ciphertext), id)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDoesNotDecrypt, err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDoesNotDecrypt, err)
	}
	return plain, nil
}

// Fingerprint is a key's fingerprint: a domain-separated SHA-256 of its name
// and value.
//
// The control plane records fingerprints, never keys. A fingerprint is stable
// across installs (so a check can compare the same key twice) and is derived
// through a fixed prefix so a fingerprint of one value cannot be replayed as a
// fingerprint of another construction. It is not salted: a fingerprint that
// could not be recomputed after a disaster would be a record nobody could
// verify, which is the only thing it is for.
func Fingerprint(name, value string) string {
	h := sha256.New()
	h.Write([]byte("kubenest-recovery-kit-v1\x00"))
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(value))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Digest is the SHA-256 of a document exactly as it is stored. It is how a
// later install proves an upload is intact without holding any key: the same
// bytes in the bucket and on the laptop digest the same.
func Digest(doc []byte) string {
	sum := sha256.Sum256(doc)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Kit is one sealed recovery kit: a plaintext header a later install can
// verify with nothing but the bucket, and a sealed body only the fleet key
// opens.
//
// The header carries no secret: the binding, the S3 location, the Velero
// repository identifier and a fingerprint of each key. That split is what
// makes `recovery-kit check` possible at all on a laptop holding no private
// key.
type Kit struct {
	Binding            Binding           `json:"binding"`
	ArtifactID         string            `json:"artifact_id"`
	S3Location         Location          `json:"s3_location"`
	VeleroRepositoryID string            `json:"velero_repository_id,omitempty"`
	WrittenAt          time.Time         `json:"written_at"`
	Fingerprints       map[string]string `json:"fingerprints"`
	Sealed             []byte            `json:"sealed"`

	// secrets is the plaintext, in memory only. It is unexported so it cannot
	// be marshalled even by accident, and SealTo drops it: a sealed kit that
	// still held its own plaintext would be one reflection away from being a
	// kit whose whole purpose is that it is sealed.
	secrets map[string]string
}

// New builds an unsealed kit for one scope.
//
// It refuses, rather than trims, a kit that carries the wrong scope's keys: a
// cluster kit holding the control plane's encryption key would be a
// cluster-admin credential in a workload cluster's kit, and "warn and drop it"
// would leave the operator believing it was held somewhere.
func New(binding Binding, artifactID string, loc Location, veleroRepositoryID string, secrets map[string]string, now time.Time) (*Kit, error) {
	if err := binding.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(artifactID) == "" {
		return nil, errors.New("a kit needs an immutable artifact id: a recovery set references the kit by it, so a kit without one cannot be the thing a set means")
	}
	if strings.ContainsAny(artifactID, "/\\") {
		return nil, fmt.Errorf("artifact id %q may not contain a path separator: it names an object", artifactID)
	}
	want := ClusterSecrets
	forbidden := ControlPlaneSecrets
	if binding.Kind == KindControlPlane {
		want = ControlPlaneSecrets
		forbidden = ClusterSecrets
	}
	for _, name := range forbidden {
		if _, ok := secrets[name]; ok {
			return nil, fmt.Errorf("%s kit may not carry %q: %s", binding.Kind, name, forbidReason(name))
		}
	}
	for _, name := range want {
		if strings.TrimSpace(secrets[name]) == "" {
			return nil, fmt.Errorf("a %s kit must carry %q: without it a recovery cannot proceed", binding.Kind, name)
		}
	}
	fingerprints := make(map[string]string, len(secrets))
	for name, value := range secrets {
		fingerprints[name] = Fingerprint(name, value)
	}
	return &Kit{
		Binding:            binding,
		ArtifactID:         artifactID,
		S3Location:         loc,
		VeleroRepositoryID: veleroRepositoryID,
		WrittenAt:          now.UTC(),
		Fingerprints:       fingerprints,
		secrets:            copySecrets(secrets),
	}, nil
}

func forbidReason(name string) string {
	switch name {
	case KeyEncryptionKey, KeyAgentJWTSecret, KeyControlPlaneCA:
		return "it is control-plane material and belongs only in the control-plane kit, which only the instance administrator may export"
	default:
		return "it is cluster material and belongs only in a cluster kit"
	}
}

func copySecrets(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// SealTo encrypts the kit's secrets to the recipient and drops the plaintext.
// After it returns, the kit carries only the header and the ciphertext, which
// is the only shape that may be written to a disk or a bucket.
func (k *Kit) SealTo(recipient string) error {
	if len(k.secrets) == 0 {
		return errors.New("there is nothing to seal: this kit holds no secrets (it may already be sealed, or it was never built by New)")
	}
	plain, err := json.Marshal(k.secrets)
	if err != nil {
		return fmt.Errorf("encoding the kit's secrets: %w", err)
	}
	sealed, err := SealTo(recipient, plain)
	if err != nil {
		return err
	}
	k.Sealed = sealed
	k.secrets = nil
	return nil
}

// Secrets opens the kit with the fleet key and returns what it carries. The
// fingerprints are NOT re-checked here: a caller that wants to report them
// separately (the check does) must call FingerprintsMatch, so that "does not
// decrypt" and "fingerprints disagree" can never be reported as one thing.
func (k *Kit) Secrets(fleetKey string) (map[string]string, error) {
	if len(k.Sealed) == 0 {
		return nil, errors.New("this kit is not sealed: there is nothing in it to open")
	}
	plain, err := Open(fleetKey, k.Sealed)
	if err != nil {
		return nil, err
	}
	var secrets map[string]string
	if err := json.Unmarshal(plain, &secrets); err != nil {
		return nil, fmt.Errorf("the kit opened but its contents are not a key/value document: %w", err)
	}
	return secrets, nil
}

// FingerprintsMatch reports whether the opened secrets are the keys this
// kit's header was written for. It is a separate answer from decryption on
// purpose: a kit can open and still not be the kit the control plane recorded.
func (k *Kit) FingerprintsMatch(secrets map[string]string) error {
	for name, want := range k.Fingerprints {
		got := Fingerprint(name, secrets[name])
		if got != want {
			return fmt.Errorf("the fingerprint of %q in this kit (%s) is not the one its header was written for (%s): the kit and the record disagree", name, got, want)
		}
	}
	for name := range secrets {
		if _, ok := k.Fingerprints[name]; !ok {
			return fmt.Errorf("the kit opened but carries %q, which its header does not fingerprint", name)
		}
	}
	return nil
}

// KeyNames returns the names of the keys this kit records, sorted, for a
// report that has to name them without holding them.
func (k *Kit) KeyNames() []string {
	names := make([]string, 0, len(k.Fingerprints))
	for name := range k.Fingerprints {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Verify refuses a kit that does not belong where the caller is. The error
// names where it DOES belong, and wraps ErrForeign so a caller can tell a
// foreign artifact from an unreadable one.
func (k *Kit) Verify(want Binding) error {
	return verifyBinding("kit", k.Binding, want)
}

func verifyBinding(what string, got, want Binding) error {
	if got.Kind != want.Kind {
		return fmt.Errorf("%w: this %s is a %s artifact and this is a %s operation (%s vs %s)", ErrForeign, what, got.Kind, want.Kind, got, want)
	}
	if got.InstanceID != want.InstanceID || got.OrganisationID != want.OrganisationID || got.ClusterID != want.ClusterID {
		return fmt.Errorf("%w: this %s belongs to %s; this is %s", ErrForeign, what, got, want)
	}
	return nil
}

// Document is the kit exactly as it is stored: the header and the sealed
// body. Digest(Document()) is what an upload check compares.
func (k *Kit) Document() ([]byte, error) {
	doc, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding the recovery kit: %w", err)
	}
	return append(doc, '\n'), nil
}

// Load reads a stored kit document back.
func Load(doc []byte) (*Kit, error) {
	var k Kit
	if err := json.Unmarshal(doc, &k); err != nil {
		return nil, fmt.Errorf("this is not a recovery kit document: %w", err)
	}
	if err := k.Binding.validate(); err != nil {
		return nil, err
	}
	if k.ArtifactID == "" {
		return nil, errors.New("this recovery kit document has no artifact id, so nothing can reference it")
	}
	return &k, nil
}

// ArtifactID mints a fresh immutable artifact id: a kit's identity for the
// life of the fleet. A recovery set references a kit by this value, so a kit
// written later is a NEW artifact and can never change what an existing set
// means.
func ArtifactID(now time.Time) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("minting a kit artifact id: %w", err)
	}
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(buf[:]), nil
}
