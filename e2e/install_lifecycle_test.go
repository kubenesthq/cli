//go:build e2e

// kn-7k8.1's acceptance, on a real host with a real control plane and no
// mocks: every one of the thirteen stages reaches the live SSE stream as an
// ordered started+terminal pair — including the transitions queued before
// registration produced a cluster id — and an injected failure names the
// component that actually broke rather than its stage's first one.
//
// The three lanes run in sequence against ONE journal, which is the only
// order that works on a single machine: each injection leaves the stages
// before it completed, and the next lane resumes from there.
//
//  1. gateway-api pinned to nothing → platform-networking fails naming gateway-api
//  2. kured pinned to nothing       → platform-day2 fails naming kured
//  3. the real bundle               → completes, and SSE saw all thirteen
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/uninstall"
)

// stageEvent is the install_stage_update payload as it arrives over SSE.
type stageEvent struct {
	Stage      string `json:"stage"`
	Component  string `json:"component"`
	Status     string `json:"status"`
	ReasonCode string `json:"reason_code"`
	Detail     string `json:"detail"`
}

// sseObserver subscribes to the control plane's live event stream and records
// every install_stage_update for one cluster. This is the console's view: if
// a transition is missing here, the console never showed it.
type sseObserver struct {
	mu     sync.Mutex
	events []stageEvent
	cancel context.CancelFunc
	done   chan struct{}
}

func observeInstallEvents(t *testing.T, env gateEnv, clusterID string) *sseObserver {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	obs := &sseObserver{cancel: cancel, done: make(chan struct{})}

	endpoint := strings.TrimRight(env.controlPlane, "/") + "/api/v1/events/stream?" + url.Values{
		"cluster_id":    {clusterID},
		"resource_type": {"cluster"},
		"token":         {env.token},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("subscribing to the event stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("event stream returned %s", resp.Status)
	}

	go func() {
		defer close(obs.done)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var envelope struct {
				EventType string     `json:"event_type"`
				ClusterID string     `json:"cluster_id"`
				Payload   stageEvent `json:"payload"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &envelope); err != nil {
				continue
			}
			if envelope.EventType != "install_stage_update" || envelope.ClusterID != clusterID {
				continue
			}
			obs.mu.Lock()
			obs.events = append(obs.events, envelope.Payload)
			obs.mu.Unlock()
		}
	}()
	t.Cleanup(func() { obs.stop() })
	return obs
}

func (o *sseObserver) stop() {
	o.cancel()
	select {
	case <-o.done:
	case <-time.After(5 * time.Second):
	}
}

func (o *sseObserver) seen() []stageEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]stageEvent(nil), o.events...)
}

// poisonPin returns the real bundle with one component pinned to a version
// that does not exist, and a short component deadline so the failure costs
// two minutes rather than ten. Both are the manifest doing its job: the pins
// and the deadlines are data.
func poisonPin(t *testing.T, client *api.Client, version, component string) *manifest.Manifest {
	t.Helper()
	m := fetchBundle(t, client, version)
	m.Core[component] = "0.0.0-does-not-exist"
	m.Limits.Timeouts["component-ready"] = 2 * time.Minute
	return m
}

func TestInstallLifecycleOverSSE(t *testing.T) {
	env := gateEnvironment(t)
	ctx := context.Background()
	journalPath := t.TempDir() + "/lifecycle.json"

	client, err := api.New(env.controlPlane, api.WithToken(env.token))
	if err != nil {
		t.Fatal(err)
	}

	opts := gateOptions(env)
	opts.StorageDevice = env.storageDevice
	var clusterID string

	// Each injection asserts the SERVER's record, not the CLI's error: what
	// matters is that the control plane can say which component broke.
	inject := func(t *testing.T, component, wantStage string) {
		t.Helper()
		s, _ := session(t, env, journalPath, poisonPin(t, client, env.bundle, component), opts)
		defer s.Close()

		_, err := install.Execute(ctx, s, install.Plan(s))
		if err == nil {
			t.Fatalf("a bundle pinning %s to a version that does not exist must fail", component)
		}
		clusterID = s.Jnl.ClusterID
		if clusterID == "" {
			t.Fatal("the run registered no cluster")
		}

		health, err := client.ClusterHealth(ctx, clusterID)
		if err != nil {
			t.Fatalf("reading the cluster back: %v", err)
		}
		if health.Status != "install_failed" {
			t.Errorf("cluster status is %q, want install_failed", health.Status)
		}

		journal, err := client.InstallJournal(ctx, clusterID)
		if err != nil {
			t.Fatalf("reading the server-side install journal: %v", err)
		}
		if len(journal) == 0 {
			t.Fatal("the server-side install journal is empty")
		}
		last := journal[len(journal)-1]
		t.Logf("server journal last entry: stage=%s component=%s status=%s reason_code=%s",
			last.Stage, last.Component, last.Status, last.ReasonCode)
		if last.Stage != wantStage {
			t.Errorf("failed at stage %q, want %q", last.Stage, wantStage)
		}
		if last.Component != component {
			t.Errorf("the record names component %q, want %q — a record that names the wrong thing teaches the operator to distrust the record",
				last.Component, component)
		}
		if last.Status != api.StageFailed {
			t.Errorf("status %q, want failed", last.Status)
		}
	}

	t.Run("a gateway-api failure names gateway-api, not traefik", func(t *testing.T) {
		inject(t, "gateway-api", install.StageNetworking)
	})

	t.Run("a kured failure names kured, not system-upgrade-controller", func(t *testing.T) {
		if clusterID == "" {
			t.Skip("the first injection did not register a cluster")
		}
		inject(t, "kured", install.StageDay2)
	})

	t.Run("all thirteen stage lifecycles reach SSE in order", func(t *testing.T) {
		if clusterID == "" {
			t.Skip("no cluster to observe")
		}
		// Subscribed BEFORE the run, so nothing can be missed — including
		// preflight's transitions, which happen before this process has a
		// cluster id to send them under.
		obs := observeInstallEvents(t, env, clusterID)

		s, _ := session(t, env, journalPath, fetchBundle(t, client, env.bundle), opts)
		defer s.Close()
		result, err := install.Execute(ctx, s, install.Plan(s))
		if err != nil {
			t.Fatalf("the clean bundle must complete: %v", err)
		}
		t.Logf("completed: ran %v, skipped %v", result.Ran, result.Skipped)

		// The stream is asynchronous; give the tail of it a moment to land.
		deadline := time.Now().Add(30 * time.Second)
		var seen []stageEvent
		for time.Now().Before(deadline) {
			seen = obs.seen()
			if countStatus(seen, "started") >= len(install.StageNames) &&
				countStatus(seen, "completed") >= len(install.StageNames) {
				break
			}
			time.Sleep(2 * time.Second)
		}
		obs.stop()
		seen = obs.seen()

		for _, e := range seen {
			t.Logf("sse: stage=%s status=%s component=%s", e.Stage, e.Status, e.Component)
		}

		// Every stage, both lifecycles, in stage order. A missing `started`
		// is not cosmetic: it is the only status that clears install_failed
		// (kubenest-backend 9c19e5e), so a stage that never reports one has
		// a failure nothing can clear.
		assertOrderedLifecycles(t, seen)
	})

	t.Run("uninstall leaves a known-clean machine", func(t *testing.T) {
		nodes := connectNodes(t, env)
		if err := uninstall.Run(ctx, uninstall.Options{Nodes: nodes, Out: testWriter{t}}); err != nil {
			t.Fatal(err)
		}
		assertHost(t, ctx, nodes[0], map[string]string{
			"k3s binary is gone":     "absent",
			"k3s service is gone":    "absent",
			"platform state is gone": "absent",
		})
	})
}

func countStatus(events []stageEvent, status string) int {
	n := 0
	for _, e := range events {
		if e.Status == status {
			n++
		}
	}
	return n
}

// assertOrderedLifecycles checks that the stream carries started then a
// terminal status for each of the thirteen stages, in the contract's order.
func assertOrderedLifecycles(t *testing.T, seen []stageEvent) {
	t.Helper()

	// Only the last attempt at each stage matters: earlier lanes on this
	// journal left failed transitions in the stream's history.
	type lifecycle struct{ startedAt, terminalAt int }
	positions := map[string]*lifecycle{}
	for i, e := range seen {
		lc := positions[e.Stage]
		if lc == nil {
			lc = &lifecycle{startedAt: -1, terminalAt: -1}
			positions[e.Stage] = lc
		}
		switch e.Status {
		case "started":
			// A new attempt supersedes the previous lifecycle.
			if lc.terminalAt >= 0 {
				lc.terminalAt = -1
			}
			lc.startedAt = i
		case "completed", "failed":
			lc.terminalAt = i
		}
	}

	prevTerminal := -1
	for _, stage := range install.StageNames {
		lc := positions[stage]
		if lc == nil {
			t.Errorf("stage %s never reached the event stream at all", stage)
			continue
		}
		if lc.startedAt < 0 {
			t.Errorf("stage %s never reported `started`: nothing can clear its install_failed state", stage)
			continue
		}
		if lc.terminalAt < 0 {
			t.Errorf("stage %s reported `started` but no terminal status", stage)
			continue
		}
		if lc.terminalAt < lc.startedAt {
			t.Errorf("stage %s reported its terminal status before `started`", stage)
		}
		if lc.startedAt < prevTerminal {
			t.Errorf("stage %s started before the previous stage finished — the stream is out of order", stage)
		}
		prevTerminal = lc.terminalAt
	}

	if t.Failed() {
		t.Log(summarize(seen))
	}
}

func summarize(seen []stageEvent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d install_stage_update events observed:\n", len(seen))
	for _, e := range seen {
		fmt.Fprintf(&b, "  %-21s %-10s %s\n", e.Stage, e.Status, e.Component)
	}
	return b.String()
}
