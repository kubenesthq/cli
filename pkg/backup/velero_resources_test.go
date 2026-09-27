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

// On hardware (2026-09-27, S4 on lab w3) a restore whose PodVolumeRestore
// completed was refused as "no Completed PodVolumeRestore" for s4-data: Velero
// names the claim only by the label velero.io/pvc-uid, the reader looked for an
// annotation Velero does not write, and fell back to the pod's volume name
// ("data"), which is not the claim's.
func TestVolumeRestoresNameTheClaimFromItsUID(t *testing.T) {
	runner := veleroAPIServer(map[string]string{
		"podvolumerestores.velero.io": `{"items":[{"metadata":{"name":"r-s4qml","labels":{"velero.io/pvc-uid":"u-1","velero.io/restore-name":"r"}},` +
			`"spec":{"pod":{"name":"s4-web-1","namespace":"e2e"},"volume":"data"},"status":{"phase":"Completed"}}]}`,
		"persistentvolumeclaims": `{"items":[{"metadata":{"name":"s4-data","uid":"u-1"}},{"metadata":{"name":"other","uid":"u-2"}}]}`,
	})
	restores, err := NewK3sCluster(runner).VolumeRestores(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(restores) != 1 || restores[0].ClaimName != "s4-data" {
		t.Errorf("restores = %+v, want one naming claim s4-data", restores)
	}
}
