package backup

import (
	"context"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/leakscan"
	"kubenest.io/cli/pkg/sshx"
)

// A secret long and distinctive enough that a coincidental match is not
// plausible, and long enough that its base64 encoding is a payload-sized run.
const leakTestSecretKey = "wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY0000"
const leakTestAccessKey = "AKIAIOSFODNN7LEAKTEST"

func leakTestTarget() Target {
	t := testTarget()
	t.AccessKeyID = leakTestAccessKey
	t.SecretAccessKey = leakTestSecretKey
	return t
}

func leakTestResponder() func(string) (sshx.Result, error) {
	return scripted(map[string]string{
		"get backupstoragelocation": `{"status":{"phase":"Available"}}`,
		"get backuprepositories":    `{"items":[]}`,
		"get schedule":              "Enabled",
	})
}

// The object store's access key and secret key must never reach the target
// host's process list, in any encoding.
//
// This test did not exist when kn-40rd was reviewed, which is why apply()
// could base64 a Secret containing both keys onto the command line for months
// under a comment saying the content "travels base64-encoded so nothing needs
// shell quoting". Nothing was watching this path at all — the leak assertions
// covered only the agent's values document.
func TestTargetCredentialsNeverReachACommandLine(t *testing.T) {
	r := &fakeRunner{Respond: leakTestResponder()}
	if err := Configure(context.Background(), r, testManifest(), leakTestTarget(), nil); err != nil {
		t.Fatal(err)
	}

	commands := r.Commands()
	if len(commands) == 0 {
		t.Fatal("Configure ran nothing; this test would pass vacuously")
	}
	for _, cmd := range commands {
		if leakscan.RecoverableFrom(cmd, leakTestSecretKey) {
			t.Errorf("the object store secret access key is recoverable from a command line: %s", cmd)
		}
		if leakscan.RecoverableFrom(cmd, leakTestAccessKey) {
			t.Errorf("the object store access key ID is recoverable from a command line: %s", cmd)
		}
	}
}

// Positive control. Without this, the test above would pass just as happily if
// Configure stopped sending the credentials to the cluster at all — a green
// earned by delivering nothing is the failure mode this whole bead is about.
func TestTargetCredentialsDoReachTheClusterOverStdin(t *testing.T) {
	r := &fakeRunner{Respond: leakTestResponder()}
	if err := Configure(context.Background(), r, testManifest(), leakTestTarget(), nil); err != nil {
		t.Fatal(err)
	}

	var sawSecret bool
	for _, a := range r.Applied() {
		if strings.Contains(a.Doc, leakTestSecretKey) {
			sawSecret = true
		}
	}
	if !sawSecret {
		t.Error("the secret access key reached the cluster in no applied document; the leak test above proves nothing")
	}
}

// The etcd snapshot credentials travel the same path and are the same class of
// secret: they are what lets a holder read every etcd snapshot in the bucket.
func TestDatastoreCredentialsNeverReachACommandLine(t *testing.T) {
	target := leakTestTarget()
	doc, err := target.datastoreSecretManifest(24)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), leakTestSecretKey) {
		t.Fatal("the fixture does not contain the secret key; this test proves nothing")
	}

	r := &fakeRunner{Respond: leakTestResponder()}
	if err := apply(context.Background(), r, "etcd snapshot S3 credentials", doc); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range r.Commands() {
		if leakscan.RecoverableFrom(cmd, leakTestSecretKey) {
			t.Errorf("the etcd snapshot secret key is recoverable from a command line: %s", cmd)
		}
	}
	var delivered bool
	for _, in := range r.Inputs() {
		if strings.Contains(string(in), leakTestSecretKey) {
			delivered = true
		}
	}
	if !delivered {
		t.Error("the etcd snapshot secret key was not streamed either; apply delivered nothing")
	}
}
