package operation

import "testing"

// The verb a record's resume command names is what an operator types, and a
// wrong one is worse than none: `kubenest platform restore` is the DATASTORE
// restore, a whole-cluster disaster operation, so a namespace restore whose
// record named it would send the operator to replace cluster state instead of
// continuing the restore they asked for (T4.1, plan 7.5).
func TestRestoreKindsResumeWithTheBackupRestoreVerb(t *testing.T) {
	for _, kind := range []Kind{KindRestoreNamespace, KindRestoreVolume} {
		record := Record{OperationID: "abcdef01", Request: Request{Kind: kind, Cluster: "prod-1"}}
		got := record.ResumeCommand()
		want := "kubenest backup restore --resume abcdef01"
		if got != want {
			t.Errorf("kind %s resumes with %q, want %q", kind, got, want)
		}
		if ResumableKind(kind) != true {
			t.Errorf("kind %s is not reported as resumable", kind)
		}
	}
}

// ResumableKind is a local helper: a kind whose verb this build knows.
func ResumableKind(k Kind) bool { return k.resumeVerb() != "" }
