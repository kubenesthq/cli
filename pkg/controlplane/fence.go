package controlplane

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
)

// The maintenance fence (PLAN 7.8, T7.0): during a control-plane upgrade the
// public API route must not reach the backend, because the backend is
// unavailable by construction — its Deployment uses the Recreate strategy and
// its schema is being migrated by a Job the CLI runs as a recorded step.
//
// WHERE THE FENCE LIVES, AND WHY IT IS NOT A CLI PATCH OF THE ROUTE.
//
// helm-controller applies this chart SERVER-SIDE with --force-conflicts=false.
// An object whose fields are also owned by another writer therefore makes the
// NEXT chart apply fail outright, which is not a theoretical hazard: it was
// observed on hardware on 2026-09-25 as `conflict with "kubectl-patch" using
// v1: .data.status`, and it is why the chart no longer renders the checkpoint
// status document's data. A fence that repointed <release>-api from the CLI
// would own .spec.rules[0].backendRefs under a second manager, and the chart
// apply that runs INSIDE this very upgrade would then fail with a conflict.
// So the route switch is a CHART VALUE (fence.enabled), which keeps every
// chart-rendered field under helm, and the fence's own objects — which helm
// does not render and therefore cannot conflict with — are written by the CLI
// into k3s's auto-deploy directory.
//
// THE FENCE OBJECTS ARE NOT CHART OBJECTS. They carry no chart version, need
// no backend code, and survive a restart because k3s keeps applying the file.
// They exist to answer 503 on every path with a body that says why, so the
// customer's client sees a maintenance page rather than a connection error.
const (
	// fenceName is the name of the fence Deployment and its Service, and the
	// name of the k3s auto-deploy file they are written to.
	fenceName = ReleaseName + "-fence"
	// fencePort is what the fence Service publishes and the fenced route
	// points at. It is deliberately not 8000: nothing should be able to
	// mistake a backend for the fence.
	fencePort = 8080
	// fenceMessageKey is the ConfigMap key carrying the body the fence serves.
	fenceMessageKey = "message"

	// fenceRaiseAnnotation carries the identity of ONE raise, and exists
	// because of a defect that made every second control-plane upgrade fail on
	// a cluster (hardware, 2026-09-25).
	//
	// k3s's deploy controller records every manifest it applies as an `Addon`
	// in kube-system, named after the file and carrying the manifest's
	// CHECKSUM (pkg/deploy/controller.go compares the file's checksum with the
	// addon's and skips the apply when they match). Lower deleted the fence's
	// objects and the manifest file but not the Addon, so the next Raise wrote
	// byte-identical content, the checksum matched, and the controller never
	// re-created the Service: `control-plane-fence FAILED: the fence Service
	// kubenest-system/kubenest-cp-fence did not appear after 2m0s`.
	//
	// STAMPING THE OBJECTS IS THE FIX THAT CANNOT LOSE A RACE. Deleting the
	// Addon is the other half, but k3s only consults an Addon it can READ: a
	// ClusterRole without `addons` access would fail the deletion silently (the
	// kubectl call is best-effort) and a raise that trusted only that would
	// still be skipped. A raise whose content has never been applied before is
	// applied whatever a cluster's RBAC allows.
	fenceRaiseAnnotation = "kubenest.io/fence-raise"

	// THE FENCE CARRIES THE FACTS ABOUT THE CONTROL PLANE BEHIND IT, and that
	// is deliberate: a failed migration leaves the backend at zero replicas and
	// the public route fenced, and the CLI's advice for that state is "fix what
	// the error names, then run the identical command again" — which is a NEW
	// operation, so the record it would have read the facts from has been
	// replaced (kn-t70-control-plane-version-identity-4xso.1). The fence's own
	// Deployment exists exactly while the fence is up, and it is written by the
	// same stage that raised the fence, so the state and the facts that describe
	// it travel together and are deleted together.
	fenceContractAnnotation = "kubenest.io/fence-contract"
	fenceBuildAnnotation    = "kubenest.io/fence-build"
	fenceWindowAnnotation   = "kubenest.io/fence-window"

	// addonName is the k3s Addon object for the fence's manifest: the file's
	// base name, in kube-system. It is the object whose checksum decides
	// whether the file is applied.
	addonName = fenceName

	// addonNamespace is where k3s keeps its Addons.
	addonNamespace = "kube-system"

	// fenceServiceAttempts and fenceServicePoll bound how long Raise waits for
	// the Service object to exist. It is a k3s manifest apply, not a pod
	// coming up, so the wait is short: the pod only decides whether the body
	// comes from the fence or from the data plane's own no-endpoints 503.
	fenceServiceAttempts = 60
	fenceServicePoll     = 2 * time.Second
)

// RaiseOptions is what a raise needs beyond the values: the identity stamped on
// the objects, and how long to wait for them.
type RaiseOptions struct {
	// Stamp is written to every fence object, and makes this raise's manifest
	// differ from the last one's. Empty falls back to the clock, which is right
	// for a raise made outside an operation and wrong to omit: two raises of the
	// same values would otherwise write the same bytes.
	Stamp string
	// ServiceWait bounds the wait for the fence's Service to exist. Zero uses
	// the production bound.
	ServiceWait time.Duration
	// Facts is what the fence records about the control plane behind it. They
	// are annotations on the fence's objects, so a re-run whose backend is gone
	// can still establish which control plane it is upgrading.
	Facts FenceFacts
}

// FenceFacts is what the fence knows about the control plane behind it.
type FenceFacts struct {
	// Contract and Build are the version the control plane reported when this
	// fence was raised.
	Contract int
	Build    string
	// Window is the maintenance window spec as JSON, which the upgrade's window
	// gate needs and cannot read while the backend is gone.
	Window string
}

// fenceStamp is the identity this raise stamps, never empty.
func (o RaiseOptions) fenceStamp() string {
	if o.Stamp != "" {
		return o.Stamp
	}
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// fenceWait is how long to wait for the fence's objects to appear.
// annotationsFor is the fence's annotations: the raise's identity, and the
// facts about the control plane behind it.
func (o RaiseOptions) annotationsFor(stamp string) map[string]any {
	out := map[string]any{fenceRaiseAnnotation: stamp}
	if o.Facts.Contract != 0 {
		out[fenceContractAnnotation] = fmt.Sprint(o.Facts.Contract)
	}
	if o.Facts.Build != "" {
		out[fenceBuildAnnotation] = o.Facts.Build
	}
	if o.Facts.Window != "" {
		out[fenceWindowAnnotation] = o.Facts.Window
	}
	return out
}

func (o RaiseOptions) fenceWait() time.Duration {
	if o.ServiceWait > 0 {
		return o.ServiceWait
	}
	return fenceServicePoll * time.Duration(fenceServiceAttempts)
}

// sleepCtx waits for d or for ctx to end, whichever is first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// FenceService is the Service the fenced API route points at.
const FenceService = fenceName

// backendDeploymentImageCmd reads the image the backend Deployment runs.
//
// THE FENCE REUSES THE BACKEND'S IMAGE rather than pinning a second one: it is
// already pulled on the node by the time the fence is raised (the backend has
// been running it), and a fence that needed its own image could not start
// before its own pull and would leave the route pointing at a Service with no
// endpoints. Only a `python -c` 503 server is borrowed from it, never the
// application: the fence must not touch the database, and must not be able to
// refuse to start for any reason the application could.
const backendDeploymentImageCmd = "get deployment/" + backendService + " -n " + Namespace +
	" -o jsonpath={.spec.template.spec.containers[0].image}"

// backendReplicasCmd reads how many replicas the backend runs.
const backendReplicasCmd = "get deployment/" + backendService + " -n " + Namespace +
	" -o jsonpath={.spec.replicas}"

// runningBackendImage reads the image the backend Deployment runs and splits it
// into the fields the chart's values carry, so an apply can be held to exactly
// the reference that is running.
//
// IT IS THE READ THE FENCE ALREADY MAKES, parsed: backendDeploymentImageCmd is
// what fenceObjects uses to run the 503 page on an image the node has already
// pulled. One read, two uses, and the same failure rule for both — a backend
// whose image cannot be read has nothing to serve the maintenance page and
// nothing to pin, so the stage stops rather than deciding for itself what to
// run.
func runningBackendImage(ctx context.Context, r k3s.Runner) (BackendImage, error) {
	out, err := k3s.Kubectl(ctx, r, backendDeploymentImageCmd)
	if err != nil {
		return BackendImage{}, fmt.Errorf("the image the backend Deployment %s/%s runs could not be read, and an apply that does not know it would roll the backend onto the new chart's image: %w", Namespace, backendService, err)
	}
	reference := strings.TrimSpace(out)
	if reference == "" {
		return BackendImage{}, fmt.Errorf("the backend Deployment %s/%s names no image, so an apply could not be held to what it runs", Namespace, backendService)
	}
	image, err := ParseBackendImage(reference)
	if err != nil {
		return BackendImage{}, fmt.Errorf("the backend Deployment %s/%s runs %q, which cannot be pinned: %w", Namespace, backendService, reference, err)
	}
	return image, nil
}

// FenceState is what Status found.
type FenceState string

const (
	// FenceUp means the API route points at the fence, so no customer request
	// can reach the backend.
	FenceUp FenceState = "up"
	// FenceDown means the route points at the backend.
	FenceDown FenceState = "down"
	// FenceUnknown means the route names neither, which is a chart this CLI
	// does not describe — reported rather than guessed at, because reading it
	// as "down" would let an upgrade proceed with the route in a state nobody
	// intended.
	FenceUnknown FenceState = "unknown"
)

// FenceReport is the observation Status made.
type FenceReport struct {
	State       FenceState
	BackendRef  string
	FenceExists bool
}

// FenceOptions is what one fence step changes.
type FenceOptions struct {
	// Up switches the public API route to the fence Service. False restores it.
	Up bool
	// BackendReplicas, when not nil, sets backend.replicas. Zero takes the
	// backend down for the migration window; the value read before the fence
	// went up brings it back afterwards.
	BackendReplicas *int32
	// MigrationOff turns the chart's migration Job OFF explicitly.
	//
	// IT IS EXPLICIT BECAUSE THE RUNNING VALUES USUALLY ENABLE IT. The values an
	// upgrade reads off the cluster are the ones the last install or upgrade
	// applied, and a migration enabled then (migrate.go's MigrationValues,
	// which nothing turns back off) is still in them. An apply before this
	// run's migration apply that reproduced them would re-render the release's
	// Job with a new pod template — and Job.spec.template is immutable, so helm
	// would try to PATCH it, fail the upgrade outright, and leave the release
	// failed under failurePolicy: abort (hardware, 2026-09-25). Turning it off
	// makes helm DELETE the Job instead; the migration stage creates it again.
	MigrationOff bool
	// BackendImage, when not nil, pins backend.image to this reference: the
	// image the backend is RUNNING, read from its Deployment.
	//
	// THE APPLY THAT RAISES THE FENCE MUST CHANGE NOTHING BUT THE ROUTE. The
	// chart a binary carries pins the NEW backend image, so an apply without
	// this pin rolls the backend Deployment and the checkpoint CronJob onto
	// code that has never been migrated — before the checkpoint this procedure
	// exists to take, and before the migration that code needs.
	BackendImage *BackendImage
}

// FenceValues returns the values document with the fence applied.
//
// THE REPLICA COUNT GOES THROUGH THE VALUES, not through `kubectl scale`,
// for the reason the package comment gives: backend.replicas is a
// chart-rendered field and a second owner of it breaks the next chart apply.
// There is NO worker Deployment in this chart (kubenest-backend/docker-compose
// runs the arq worker in-process), so the backend is the only workload the
// fence stops; if the chart gains a worker, its replica value belongs here
// beside the backend's.
func FenceValues(valuesYAML string, o FenceOptions) (string, error) {
	doc := map[string]any{}
	if err := yaml.Unmarshal([]byte(valuesYAML), &doc); err != nil {
		return "", fmt.Errorf("reading the control-plane values: %w", err)
	}
	doc["fence"] = map[string]any{"enabled": o.Up}
	if o.BackendReplicas != nil {
		backend, _ := doc["backend"].(map[string]any)
		if backend == nil {
			backend = map[string]any{}
		}
		backend["replicas"] = *o.BackendReplicas
		doc["backend"] = backend
	}
	if o.MigrationOff {
		doc["migration"] = map[string]any{"enabled": false}
	}
	if o.BackendImage != nil {
		backend, _ := doc["backend"].(map[string]any)
		if backend == nil {
			backend = map[string]any{}
		}
		// A REFERENCE WITH NO DIGEST WRITES AN EMPTY ONE, never an absent one.
		// Helm merges these values over the chart's defaults, the chart's
		// default backend.image carries the new release's digest, and the
		// helper renders repository@digest whenever a digest is set. An absent
		// key leaves that default in force, so the apply would roll the backend
		// onto the new image after all; so would a digest left over from an
		// earlier pin. A reference with a digest needs no tag, because the digest
		// is what renders. Everything else under backend.image (pullPolicy, and
		// whatever the chart grows there) is left alone.
		image, _ := backend["image"].(map[string]any)
		if image == nil {
			image = map[string]any{}
		}
		image["repository"] = o.BackendImage.Repository
		image["digest"] = o.BackendImage.Digest
		if o.BackendImage.Tag != "" {
			image["tag"] = o.BackendImage.Tag
		} else {
			delete(image, "tag")
		}
		backend["image"] = image
		doc["backend"] = backend
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("rendering the control-plane values: %w", err)
	}
	return string(out), nil
}

// Raise writes the fence's objects and returns the values document that
// repoints <release>-api at the fence.
//
// IT CONVERGES ON THE SERVICE EXISTING BEFORE RETURNING, because the caller
// applies the fenced route next: an HTTPRoute naming a Service that does not
// exist is not a 503, it is a route the data plane refuses to resolve, and the
// whole point of the fence is that every request gets a maintenance answer
// rather than an error nobody can explain. The Service is up as soon as the
// manifest is applied; the fence's pod only decides whether the body comes
// from the fence or from the data plane's own no-endpoints 503.
func Raise(ctx context.Context, r k3s.Runner, valuesYAML string, o RaiseOptions) (string, error) {
	manifest, err := fenceObjects(ctx, r, o.fenceStamp(), o.annotationsFor(o.fenceStamp()))
	if err != nil {
		return "", err
	}
	// THE MANIFEST MUST NEVER BE THE BYTES THE LAST RAISE WROTE. k3s skips a
	// file whose checksum matches the Addon it recorded, so an identical raise
	// is an apply that never happens — see fenceRaiseAnnotation.
	if err := k3s.WriteManifest(ctx, r, fenceName, manifest); err != nil {
		return "", err
	}
	if err := waitForService(ctx, r, FenceService, o.fenceWait()); err != nil {
		return "", err
	}
	return FenceValues(valuesYAML, FenceOptions{Up: true})
}

// Lower restores the API route to the backend and then removes the fence's
// objects.
//
// IT OWNS THE ORDER, which is why it takes the apply. Deleting the fence
// Service before the route points back at the backend leaves a route that
// cannot resolve — the failure Raise converges to avoid, in the other
// direction. So the restored values are applied FIRST (apply is
// controlplane.Apply in production), and only then is the fence dismantled.
func Lower(
	ctx context.Context,
	r k3s.Runner,
	valuesYAML string,
	replicas *int32,
	apply func(valuesYAML string) error,
	confirm func() error,
) error {
	values, err := FenceValues(valuesYAML, FenceOptions{Up: false, BackendReplicas: replicas})
	if err != nil {
		return err
	}
	if err := apply(values); err != nil {
		return fmt.Errorf("restoring the API route: %w", err)
	}
	// THE CONFIRMATION IS NOT OPTIONAL IN PRODUCTION, and it is why Lower takes
	// it: the apply only writes the HelmChart, so the route still names the
	// fence until helm-controller re-renders it. Deleting the fence before that
	// leaves api.<domain> pointing at a Service that is gone (hardware,
	// 2026-09-25). A nil confirm is for a caller with no route to observe.
	if confirm != nil {
		if err := confirm(); err != nil {
			return err
		}
	}
	return DeleteFenceObjects(ctx, r)
}

// Status reports where the API route points and whether the fence exists.
//
// It is what a resume reads instead of assuming: an operation interrupted
// while the fence was up leaves a route pointing at a 503 page, and a CLI that
// repeated "raise the fence" without looking would be raising a fence that is
// already up while believing it just switched the route.
func Status(ctx context.Context, r k3s.Runner) (FenceReport, error) {
	report := FenceReport{State: FenceUnknown}

	ref, err := apiRouteBackendRef(ctx, r)
	if err != nil {
		return report, err
	}
	report.BackendRef = ref
	switch ref {
	case FenceService:
		report.State = FenceUp
	case backendService:
		report.State = FenceDown
	}
	exists, err := serviceExists(ctx, r, FenceService)
	if err != nil {
		return report, err
	}
	report.FenceExists = exists
	return report, nil
}

// apiRouteBackendRef reads which Service the api.<domain> route points at.
//
// IT IS A SEPARATE READ FROM Status because the fence's stages have to WAIT on
// it: Apply writes the HelmChart, and helm-controller re-renders the route
// afterwards, so the route is a fact about a different object than the one the
// apply wrote. A stage that assumed it had switched once the apply returned
// reported "the fence is up" while api.<domain> still reached the backend
// (hardware, 2026-09-25).
func apiRouteBackendRef(ctx context.Context, r k3s.Runner) (string, error) {
	var route struct {
		Spec struct {
			Rules []struct {
				BackendRefs []struct {
					Name string `json:"name"`
				} `json:"backendRefs"`
			} `json:"rules"`
		} `json:"spec"`
	}
	out, err := k3s.Kubectl(ctx, r, "get httproute "+apiRouteName+" -n "+Namespace+" -o json")
	if err != nil {
		return "", fmt.Errorf("reading the API route %s/%s: %w", Namespace, apiRouteName, err)
	}
	if err := json.Unmarshal([]byte(out), &route); err != nil {
		return "", fmt.Errorf("reading the API route %s/%s: %w", Namespace, apiRouteName, err)
	}
	ref := ""
	for _, rule := range route.Spec.Rules {
		for _, backend := range rule.BackendRefs {
			ref = backend.Name
		}
	}
	return ref, nil
}

// WaitForRouteBackend converges until the api.<domain> route points at want.
//
// THE CALLER MUST WAIT, in both directions: on the way in so the fence is real
// before anything changes, and on the way out BEFORE the fence's objects are
// deleted — a route naming a Service that no longer exists is not a 503, it is
// a hostname that answers nothing.
func WaitForRouteBackend(ctx context.Context, r k3s.Runner, want string, deadline, interval time.Duration, rep converge.Reporter) error {
	object := "httproute/" + apiRouteName + " in " + Namespace
	probe := func(ctx context.Context) (bool, converge.State, error) {
		ref, err := apiRouteBackendRef(ctx, r)
		if err != nil {
			return false, converge.State{Object: object, Status: "unreadable"}, err
		}
		if ref == want {
			return true, converge.State{Object: object, Status: "points at " + want}, nil
		}
		return false, converge.State{
			Object: object,
			Status: "points at " + ref + ", not " + want,
			Detail: "the chart was applied; helm-controller re-renders this route afterwards, and until it does the public API is served by whatever the route names now",
		}, nil
	}
	res, err := converge.Wait(ctx, probe, converge.Options{
		Name:     fenceRouteCheckName,
		Deadline: deadline,
		Interval: interval,
		Reporter: rep,
	})
	if err != nil {
		return err
	}
	if err := res.Err(); err != nil {
		return fmt.Errorf("%s: the API route did not start pointing at %s: %w", fenceRouteCheckName, want, err)
	}
	return nil
}

// fenceRouteCheckName is what the route wait reports itself as.
const fenceRouteCheckName = "kubenest-control-plane-fence-route"

// WaitForFenceAvailable converges until the fence's own Deployment serves, so
// the maintenance answer comes from the fence rather than incidentally from a
// Service with no endpoints.
func WaitForFenceAvailable(ctx context.Context, r k3s.Runner, deadline, interval time.Duration, rep converge.Reporter) error {
	object := "deployment/" + fenceName + " in " + Namespace
	probe := func(ctx context.Context) (bool, converge.State, error) {
		var d struct {
			Metadata struct {
				Generation int64 `json:"generation"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int32 `json:"replicas"`
			} `json:"spec"`
			Status struct {
				ObservedGeneration int64 `json:"observedGeneration"`
				AvailableReplicas  int32 `json:"availableReplicas"`
			} `json:"status"`
		}
		out, err := k3s.Kubectl(ctx, r, "get deployment/"+fenceName+" -n "+Namespace+" -o json")
		if err != nil {
			return false, converge.State{Object: object, Status: "not found yet"}, err
		}
		if err := json.Unmarshal([]byte(out), &d); err != nil {
			return false, converge.State{Object: object, Status: "unparsable"}, err
		}
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		if d.Metadata.Generation > d.Status.ObservedGeneration {
			return false, converge.State{Object: object, Status: "the fence pod template is not observed yet"}, nil
		}
		if d.Status.AvailableReplicas < want {
			return false, converge.State{
				Object: object,
				Status: fmt.Sprintf("%d/%d available", d.Status.AvailableReplicas, want),
				Detail: "the fence must answer 503 itself, rather than the route resting on a Service with no endpoints",
			}, nil
		}
		return true, converge.State{Object: object, Status: fmt.Sprintf("%d/%d available", d.Status.AvailableReplicas, want)}, nil
	}
	res, err := converge.Wait(ctx, probe, converge.Options{
		Name:     fenceAvailableCheckName,
		Deadline: deadline,
		Interval: interval,
		Reporter: rep,
	})
	if err != nil {
		return err
	}
	if err := res.Err(); err != nil {
		return fmt.Errorf("%s: the fence never started answering: %w", fenceAvailableCheckName, err)
	}
	return nil
}

// fenceAvailableCheckName is what the fence-readiness wait reports itself as.
const fenceAvailableCheckName = "kubenest-control-plane-fence-ready"

// DeleteFenceObjects removes the fence's objects after the route has been
// restored, and its durable manifest with them.
//
// Both halves are needed: the k3s deploy controller is what created the
// objects from the file, and removing the file does not reliably tear down
// what it already applied.
func DeleteFenceObjects(ctx context.Context, r k3s.Runner) error {
	if _, err := k3s.Kubectl(ctx, r, "delete deployment/"+fenceName+" service/"+fenceName+
		" configmap/"+fenceName+" -n "+Namespace+" --ignore-not-found"); err != nil {
		return fmt.Errorf("removing the fence objects: %w", err)
	}
	// THE ADDON GOES TOO, or the next raise's manifest is one k3s may decide is
	// already applied (see fenceRaiseAnnotation). It is a SECOND, BELT-AND-
	// BRACES step: the raise stamps its objects, so this is not what makes the
	// next raise work — it is what stops a stale Addon from being consulted at
	// all, and it is best-effort because a cluster whose RBAC does not grant
	// `addons` cannot be repaired by an object this CLI cannot see.
	if _, err := k3s.Kubectl(ctx, r, "delete addon "+addonName+" -n "+addonNamespace+" --ignore-not-found"); err != nil {
		return fmt.Errorf("removing the fence's k3s Addon %s/%s: %w", addonNamespace, addonName, err)
	}
	// AND THE PREVIOUS RELEASE GOES WITH IT. It exists to serve a FAILED
	// migration while the fence is up; once the fence is down the release it
	// records is either what runs (after a restore) or older than what runs, and
	// keeping the generated secrets of a superseded release on the cluster is
	// the kind of residue a recovery kit is supposed to make unnecessary.
	if _, err := k3s.Kubectl(ctx, r, "delete secret "+PreviousReleaseSecret+" -n "+Namespace+" --ignore-not-found"); err != nil {
		return fmt.Errorf("removing the previous release Secret %s/%s: %w", Namespace, PreviousReleaseSecret, err)
	}
	res, err := r.Run(ctx, "sudo -n rm -f "+k3s.ManifestDir+"/"+fenceName+".yaml")
	if err != nil {
		return fmt.Errorf("removing the fence manifest: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("removing the fence manifest: exit %d", res.ExitCode)
	}
	return nil
}

// Replicas reads how many replicas the backend runs at, so a fence that stops
// it can start it again with the same number rather than with a number this
// CLI guessed.
func Replicas(ctx context.Context, r k3s.Runner) (int32, error) {
	out, err := k3s.Kubectl(ctx, r, backendReplicasCmd)
	if err != nil {
		return 0, fmt.Errorf("reading the backend's replica count: %w", err)
	}
	var n int32
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &n); err != nil {
		return 0, fmt.Errorf("the backend's replica count %q is not a number", strings.TrimSpace(out))
	}
	return n, nil
}

// fenceObjects renders the ConfigMap, Deployment and Service the fence is made
// of, using the image the backend Deployment already runs.
func fenceObjects(ctx context.Context, r k3s.Runner, stamp string, extra map[string]any) ([]byte, error) {
	image, err := k3s.Kubectl(ctx, r, backendDeploymentImageCmd)
	if err != nil {
		return nil, fmt.Errorf("the fence needs the backend's image, which could not be read: %w", err)
	}
	image = strings.TrimSpace(image)
	if image == "" {
		return nil, fmt.Errorf("the backend Deployment %s/%s names no image, so the fence has nothing to run", Namespace, backendService)
	}

	labels := map[string]any{
		"app.kubernetes.io/name":      "kubenest-fence",
		"app.kubernetes.io/instance":  ReleaseName,
		"app.kubernetes.io/component": "fence",
		"app.kubernetes.io/part-of":   "kubenest",
	}
	// One annotation plus the facts, on the object's metadata rather than on the
	// Deployment's pod template: what has to differ is the DOCUMENT's checksum,
	// and a pod template that changed would roll the fence for no reason.
	annotations := map[string]any{fenceRaiseAnnotation: stamp}
	for key, value := range extra {
		annotations[key] = value
	}
	objectMeta := func(name string) map[string]any {
		return map[string]any{"name": name, "namespace": Namespace, "labels": labels, "annotations": annotations}
	}

	configMap := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   objectMeta(fenceName),
		"data": map[string]any{
			fenceMessageKey: "The KubeNest control plane is being upgraded and is briefly unavailable. " +
				"Your clusters keep running and their agents stay connected to the relay; reports sent " +
				"while the API is fenced are resent when it returns. Retry shortly.",
		},
	}

	// The container is built separately so its many keys cannot be mistaken
	// for the pod spec's.
	fenceContainer := map[string]any{
		"name":            "fence",
		"image":           image,
		"imagePullPolicy": "IfNotPresent",
		"command": []any{"python", "-u", "-c",
			// A FEW LINES OF STATIC 503 SERVER, and nothing of the
			// application: no database, no schema guard, no migration. The
			// fence must be unable to fail for any reason the backend could,
			// because a fence that will not start is indistinguishable from a
			// fence that was never raised.
			"import http.server as h, os\n" +
				"BODY = os.environ['FENCE_MESSAGE'].encode()\n" +
				"class F(h.BaseHTTPRequestHandler):\n" +
				"    def r(s):\n" +
				"        s.send_response(503)\n" +
				// THE FENCE IDENTIFIES ITSELF (kn-t70...4xso.1). A command that
				// must run behind the fence — the resume of the upgrade that
				// raised it — cannot tell "fenced" from "broken" by the status
				// code: a load balancer answers 503 too. This header is what
				// makes the difference checkable.
				"        s.send_header('" + api.FenceHeader + "', '" + api.FenceHeaderUp + "')\n" +
				"        s.send_header('Content-Type', 'text/plain; charset=utf-8')\n" +
				"        s.send_header('Retry-After', '60')\n" +
				"        s.send_header('Content-Length', str(len(BODY)))\n" +
				"        s.end_headers()\n" +
				"        s.wfile.write(BODY)\n" +
				"    do_GET = r\n" +
				"    do_HEAD = r\n" +
				"    do_POST = r\n" +
				"    do_PUT = r\n" +
				"    do_PATCH = r\n" +
				"    do_DELETE = r\n" +
				"    do_OPTIONS = r\n" +
				"    def log_message(s, *a):\n" +
				"        pass\n" +
				"h.ThreadingHTTPServer(('0.0.0.0', " + fmt.Sprint(fencePort) + "), F).serve_forever()\n",
		},
		"ports": []any{map[string]any{"name": "http", "containerPort": fencePort}},
		"env": []any{map[string]any{
			"name": "FENCE_MESSAGE",
			"valueFrom": map[string]any{"configMapKeyRef": map[string]any{
				"name": fenceName, "key": fenceMessageKey,
			}},
		}},
		// A TCP check, not an HTTP one: the fence answers 503 to every request,
		// and an HTTP probe passes only on 2xx/3xx, so it could never be Ready
		// (measured on hardware, 2026-09-25). Listening is what readiness means
		// for a page whose only answer is "unavailable".
		"readinessProbe": map[string]any{
			"tcpSocket":     map[string]any{"port": "http"},
			"periodSeconds": 2,
		},
	}

	deployment := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   objectMeta(fenceName),
		"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{"matchLabels": map[string]any{
				"app.kubernetes.io/name": "kubenest-fence",
			}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"containers": []any{fenceContainer},
				},
			},
		},
	}

	service := map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   objectMeta(fenceName),
		"spec": map[string]any{
			"type":     "ClusterIP",
			"selector": map[string]any{"app.kubernetes.io/name": "kubenest-fence"},
			"ports":    []any{map[string]any{"name": "http", "port": fencePort, "targetPort": "http"}},
		},
	}

	var docs [][]byte
	for _, object := range []map[string]any{configMap, deployment, service} {
		body, err := yaml.Marshal(object)
		if err != nil {
			return nil, fmt.Errorf("rendering the fence: %w", err)
		}
		docs = append(docs, body)
	}
	return []byte(strings.Join(renderDocs(docs), "")), nil
}

// renderDocs joins documents into one YAML stream.
func renderDocs(docs [][]byte) []string {
	out := make([]string, 0, len(docs))
	for _, doc := range docs {
		out = append(out, "---\n"+string(doc))
	}
	return out
}

// apiRouteName is the HTTPRoute serving api.<domain>.
const apiRouteName = ReleaseName + "-api"

func serviceExists(ctx context.Context, r k3s.Runner, name string) (bool, error) {
	out, err := k3s.Kubectl(ctx, r, "get service "+name+" -n "+Namespace+
		" -o jsonpath={.metadata.name}")
	if err != nil {
		return false, nil // not found yet is an observation, not a failure
	}
	return strings.TrimSpace(out) != "", nil
}

// waitForService blocks until the fence's Service exists.
func waitForService(ctx context.Context, r k3s.Runner, name string, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		exists, err := serviceExists(ctx, r, name)
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the fence Service %s/%s did not appear after %s, so the API route was not switched to it; nothing else has been changed. If a previous raise deleted the fence's objects while k3s still records the manifest as applied, the Addon %s/%s is what must go: `kubectl delete addon %s -n %s`",
				Namespace, name, within, addonNamespace, addonName, addonName, addonNamespace)
		}
		if err := sleepCtx(ctx, fenceServicePoll); err != nil {
			return err
		}
	}
}

// NodeClient builds a client for the backend at the address only the NODE can
// route to, over the SSH connection the caller already holds.
//
// It is the same path the upgrade's validation uses (BackendAddr + the
// connection's DialTCP + api.New with WithDialContext), extracted because a
// command that must run behind the fence needs it for its ordinary reads too:
// while the public route points at the fence, the node is the only way to the
// backend, and after a failed migration the backend may not be there at all
// (kn-t70-control-plane-version-identity-4xso.1).
func NodeClient(ctx context.Context, server k3s.Runner, open ClientOpener) (*api.Client, error) {
	addr, err := BackendAddr(ctx, server)
	if err != nil {
		return nil, err
	}
	tunnel, ok := server.(portDialer)
	if !ok {
		return nil, fmt.Errorf("the SSH connection to the server cannot open a tunnel to the control plane backend (%s), so the control plane cannot be reached while the fence is up", addr)
	}
	return open(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return tunnel.DialTCP(ctx, addr)
	})
}

// FenceDeploymentFacts reads the facts the fence carries about the control plane
// behind it, and whether the fence exists at all.
//
// IT IS A KUBECTL READ OVER THE NODE, so it needs no backend: that is the point.
// The fence exists exactly while the fence is up, and this is what a NEW run
// (after a failed migration, whose record is terminal and replaced) reads to
// learn which control plane it is upgrading and inside which window.
func FenceDeploymentFacts(ctx context.Context, r k3s.Runner) (FenceFacts, bool, error) {
	var deployment struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	out, err := k3s.Kubectl(ctx, r, "get deployment/"+fenceName+" -n "+Namespace+" -o json")
	if err != nil {
		// No fence is not an error: it is the ordinary state of a cluster whose
		// control plane is not mid-upgrade.
		return FenceFacts{}, false, nil
	}
	if err := json.Unmarshal([]byte(out), &deployment); err != nil {
		return FenceFacts{}, false, fmt.Errorf("reading the fence Deployment %s/%s: %w", Namespace, fenceName, err)
	}
	facts := FenceFacts{
		Build:  deployment.Metadata.Annotations[fenceBuildAnnotation],
		Window: deployment.Metadata.Annotations[fenceWindowAnnotation],
	}
	if contract := deployment.Metadata.Annotations[fenceContractAnnotation]; contract != "" {
		n, err := strconv.Atoi(contract)
		if err != nil {
			return FenceFacts{}, false, fmt.Errorf("the fence records contract %q, which is not a number", contract)
		}
		facts.Contract = n
	}
	return facts, true, nil
}

// PreviousReleaseSecret is where the release the control plane ran BEFORE this
// upgrade is kept while the fence is up (kn-t70-control-plane-version-identity-4xso.2).
//
// WHY A SECRET AND NOT AN ANNOTATION. `valuesContent` carries the generated
// secrets — the JWT key, the agent signing key, the encryption key, the
// administrator's password — so it cannot go in a ConfigMap or on an object
// that anything may read. It is next to the fence in the sense that matters:
// written by `CapturePreviousRelease` at the fence stage, read by the migration
// stage when its Job fails, and deleted by `Lower` with the fence's own objects.
//
// It is durable because the process that raised the fence may be gone: the CLI's
// advice after a failed migration is "fix what the error names, then run the
// identical command again", and that command may be a second laptop.
//
// SIZE: the control-plane chart archive is ~209 KB and base64-encoded inside the
// Secret (the HelmChart's chartContent already is), so the object is ~280 KB
// against the API server's 1 MiB limit. TestTheCapturedPreviousReleaseFitsInASecret
// asserts it from the embedded archive rather than trusting this sentence.
const PreviousReleaseSecret = ReleaseName + "-fence-previous"

// Keys of the Secret.
const (
	previousChartContentKey = "chartContent"
	previousValuesKey       = "valuesContent"
	previousReplicasKey     = "replicas"
)

// PreviousRelease is the release the control plane ran before this upgrade: the
// chart archive the HelmChart carried, the values it was applied with, and how
// many replicas its backend ran.
type PreviousRelease struct {
	// ChartContent is the chart ARCHIVE's bytes, decoded. The HelmChart carries
	// them base64-encoded and the Secret carries them base64-encoded again, so
	// this type holds them once, decoded, and nothing has to remember which
	// layer encoded what.
	ChartContent string
	// ValuesContent is spec.valuesContent verbatim.
	ValuesContent string
	// Replicas is the backend's replica count before the fence stopped it, so a
	// restore in a NEW process (which has no journal) brings the backend back at
	// the size it had rather than at a number this binary guessed.
	Replicas int32
}

// CapturePreviousRelease reads the release the control plane runs now, from the
// live HelmChart, BEFORE the upgrade's first apply.
//
// AFTER the first apply there is nothing to read: the HelmChart is the upgrade's,
// and the release it replaced exists nowhere else.
func CapturePreviousRelease(ctx context.Context, r k3s.Runner, replicas int32) (PreviousRelease, error) {
	content, err := k3s.Kubectl(ctx, r, "get helmchart "+ReleaseName+" -n kube-system -o jsonpath={.spec.chartContent}")
	if err != nil {
		return PreviousRelease{}, fmt.Errorf("reading the control plane's chart from HelmChart %s in kube-system: %w (an upgrade with no previous release to go back to must not start)", ReleaseName, err)
	}
	values, err := k3s.Kubectl(ctx, r, "get helmchart "+ReleaseName+" -n kube-system -o jsonpath={.spec.valuesContent}")
	if err != nil {
		return PreviousRelease{}, fmt.Errorf("reading the control plane's values from HelmChart %s in kube-system: %w", ReleaseName, err)
	}
	archive, err := base64.StdEncoding.DecodeString(strings.TrimSpace(content))
	if err != nil {
		return PreviousRelease{}, fmt.Errorf("the chart HelmChart %s in kube-system carries is not base64: %w", ReleaseName, err)
	}
	previous := PreviousRelease{
		ChartContent:  string(archive),
		ValuesContent: values,
		Replicas:      replicas,
	}
	if previous.ChartContent == "" || strings.TrimSpace(previous.ValuesContent) == "" {
		return PreviousRelease{}, fmt.Errorf("HelmChart %s in kube-system carries no chart content or no values, so the release this upgrade would replace cannot be captured: a failed migration would leave the control plane running nothing", ReleaseName)
	}
	return previous, nil
}

// StorePreviousRelease writes the capture into the fence's Secret.
func StorePreviousRelease(ctx context.Context, r k3s.Runner, previous PreviousRelease) error {
	doc := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      PreviousReleaseSecret,
			"namespace": Namespace,
			"labels":    map[string]any{"app.kubernetes.io/component": "fence", "kubenest.io/purpose": "previous-release"},
		},
		"type": "Opaque",
		// data, not stringData: the API server base64-decodes data, so this is
		// the one place the archive's bytes and the values' text are encoded,
		// and a read gives them back decoded.
		"data": map[string]any{
			previousChartContentKey: base64.StdEncoding.EncodeToString([]byte(previous.ChartContent)),
			previousValuesKey:       base64.StdEncoding.EncodeToString([]byte(previous.ValuesContent)),
			previousReplicasKey:     base64.StdEncoding.EncodeToString([]byte(fmt.Sprint(previous.Replicas))),
		},
	}
	body, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("rendering the previous release Secret: %w", err)
	}
	return kubectlApply(ctx, r, body)
}

// ReadPreviousRelease reads the capture. Not found is (zero, false, nil): no
// fence means no previous release to go back to, which is the ordinary state.
func ReadPreviousRelease(ctx context.Context, r k3s.Runner) (PreviousRelease, bool, error) {
	var secret struct {
		Data map[string]string `json:"data"`
	}
	out, err := k3s.Kubectl(ctx, r, "get secret "+PreviousReleaseSecret+" -n "+Namespace+" -o json")
	if err != nil {
		return PreviousRelease{}, false, nil
	}
	if err := json.Unmarshal([]byte(out), &secret); err != nil {
		return PreviousRelease{}, false, fmt.Errorf("reading the previous release Secret %s/%s: %w", Namespace, PreviousReleaseSecret, err)
	}
	decode := func(key string) (string, error) {
		raw, ok := secret.Data[key]
		if !ok {
			return "", fmt.Errorf("the previous release Secret %s/%s has no %s", Namespace, PreviousReleaseSecret, key)
		}
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return "", fmt.Errorf("the previous release Secret %s/%s has an unreadable %s: %w", Namespace, PreviousReleaseSecret, key, err)
		}
		return string(decoded), nil
	}
	content, err := decode(previousChartContentKey)
	if err != nil {
		return PreviousRelease{}, false, err
	}
	values, err := decode(previousValuesKey)
	if err != nil {
		return PreviousRelease{}, false, err
	}
	replicas, err := decode(previousReplicasKey)
	if err != nil {
		return PreviousRelease{}, false, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(replicas))
	if err != nil {
		return PreviousRelease{}, false, fmt.Errorf("the previous release Secret %s/%s records replicas %q, which is not a number", Namespace, PreviousReleaseSecret, replicas)
	}
	return PreviousRelease{ChartContent: content, ValuesContent: values, Replicas: int32(n)}, true, nil
}

// kubectlApply applies one document over stdin.
//
// STDIN, NEVER A COMMAND STRING: the document is the previous release's values,
// which carry the install's generated secrets — the same rule k3s.WriteManifest
// keeps for the agent's values.
//
// SERVER-SIDE, because a client-side apply copies the whole object into the
// last-applied-configuration annotation, annotations are limited to 256 KiB,
// and the real release does not fit (hardware, 2026-09-26: "metadata.annotations:
// Too long: may not be more than 262144 bytes").
func kubectlApply(ctx context.Context, r k3s.Runner, doc []byte) error {
	res, err := r.RunInput(ctx, "sudo -n k3s kubectl apply --server-side --force-conflicts --field-manager=kubenest-cli -f -", bytes.NewReader(doc))
	if err != nil {
		return fmt.Errorf("applying the previous release Secret: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("applying the previous release Secret: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
