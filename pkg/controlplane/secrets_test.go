package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"

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
// the four keys, from the values they hold.
func kubectlSecretJSON(t *testing.T, sec Secrets) string {
	t.Helper()
	doc, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"data": map[string]string{
			keyJWTSecret:        base64.StdEncoding.EncodeToString([]byte(sec.JWTSecret)),
			keyEncryptionKey:    base64.StdEncoding.EncodeToString([]byte(sec.EncryptionKey)),
			keyPostgresPassword: base64.StdEncoding.EncodeToString([]byte(sec.PostgresPassword)),
			keyAdminPassword:    base64.StdEncoding.EncodeToString([]byte(sec.AdminPassword)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(doc)
}

// secretManifestValues reads the four values out of a streamed Secret
// document, which is what the cluster would store from it.
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
		JWTSecret:        manifest.StringData[keyJWTSecret],
		EncryptionKey:    manifest.StringData[keyEncryptionKey],
		PostgresPassword: manifest.StringData[keyPostgresPassword],
		AdminPassword:    manifest.StringData[keyAdminPassword],
	}
}

// The values document is the whole interface to the chart: exactly the
// operator's two settings and the four generated secrets. A key that sneaks
// in here silently changes what a customer's cluster runs.
func TestValuesCarriesExactlyTheInstallInputs(t *testing.T) {
	settings := Settings{Domain: "kn.example.com", AdminEmail: "admin@kn.example.com"}
	sec := Secrets{JWTSecret: "jwt", EncryptionKey: "enc", PostgresPassword: "pg", AdminPassword: "adm"}

	out, err := Values(settings, sec)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("Values is not valid YAML: %v", err)
	}
	if got := sortedKeys(doc); !equalStrings(got, []string{"backend", "domain", "encryptionKey", "jwtSecret", "postgresql"}) {
		t.Errorf("top-level keys = %v, want exactly backend, domain, encryptionKey, jwtSecret, postgresql", got)
	}
	if got := doc["domain"]; got != settings.Domain {
		t.Errorf("domain = %v, want %q", got, settings.Domain)
	}
	if got := doc["jwtSecret"]; got != sec.JWTSecret {
		t.Errorf("jwtSecret = %v, want %q", got, sec.JWTSecret)
	}
	if got := doc["encryptionKey"]; got != sec.EncryptionKey {
		t.Errorf("encryptionKey = %v, want %q", got, sec.EncryptionKey)
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
	sec := Secrets{JWTSecret: hostile, EncryptionKey: hostile, PostgresPassword: hostile, AdminPassword: hostile}

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
	if got := doc["encryptionKey"]; got != hostile {
		t.Errorf("encryptionKey round-tripped as %q, want %q", got, hostile)
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
			// streamed the wrong document fails this test.
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
	if got := secretManifestValues(t, stored); got != sec {
		t.Errorf("returned %+v, but the created Secret holds %+v", sec, got)
	}

	// The secrets never reach a command line: a command string is the argv of
	// the shell on the target host, visible in `ps auxww` to every local user.
	for _, command := range r.Commands() {
		for _, value := range []string{sec.JWTSecret, sec.EncryptionKey, sec.PostgresPassword, sec.AdminPassword} {
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
	want := Secrets{JWTSecret: "aa", EncryptionKey: "bb", PostgresPassword: "cc", AdminPassword: "dd"}
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
	complete := Secrets{JWTSecret: "jwt", EncryptionKey: "enc", PostgresPassword: "pg", AdminPassword: "adm"}
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
		mutate func(data map[string]string)
	}{
		{"key missing", func(data map[string]string) { delete(data, keyAdminPassword) }},
		{"key empty", func(data map[string]string) { data[keyAdminPassword] = "" }},
		{"key not base64", func(data map[string]string) { data[keyAdminPassword] = "not base64!" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fakeRunner(t, map[string]sshx.Result{readSecretCmd: {Stdout: broken(t, tc.mutate)}})
			_, _, err := EnsureSecrets(context.Background(), r)
			if err == nil || !strings.Contains(err.Error(), keyAdminPassword) {
				t.Fatalf("error = %v, want one naming the unusable %q key", err, keyAdminPassword)
			}
			for _, command := range r.Commands() {
				if command == createSecretCmd {
					t.Error("an incomplete Secret was overwritten")
				}
			}
		})
	}
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
