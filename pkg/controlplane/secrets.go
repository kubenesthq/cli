package controlplane

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"

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

// Keys of the values inside SecretName. The backend and the chart read them by
// name — jwt-secret and agent-jwt-secret through templates/secret.yaml, the
// postgres password through the chart's own credential, the two gateway-CA
// keys as gatewayCA.certificate/privateKey — so they are part of the install
// contract.
const (
	keyJWTSecret        = "jwt-secret"
	keyAgentJWTSecret   = "agent-jwt-secret"
	keyEncryptionKey    = "encryption-key"
	keyPostgresPassword = "postgres-password"
	keyAdminPassword    = "admin-password"
	// The control plane's certificate authority, certificate and private key.
	// The key is a SECRET and the certificate is not, but they are one
	// identity: a restore that fetched only the certificate could not renew
	// the control plane's serving certificate, and every CLI and agent pins
	// this CA.
	keyGatewayCACertificate = "gateway-ca-certificate"
	keyGatewayCAPrivateKey  = "gateway-ca-private-key"
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
	// JWTSecret signs the backend's USER SESSIONS (chart value jwtSecret). It
	// is the one key of this set that never leaves the backend and never
	// travels in a recovery kit.
	JWTSecret string
	// AgentJWTSecret signs every agent token and the backend's own
	// `client_type: backend` token to the hub (chart value agentJwtSecret).
	//
	// A SEPARATE KEY ON PURPOSE (kn-t47). The hub verifies agent tokens and
	// must be rebuildable from the control-plane kit's contents alone, so the
	// key it holds has to be the key the kit carries — and a leaked session
	// key must not be able to mint an agent token, nor the reverse.
	AgentJWTSecret string
	// EncryptionKey encrypts stored cloud credentials at rest. A Fernet key,
	// because the backend decrypts with cryptography.fernet.
	EncryptionKey string
	// PostgresPassword is the password of the chart's PostgreSQL user.
	PostgresPassword string
	// AdminPassword is the initial administrator's password.
	AdminPassword string
	// GatewayCACertificate is the PEM certificate of the control plane's OWN
	// certificate authority, and GatewayCAPrivateKey is its PEM private key
	// (chart value gatewayCA.certificate/privateKey).
	//
	// The CLI mints this CA at install because the control plane used to take
	// its TLS identity from the host cluster's CA, which every CLI and agent
	// pins: a control plane restored or moved onto another cluster was then
	// rejected as an unknown authority, and no restore could be trusted. The
	// key travels in the control-plane kit, so the identity survives one.
	GatewayCACertificate string
	GatewayCAPrivateKey  string
}

// CABundle is the CA's certificate and its private key as one PEM stream.
//
// THIS IS WHAT THE CONTROL-PLANE KIT CARRIES under
// recoverykit.KeyControlPlaneCA. The kit's vocabulary has one entry for the
// control plane's authority, and an authority without its key cannot renew the
// certificate every CLI and agent pinned — which is the one thing the kit
// exists to make possible after a restore.
func (s Secrets) CABundle() string {
	return s.GatewayCACertificate + s.GatewayCAPrivateKey
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

// readSecrets reads the keys of the install Secret. errNoSecret means it is
// not there; any other error means it is there and unusable.
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
		{keyAgentJWTSecret, &sec.AgentJWTSecret},
		{keyEncryptionKey, &sec.EncryptionKey},
		{keyPostgresPassword, &sec.PostgresPassword},
		{keyAdminPassword, &sec.AdminPassword},
		{keyGatewayCACertificate, &sec.GatewayCACertificate},
		{keyGatewayCAPrivateKey, &sec.GatewayCAPrivateKey},
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
			keyJWTSecret:            sec.JWTSecret,
			keyAgentJWTSecret:       sec.AgentJWTSecret,
			keyEncryptionKey:        sec.EncryptionKey,
			keyPostgresPassword:     sec.PostgresPassword,
			keyAdminPassword:        sec.AdminPassword,
			keyGatewayCACertificate: sec.GatewayCACertificate,
			keyGatewayCAPrivateKey:  sec.GatewayCAPrivateKey,
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
		"domain":         s.Domain,
		"jwtSecret":      sec.JWTSecret,
		"agentJwtSecret": sec.AgentJWTSecret,
		"encryptionKey":  sec.EncryptionKey,
		// The control plane's own CA. The chart refuses to render without it:
		// its Gateway certificate is signed by this Issuer, and every CLI and
		// agent pins this authority.
		"gatewayCA": map[string]any{
			"certificate": sec.GatewayCACertificate,
			"privateKey":  sec.GatewayCAPrivateKey,
		},
		"postgresql": map[string]any{
			"auth": map[string]any{"password": sec.PostgresPassword},
		},
		// THE CHECKPOINT CRONJOB IS RENDERED OFF, and this is a limitation of
		// the installer rather than a decision about recovery (kn-t47).
		//
		// The chart turns it on by default and refuses to render without two
		// values: the checkpoint tools image PINNED BY DIGEST
		// (checkpoint.tools.image.digest) and the fleet recipient
		// (checkpoint.recipient). The recipient is known here — it is the
		// public half of the fleet recovery key this install generated — but
		// the digest has NO source on this path: no constant, bundle manifest
		// or catalog entry in this wave carries the digest of
		// ghcr.io/kubenesthq/checkpoint-tools, and a digest invented here
		// would name an image nobody built. Helm refuses the whole release
		// over a value it cannot render, so leaving the group on would mean
		// the control plane does not install at all.
		//
		// Off, the install works and the omission is visible where it matters:
		// the management cluster's `backup` verdict reports that the control
		// plane has no checkpoint. Turning it on needs the digest to be
		// recorded somewhere the installer reads (the bundle catalog is the
		// natural home) and the checkpoint bucket and its separate principal
		// to be configured, which is the rest of the checkpoint wiring.
		"checkpoint": map[string]any{"enabled": false},
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
	// Drawn separately from jwtSecret, which is the point: the hub is given
	// this key and never the session key.
	agentJWTSecret, err := randomHex(32)
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
	caCertificate, caPrivateKey, err := generateControlPlaneCA()
	if err != nil {
		return Secrets{}, err
	}
	return Secrets{
		JWTSecret:            jwtSecret,
		AgentJWTSecret:       agentJWTSecret,
		EncryptionKey:        encryptionKey,
		PostgresPassword:     postgresPassword,
		AdminPassword:        adminPassword,
		GatewayCACertificate: caCertificate,
		GatewayCAPrivateKey:  caPrivateKey,
	}, nil
}

// The control plane's own certificate authority (kn-t47).
//
// It is an ECDSA P-256 self-signed CA, because a P-256 key and certificate are
// small enough to travel in a kit, a values document and a chart Secret without
// anyone thinking about size, and every consumer (cert-manager's CA issuer,
// Go's crypto/tls, OpenSSL) reads the format without a flag.
const (
	// controlPlaneCAName is the subject common name. It is what shows up in
	// `openssl s_client` on a control plane nobody can explain, so it names
	// the platform rather than the instance: the instance's identity is the
	// key, not the label.
	controlPlaneCAName = "kubenest-control-plane-ca"
	// controlPlaneCAValidity is deliberately longer than any cluster's life.
	// The CA is pinned by every CLI and every agent through
	// Config.ControlPlaneCA and the kubenest-platform-ca ConfigMap, so its
	// expiry is the expiry of the whole fleet's trust — and it can only be
	// replaced by re-pinning every one of them.
	controlPlaneCAValidity = 10 * 365 * 24 * time.Hour
)

// generateControlPlaneCA mints the control plane's own certificate authority:
// one self-signed CA certificate and the private key that signs with it, both
// PEM.
//
// The certificate is the CA — IsCA with CertSign — because cert-manager's
// Issuer reads this key pair and signs the Gateway's certificate with it. Its
// path length is zero: it signs the control plane's serving certificates and
// nothing below them.
func generateControlPlaneCA() (certificate, privateKey string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generating the control-plane CA key: %w", err)
	}
	// A 128-bit random serial. RFC 5280 requires a positive one, and a
	// collision is the one thing a serial must not have: a reissued
	// certificate would look like a different one signed by the same CA.
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", fmt.Errorf("generating the control-plane CA serial: %w", err)
	}
	serial.Add(serial, big.NewInt(1))

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   controlPlaneCAName,
			Organization: []string{"KubeNest"},
		},
		// Backdated: the CA is minted and used within the same install, and a
		// certificate whose NotBefore is a moment in the future is refused by
		// a verifier whose clock is a second behind this machine's.
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(controlPlaneCAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// Only end-entity certificates below this CA: an intermediate it
		// signed would be an authority nobody pinned.
		MaxPathLen:     0,
		MaxPathLenZero: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", "", fmt.Errorf("minting the control-plane CA certificate: %w", err)
	}
	// SEC1 rather than PKCS#8, which is the form the chart's sample values
	// show and the one `openssl ec` writes. crypto/tls and cert-manager both
	// read it.
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", fmt.Errorf("encoding the control-plane CA key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})), nil
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
