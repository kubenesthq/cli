package backup

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/sshx"
)

// The namespaces a backup covers come from its expected-coverage record, the
// only thing that can say what the backup holds. A backup with no record
// covers nothing a recovery could restore.
func TestCoveredNamespacesReadsTheBackupsRecord(t *testing.T) {
	record := `{"backup":"manual-1","recorded_by":"cli","namespaces":[{"name":"shop","uid":"u2","volumes":[]},{"name":"app","uid":"u1","volumes":[]}]}`
	doc, err := json.Marshal(map[string]any{"data": map[string]string{"coverage.json": record}})
	if err != nil {
		t.Fatal(err)
	}
	runner := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.Contains(command, "get configmap "+CoverageRecordName("manual-1")):
			return sshx.Result{Stdout: string(doc)}, nil
		case strings.Contains(command, "get configmap "+CoverageRecordName("manual-2")):
			return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): configmaps "` + CoverageRecordName("manual-2") + `" not found`}, nil
		}
		return sshx.Result{ExitCode: 1, Stderr: "unexpected: " + command}, nil
	}}
	got, err := CoveredNamespaces(context.Background(), runner, "manual-1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "app,shop" {
		t.Errorf("manual-1's record covers app and shop, got %v", got)
	}
	none, err := CoveredNamespaces(context.Background(), runner, "manual-2")
	if err != nil {
		t.Fatalf("a backup with no record is not an error, it covers nothing: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("a backup with no record was said to cover %v", none)
	}
}
