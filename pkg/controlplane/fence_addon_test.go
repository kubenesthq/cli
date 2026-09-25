package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/sshx"
)

// THE SECOND RAISE ON A CLUSTER MUST PUT THE FENCE BACK (hardware, 2026-09-25).
//
// k3s's deploy controller records every manifest it applies as an `Addon` in
// kube-system, named after the file and carrying the manifest's CHECKSUM
// (pkg/deploy/controller.go: the file's checksum is compared with the addon's
// and the apply is skipped when they match). Lower removed the fence's objects
// and the manifest file but left the Addon, so the next Raise wrote
// byte-identical content, the checksum matched, and the controller never
// re-created the objects: `control-plane-fence FAILED: the fence Service
// kubenest-system/kubenest-cp-fence did not appear after 2m0s`, on every second
// control-plane upgrade of a cluster.
//
// k3s's DETECTION IS ITS OWN, and the gate cannot rely on it: a ClusterRole
// without `addons` read would fail closed on a cluster that is fine. Real k3s
// skips when it CANNOT read the addon
// ("failed to get deployed addon, applying manifest anyway"), so the safe fix
// is the one that makes every raise unique.
//
// The fake below models exactly that rule — a manifest whose checksum equals
// the recorded Addon's is not applied — and nothing else.
type deployController struct {
	*componenttest.FakeRunner
	checksum string
	// objects tracks the fence's objects, keyed as the fake's own deletes are.
	objects map[string]bool
	// manifests is every fence manifest the fake was handed, in order.
	manifests [][]byte
	// addonDeletes counts `addon` deletions, for the assertion that Lower does
	// not need one.
	addonDeletes int
	// unreadable makes the Addon read fail, which is what a ClusterRole
	// without `addons` read looks like.
	unreadable bool
}

func newDeployController(t *testing.T) *deployController {
	t.Helper()
	d := &deployController{objects: map[string]bool{}}
	d.FakeRunner = &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.HasPrefix(command, "sudo -n install -m 0600 "):
			stdin := d.lastInput(t)
			if !strings.Contains(command, fenceName+".yaml") {
				return sshx.Result{}, nil
			}
			d.manifests = append(d.manifests, stdin)
			sum := sha256.Sum256(stdin)
			if hex.EncodeToString(sum[:]) == d.checksum {
				// THE CONTROLLER'S OWN RULE: unchanged content is not applied.
				return sshx.Result{}, nil
			}
			d.checksum = hex.EncodeToString(sum[:])
			for _, name := range []string{"configmap", "deployment", "service"} {
				d.objects[name] = true
			}
			return sshx.Result{}, nil
		case strings.Contains(command, "delete deployment/"+fenceName):
			for _, name := range []string{"configmap", "deployment", "service"} {
				delete(d.objects, name)
			}
			return sshx.Result{}, nil
		case strings.Contains(command, "delete addon "):
			d.addonDeletes++
			if d.unreadable {
				return sshx.Result{ExitCode: 1, Stderr: "error: the server doesn't have a resource type \"addon\""}, nil
			}
			d.checksum = ""
			return sshx.Result{}, nil
		case strings.Contains(command, "rm -f") && strings.Contains(command, fenceName+".yaml"):
			return sshx.Result{}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl get service "+FenceService):
			if d.objects["service"] {
				return sshx.Result{Stdout: FenceService}, nil
			}
			return sshx.Result{ExitCode: 1, Stderr: "not found"}, nil
		case strings.Contains(command, backendDeploymentImageCmd):
			return sshx.Result{Stdout: "ghcr.io/kubenesthq/kubenest-backend:132b7ea"}, nil
		default:
			return sshx.Result{}, nil
		}
	}}
	return d
}

func (d *deployController) lastInput(t *testing.T) []byte {
	t.Helper()
	inputs := d.Inputs()
	if len(inputs) == 0 {
		t.Fatal("a manifest was written without streaming a document")
	}
	return inputs[len(inputs)-1]
}

func TestASecondRaiseAfterALowerRecreatesTheFence(t *testing.T) {
	ctx := context.Background()
	values := "domain: kn.example.com\njwtSecret: s\n"
	short := RaiseOptions{Stamp: "op-1", ServiceWait: time.Second}

	d := newDeployController(t)
	if _, err := Raise(ctx, d, values, short); err != nil {
		t.Fatalf("the first raise: %v", err)
	}
	if !d.objects["service"] {
		t.Fatal("the first raise did not create the fence's objects, so this test would prove nothing about the second")
	}
	if len(d.manifests) != 1 {
		t.Fatalf("the fake was handed %d manifest(s), want 1", len(d.manifests))
	}

	// Lower: the route is already back on the backend, so the objects go.
	if err := DeleteFenceObjects(ctx, d); err != nil {
		t.Fatal(err)
	}
	if d.objects["service"] {
		t.Fatal("Lower left the fence Service behind")
	}

	// THE SECOND RAISE. With the leftover Addon and identical content the
	// controller does not re-apply, and the fence never comes back.
	if _, err := Raise(ctx, d, values, RaiseOptions{Stamp: "op-2", ServiceWait: 300 * time.Millisecond}); err != nil {
		t.Fatalf("the second raise could not put the fence back, so the next control-plane upgrade on this cluster fails at the fence: %v", err)
	}
	if !d.objects["service"] {
		t.Fatal("the fence Service does not exist after the second raise")
	}
	if len(d.manifests) != 2 {
		t.Fatalf("the fake was handed %d manifest(s), want 2", len(d.manifests))
	}
	if string(d.manifests[0]) == string(d.manifests[1]) {
		t.Error("the two raises wrote byte-identical manifests, so a leftover Addon's checksum still matches and k3s does not re-apply")
	}
	// And the difference is an annotation ON EVERY OBJECT, which is what makes
	// the content unique without changing what the fence does.
	for _, object := range fenceObjectsOf(t, componenttest.Execution{Stdin: d.manifests[1]}) {
		metadata, _ := object["metadata"].(map[string]any)
		annotations, _ := metadata["annotations"].(map[string]any)
		if annotations[fenceRaiseAnnotation] != "op-2" {
			t.Errorf("the %v object carries %v = %v, want the raise's stamp %q under %s",
				object["kind"], fenceRaiseAnnotation, annotations[fenceRaiseAnnotation], "op-2", fenceRaiseAnnotation)
		}
	}
}

// THE ADDON IS DELETED BY LOWER, and that is the half that works on every k3s
// version: k3s only consults the Addon it can read, and the deletion is
// idempotent with --ignore-not-found.
func TestLowerRemovesTheFencesAddon(t *testing.T) {
	ctx := context.Background()
	d := newDeployController(t)
	// A leftover Addon from an older CLI, whose checksum will match the content
	// this raise writes unless the raise stamps something.
	d.checksum = "stale-checksum-from-an-earlier-raise"

	if _, err := Raise(ctx, d, "domain: kn.example.com\n", RaiseOptions{Stamp: "op-3", ServiceWait: time.Second}); err != nil {
		t.Fatal(err)
	}
	if err := DeleteFenceObjects(ctx, d); err != nil {
		t.Fatal(err)
	}
	if d.addonDeletes != 1 {
		t.Errorf("Lower deleted the fence's Addon %d time(s), want exactly 1 with --ignore-not-found: the leftover Addon is what stops the next raise", d.addonDeletes)
	}
}

// The stamp falls back to the clock, so a raise made outside an operation
// still differs from the last one.
func TestARaiseWithoutAnOperationStillStamps(t *testing.T) {
	ctx := context.Background()
	d := newDeployController(t)
	for i := 0; i < 2; i++ {
		if _, err := Raise(ctx, d, "domain: kn.example.com\n", RaiseOptions{ServiceWait: time.Second}); err != nil {
			t.Fatalf("raise %d: %v", i+1, err)
		}
		if err := DeleteFenceObjects(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if len(d.manifests) != 2 || string(d.manifests[0]) == string(d.manifests[1]) {
		t.Fatalf("two raises of the same values %d time(s) wrote identical manifests: an un-stamped raise is a raise k3s may never apply", len(d.manifests))
	}
}
