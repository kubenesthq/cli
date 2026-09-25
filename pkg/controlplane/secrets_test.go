package controlplane

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/sshx"
)

// Commands EnsureSecrets runs. Named here so a test that stops seeing one of
// them fails on the comparison rather than silently scripting nothing.
const (
	readSecretCmd   = "sudo -n k3s kubectl get secret " + SecretName + " -n " + Namespace + " -o json"
	createSecretCmd = "sudo -n k3s kubectl create -f -"
)

// fakeRunner answers scripted commands and fails on any other, so every
// remote call EnsureSecrets makes is visible to the test that scripts it.
func fakeRunner(t *testing.T, answers map[string]sshx.Result) *componenttest.FakeRunner {
	t.Helper()
	r := &componenttest.FakeRunner{}
	r.Respond = func(command string) (sshx.Result, error) {
		res, ok := answers[command]
		if !ok {
			t.Fatalf("unscripted command: %q", command)
		}
		return res, nil
	}
	return r
}

// kubectlSecretJSON renders the answer `kubectl get secret -o json` gives for
// the install Secret, from the values it holds.
func kubectlSecretJSON(t *testing.T, sec Secrets) string {
	t.Helper()
	doc, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"data": map[string]string{
			keyJWTSecret:            base64.StdEncoding.EncodeToString([]byte(sec.JWTSecret)),
			keyAgentJWTSecret:       base64.StdEncoding.EncodeToString([]byte(sec.AgentJWTSecret)),
			keyEncryptionKey:        base64.StdEncoding.EncodeToString([]byte(sec.EncryptionKey)),
			keyPostgresPassword:     base64.StdEncoding.EncodeToString([]byte(sec.PostgresPassword)),
			keyAdminPassword:        base64.StdEncoding.EncodeToString([]byte(sec.AdminPassword)),
			keyGatewayCACertificate: base64.StdEncoding.EncodeToString([]byte(sec.GatewayCACertificate)),
			keyGatewayCAPrivateKey:  base64.StdEncoding.EncodeToString([]byte(sec.GatewayCAPrivateKey)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(doc)
}

// clusterWithoutSecret is a cluster whose install Secret is not there yet: a
// create stores the document the CLI streamed, and every read answers that
// document back — which is what the next run of the install sees.
func clusterWithoutSecret(t *testing.T) (*componenttest.FakeRunner, func() Secrets) {
	t.Helper()
	var stored []byte
	var created bool
	var r *componenttest.FakeRunner
	r = &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch command {
		case readSecretCmd:
			if !created {
				return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): secrets "kubenest-cp-install" not found`}, nil
			}
			// Read back from what was actually created, so a create that
			// streamed the wrong document fails the test that scripts it.
			return sshx.Result{Stdout: kubectlSecretJSON(t, secretManifestValues(t, stored))}, nil
		case createSecretCmd:
			inputs := r.Inputs()
			if len(inputs) == 0 {
				t.Fatal("the Secret was created without streaming a document")
			}
			stored = inputs[len(inputs)-1]
			created = true
			return sshx.Result{}, nil
		default:
			t.Fatalf("unscripted command: %q", command)
		}
		return sshx.Result{}, nil
	}}
	return r, func() Secrets { return secretManifestValues(t, stored) }
}

// secretManifestValues reads the values out of a streamed Secret document,
// which is what the cluster would store from it.
func secretManifestValues(t *testing.T, doc []byte) Secrets {
	t.Helper()
	var manifest struct {
		Metadata struct {
			Name      string            `yaml:"name"`
			Namespace string            `yaml:"namespace"`
			Labels    map[string]string `yaml:"labels"`
		} `yaml:"metadata"`
		StringData map[string]string `yaml:"stringData"`
	}
	if err := yaml.Unmarshal(doc, &manifest); err != nil {
		t.Fatalf("the streamed Secret is not valid YAML: %v", err)
	}
	if manifest.Metadata.Name != SecretName || manifest.Metadata.Namespace != Namespace {
		t.Errorf("streamed Secret is %s/%s, want %s/%s", manifest.Metadata.Namespace, manifest.Metadata.Name, Namespace, SecretName)
	}
	if got := manifest.Metadata.Labels["app.kubernetes.io/managed-by"]; got != "kubenest-cli" {
		t.Errorf("streamed Secret is labelled managed-by %q, want kubenest-cli", got)
	}
	return Secrets{
		JWTSecret:            manifest.StringData[keyJWTSecret],
		AgentJWTSecret:       manifest.StringData[keyAgentJWTSecret],
		EncryptionKey:        manifest.StringData[keyEncryptionKey],
		PostgresPassword:     manifest.StringData[keyPostgresPassword],
		AdminPassword:        manifest.StringData[keyAdminPassword],
		GatewayCACertificate: manifest.StringData[keyGatewayCACertificate],
		GatewayCAPrivateKey:  manifest.StringData[keyGatewayCAPrivateKey],
	}
}

// The values document is the whole interface to the chart: the operator's two
// settings, the generated secrets, and the control plane's own CA. A key that
// sneaks in here silently changes what a customer's cluster runs.
func TestValuesCarriesExactlyTheInstallInputs(t *testing.T) {
	settings := Settings{Domain: "kn.example.com", AdminEmail: "admin@kn.example.com"}
	sec := Secrets{
		JWTSecret:            "jwt",
		AgentJWTSecret:       "agent",
		EncryptionKey:        "enc",
		PostgresPassword:     "pg",
		AdminPassword:        "adm",
		GatewayCACertificate: "ca-pem",
		GatewayCAPrivateKey:  "ca-key-pem",
	}

	out, err := Values(settings, sec)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("Values is not valid YAML: %v", err)
	}
	if got := sortedKeys(doc); !equalStrings(got, []string{"agentJwtSecret", "backend", "checkpoint", "domain", "encryptionKey", "gatewayCA", "jwtSecret", "postgresql"}) {
		t.Errorf("top-level keys = %v, want exactly agentJwtSecret, backend, checkpoint, domain, encryptionKey, gatewayCA, jwtSecret, postgresql", got)
	}
	if got := doc["domain"]; got != settings.Domain {
		t.Errorf("domain = %v, want %q", got, settings.Domain)
	}
	if got := doc["jwtSecret"]; got != sec.JWTSecret {
		t.Errorf("jwtSecret = %v, want %q", got, sec.JWTSecret)
	}
	// The hub's key is a value of its own: a chart that read jwtSecret here
	// would give the hub the key that signs user sessions.
	if got := doc["agentJwtSecret"]; got != sec.AgentJWTSecret {
		t.Errorf("agentJwtSecret = %v, want %q", got, sec.AgentJWTSecret)
	}
	if got := doc["encryptionKey"]; got != sec.EncryptionKey {
		t.Errorf("encryptionKey = %v, want %q", got, sec.EncryptionKey)
	}
	// The control plane's CA, both halves: the chart's Issuer signs from it,
	// and the kit carries the key.
	gatewayCA := section(t, doc, "gatewayCA")
	if got := sortedKeys(gatewayCA); !equalStrings(got, []string{"certificate", "privateKey"}) {
		t.Errorf("gatewayCA keys = %v, want exactly certificate and privateKey", got)
	}
	if got := gatewayCA["certificate"]; got != sec.GatewayCACertificate {
		t.Errorf("gatewayCA.certificate = %v, want %q", got, sec.GatewayCACertificate)
	}
	if got := gatewayCA["privateKey"]; got != sec.GatewayCAPrivateKey {
		t.Errorf("gatewayCA.privateKey = %v, want %q", got, sec.GatewayCAPrivateKey)
	}
	// The checkpoint CronJob is explicitly OFF. The chart turns it on by
	// default and refuses to render without a tools image pinned by digest —
	// a value no source on this path records — and a value helm cannot render
	// fails the WHOLE release, so an install would not happen at all.
	checkpoint := section(t, doc, "checkpoint")
	if got := sortedKeys(checkpoint); !equalStrings(got, []string{"enabled"}) {
		t.Errorf("checkpoint keys = %v, want exactly enabled", got)
	}
	if checkpoint["enabled"] != false {
		t.Errorf("checkpoint.enabled = %v, want false: the chart cannot render its checkpoint CronJob without pins this installer does not have", checkpoint["enabled"])
	}
	postgres := section(t, doc, "postgresql")
	if got := sortedKeys(postgres); !equalStrings(got, []string{"auth"}) {
		t.Errorf("postgresql keys = %v, want exactly auth (everything else is the chart's default)", got)
	}
	auth := section(t, postgres, "auth")
	if got := sortedKeys(auth); !equalStrings(got, []string{"password"}) {
		t.Errorf("postgresql.auth keys = %v, want exactly password", got)
	}
	if got := auth["password"]; got != sec.PostgresPassword {
		t.Errorf("postgresql.auth.password = %v, want %q", got, sec.PostgresPassword)
	}
	backend := section(t, doc, "backend")
	if got := sortedKeys(backend); !equalStrings(got, []string{"admin"}) {
		t.Errorf("backend keys = %v, want exactly admin", got)
	}
	admin := section(t, backend, "admin")
	if got := sortedKeys(admin); !equalStrings(got, []string{"email", "password"}) {
		t.Errorf("backend.admin keys = %v, want exactly email and password", got)
	}
	if got := admin["email"]; got != settings.AdminEmail {
		t.Errorf("backend.admin.email = %v, want %q", got, settings.AdminEmail)
	}
	if got := admin["password"]; got != sec.AdminPassword {
		t.Errorf("backend.admin.password = %v, want %q", got, sec.AdminPassword)
	}
}

// The values are random, so a password that YAML gives meaning to is a matter
// of when rather than whether. Marshalling must carry it through unchanged:
// string concatenation would silently alter the chart's values instead.
func TestValuesSurvivesYAMLSignificantCharacters(t *testing.T) {
	hostile := `p@ss: word "quoted" #1` + "\n" + `- dash | pipe > fold & anchor * alias !tag %pct {brace} [bracket] 	tab`
	settings := Settings{Domain: "kn.example.com", AdminEmail: "admin@kn.example.com"}
	sec := Secrets{
		JWTSecret:            hostile,
		AgentJWTSecret:       hostile,
		EncryptionKey:        hostile,
		PostgresPassword:     hostile,
		AdminPassword:        hostile,
		GatewayCACertificate: hostile,
		GatewayCAPrivateKey:  hostile,
	}

	out, err := Values(settings, sec)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("Values is not valid YAML: %v", err)
	}
	if got := doc["jwtSecret"]; got != hostile {
		t.Errorf("jwtSecret round-tripped as %q, want %q", got, hostile)
	}
	if got := doc["agentJwtSecret"]; got != hostile {
		t.Errorf("agentJwtSecret round-tripped as %q, want %q", got, hostile)
	}
	if got := doc["encryptionKey"]; got != hostile {
		t.Errorf("encryptionKey round-tripped as %q, want %q", got, hostile)
	}
	// A CA is multi-line PEM, so this pair is the one that most needs the
	// marshaller rather than concatenation.
	gatewayCA := section(t, doc, "gatewayCA")
	if got := gatewayCA["certificate"]; got != hostile {
		t.Errorf("gatewayCA.certificate round-tripped as %q, want %q", got, hostile)
	}
	if got := gatewayCA["privateKey"]; got != hostile {
		t.Errorf("gatewayCA.privateKey round-tripped as %q, want %q", got, hostile)
	}
	if got := section(t, section(t, doc, "postgresql"), "auth")["password"]; got != hostile {
		t.Errorf("postgresql.auth.password round-tripped as %q, want %q", got, hostile)
	}
	if got := section(t, section(t, doc, "backend"), "admin")["password"]; got != hostile {
		t.Errorf("backend.admin.password round-tripped as %q, want %q", got, hostile)
	}
}

func TestValuesRejectsAnIncompleteSelection(t *testing.T) {
	sec := Secrets{JWTSecret: "jwt", EncryptionKey: "enc", PostgresPassword: "pg", AdminPassword: "adm"}
	for _, tc := range []struct {
		name     string
		settings Settings
		want     string
	}{
		{"no domain", Settings{AdminEmail: "admin@kn.example.com"}, "domain"},
		{"no admin email", Settings{Domain: "kn.example.com"}, "email"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Values(tc.settings, sec)
			if err == nil {
				t.Fatalf("Values accepted %+v and produced %q", tc.settings, out)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q must name what is missing (%q)", err, tc.want)
			}
		})
	}
}

// The first run of an install generates the secrets, writes them to the
// cluster, and reports created=true so the caller prints the administrator
// password the one and only time it can.
func TestEnsureSecretsGeneratesAndCreatesWhenAbsent(t *testing.T) {
	ctx := context.Background()
	r, stored := clusterWithoutSecret(t)

	sec, createdFlag, err := EnsureSecrets(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !createdFlag {
		t.Error("created = false on a cluster with no Secret: the caller would never show the administrator password")
	}

	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(sec.JWTSecret) {
		t.Errorf("jwtSecret %q is not 64 hex characters", sec.JWTSecret)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(sec.PostgresPassword) {
		t.Errorf("postgres-password %q is not 32 hex characters", sec.PostgresPassword)
	}
	if len(sec.AdminPassword) != 32 || strings.Trim(sec.AdminPassword, passwordAlphabet) != "" {
		t.Errorf("admin-password %q is not 32 characters from [A-Za-z0-9]", sec.AdminPassword)
	}
	// Fernet: urlsafe base64 of exactly 32 bytes, padding included.
	key, err := base64.URLEncoding.DecodeString(sec.EncryptionKey)
	if err != nil {
		t.Fatalf("encryptionKey %q is not urlsafe base64: %v", sec.EncryptionKey, err)
	}
	if len(key) != 32 {
		t.Errorf("encryptionKey decodes to %d bytes, want 32 (cryptography.fernet)", len(key))
	}
	if strings.ContainsAny(sec.EncryptionKey, "+/") || !strings.HasSuffix(sec.EncryptionKey, "=") {
		t.Errorf("encryptionKey %q is not URL-safe base64 with padding", sec.EncryptionKey)
	}

	// What EnsureSecrets returns is what the cluster stores.
	if got := stored(); got != sec {
		t.Errorf("returned %+v, but the created Secret holds %+v", sec, got)
	}

	// The secrets never reach a command line: a command string is the argv of
	// the shell on the target host, visible in `ps auxww` to every local user.
	for _, command := range r.Commands() {
		for _, value := range []string{
			sec.JWTSecret, sec.AgentJWTSecret, sec.EncryptionKey, sec.PostgresPassword, sec.AdminPassword,
			sec.GatewayCACertificate, sec.GatewayCAPrivateKey,
		} {
			if strings.Contains(command, value) {
				t.Errorf("command %q carries a secret value", command)
			}
			if strings.Contains(command, base64.StdEncoding.EncodeToString([]byte(value))) {
				t.Errorf("command %q carries a base64-encoded secret value", command)
			}
		}
	}
}

// A second run must reuse the Secret it finds: regenerating would invalidate
// every token the running control plane has issued and orphan the PostgreSQL
// data behind the old password.
func TestEnsureSecretsReturnsTheStoredSecretsAndCreatesNothing(t *testing.T) {
	want := Secrets{
		JWTSecret: "aa", AgentJWTSecret: "ab", EncryptionKey: "bb",
		PostgresPassword: "cc", AdminPassword: "dd",
		GatewayCACertificate: "ca", GatewayCAPrivateKey: "ca-key",
	}
	r := fakeRunner(t, map[string]sshx.Result{readSecretCmd: {Stdout: kubectlSecretJSON(t, want)}})

	got, created, err := EnsureSecrets(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("created = true with the Secret already present: the caller would print an administrator password that is not the one in use")
	}
	if got != want {
		t.Errorf("secrets = %+v, want the stored %+v", got, want)
	}
	if commands := r.Commands(); len(commands) != 1 || commands[0] != readSecretCmd {
		t.Errorf("commands = %q, want only the read", commands)
	}
}

// An incomplete Secret is refused rather than silently half-regenerated: the
// values in it are already in use by whatever control plane is running.
func TestEnsureSecretsRefusesAnIncompleteSecret(t *testing.T) {
	complete := Secrets{
		JWTSecret: "jwt", AgentJWTSecret: "agent", EncryptionKey: "enc",
		PostgresPassword: "pg", AdminPassword: "adm",
		GatewayCACertificate: "ca", GatewayCAPrivateKey: "ca-key",
	}
	broken := func(t *testing.T, mutate func(data map[string]string)) string {
		t.Helper()
		var doc struct {
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal([]byte(kubectlSecretJSON(t, complete)), &doc); err != nil {
			t.Fatal(err)
		}
		mutate(doc.Data)
		body, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	for _, tc := range []struct {
		name   string
		key    string
		mutate func(data map[string]string)
	}{
		{"key missing", keyAdminPassword, func(data map[string]string) { delete(data, keyAdminPassword) }},
		{"key empty", keyAdminPassword, func(data map[string]string) { data[keyAdminPassword] = "" }},
		{"key not base64", keyAdminPassword, func(data map[string]string) { data[keyAdminPassword] = "not base64!" }},
		// The agent signing key and both halves of the CA are as load-bearing
		// as the rest: the hub refuses to start without the first, and the
		// chart's Issuer cannot sign without the others.
		{"agent signing key missing", keyAgentJWTSecret, func(data map[string]string) { delete(data, keyAgentJWTSecret) }},
		{"CA certificate missing", keyGatewayCACertificate, func(data map[string]string) { delete(data, keyGatewayCACertificate) }},
		{"CA private key empty", keyGatewayCAPrivateKey, func(data map[string]string) { data[keyGatewayCAPrivateKey] = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fakeRunner(t, map[string]sshx.Result{readSecretCmd: {Stdout: broken(t, tc.mutate)}})
			_, _, err := EnsureSecrets(context.Background(), r)
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("error = %v, want one naming the unusable %q key", err, tc.key)
			}
			for _, command := range r.Commands() {
				if command == createSecretCmd {
					t.Error("an incomplete Secret was overwritten")
				}
			}
		})
	}
}

// The control plane's OWN certificate authority is minted once — by the first
// install — and every later run keeps the one it finds.
//
// Every CLI pins it (Config.ControlPlaneCA) and every agent mounts it
// (kubenest-platform-ca), so a re-run that minted a new authority would leave
// a whole fleet trusting certificates the control plane no longer serves under:
// the exact failure this CA exists to end.
func TestEnsureSecretsMintsTheControlPlaneCAOnceAndKeepsIt(t *testing.T) {
	ctx := context.Background()
	r, _ := clusterWithoutSecret(t)

	first, created, err := EnsureSecrets(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("the first run did not create the install Secret")
	}

	certificate, key := parseControlPlaneCA(t, first.GatewayCACertificate, first.GatewayCAPrivateKey)
	if !certificate.IsCA {
		t.Error("the minted certificate is not a CA: cert-manager's Issuer would refuse it, and no CLI or agent could verify anything it signed")
	}
	if certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Errorf("the CA's key usage is %v, which does not include certificate signing", certificate.KeyUsage)
	}
	if !key.PublicKey.Equal(certificate.PublicKey) {
		t.Error("the CA private key does not belong to the CA certificate: the chart would render a key pair cert-manager rejects, and a restore could not renew the control plane's serving certificate")
	}
	if certificate.NotBefore.After(time.Now()) {
		t.Errorf("the CA is not valid until %s, so a verifier a second behind this machine refuses the control plane it just installed", certificate.NotBefore)
	}
	if remaining := time.Until(certificate.NotAfter); remaining < 9*365*24*time.Hour {
		t.Errorf("the CA expires in %s: every CLI and agent pins it, so its expiry is the expiry of the fleet's trust and can only be fixed by re-pinning all of them", remaining)
	}
	// The certificate alone is not a re-issuable authority, and the kit
	// carries both.
	if bundle := first.CABundle(); !strings.Contains(bundle, first.GatewayCACertificate) || !strings.Contains(bundle, first.GatewayCAPrivateKey) {
		t.Error("Secrets.CABundle does not carry the certificate and its key: the control-plane kit would hold an authority that cannot renew anything")
	}

	// A second run — the resume of an install that failed after this stage —
	// reads the same values back and creates nothing.
	second, createdAgain, err := EnsureSecrets(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if createdAgain {
		t.Error("the second run created the Secret again: the running control plane's tokens and its pinned CA would both change under it")
	}
	if second != first {
		t.Errorf("the second run's secrets differ from the first run's:\n first  %+v\n second %+v", first, second)
	}
	if second.GatewayCACertificate != first.GatewayCACertificate || second.GatewayCAPrivateKey != first.GatewayCAPrivateKey {
		t.Error("the control plane's CA changed between runs")
	}
}

// The agent signing key is a second, independent secret: drawn in the same
// generate-and-keep way as the rest, and never the key that signs user
// sessions.
//
// THE SPLIT IS THE POINT (kn-t47). The hub holds this key, and the hub must be
// rebuildable from the control-plane kit's contents alone — so the key the kit
// carries has to be the key the hub verifies with, while SECRET_KEY must never
// leave the backend.
func TestAgentJWTSecretIsStableAndIsNotTheUserKey(t *testing.T) {
	ctx := context.Background()
	r, _ := clusterWithoutSecret(t)

	sec, _, err := EnsureSecrets(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(sec.AgentJWTSecret) {
		t.Errorf("agentJwtSecret %q is not 64 hex characters", sec.AgentJWTSecret)
	}
	if sec.AgentJWTSecret == sec.JWTSecret {
		t.Error("the agent signing key is the user session key: a token minted for an agent would be a user session, and the hub would have to be given the key that signs them")
	}

	// Stable across runs, like every other value this install keeps.
	again, createdAgain, err := EnsureSecrets(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if createdAgain {
		t.Error("the second run created the Secret again")
	}
	if again.AgentJWTSecret != sec.AgentJWTSecret {
		t.Errorf("agentJwtSecret = %q on the second run, %q on the first: existing agent tokens would stop verifying", again.AgentJWTSecret, sec.AgentJWTSecret)
	}

	// And it reaches the chart under its own name, not under jwtSecret.
	values, err := Values(Settings{Domain: "kn.example.com", AdminEmail: "admin@kn.example.com"}, sec)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(values), &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc["agentJwtSecret"]; got != sec.AgentJWTSecret {
		t.Errorf("agentJwtSecret = %v, want the generated %q", got, sec.AgentJWTSecret)
	}
	if got := doc["jwtSecret"]; got != sec.JWTSecret {
		t.Errorf("jwtSecret = %v, want the user session key", got)
	}
}

// parseControlPlaneCA decodes the minted CA, and fails the test rather than
// returning anything half-checked.
func parseControlPlaneCA(t *testing.T, certificatePEM, keyPEM string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	block, rest := pem.Decode([]byte(certificatePEM))
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("the CA certificate is not a PEM CERTIFICATE block:\n%s", certificatePEM)
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		t.Errorf("the CA certificate carries more than one block: %q", rest)
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("the CA certificate does not parse: %v", err)
	}
	keyBlock, _ := pem.Decode([]byte(keyPEM))
	if keyBlock == nil {
		t.Fatalf("the CA private key is not a PEM block:\n%s", keyPEM)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatalf("the CA private key does not parse as an EC key: %v", err)
	}
	return certificate, key
}

func section(t *testing.T, doc map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := doc[key]
	if !ok {
		t.Fatalf("key %q is missing", key)
	}
	nested, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("key %q is %T, want a map", key, value)
	}
	return nested
}

func sortedKeys(doc map[string]any) []string {
	out := make([]string, 0, len(doc))
	for key := range doc {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func equalStrings(got, want []string) bool {
	return len(got) == len(want) && bytes.Equal([]byte(strings.Join(got, "\x00")), []byte(strings.Join(want, "\x00")))
}
