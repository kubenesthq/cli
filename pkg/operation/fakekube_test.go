package operation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"

	"kubenest.io/cli/pkg/sshx"
)

// fakeKube is an in-memory kube-system with real resourceVersion semantics.
//
// It stands in for the SSH transport, so it sees exactly what the host sees:
// `sudo -n k3s kubectl …` command lines, and documents arriving over stdin. A
// write carrying an out-of-date resourceVersion is refused with the text a real
// API server returns, creating an existing object is AlreadyExists, and every
// accepted write bumps the revision — a fake that let a stale write through
// would make the compare-and-swap untestable, which is the one thing this
// package cannot afford.
type fakeKube struct {
	t  *testing.T
	mu sync.Mutex

	objects map[string]map[string]any
	nextRV  int
	// replies are answers for commands that are not record traffic: the
	// read-only probes a resume runs, and anything else a test scripts.
	replies []fakeReply
	// calls is every command the store issued, in order, so a test can assert
	// that a resume submitted nothing.
	calls []string
	// writes counts accepted create/replace documents.
	writes int
}

type fakeReply struct {
	prefix string
	res    sshx.Result
}

var (
	getCmdRe     = regexp.MustCompile(`^sudo -n k3s kubectl get configmap ([a-z0-9.-]+) -n kube-system -o json$`)
	createDocRe  = regexp.MustCompile(`^sudo -n k3s kubectl create -f - -o json$`)
	replaceDocRe = regexp.MustCompile(`^sudo -n k3s kubectl replace -f - -o json$`)
)

func newFakeKube(t *testing.T) *fakeKube {
	t.Helper()
	return &fakeKube{t: t, objects: map[string]map[string]any{}}
}

// script answers any command with this prefix.
func (k *fakeKube) script(prefix string, res sshx.Result) {
	k.replies = append(k.replies, fakeReply{prefix: prefix, res: res})
}

func (k *fakeKube) Run(_ context.Context, command string) (sshx.Result, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, command)

	if m := getCmdRe.FindStringSubmatch(command); m != nil {
		obj, ok := k.objects[m[1]]
		if !ok {
			return sshx.Result{ExitCode: 1, Stderr: fmt.Sprintf("Error from server (NotFound): configmaps %q not found\n", m[1])}, nil
		}
		return sshx.Result{Stdout: k.encode(obj)}, nil
	}
	for _, r := range k.replies {
		if strings.HasPrefix(command, r.prefix) {
			return r.res, nil
		}
	}
	k.t.Fatalf("unscripted command: %q", command)
	return sshx.Result{}, nil
}

func (k *fakeKube) RunInput(_ context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, command)

	verb := ""
	switch {
	case createDocRe.MatchString(command):
		verb = "create"
	case replaceDocRe.MatchString(command):
		verb = "replace"
	default:
		k.t.Fatalf("unscripted streamed command: %q", command)
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		k.t.Fatalf("reading stdin for %q: %v", command, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		k.t.Fatalf("the document on stdin of %q is not JSON: %v", command, err)
	}
	meta, _ := doc["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if name == "" {
		k.t.Fatalf("%s arrived with no metadata.name: %s", verb, string(raw))
	}
	existing, exists := k.objects[name]

	switch verb {
	case "create":
		if exists {
			return sshx.Result{ExitCode: 1, Stderr: fmt.Sprintf("Error from server (AlreadyExists): error when creating \"STDIN\": configmaps %q already exists\n", name)}, nil
		}
	case "replace":
		if !exists {
			return sshx.Result{ExitCode: 1, Stderr: fmt.Sprintf("Error from server (NotFound): configmaps %q not found\n", name)}, nil
		}
		want, _ := meta["resourceVersion"].(string)
		have, _ := existing["metadata"].(map[string]any)["resourceVersion"].(string)
		if want != have {
			return sshx.Result{ExitCode: 1, Stderr: fmt.Sprintf(
				"Error from server (Conflict): error when replacing \"STDIN\": Operation cannot be fulfilled on configmaps %q: the object has been modified; please apply your changes to the latest version and try again\n", name)}, nil
		}
	}

	k.nextRV++
	k.writes++
	meta["resourceVersion"] = fmt.Sprintf("%d", k.nextRV)
	doc["metadata"] = meta
	k.objects[name] = doc
	return sshx.Result{Stdout: k.encode(doc)}, nil
}

func (k *fakeKube) encode(obj map[string]any) string {
	raw, err := json.Marshal(obj)
	if err != nil {
		k.t.Fatalf("encoding the fake's object: %v", err)
	}
	return string(raw)
}

// object returns a stored ConfigMap document, failing the test when it is
// absent, because every caller wants "it is there".
func (k *fakeKube) object(name string) map[string]any {
	k.t.Helper()
	k.mu.Lock()
	defer k.mu.Unlock()
	obj, ok := k.objects[name]
	if !ok {
		k.t.Fatalf("%s does not exist; the cluster holds %v", name, k.names())
	}
	return obj
}

func (k *fakeKube) exists(name string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, ok := k.objects[name]
	return ok
}

func (k *fakeKube) names() []string {
	out := make([]string, 0, len(k.objects))
	for name := range k.objects {
		out = append(out, name)
	}
	return out
}

// liveRecord decodes the record the cluster actually holds.
func (k *fakeKube) liveRecord(name string) *Record {
	k.t.Helper()
	obj := k.object(name)
	data, _ := obj["data"].(map[string]any)
	raw, _ := data[dataKey].(string)
	if raw == "" {
		k.t.Fatalf("%s carries no %s", name, dataKey)
	}
	var rec Record
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		k.t.Fatalf("decoding %s: %v", name, err)
	}
	return &rec
}

func (k *fakeKube) resourceVersion(name string) string {
	k.t.Helper()
	meta, _ := k.object(name)["metadata"].(map[string]any)
	rv, _ := meta["resourceVersion"].(string)
	return rv
}

// setTokenWithoutBumpingRevision rewrites the live record's ownership token at
// the same resourceVersion.
//
// A real API server cannot do this — every write bumps the revision — and that
// is exactly why it is here: the requirement is that a mismatched token is
// refused even when the resourceVersion the handle read is current, so the test
// has to be able to produce that state. If the refusal only ever fired on a
// conflict it would be the resourceVersion doing the work, not the token.
func (k *fakeKube) setTokenWithoutBumpingRevision(name, token string) {
	k.t.Helper()
	obj := k.object(name)
	data, _ := obj["data"].(map[string]any)
	raw, _ := data[dataKey].(string)
	var rec Record
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		k.t.Fatalf("decoding %s: %v", name, err)
	}
	rec.Executor.Token = token
	next, err := json.Marshal(&rec)
	if err != nil {
		k.t.Fatalf("encoding %s: %v", name, err)
	}
	data[dataKey] = string(next)
	obj["data"] = data
}

// writeCount is how many documents the cluster accepted.
func (k *fakeKube) writeCount() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.writes
}

func (k *fakeKube) commandCalls(prefix string) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []string
	for _, c := range k.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// spyRunner is the remote transport Guarded decorates. It answers whatever the
// test scripts and hands every submission to onRun, which is where a test
// asserts what the record said at the moment the action was submitted.
type spyRunner struct {
	t     *testing.T
	calls []string
	onRun func(command string)
	res   sshx.Result
	err   error
}

func (s *spyRunner) Run(_ context.Context, command string) (sshx.Result, error) {
	s.calls = append(s.calls, command)
	if s.onRun != nil {
		s.onRun(command)
	}
	return s.res, s.err
}

func (s *spyRunner) RunInput(_ context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	if _, err := io.ReadAll(stdin); err != nil {
		s.t.Fatalf("reading stdin: %v", err)
	}
	s.calls = append(s.calls, command)
	if s.onRun != nil {
		s.onRun(command)
	}
	return s.res, s.err
}
