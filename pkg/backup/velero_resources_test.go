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

// TestPodVolumeBackupsReadTheBackupsPodsAndVolumes reads mode 2's modifier
// input the way Velero writes it (1.18.1): spec.pod names the pod AT BACKUP
// TIME, spec.volume the volume within it, and the label velero.io/pvc-uid the
// claim behind that volume. Two things the fields decide:
//
//   - the velero namespace holds EVERY backup's PodVolumeBackups, so the reader
//     returns only the chosen backup's — by the owner reference, which a long
//     backup name cannot truncate the way the velero.io/backup-name label can;
//   - the claim link is the UID label, not the velero.io/pvc-name annotation
//     the hardware did not carry, so an object with the annotation and no UID
//     label names no claim and must not be read as one.
func TestPodVolumeBackupsReadTheBackupsPodsAndVolumes(t *testing.T) {
	runner := veleroAPIServer(map[string]string{
		"podvolumebackups.velero.io": `{"items":[` +
			`{"metadata":{"name":"daily-good-a","labels":{"velero.io/backup-name":"daily-good","velero.io/pvc-uid":"u-a"},` +
			`"ownerReferences":[{"apiVersion":"velero.io/v1","kind":"Backup","name":"daily-good"}]},` +
			`"spec":{"pod":{"name":"dead-b87c65446-d7vlj","namespace":"e2e-restore-volumes"},"volume":"a"}},` +
			`{"metadata":{"name":"daily-other-a","labels":{"velero.io/backup-name":"daily-other"},` +
			`"ownerReferences":[{"apiVersion":"velero.io/v1","kind":"Backup","name":"daily-other"}]},` +
			`"spec":{"pod":{"name":"other-1","namespace":"e2e-restore-volumes"},"volume":"a"}},` +
			`{"metadata":{"name":"daily-good-b","labels":{"velero.io/backup-name":"daily-good"},"annotations":{"velero.io/pvc-name":"b"},` +
			`"ownerReferences":[{"apiVersion":"velero.io/v1","kind":"Backup","name":"daily-good"}]},` +
			`"spec":{"pod":{"name":"dead-b87c65446-d7vlj","namespace":"e2e-restore-volumes"},"volume":"b"}}]}`,
	})
	volumes, err := NewK3sCluster(runner).PodVolumeBackups(context.Background(), "daily-good")
	if err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 2 {
		t.Fatalf("volumes = %+v, want only the chosen backup's two", volumes)
	}
	if volumes[0] != (BackupVolumeState{Pod: "dead-b87c65446-d7vlj", Namespace: "e2e-restore-volumes", Volume: "a", ClaimUID: "u-a"}) {
		t.Errorf("volumes[0] = %+v, want the backup's pod, its volume and the claim's UID", volumes[0])
	}
	if volumes[1].ClaimUID != "" {
		t.Errorf("volume b carries the pvc-name annotation and no velero.io/pvc-uid label, so it names no claim: got %+v", volumes[1])
	}
	if volumes[1].Pod != "dead-b87c65446-d7vlj" || volumes[1].Volume != "b" {
		t.Errorf("volumes[1] = %+v, want pod dead-b87c65446-d7vlj volume b", volumes[1])
	}
}
