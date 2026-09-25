package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/component/day2"
	"kubenest.io/cli/pkg/hostpolicy"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/window"
)

// fakeNode is one Kubernetes Node as the cluster reports it: its name, the
// addresses the cluster knows it by, its role, and its labels.
type fakeNode struct {
	name      string
	addresses []string
	server    bool
	labels    map[string]string
}

func (n *fakeNode) hold() (string, bool) {
	value, ok := n.labels[day2.NoAutoRebootLabel]
	return value, ok
}

// fakeRun is one command the stage ran, on which host, with any streamed
// payload.
type fakeRun struct {
	host    string
	command string
	stdin   string
}

// fakeCluster is a cluster with one host filesystem per node: enough for the
// drop-in, the effective-policy read and the label writes to be exercised end
// to end. A fake that only recorded commands could not tell a converged re-run
// from a rewrite — the second run's decision is made from what the first one
// wrote, exactly as on a real host.
type fakeCluster struct {
	t     *testing.T
	nodes []*fakeNode
	files map[string]map[string]string
	runs  []fakeRun
	// laterRules is a configuration file that sorts AFTER the platform's
	// drop-in, as one line of `apt-config dump`. apt reads the directory in
	// lexicographic order, so a file after ours has the last word — which is
	// why the stage reads the EFFECTIVE policy back instead of trusting the
	// file it wrote.
	laterRules string
}

func newFakeCluster(t *testing.T, nodes ...*fakeNode) *fakeCluster {
	t.Helper()
	c := &fakeCluster{t: t, files: map[string]map[string]string{}}
	for _, n := range nodes {
		c.files[n.name] = map[string]string{}
		c.nodes = append(c.nodes, n)
	}
	return c
}

func (c *fakeCluster) node(name string) *fakeNode {
	for _, n := range c.nodes {
		if n.name == name {
			return n
		}
	}
	return nil
}

// filesOf is the host's filesystem.
func (c *fakeCluster) filesOf(node string) map[string]string { return c.files[node] }

func (c *fakeCluster) runner(n *fakeNode) *fakeRunner { return &fakeRunner{cluster: c, node: n} }

// fakeRunner is one node's connection. It answers as that host would: the
// policy files, the APT configuration, and — on the server — kubectl.
type fakeRunner struct {
	cluster *fakeCluster
	node    *fakeNode
}

func (r *fakeRunner) Run(_ context.Context, command string) (sshx.Result, error) {
	return r.cluster.run(r.node, command, nil)
}

func (r *fakeRunner) RunInput(_ context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return sshx.Result{}, err
	}
	return r.cluster.run(r.node, command, payload)
}

func (c *fakeCluster) run(node *fakeNode, command string, stdin []byte) (sshx.Result, error) {
	c.runs = append(c.runs, fakeRun{host: node.name, command: command, stdin: string(stdin)})
	switch {
	case strings.HasPrefix(command, "sudo -n cat "):
		// `|| true` at the end is the writer's own contract: an absent file
		// reads as empty.
		file, _, _ := strings.Cut(strings.TrimPrefix(command, "sudo -n cat "), " ")
		return sshx.Result{Stdout: c.files[node.name][file]}, nil
	case command == "apt-config dump":
		return sshx.Result{Stdout: c.effective(node)}, nil
	case strings.HasPrefix(command, "sudo -n install -d "):
		return sshx.Result{}, nil
	case strings.Contains(command, "mv -f "):
		_, dest := writeTarget(command)
		c.files[node.name][dest] = string(stdin)
		return sshx.Result{}, nil
	case command == "sudo -n k3s kubectl get nodes -o json":
		return sshx.Result{Stdout: c.nodesJSON()}, nil
	case strings.HasPrefix(command, "sudo -n k3s kubectl label node "):
		c.applyLabelCommand(command)
		return sshx.Result{}, nil
	case strings.HasPrefix(command, "sudo -n k3s kubectl get daemonset "):
		// kured, carrying no lock: this stage places the hold and never takes
		// kured's lock, so the only lock it ever clears is one the label has
		// just orphaned (P1's finding, day2.HoldAutomaticReboots).
		return sshx.Result{Stdout: `{"metadata":{"resourceVersion":"100"}}`}, nil
	}
	c.t.Fatalf("unscripted command on %s: %q", node.name, command)
	return sshx.Result{}, nil
}

// effective renders the host's `apt-config dump`: the platform's own policy
// when the drop-in is in place, Ubuntu's default — which also installs the
// release pocket — when it is not. The `#clear` in the drop-in is why the
// first case lists one origin and the second three.
func (c *fakeCluster) effective(node *fakeNode) string {
	if c.files[node.name][hostpolicy.APTConfPath] == string(hostpolicy.DropIn()) {
		return `Unattended-Upgrade::Allowed-Origins "";` + "\n" +
			`Unattended-Upgrade::Allowed-Origins:: "${distro_id}:${distro_codename}-security";` + "\n" +
			`Unattended-Upgrade::Automatic-Reboot "false";` + "\n" +
			c.laterRules
	}
	return `Unattended-Upgrade::Allowed-Origins "";` + "\n" +
		`Unattended-Upgrade::Allowed-Origins:: "${distro_id}:${distro_codename}";` + "\n" +
		`Unattended-Upgrade::Allowed-Origins:: "${distro_id}:${distro_codename}-security";` + "\n" +
		`Unattended-Upgrade::Allowed-Origins:: "${distro_id}ESMApps:${distro_codename}-apps-security";` + "\n" +
		`Unattended-Upgrade::Automatic-Reboot "false";` + "\n"
}

func (c *fakeCluster) nodesJSON() string {
	type address struct {
		Type    string `json:"type"`
		Address string `json:"address"`
	}
	type node struct {
		Metadata struct {
			Name   string            `json:"name"`
			UID    string            `json:"uid"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Status struct {
			Addresses []address `json:"addresses"`
		} `json:"status"`
	}
	var out struct {
		Items []node `json:"items"`
	}
	for _, n := range c.nodes {
		var item node
		item.Metadata.Name = n.name
		item.Metadata.UID = "uid-" + n.name
		item.Metadata.Labels = n.labels
		for i, a := range n.addresses {
			kind := "InternalIP"
			if i == 0 && a == n.name {
				kind = "Hostname"
			}
			item.Status.Addresses = append(item.Status.Addresses, address{Type: kind, Address: a})
		}
		out.Items = append(out.Items, item)
	}
	b, err := json.Marshal(out)
	if err != nil {
		c.t.Fatal(err)
	}
	return string(b)
}

func (c *fakeCluster) applyLabelCommand(command string) {
	rest := strings.TrimPrefix(command, "sudo -n k3s kubectl label node ")
	name, rest, ok := strings.Cut(rest, " ")
	if !ok {
		c.t.Fatalf("a label command names no node: %q", command)
	}
	arg, _, _ := strings.Cut(rest, " ")
	node := c.node(name)
	if node == nil {
		c.t.Fatalf("a label command names a node the cluster does not have: %q", command)
	}
	// `kubectl label node n <key>-` removes a label; `<key>=<value>` sets one.
	if strings.HasSuffix(arg, "-") && !strings.Contains(arg, "=") {
		key := strings.TrimSuffix(arg, "-")
		if key != day2.NoAutoRebootLabel {
			c.t.Fatalf("a label command touches %q, not the reboot hold: %q", key, command)
		}
		delete(node.labels, key)
		return
	}
	key, value, _ := strings.Cut(arg, "=")
	if key != day2.NoAutoRebootLabel {
		c.t.Fatalf("a label command touches %q, not the reboot hold: %q", key, command)
	}
	node.labels[key] = value
}

// writeTarget pulls the temp file and the final path out of the write command,
// the same parse pkg/hostpolicy's own test makes: the shape IS the discipline
// under test, so the fake applies whatever the code sent.
func writeTarget(command string) (tmp, dest string) {
	_, rest, ok := strings.Cut(command, "mv -f ")
	if !ok {
		return "", ""
	}
	tmp, rest, _ = strings.Cut(rest, " ")
	dest, _, _ = strings.Cut(rest, " ")
	return tmp, dest
}

// isWriteCommand reports whether a command CHANGED a host: a policy file
// renamed into place, the directory it lives in, or a reboot-hold label. A
// read is not a write, and that difference is the acceptance of a converged
// re-run.
func isWriteCommand(command string) bool {
	return strings.Contains(command, "mv -f ") ||
		strings.HasPrefix(command, "sudo -n install -d ") ||
		strings.HasPrefix(command, "sudo -n k3s kubectl label node ")
}

// writesIn names the commands that changed a host, with the host they changed.
func writesIn(runs []fakeRun) []string {
	var out []string
	for _, r := range runs {
		if isWriteCommand(r.command) {
			out = append(out, r.host+": "+r.command)
		}
	}
	return out
}

// newUpgradeJournal is a journal in a temp dir, so the state the stage records
// is persisted and read back the way a resume reads it.
func newUpgradeJournal(t *testing.T) *stages.Journal {
	t.Helper()
	j, err := stages.OpenJournal(filepath.Join(t.TempDir(), "upgrade.json"),
		stages.Identity{Kind: Kind, Cluster: "prod-1"})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// sessionWith builds a session over this cluster and a caller-supplied
// journal, reading the recorded state back exactly as Connect does — so a test
// that hands over a predecessor's journal is testing a resume and not a
// process that happens to have the record in memory.
func (c *fakeCluster) sessionWith(t *testing.T, j *stages.Journal) *Session {
	t.Helper()
	s := &Session{
		ID:   "run-1",
		Opts: Options{Cluster: "prod-1", To: "1.2"},
		Jnl:  j,
		Out:  io.Discard,
	}
	for _, n := range c.nodes {
		s.Nodes = append(s.Nodes, Node{Address: n.addresses[0], Server: n.server, Runner: c.runner(n)})
	}
	if err := j.DecodeState(&s.Record); err != nil {
		t.Fatal(err)
	}
	return s
}

func (c *fakeCluster) session(t *testing.T) *Session {
	t.Helper()
	return c.sessionWith(t, newUpgradeJournal(t))
}

// hostPolicyStageFor returns the stage exactly as Plan wires it, so the tests
// exercise the registration and the window bind rather than a copy of them.
func hostPolicyStageFor(t *testing.T, s *Session) stages.Stage {
	t.Helper()
	for _, stage := range Plan(s) {
		if stage.Name == StageHostPolicy {
			return stage
		}
	}
	t.Fatalf("Plan does not register the %s stage", StageHostPolicy)
	return stages.Stage{}
}

func runHostPolicy(t *testing.T, s *Session) error {
	t.Helper()
	return hostPolicyStageFor(t, s).Run(context.Background())
}

// assertHoldsAreThePlatforms is the postcondition both the drop-in half and a
// resume re-verify.
func assertDropInIsInForce(t *testing.T, c *fakeCluster) {
	t.Helper()
	for _, n := range c.nodes {
		if got := c.filesOf(n.name)[hostpolicy.APTConfPath]; got != string(hostpolicy.DropIn()) {
			t.Errorf("%s holds %q, not the platform's drop-in", n.name, got)
		}
		if got := c.filesOf(n.name)[hostpolicy.NeedrestartConfPath]; got != string(hostpolicy.NeedrestartConf()) {
			t.Errorf("%s holds %q, not the platform's needrestart list", n.name, got)
		}
	}
}

// The host step is the LAST thing an existing cluster is given while retreating
// is still cheap: a failure in it must still cost a Helm revert and not a
// datastore restore, and the nodes must be held before the one stage that takes
// them down deliberately.
func TestTheStageIsInsertedBeforeThePointOfNoReturn(t *testing.T) {
	if PointOfNoReturn != StageKubernetes {
		t.Fatalf("the point of no return is %q, want %q", PointOfNoReturn, StageKubernetes)
	}

	var seen int
	for i, name := range StageNames {
		if name != StageHostPolicy {
			continue
		}
		seen++
		if i+1 >= len(StageNames) || StageNames[i+1] != StageKubernetes {
			t.Fatalf("%s is at position %d and is not immediately before %s: the host step is on the reversible side of the point of no return, and that place is the argument for it", name, i+1, StageKubernetes)
		}
		if i == 0 {
			t.Fatalf("%s must not be the first stage", name)
		}
	}
	if seen != 1 {
		t.Fatalf("StageNames names %s %d times, want exactly 1", StageHostPolicy, seen)
	}
	if windowExempt(StageHostPolicy) {
		t.Errorf("%s must not be exempt from the window: it writes to every host, and it is not one of the four stages whose work a clock must not interrupt", StageHostPolicy)
	}

	// The sequence and the vocabulary agree, and every stage is wired: a name
	// in StageNames with no Run is a stage that stops the upgrade.
	planned := Plan(&Session{})
	if len(planned) != len(StageNames) {
		t.Fatalf("Plan wires %d stages, StageNames names %d", len(planned), len(StageNames))
	}
	for i, stage := range planned {
		if stage.Name != StageNames[i] {
			t.Errorf("Plan[%d] is %q, StageNames[%d] is %q", i, stage.Name, i, StageNames[i])
		}
		if stage.Run == nil {
			t.Errorf("Plan[%d] (%s) has no Run: it would fail the upgrade as unimplemented", i, stage.Name)
		}
	}
}

// A re-run on a cluster that already carries what this stage writes changes
// nothing and reports no change. The state under test is the one an interrupted
// run leaves — policy in force, hold applied — and it is also how the platform
// holds a cluster whose window is still applying.
func TestTheStageConvergesOnAnAlreadyPolicyDHost(t *testing.T) {
	server := &fakeNode{name: "kb-server", addresses: []string{"203.0.113.10"}, server: true,
		labels: map[string]string{day2.NoAutoRebootLabel: day2.NoAutoRebootValue}}
	agent := &fakeNode{name: "kb-agent", addresses: []string{"203.0.113.11"},
		labels: map[string]string{day2.NoAutoRebootLabel: day2.NoAutoRebootValue}}
	c := newFakeCluster(t, server, agent)
	for _, n := range c.nodes {
		c.filesOf(n.name)[hostpolicy.APTConfPath] = string(hostpolicy.DropIn())
		c.filesOf(n.name)[hostpolicy.NeedrestartConfPath] = string(hostpolicy.NeedrestartConf())
	}

	for run := 1; run <= 2; run++ {
		s := c.session(t)
		if err := runHostPolicy(t, s); err != nil {
			t.Fatalf("run %d of an already-policy'd cluster: %v", run, err)
		}
		if w := writesIn(c.runs); len(w) != 0 {
			t.Fatalf("run %d wrote to a cluster that already carried the policy and the hold:\n  %s", run, strings.Join(w, "\n  "))
		}
		for _, state := range s.Record.HostPolicy {
			if len(state.PolicyFiles) != 0 {
				t.Errorf("run %d reports writing %q on %s, which already held the platform's policy", run, state.PolicyFiles, state.Node)
			}
			if state.HoldApplied {
				t.Errorf("run %d reports applying the hold on %s, which already carried it", run, state.Node)
			}
			if !state.HoldBefore || state.HoldValue != day2.NoAutoRebootValue {
				t.Errorf("run %d did not record %s's pre-operation value as the hold it already carried: %+v", run, state.Node, state)
			}
		}
	}

	// The other half of the acceptance: the same code DOES write when
	// something is missing, so the no-write case above is not vacuous — and
	// when it does, the state it leaves is the state it finds.
	t.Run("a cluster holding neither is written to", func(t *testing.T) {
		fresh := newFakeCluster(t,
			&fakeNode{name: "kb-server", addresses: []string{"203.0.113.20"}, server: true, labels: map[string]string{}},
			&fakeNode{name: "kb-agent", addresses: []string{"203.0.113.21"}, labels: map[string]string{}},
		)
		s := fresh.session(t)
		if err := runHostPolicy(t, s); err != nil {
			t.Fatal(err)
		}
		if w := writesIn(fresh.runs); len(w) == 0 {
			t.Fatal("a cluster with neither the policy nor the hold must be written to")
		}
		assertDropInIsInForce(t, fresh)
		// What was REPLACED is recorded, not silently overwritten: Ubuntu's
		// own rule installs the release pocket as well, and the operator can
		// read afterwards what this host's update policy used to be.
		for _, state := range s.Record.HostPolicy {
			if state.PolicyBefore == "" {
				t.Errorf("%s ran Ubuntu's own APT policy, and the record must say what the platform's policy replaced: %+v", state.Node, state)
			}
		}

		before := snapshotHosts(fresh)
		if err := runHostPolicy(t, fresh.session(t)); err != nil {
			t.Fatal(err)
		}
		if after := snapshotHosts(fresh); after != before {
			t.Errorf("a second run left the hosts different from how it found them:\n%s\nwant\n%s", after, before)
		}
	})
}

// A host whose own configuration keeps contradicting the platform's policy —
// a file that sorts after the drop-in, which apt reads last — fails the stage
// by name rather than passing silently on the strength of the file we wrote.
func TestAPolicyThatStillContradictsIsRefusedByNode(t *testing.T) {
	server := &fakeNode{name: "kb-server", addresses: []string{"203.0.113.30"}, server: true, labels: map[string]string{}}
	agent := &fakeNode{name: "kb-agent", addresses: []string{"203.0.113.31"}, labels: map[string]string{}}
	c := newFakeCluster(t, server, agent)
	// 99-customer.conf, read after 52kubenest-unattended-upgrades: it puts the
	// release pocket, and with it every non-security update, back.
	c.laterRules = `Unattended-Upgrade::Allowed-Origins:: "${distro_id}:${distro_codename}";` + "\n"

	err := runHostPolicy(t, c.session(t))
	if err == nil {
		t.Fatal("a host whose effective policy still installs the release pocket must fail the stage, not pass on the strength of the file we wrote")
	}
	for _, want := range []string{server.addresses[0], "${distro_id}:${distro_codename}"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name the host and the offending origin (%q): %v", want, err)
		}
	}
	// It stopped there: the holds are a LATER step, so nothing was labelled on
	// a host whose policy is not what the platform writes.
	for _, n := range c.nodes {
		if value, ok := n.hold(); ok {
			t.Errorf("%s was labelled %q while its APT policy was still wrong: the stage must stop at the first host it cannot converge", n.name, value)
		}
	}
}

// snapshotHosts is every host's files and labels, as one comparable string: the
// state a stage run must leave alone to have converged.
func snapshotHosts(c *fakeCluster) string {
	var b strings.Builder
	for _, n := range c.nodes {
		files := make([]string, 0, len(c.filesOf(n.name)))
		for path, content := range c.filesOf(n.name) {
			files = append(files, path+"="+content)
		}
		sort.Strings(files)
		labels := make([]string, 0, len(n.labels))
		for key, value := range n.labels {
			labels = append(labels, key+"="+value)
		}
		sort.Strings(labels)
		b.WriteString(n.name + " files: " + strings.Join(files, " ") + " labels: " + strings.Join(labels, " ") + "\n")
	}
	return b.String()
}

// A resume RE-CHECKS rather than REPEATS: an interrupted run is re-run over the
// state it left — reading the hosts rather than trusting the last run's
// intentions — and a completed one is skipped from the journal alone with its
// postcondition verified on the hosts rather than re-applied.
func TestAResumeReChecksRatherThanRepeating(t *testing.T) {
	// What a killed laptop leaves: the policy written and the holds applied,
	// with the process gone before it lifted them. The labels are the hold
	// THIS operation placed, which is exactly why the restore cannot be
	// derived from them.
	server := &fakeNode{name: "kb-server", addresses: []string{"203.0.113.10"}, server: true,
		labels: map[string]string{day2.NoAutoRebootLabel: day2.NoAutoRebootValue}}
	agent := &fakeNode{name: "kb-agent", addresses: []string{"203.0.113.11"},
		labels: map[string]string{day2.NoAutoRebootLabel: day2.NoAutoRebootValue}}
	c := newFakeCluster(t, server, agent)
	for _, n := range c.nodes {
		c.filesOf(n.name)[hostpolicy.APTConfPath] = string(hostpolicy.DropIn())
		c.filesOf(n.name)[hostpolicy.NeedrestartConfPath] = string(hostpolicy.NeedrestartConf())
	}

	j := newUpgradeJournal(t)
	s := c.sessionWith(t, j)
	s.Record.HostPolicy = []hostPolicyNode{
		{Node: server.name, Address: server.addresses[0], Server: true, HoldApplied: true},
		{Node: agent.name, Address: agent.addresses[0], HoldApplied: true},
	}
	if err := j.SetState(&s.Record); err != nil {
		t.Fatal(err)
	}
	// The journal has the stage STARTED and no terminal entry: what "in
	// progress" looks like, and the only thing a resume may read the stage's
	// state from.
	if err := j.Append(stages.Entry{Stage: StageHostPolicy, Status: stages.StatusStarted}); err != nil {
		t.Fatal(err)
	}
	if _, done := j.Completed(StageHostPolicy); done {
		t.Fatal("this fixture is the INTERRUPTED case: the stage must not read as completed")
	}

	// A second process, reading the journal rather than the first one's memory.
	resumed := c.sessionWith(t, j)
	before := len(c.runs)
	result, err := stages.Execute(context.Background(), &pauseController{journal: j},
		[]stages.Stage{hostPolicyStageFor(t, resumed)})
	if err != nil {
		t.Fatalf("the resume must converge, not fail: %v", err)
	}
	if len(result.Ran) != 1 || result.Ran[0] != StageHostPolicy {
		t.Fatalf("the interrupted stage must be re-run, ran %v", result.Ran)
	}
	recheck := c.runs[before:]
	if !readSomething(recheck) {
		t.Fatal("a resume must RE-CHECK: the re-run read neither the policy nor the cluster's nodes")
	}
	// It did not repeat the work: the policy was not rewritten, and a hold
	// already in place was not applied again. The one write it makes is the
	// restore the killed run never got to — finishing, not repeating.
	wrote := writesIn(recheck)
	if len(wrote) != 1 || !strings.Contains(wrote[0], day2.NoAutoRebootLabel) {
		t.Errorf("the resume's writes must be exactly the one restore it inherited, got\n  %s", strings.Join(wrote, "\n  "))
	}
	assertDropInIsInForce(t, c)
	if value, ok := server.hold(); !ok || value != day2.NoAutoRebootValue {
		t.Errorf("the server's hold must survive a resume, got %q present=%v", value, ok)
	}
	if value, ok := agent.hold(); ok {
		t.Errorf("the restore the killed run never made must happen on the resume: the agent still carries %q", value)
	}
	// And the record it wrote back is the pre-operation one, not the hold it
	// found on the cluster.
	for _, state := range resumed.Record.HostPolicy {
		if state.Node == agent.name && (state.HoldBefore || state.HoldValue != "") {
			t.Errorf("the resume recorded the operation's hold as the agent's pre-operation value: %+v", state)
		}
	}

	// And the completed half: the engine skips the stage from the journal, and
	// the postcondition is verified by reading the hosts.
	if _, done := j.Completed(StageHostPolicy); !done {
		t.Fatal("the stage must read as completed once the resume ran it")
	}
	before = len(c.runs)
	result, err = stages.Execute(context.Background(), &pauseController{journal: j},
		[]stages.Stage{hostPolicyStageFor(t, resumed)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != StageHostPolicy {
		t.Errorf("a completed stage must be skipped, got skipped=%v ran=%v", result.Skipped, result.Ran)
	}
	if len(result.Ran) != 0 {
		t.Errorf("a completed stage must not run again, ran %v", result.Ran)
	}
	if len(c.runs) != before {
		t.Errorf("a skipped stage ran %d command(s): %v", len(c.runs)-before, c.runs[before:])
	}
	assertDropInIsInForce(t, c)
	if value, ok := server.hold(); !ok || value != day2.NoAutoRebootValue {
		t.Errorf("the server's hold must survive a skip, got %q present=%v", value, ok)
	}
	if value, ok := agent.hold(); ok {
		t.Errorf("the agent must still be unheld after a skip, got %q", value)
	}
}

// readSomething reports whether a run of commands read anything at all.
func readSomething(runs []fakeRun) bool {
	for _, r := range runs {
		if !isWriteCommand(r.command) {
			return true
		}
	}
	return false
}

// Servers keep the hold permanently in 1.2 — their reboots go through
// `kubenest node reboot` — and an agent does not, unless it already carried the
// hold, which an upgrade passing by must not release.
func TestServersAreLabelledAndAgentsAreNotUnlabelled(t *testing.T) {
	server := &fakeNode{name: "kb-server", addresses: []string{"203.0.113.10"}, server: true, labels: map[string]string{}}
	agent := &fakeNode{name: "kb-agent", addresses: []string{"203.0.113.11"}, labels: map[string]string{}}
	// A node that already carries the hold: `node add` has not recorded it
	// yet, or the platform is holding it while a new window applies.
	held := &fakeNode{name: "kb-agent-held", addresses: []string{"203.0.113.12"},
		labels: map[string]string{day2.NoAutoRebootLabel: day2.NoAutoRebootValue}}
	c := newFakeCluster(t, server, agent, held)

	s := c.session(t)
	if err := runHostPolicy(t, s); err != nil {
		t.Fatal(err)
	}

	if value, ok := server.hold(); !ok || value != day2.NoAutoRebootValue {
		t.Errorf("the server must carry %s=%s permanently after the upgrade (got %q present=%v): in 1.2 a server never reboots by itself", day2.NoAutoRebootLabel, day2.NoAutoRebootValue, value, ok)
	}
	if value, ok := agent.hold(); ok {
		t.Errorf("the agent's hold must be lifted when the stage completes so it can still reboot automatically in its window, got %q", value)
	}
	if value, ok := held.hold(); !ok || value != day2.NoAutoRebootValue {
		t.Errorf("a node that already carried the hold must keep it: an upgrade passing by does not release it (got %q present=%v)", value, ok)
	}

	// The hold really was applied to the two nodes that needed it, for the
	// duration of the operation — the record is what says so, and it is why
	// the agent's label went on and came back off.
	applied := map[string]bool{}
	for _, state := range s.Record.HostPolicy {
		applied[state.Node] = state.HoldApplied
	}
	if !applied[server.name] {
		t.Errorf("the server did not carry the hold before this operation, so the stage must have applied it")
	}
	if !applied[agent.name] {
		t.Errorf("the agent must be held for the duration of the operation: the upgrade is disruptive")
	}
	if applied[held.name] {
		t.Errorf("the stage reports applying a hold %s already carried: a write it must not make", held.name)
	}
}

// Outside the window the whole upgrade is REFUSED, naming the next opening in
// local time and UTC — and the host step is not reachable before that refusal.
func TestTheStageRefusesOutsideTheWindowAndNamesTheOpeningInLocalTimeAndUTC(t *testing.T) {
	// 02:00-06:00 on Saturdays, in a zone that is not UTC, so that "local time"
	// and "UTC" are two different renderings of one instant.
	w, err := window.Parse(window.Spec{Days: []string{"sat"}, Start: "02:00", End: "06:00", Timezone: "Asia/Kolkata"})
	if err != nil {
		t.Fatal(err)
	}
	if w.Location == nil {
		t.Fatal("the fixture window has no zone, so it cannot name an opening in local time")
	}
	closed := time.Date(2026, 8, 22, 7, 0, 0, 0, w.Location) // Saturday, 07:00 local: the window shut an hour ago
	c := newFakeCluster(t,
		&fakeNode{name: "kb-server", addresses: []string{"203.0.113.10"}, server: true, labels: map[string]string{}},
		&fakeNode{name: "kb-agent", addresses: []string{"203.0.113.11"}, labels: map[string]string{}},
	)
	s := c.session(t)
	s.Window = &w
	s.Opts.Now = func() time.Time { return closed }

	next, ok := w.NextOpen(closed)
	if !ok {
		t.Fatal("the fixture window has no next opening, so this test cannot name one")
	}
	local := next.In(w.Location).Format("Mon 2 Jan 15:04 MST")
	utc := next.UTC().Format("Mon 2 Jan 15:04 MST")
	if local == utc {
		t.Fatalf("the fixture must render the opening differently in local time (%s) and UTC (%s)", local, utc)
	}

	// THE GATE owns the refusal: an upgrade outside its window does not start.
	gate := checkWindow(s)
	if gate.Passed {
		t.Fatalf("the window gate must refuse an upgrade outside the window: %s", gate.Detail)
	}
	for _, want := range []string{local, utc} {
		if !strings.Contains(gate.Detail, want) {
			t.Errorf("the refusal must name the next opening as %q, got %q", want, gate.Detail)
		}
	}

	// And the host step is behind it: bound as Plan wires it, it stops before
	// writing anything and names the same opening.
	before := len(c.runs)
	err = hostPolicyStageFor(t, s).Run(context.Background())
	if err == nil {
		t.Fatal("the host step must not run outside the window, and must not write anything if it does")
	}
	if !errors.Is(err, stages.ErrPaused) {
		t.Fatalf("a closed window stops the sequence without failing it: %v", err)
	}
	for _, want := range []string{local, utc} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the stop must name the next opening as %q, got %q", want, err.Error())
		}
	}
	if len(c.runs) != before {
		t.Errorf("nothing may be touched outside the window, and %d command(s) ran: %v", len(c.runs)-before, c.runs[before:])
	}
}

// The restore puts back the value that was there BEFORE the operation, and the
// recorded value is the only thing that knows it: by the time a resume runs,
// the node's label is the operation's hold, so reading the cluster would
// restore the hold to itself and leave every agent held for ever.
func TestTheLabelIsRestoredToItsPreOperationValueOnCompletion(t *testing.T) {
	// The state a killed run leaves: the holds applied, the process gone
	// before it lifted them.
	server := &fakeNode{name: "kb-server", addresses: []string{"203.0.113.10"}, server: true,
		labels: map[string]string{day2.NoAutoRebootLabel: day2.NoAutoRebootValue}}
	agent := &fakeNode{name: "kb-agent", addresses: []string{"203.0.113.11"},
		labels: map[string]string{day2.NoAutoRebootLabel: day2.NoAutoRebootValue}}
	carried := &fakeNode{name: "kb-agent-carried", addresses: []string{"203.0.113.12"},
		labels: map[string]string{day2.NoAutoRebootLabel: day2.NoAutoRebootValue}}
	c := newFakeCluster(t, server, agent, carried)
	for _, n := range c.nodes {
		c.filesOf(n.name)[hostpolicy.APTConfPath] = string(hostpolicy.DropIn())
		c.filesOf(n.name)[hostpolicy.NeedrestartConfPath] = string(hostpolicy.NeedrestartConf())
	}

	s := c.session(t)
	s.Record.HostPolicy = []hostPolicyNode{
		{Node: server.name, Address: server.addresses[0], Server: true, HoldApplied: true},
		{Node: agent.name, Address: agent.addresses[0], HoldApplied: true},
		{Node: carried.name, Address: carried.addresses[0], HoldBefore: true, HoldValue: "true", HoldApplied: true},
	}
	if err := s.Jnl.SetState(&s.Record); err != nil {
		t.Fatal(err)
	}

	before := len(c.runs)
	if err := runHostPolicy(t, s); err != nil {
		t.Fatal(err)
	}

	if value, ok := agent.hold(); ok {
		t.Errorf("the agent carried no hold before this operation, so it must have none after it: %q=%q", day2.NoAutoRebootLabel, value)
	}
	if value, ok := carried.hold(); !ok || value != "true" {
		t.Errorf("the agent's pre-operation value was %q, and the restore must put THAT back, not the hold (got %q present=%v)", "true", value, ok)
	}
	if value, ok := server.hold(); !ok || value != day2.NoAutoRebootValue {
		t.Errorf("the server keeps the hold: in 1.2 a server never reboots by itself (got %q present=%v)", value, ok)
	}
	if w := writesIn(c.runs[before:]); len(w) == 0 {
		t.Fatal("this fixture must write the restores, or the assertions above pass for the wrong reason")
	}
}

// The stage names are a cross-repository contract: they are the journal's
// vocabulary AND the wire's payload.stage, so a name that exists on one side
// only is a 422 on every journal append.
func TestTheStageNamesMatchTheBackendStageLiteral(t *testing.T) {
	path := filepath.Join("..", "..", "..", "kubenest-backend", "app", "schemas", "cluster.py")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("kubenest-backend is not checked out beside this repo (at %s), so the wire vocabulary cannot be checked here: %v", path, err)
	}
	literal := installStageLiteral(t, string(source))

	required := map[string]bool{}
	// The installer's stages, plus the one stage its PLAN adds for
	// --control-plane and StageNames therefore does not carry (install.go's
	// own comment says so).
	for _, name := range append(append([]string{}, install.StageNames...), install.StageControlPlane) {
		required[name] = true
	}
	for _, name := range StageNames {
		required[name] = true
	}
	// The operator's own bring-up, which no CLI stage emits: it carries the
	// cluster lifecycle events and is documented as such beside the literal.
	operatorOnly := map[string]bool{"bootstrap": true}

	for name := range required {
		if !literal[name] {
			t.Errorf("this CLI writes stage %q to a cluster's journal and kubenest-backend's InstallStage does not name it, so every append carrying it is a 422", name)
		}
	}
	for name := range literal {
		if !required[name] && !operatorOnly[name] {
			t.Errorf("kubenest-backend's InstallStage names %q and no CLI stage emits it: the vocabulary may only grow together with the thing that speaks it", name)
		}
	}
	if len(literal) != len(required)+len(operatorOnly) {
		t.Errorf("the literal has %d values; the CLI's stages (%d) plus the operator's %d make %d, so the two lists have drifted",
			len(literal), len(required), len(operatorOnly), len(required)+len(operatorOnly))
	}
	if !literal[StageHostPolicy] {
		t.Errorf("%s is not in the backend's InstallStage literal: its name is on the wire like every other stage's", StageHostPolicy)
	}
}

// installStageLiteral reads the names out of `InstallStage = Literal[...]`.
func installStageLiteral(t *testing.T, source string) map[string]bool {
	t.Helper()
	start := strings.Index(source, "InstallStage = Literal[")
	if start < 0 {
		t.Fatal("kubenest-backend/app/schemas/cluster.py does not declare InstallStage = Literal[")
	}
	block := source[start:]
	if end := strings.Index(block, "]"); end >= 0 {
		block = block[:end]
	}
	out := map[string]bool{}
	quoted := regexp.MustCompile(`"([a-z0-9-]+)"`)
	for _, line := range strings.Split(block, "\n") {
		if comment := strings.Index(line, "#"); comment >= 0 {
			line = line[:comment]
		}
		for _, match := range quoted.FindAllStringSubmatch(line, -1) {
			out[match[1]] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("no stage names were read out of the InstallStage literal")
	}
	return out
}
