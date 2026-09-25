package controlplane

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/k3s"
)

const (
	// ReleaseName is the helm release name, and therefore the name of the
	// k3s HelmChart resource and the prefix of every object the chart
	// creates: kubenest-cp-backend, kubenest-cp-hub, kubenest-cp-ui,
	// kubenest-cp-postgresql, kubenest-cp-redis.
	ReleaseName = "kubenest-cp"
	// Namespace is where the control plane runs, and the namespace the
	// chart's Gateway and HTTPRoutes point at.
	Namespace = "kubenest-system"
	// SecretName is the Secret holding this install's generated secrets.
	//
	// It is NOT a chart object and is never managed by helm: the values built
	// by this package carry the secrets into the HelmChart, and if helm owned
	// the Secret a `helm uninstall` — or a failed upgrade rolling back —
	// would delete the JWT secret, the credential-encryption key and the
	// administrator password of a control plane whose PostgreSQL data
	// survived. Creating it here also means a re-run of the install reads the
	// first run's secrets back instead of generating new ones.
	SecretName = "kubenest-cp-install"
)

// Keys of the four values inside SecretName. The backend and the chart read
// the first two by name, so they are part of the install contract.
const (
	keyJWTSecret        = "jwt-secret"
	keyEncryptionKey    = "encryption-key"
	keyPostgresPassword = "postgres-password"
	keyAdminPassword    = "admin-password"
)

// Settings is what the operator decided: the domain the control plane's
// hostnames hang off, and the administrator the backend creates on first
// boot. Everything else the chart defaults.
type Settings struct {
	Domain     string
	AdminEmail string
}

// Secrets is what this install generated once and must keep: the random
// values the backend cannot re-derive from anywhere. They are named for the
// chart values they fill.
type Secrets struct {
	// JWTSecret signs the backend's tokens (chart value jwtSecret).
	JWTSecret string
	// EncryptionKey encrypts stored cloud credentials at rest. A Fernet key,
	// because the backend decrypts with cryptography.fernet.
	EncryptionKey string
	// PostgresPassword is the password of the chart's PostgreSQL user.
	PostgresPassword string
	// AdminPassword is the initial administrator's password.
	AdminPassword string
}

// errNoSecret reports that the install Secret is not on the cluster. Every
// other error out of readSecrets means the object WAS found and cannot be
// used as it stands.
var errNoSecret = errors.New("the install Secret is not on this cluster yet")

// EnsureSecrets returns the install's secrets, generating them on the first
// run and reading them back on every subsequent one.
//
// created reports whether this call generated them: the caller shows the
// administrator password exactly once, when it did, and never again —
// afterwards the cluster Secret is the only copy.
//
// Creation is `kubectl create`, not `apply`, and that is load-bearing.
// Generate-then-apply means two concurrent installs of the same cluster both
// generate, both write, and the loser's HelmChart — already rendered with its
// own JWT secret — is what lands: the backend would refuse tokens signed by
// the Secret that survived. Create fails on the second writer instead, before
// anything has been rendered.
func EnsureSecrets(ctx context.Context, r k3s.Runner) (sec Secrets, created bool, err error) {
	stored, err := readSecrets(ctx, r)
	switch {
	case err == nil:
		return stored, false, nil
	case !errors.Is(err, errNoSecret):
		// The Secret is there and unusable — a missing or unparsable key. It
		// is not regenerated: whatever control plane is running behind those
		// values cannot re-derive them either.
		return Secrets{}, false, err
	}
	// Either the Secret is not there (the first run) or it could not be read.
	// An unreadable Secret is not overwritten blindly: the create below fails
	// on an object that already exists, so a transient read failure ends as a
	// failed install, never as a lost encryption key with a live database
	// behind it.
	generated, err := generateSecrets()
	if err != nil {
		return Secrets{}, false, err
	}
	manifest, err := secretManifest(generated)
	if err != nil {
		return Secrets{}, false, err
	}
	// The document travels over stdin and never in the command string: it IS
	// the secrets, and a command string is the argv of the shell on the
	// target host (pkg/k3s documents the same rule for manifests).
	res, err := r.RunInput(ctx, "sudo -n k3s kubectl create -f -", bytes.NewReader(manifest))
	if err != nil {
		return Secrets{}, false, fmt.Errorf("creating secret %s/%s: %w", Namespace, SecretName, err)
	}
	if res.ExitCode != 0 {
		return Secrets{}, false, fmt.Errorf("creating secret %s/%s: exit %d: %s (a control plane already installed on this cluster keeps its own secrets: remove the Secret to regenerate them, which also invalidates the running control plane's tokens)",
			Namespace, SecretName, res.ExitCode, firstLine(res.Stderr))
	}
	// Read back rather than return the generated values, so what this install
	// renders into the chart is what the cluster actually stores.
	stored, err = readSecrets(ctx, r)
	if err != nil {
		return Secrets{}, false, err
	}
	return stored, true, nil
}

// readSecrets reads the four keys of the install Secret. errNoSecret means it
// is not there; any other error means it is there and unusable.
func readSecrets(ctx context.Context, r k3s.Runner) (Secrets, error) {
	out, err := k3s.Kubectl(ctx, r, "get secret "+SecretName+" -n "+Namespace+" -o json")
	if err != nil {
		// A read failure cannot be told apart from an absent Secret here, and
		// it does not need to be: the create that follows this answer fails on
		// an object that already exists.
		return Secrets{}, fmt.Errorf("%w: %v", errNoSecret, err)
	}
	var stored struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &stored); err != nil {
		return Secrets{}, fmt.Errorf("secret %s/%s is unparsable: %w", Namespace, SecretName, err)
	}
	sec := Secrets{}
	for _, key := range []struct {
		name string
		into *string
	}{
		{keyJWTSecret, &sec.JWTSecret},
		{keyEncryptionKey, &sec.EncryptionKey},
		{keyPostgresPassword, &sec.PostgresPassword},
		{keyAdminPassword, &sec.AdminPassword},
	} {
		encoded, ok := stored.Data[key.name]
		if !ok {
			return Secrets{}, fmt.Errorf("secret %s/%s has no %q key: restore it or delete the Secret so this install generates a complete set",
				Namespace, SecretName, key.name)
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return Secrets{}, fmt.Errorf("secret %s/%s key %q is not base64: %w", Namespace, SecretName, key.name, err)
		}
		// An empty value is as unusable as a missing one, and is what a Secret
		// created by hand usually looks like. The chart requires a non-empty
		// JWT secret, encryption key and administrator password, so an install
		// rendered from this would fail at helm with nothing naming the cause.
		if len(raw) == 0 {
			return Secrets{}, fmt.Errorf("secret %s/%s has an empty %q key: restore it or delete the Secret so this install generates a complete set",
				Namespace, SecretName, key.name)
		}
		*key.into = string(raw)
	}
	return sec, nil
}

// secretManifest renders the install Secret. stringData keeps the values out
// of the document as base64 noise and lets the API server do the encoding.
func secretManifest(sec Secrets) ([]byte, error) {
	doc := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      SecretName,
			"namespace": Namespace,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": "kubenest-cli",
			},
		},
		"type": "Opaque",
		"stringData": map[string]any{
			keyJWTSecret:        sec.JWTSecret,
			keyEncryptionKey:    sec.EncryptionKey,
			keyPostgresPassword: sec.PostgresPassword,
			keyAdminPassword:    sec.AdminPassword,
		},
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("rendering secret %s/%s: %w", Namespace, SecretName, err)
	}
	return out, nil
}

// InstanceName is the Secret holding the INSTANCE's own identity: its
// immutable id, and the PUBLIC recipient of the fleet recovery key.
//
// It is separate from SecretName on purpose. SecretName holds credentials the
// chart renders (a JWT secret, an encryption key, a password); this holds what
// the instance IS. It is not a chart value, it is never rendered into
// anything, and it must outlive a control-plane upgrade and travel inside a
// datastore snapshot, because it is what every kit and every recovery set is
// bound to.
const InstanceName = "kubenest-cp-instance"

// Keys of InstanceName.
const (
	keyInstanceID     = "instance-id"
	keyFleetRecipient = "fleet-recipient"
)

// Instance is the instance's own identity: the immutable id kits and recovery
// sets are bound to, and the fleet recipient every later install encrypts to.
type Instance struct {
	// ID is the instance's immutable id. It is minted once, at the first
	// control-plane install, and never changes: a kit or a recovery set that
	// named an id the instance no longer has would be refused by the very
	// check that exists to tell right from wrong.
	ID string
	// FleetRecipient is the public age recipient of the fleet recovery key.
	// It is not a secret — it can only encrypt.
	FleetRecipient string
}

// ErrNoInstance reports that the instance identity is not on this cluster
// yet. It is exported because the caller that has just generated a fleet key
// must be able to tell "nothing recorded yet" (generate one) from "recorded
// and unreadable" (stop).
var ErrNoInstance = errors.New("this instance has no recorded identity yet")

// EnsureInstance returns the instance's identity, recording it on the first
// control-plane install.
//
// recipient is the public half of the fleet key THIS install generated. It is
// required to create the record and is never replaced once stored: kits are
// sealed to the recorded recipient, so accepting a different one would leave
// every kit ever written unopenable by the key the operator holds. A later run
// that presents a different recipient is refused, which is the only honest
// answer — the alternative is silently re-pointing the fleet at a key whose
// private half may not exist.
//
// On a later run recipient may be empty: the CLI holds only the recipient it
// read back (from this cluster or from its own config), and there is nothing
// to create.
func EnsureInstance(ctx context.Context, r k3s.Runner, recipient string) (inst Instance, created bool, err error) {
	stored, err := readInstance(ctx, r)
	switch {
	case err == nil:
		if recipient != "" && recipient != stored.FleetRecipient {
			return Instance{}, false, fmt.Errorf(
				"this instance already has a fleet recovery recipient (%s); the recipient offered (%s) is a different key, and every kit in the fleet is sealed to the recorded one. Refusing: overwriting it would leave those kits openable by nothing",
				stored.FleetRecipient, recipient)
		}
		return stored, false, nil
	case !errors.Is(err, ErrNoInstance):
		return Instance{}, false, err
	}
	if recipient == "" {
		return Instance{}, false, fmt.Errorf("%w: a first control-plane install must record the fleet recovery recipient it just generated; without one no kit could ever be written, and an instance that has never had a fleet key cannot be given one retroactively (F19: 1.1 clusters are not backfilled)", ErrNoInstance)
	}
	id, err := randomHex(16)
	if err != nil {
		return Instance{}, false, err
	}
	manifest, err := instanceManifest(Instance{ID: id, FleetRecipient: recipient})
	if err != nil {
		return Instance{}, false, err
	}
	// create, not apply, for the same reason EnsureSecrets uses create: two
	// concurrent first installs must not both mint an instance id, and the
	// loser must fail rather than overwrite the winner's.
	res, err := r.RunInput(ctx, "sudo -n k3s kubectl create -f -", bytes.NewReader(manifest))
	if err != nil {
		return Instance{}, false, fmt.Errorf("recording the instance identity: %w", err)
	}
	if res.ExitCode != 0 {
		return Instance{}, false, fmt.Errorf("recording the instance identity: exit %d: %s (an instance already recorded on this cluster keeps its own id)",
			res.ExitCode, firstLine(res.Stderr))
	}
	stored, err = readInstance(ctx, r)
	if err != nil {
		return Instance{}, false, err
	}
	return stored, true, nil
}

// ReadInstance reads the instance identity, and reports a clear error when
// this cluster has none.
func ReadInstance(ctx context.Context, r k3s.Runner) (Instance, error) { return readInstance(ctx, r) }

func readInstance(ctx context.Context, r k3s.Runner) (Instance, error) {
	out, err := k3s.Kubectl(ctx, r, "get secret "+InstanceName+" -n "+Namespace+" -o json")
	if err != nil {
		// A read failure cannot be told from an absent Secret here, and it
		// does not need to be: the create that follows one fails on an object
		// that already exists.
		return Instance{}, fmt.Errorf("%w: %v", ErrNoInstance, err)
	}
	var stored struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &stored); err != nil {
		return Instance{}, fmt.Errorf("secret %s/%s is unparsable: %w", Namespace, InstanceName, err)
	}
	inst := Instance{}
	for _, key := range []struct {
		name string
		into *string
	}{
		{keyInstanceID, &inst.ID},
		{keyFleetRecipient, &inst.FleetRecipient},
	} {
		encoded, ok := stored.Data[key.name]
		if !ok {
			return Instance{}, fmt.Errorf("secret %s/%s has no %q key: this instance's identity is incomplete, and an id or recipient invented to fill the gap would be a different instance",
				Namespace, InstanceName, key.name)
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return Instance{}, fmt.Errorf("secret %s/%s key %q is not base64: %w", Namespace, InstanceName, key.name, err)
		}
		if len(raw) == 0 {
			return Instance{}, fmt.Errorf("secret %s/%s has an empty %q key", Namespace, InstanceName, key.name)
		}
		*key.into = string(raw)
	}
	return inst, nil
}

// instanceManifest renders the instance identity Secret. stringData keeps the
// values out of the document as base64 noise.
func instanceManifest(inst Instance) ([]byte, error) {
	doc := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      InstanceName,
			"namespace": Namespace,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": "kubenest-cli",
			},
		},
		"type": "Opaque",
		"stringData": map[string]any{
			keyInstanceID:     inst.ID,
			keyFleetRecipient: inst.FleetRecipient,
		},
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("rendering secret %s/%s: %w", Namespace, InstanceName, err)
	}
	return out, nil
}

// Values renders the helm values document the control plane is installed
// with: the operator's settings and this install's generated secrets, and
// nothing else. The Gateway, the hub's public URL (wss://hub.<domain>) and
// the CRUD admin API are chart defaults — the values document says so by not
// mentioning them.
func Values(s Settings, sec Secrets) (string, error) {
	if s.Domain == "" {
		return "", fmt.Errorf("the control plane needs a domain: its console is app.<domain>, its API api.<domain> and its hub hub.<domain>")
	}
	if s.AdminEmail == "" {
		return "", fmt.Errorf("the control plane needs an administrator email: the backend creates that user on first boot")
	}
	// Marshalled, never concatenated: the generated passwords are random and
	// may hold any character YAML gives meaning to, and one that did would
	// otherwise silently change the chart's values instead of failing.
	doc := map[string]any{
		"domain":        s.Domain,
		"jwtSecret":     sec.JWTSecret,
		"encryptionKey": sec.EncryptionKey,
		"postgresql": map[string]any{
			"auth": map[string]any{"password": sec.PostgresPassword},
		},
		"backend": map[string]any{
			"admin": map[string]any{
				"email":    s.AdminEmail,
				"password": sec.AdminPassword,
			},
		},
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("rendering the control-plane values: %w", err)
	}
	return string(out), nil
}

// generateSecrets draws one fresh set from the system CSPRNG. Each format is
// the format its consumer needs, and each is deliberately free of characters
// that a shell, a YAML scalar, an HTML form post or a terminal would mangle:
// the administrator password is typed by a human out of the install log.
func generateSecrets() (Secrets, error) {
	jwtSecret, err := randomHex(32)
	if err != nil {
		return Secrets{}, err
	}
	encryptionKey, err := fernetKey()
	if err != nil {
		return Secrets{}, err
	}
	postgresPassword, err := randomHex(16)
	if err != nil {
		return Secrets{}, err
	}
	adminPassword, err := randomPassword(32)
	if err != nil {
		return Secrets{}, err
	}
	return Secrets{
		JWTSecret:        jwtSecret,
		EncryptionKey:    encryptionKey,
		PostgresPassword: postgresPassword,
		AdminPassword:    adminPassword,
	}, nil
}

// randomHex returns n random bytes as 2n lowercase hex characters.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating a secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// fernetKey returns a Fernet key: 32 random bytes, URL-safe base64, with the
// padding Fernet's parser requires. cryptography.fernet in the backend
// decrypts what this key encrypted.
func fernetKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating the credential-encryption key: %w", err)
	}
	return base64.URLEncoding.EncodeToString(buf), nil
}

// passwordAlphabet is the character set for generated passwords that a human
// has to retype from a terminal, and that also travels through YAML, form
// posts and log lines.
const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// randomPassword returns n characters drawn uniformly from passwordAlphabet.
//
// Bytes at or above the largest multiple of the alphabet length are
// discarded rather than folded, so every character is equally likely: a
// modulo over the whole byte range would make the first eight letters of the
// alphabet ~1.5% more likely than the rest.
func randomPassword(n int) (string, error) {
	const limit = 256 - (256 % len(passwordAlphabet))
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generating the administrator password: %w", err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, passwordAlphabet[int(b)%len(passwordAlphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

// firstLine trims a command's stderr to the line that names what happened.
func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}
