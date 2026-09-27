package backup

import (
	"context"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/sshx"
)

// veleroAPIServer answers the way the API server does: a `kubectl get` of a
// resource Velero defines returns its list, and any other resource name is
// refused with "the server doesn't have a resource type".
func veleroAPIServer(lists map[string]string) *componenttest.FakeRunner {
	return &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		for resource, body := range lists {
			if strings.Contains(command, " get "+resource+" ") {
				return sshx.Result{Stdout: body}, nil
			}
		}
		fields := strings.Fields(command)
		for i, f := range fields {
			if f == "get" && i+1 < len(fields) {
				return sshx.Result{ExitCode: 1, Stderr: `error: the server doesn't have a resource type "` + strings.SplitN(fields[i+1], ".", 2)[0] + `"`}, nil
			}
		}
		return sshx.Result{ExitCode: 1, Stderr: "unexpected command"}, nil
	}}
}

// On hardware (2026-09-27, lab w3) the volume restore stopped at "reading the
// BackupStorageLocations: ... the server doesn't have a resource type
// \"backupsstoragelocations\"": the read named a resource Velero does not
// define, and every test faked the reader above it, so none ran the command.
func TestStorageLocationsReadsVelerosBackupStorageLocations(t *testing.T) {
	runner := veleroAPIServer(map[string]string{
		"backupstoragelocations.velero.io": `{"items":[{"metadata":{"name":"default"},"status":{"phase":"Available"}}]}`,
	})
	locations, err := (&veleroBackups{runner: runner}).storageLocations(context.Background())
	if err != nil {
		t.Fatalf("reading the storage locations from a cluster that has them: %v", err)
	}
	if locations["default"] != "Available" {
		t.Errorf("locations = %v, want default Available", locations)
	}
}
