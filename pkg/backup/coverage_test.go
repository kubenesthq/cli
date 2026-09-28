package backup

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/sshx"
)

// The namespaces a recovery can restore from one backup are the namespaces its
// expected-coverage record covers that ALSO have a Project in kubenest-system.
//
// A recovery restores a namespace by pausing its Project first (the namespace
// restore writes kubenest.io/reconcile-paused on Project <namespace> in
// kubenest-system), and a --replace restore of a namespace the install itself
// built would delete a namespace the recovery's own install just created. So a
// covered namespace with no Project is not one a recovery restores, and naming
// it in a recovery set fails the recovery on it: lab s6, 2026-09-28, the
// recovery's stage 16 recovery-restore could not annotate Project
// kubenest-system/cert-manager, because no such Project exists.
//
// PLANTED NEGATIVES: cert-manager is covered but has no Project, and ghost has
// a Project but the backup does not cover it. Neither may be named.
func TestRecoverableNamespacesNamesOnlyCoveredNamespacesWithAProject(t *testing.T) {
	record := `{"backup":"manual-1","recorded_by":"cli","namespaces":[` +
		`{"name":"cert-manager","uid":"u1","volumes":[]},` +
		`{"name":"openebs","uid":"u2","volumes":[]},` +
		`{"name":"s6-data","uid":"u3","volumes":[]},` +
		`{"name":"shop","uid":"u4","volumes":[]}]}`
	doc, err := json.Marshal(map[string]any{"data": map[string]string{"coverage.json": record}})
	if err != nil {
		t.Fatal(err)
	}
	projects := `{"items":[{"metadata":{"name":"ghost"}},{"metadata":{"name":"s6-data"}},{"metadata":{"name":"shop"}}]}`
	runner := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.Contains(command, "get configmap "+CoverageRecordName("manual-1")):
			return sshx.Result{Stdout: string(doc)}, nil
		case strings.Contains(command, "get projects -n "+ProjectCRNamespace):
			return sshx.Result{Stdout: projects}, nil
		case strings.Contains(command, "get configmap "+CoverageRecordName("manual-2")):
			return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): configmaps "` + CoverageRecordName("manual-2") + `" not found`}, nil
		}
		return sshx.Result{ExitCode: 1, Stderr: "unexpected: " + command}, nil
	}}

	got, err := RecoverableNamespaces(context.Background(), runner, "manual-1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Recorded {
		t.Error("manual-1 has an expected-coverage record, so Recorded must say so")
	}
	if strings.Join(got.Namespaces, ",") != "s6-data,shop" {
		t.Errorf("a recovery from manual-1 can restore s6-data and shop, got %v", got.Namespaces)
	}
	for _, refused := range []string{"cert-manager", "openebs", "ghost"} {
		if contains(got.Namespaces, refused) {
			t.Errorf("%s is not a namespace a recovery can restore from manual-1 and must not be named: %v", refused, got.Namespaces)
		}
	}

	// A backup with no expected-coverage record covers nothing a recovery
	// could restore, and that is not an error.
	none, err := RecoverableNamespaces(context.Background(), runner, "manual-2")
	if err != nil {
		t.Fatalf("a backup with no record is not an error, it covers nothing: %v", err)
	}
	if len(none.Namespaces) != 0 || none.Recorded {
		t.Errorf("a backup with no record was said to cover %v (recorded=%v)", none.Namespaces, none.Recorded)
	}
}

// A Projects listing that fails must be an error, never an empty answer. An
// empty answer here is read as "nothing a recovery can restore", so returning
// it when the truth is unknown would record a backup in the set as though it
// had nothing in it.
func TestRecoverableNamespacesFailsWhenTheProjectsListingFails(t *testing.T) {
	record := `{"backup":"manual-1","recorded_by":"cli","namespaces":[{"name":"shop","uid":"u4","volumes":[]}]}`
	doc, err := json.Marshal(map[string]any{"data": map[string]string{"coverage.json": record}})
	if err != nil {
		t.Fatal(err)
	}
	runner := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.Contains(command, "get configmap "+CoverageRecordName("manual-1")):
			return sshx.Result{Stdout: string(doc)}, nil
		case strings.Contains(command, "get projects -n "+ProjectCRNamespace):
			return sshx.Result{ExitCode: 1, Stderr: "Error from server: the API server is unreachable"}, nil
		}
		return sshx.Result{ExitCode: 1, Stderr: "unexpected: " + command}, nil
	}}
	if _, err := RecoverableNamespaces(context.Background(), runner, "manual-1"); err == nil {
		t.Error("a Projects listing that fails must not be read as 'nothing to restore'")
	}
}
