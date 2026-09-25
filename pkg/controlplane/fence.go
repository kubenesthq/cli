package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

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

	// fenceServiceAttempts and fenceServicePoll bound how long Raise waits for
	// the Service object to exist. It is a k3s manifest apply, not a pod
	// coming up, so the wait is short: the pod only decides whether the body
	// comes from the fence or from the data plane's own no-endpoints 503.
	fenceServiceAttempts = 60
	fenceServicePoll     = 2 * time.Second
)

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
func Raise(ctx context.Context, r k3s.Runner, valuesYAML string) (string, error) {
	manifest, err := fenceObjects(ctx, r)
	if err != nil {
		return "", err
	}
	if err := k3s.WriteManifest(ctx, r, fenceName, manifest); err != nil {
		return "", err
	}
	if err := waitForService(ctx, r, FenceService); err != nil {
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
func fenceObjects(ctx context.Context, r k3s.Runner) ([]byte, error) {
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

	configMap := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": fenceName, "namespace": Namespace, "labels": labels},
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
		"readinessProbe": map[string]any{
			"httpGet": map[string]any{"path": "/", "port": "http"},
		},
	}

	deployment := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": fenceName, "namespace": Namespace, "labels": labels},
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
		"metadata":   map[string]any{"name": fenceName, "namespace": Namespace, "labels": labels},
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
func waitForService(ctx context.Context, r k3s.Runner, name string) error {
	for i := 0; i < fenceServiceAttempts; i++ {
		exists, err := serviceExists(ctx, r, name)
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
		if err := sleepCtx(ctx, fenceServicePoll); err != nil {
			return err
		}
	}
	return fmt.Errorf("the fence Service %s/%s did not appear after %s, so the API route was not switched to it; nothing else has been changed",
		Namespace, name, fenceServicePoll*fenceServiceAttempts)
}
