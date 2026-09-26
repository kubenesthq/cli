package backup

import (
	"context"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/sshx"
)

// A failed proof snapshot must report the line that says what went wrong, not
// just the first line of stderr. k3s writes a harmless warning first and the
// fatal error on a later line, so firstLine reported the one line that was not
// a failure: on hardware the install printed "Unknown flag
// --etcd-snapshot-schedule-cron found in config.yaml, skipping" as its reason
// for failing while the real error sat on the next line.
func TestProofSnapshotFailureNamesTheFatalLine(t *testing.T) {
	const (
		warning = "Unknown flag --etcd-snapshot-schedule-cron found in config.yaml, skipping"
		fatal   = `level=fatal msg="Error: see server log for details: failed to initialize S3 client: failed to test for existence of bucket kubenest-lab: Access Denied."`
	)
	cases := []struct {
		name     string
		stderr   string
		want     string
		unwanted string
	}{
		{
			name:     "the fatal line after a warning",
			stderr:   warning + "\n" + fatal + "\n",
			want:     fatal,
			unwanted: warning,
		},
		{
			// Output that does not use k3s's logger keeps its last non-empty
			// line: the end of a command's stderr is where the reason lands.
			name:     "the last non-empty line when nothing is marked fatal",
			stderr:   "pulling image quay.io/k3s/k3s:v1.31.1\nconnection refused\n\n",
			want:     "connection refused",
			unwanted: "pulling image",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &fakeRunner{
				// The k3s settings write compares the rendered file and answers
				// "unchanged", so no restart is needed and the run reaches the
				// proof snapshot.
				RespondInput: func(string, []byte) (sshx.Result, error) {
					return sshx.Result{Stdout: "unchanged"}, nil
				},
				Respond: func(command string) (sshx.Result, error) {
					if strings.Contains(command, "etcd-snapshot save") {
						return sshx.Result{ExitCode: 1, Stderr: c.stderr}, nil
					}
					return sshx.Result{}, nil
				},
			}
			err := ConfigureDatastoreSnapshots(context.Background(), r, testManifest(), testTarget(), nil)
			if err == nil {
				t.Fatal("a proof snapshot that exited non-zero must fail, or set-target claims a datastore path that does not work")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the failure must report %q: %v", c.want, err)
			}
			if strings.Contains(err.Error(), c.unwanted) {
				t.Errorf("the failure must not report %q: %v", c.unwanted, err)
			}
		})
	}
}
