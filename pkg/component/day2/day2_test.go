package day2

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/interlock"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

// testBundle is a bundle manifest of the shape the installer reads, carrying
// only what this package consumes: the kured pin and the two timeouts. The
// timeouts are arguments because the values they produce must MOVE when the
// manifest moves — a kured values document that ignores the manifest is a
// constant wearing the manifest's name.
func testBundle(t *testing.T, drain, reboot string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(`bundle: "1.1"
core:
  kured: 6.1.0
limits:
  timeouts:
    node-drain: ` + drain + `
    node-reboot: ` + reboot + `
`))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// kuredChartValues renders the chart and returns the values document kured's
// helm install actually receives — spec.valuesContent out of the HelmChart CR
// — so these tests cover the whole path the CLI writes into the auto-deploy
// directory, not the string building inside it.
func kuredChartValues(t *testing.T, chart k3s.HelmChart) string {
	t.Helper()
	doc, err := chart.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var cr struct {
		Spec struct {
			ValuesContent string `yaml:"valuesContent"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(doc, &cr); err != nil {
		t.Fatal(err)
	}
	if cr.Spec.ValuesContent == "" {
		t.Fatal("the kured HelmChart carries no values at all")
	}
	var anyDoc map[string]any
	if err := yaml.Unmarshal([]byte(cr.Spec.ValuesContent), &anyDoc); err != nil {
		t.Fatalf("kured values are not valid YAML: %v\n%s", err, cr.Spec.ValuesContent)
	}
	return cr.Spec.ValuesContent
}

func chartValues(t *testing.T, drain, reboot string) string {
	t.Helper()
	chart, err := Chart(testBundle(t, drain, reboot))
	if err != nil {
		t.Fatal(err)
	}
	return kuredChartValues(t, chart)
}

func configuration(t *testing.T, values string) map[string]any {
	t.Helper()
	var doc struct {
		Configuration map[string]any `yaml:"configuration"`
	}
	if err := yaml.Unmarshal([]byte(values), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Configuration == nil {
		t.Fatalf("kured values carry no configuration section:\n%s", values)
	}
	return doc.Configuration
}

// A held node must have no kured: the DaemonSet's node affinity excludes the
// hold label, and NotIn is what makes the label a HOLD rather than a switch
// that also empties the DaemonSet on a fresh cluster — a node where the key is
// absent matches NotIn, so only an explicit false is held out.
func TestKuredValuesHoldLabelledNodesOut(t *testing.T) {
	values := chartValues(t, "15m", "20m")
	var doc struct {
		Affinity struct {
			NodeAffinity struct {
				Required struct {
					NodeSelectorTerms []struct {
						MatchExpressions []struct {
							Key      string   `yaml:"key"`
							Operator string   `yaml:"operator"`
							Values   []string `yaml:"values"`
						} `yaml:"matchExpressions"`
					} `yaml:"nodeSelectorTerms"`
				} `yaml:"requiredDuringSchedulingIgnoredDuringExecution"`
			} `yaml:"nodeAffinity"`
		} `yaml:"affinity"`
	}
	if err := yaml.Unmarshal([]byte(values), &doc); err != nil {
		t.Fatal(err)
	}
	terms := doc.Affinity.NodeAffinity.Required.NodeSelectorTerms
	if len(terms) != 1 {
		t.Fatalf("%d nodeSelectorTerms, want 1 that kured is scheduled nowhere without:\n%s", len(terms), values)
	}
	exprs := terms[0].MatchExpressions
	if len(exprs) != 1 {
		t.Fatalf("%d matchExpressions, want the hold label alone:\n%s", len(exprs), values)
	}
	got := exprs[0]
	if got.Key != NoAutoRebootLabel || got.Operator != "NotIn" || len(got.Values) != 1 || got.Values[0] != NoAutoRebootValue {
		t.Errorf("affinity excludes %s %s %v, want %s NotIn [%s]: a labelled node keeps a kured pod and keeps rebooting otherwise",
			got.Key, got.Operator, got.Values, NoAutoRebootLabel, NoAutoRebootValue)
	}
}

// annotateNodes is what T2.7 reads to tell a planned reboot from a dead node,
// and drainTimeout is a hard bound in place of kured's default of 0, which
// means drain for ever — a hung drain would hold kured's lock and the whole
// sequence with it. Both come from the manifest: 7m here, where the released
// bundle says 15m, so a constant wearing the manifest's name fails.
func TestKuredValuesSetAnnotateNodesAndDrainTimeout(t *testing.T) {
	conf := configuration(t, chartValues(t, "7m", "20m"))
	if annotate, ok := conf["annotateNodes"].(bool); !ok || !annotate {
		t.Errorf("configuration.annotateNodes = %v, want true: without it no node carries kured's reboot-in-progress annotation", conf["annotateNodes"])
	}
	if got := conf["drainTimeout"]; got != "7m0s" {
		t.Errorf("configuration.drainTimeout = %v, want the bundle's limits.timeouts.node-drain (7m0s)", got)
	}
}

// With the label as the entire hold, the trigger must be the plain file the
// distro writes (/var/run/reboot-required, the chart's own default). A
// KubeNest-specific sentinel is a second hold mechanism nothing creates, and
// leaving one in the values is how a node waits for ever for a file that will
// never appear.
func TestKuredValuesDoNotRedirectTheSentinel(t *testing.T) {
	values := chartValues(t, "15m", "20m")
	if _, ok := configuration(t, values)["rebootSentinel"]; ok {
		t.Errorf("configuration.rebootSentinel is set; kured must watch the distro's own file:\n%s", values)
	}
	if strings.Contains(values, "kubenest-reboot-approved") {
		t.Errorf("the removed KubeNest sentinel is still in the values:\n%s", values)
	}
}

// The config is what the operator writes the WINDOW into, so the CLI must be
// able to render one that k3s matches to the kured chart it owns: the deploy
// controller pairs a HelmChartConfig with its HelmChart by metadata.name and
// metadata.namespace, and a config that matches nothing is silently ignored.
func TestHelmChartConfigTargetsTheKuredChart(t *testing.T) {
	chart, err := Chart(testBundle(t, "15m", "20m"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := k3s.HelmChartConfig{
		Name:       chart.Name,
		Namespace:  chart.TargetNamespace,
		ValuesYAML: chart.ValuesYAML,
	}.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
		Spec struct {
			ValuesContent string `yaml:"valuesContent"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.APIVersion != "helm.cattle.io/v1" || cfg.Kind != "HelmChartConfig" {
		t.Errorf("rendered %s/%s, want helm.cattle.io/v1 HelmChartConfig", cfg.APIVersion, cfg.Kind)
	}
	if cfg.Metadata.Name != chart.Name || cfg.Metadata.Namespace != chart.TargetNamespace {
		t.Errorf("config targets %s/%s, want the chart's %s/%s", cfg.Metadata.Namespace, cfg.Metadata.Name, chart.TargetNamespace, chart.Name)
	}
	if cfg.Spec.ValuesContent != chart.ValuesYAML {
		t.Errorf("spec.valuesContent is the values document verbatim; got %q", cfg.Spec.ValuesContent)
	}
	if err := yaml.Unmarshal([]byte(cfg.Spec.ValuesContent), &map[string]any{}); err != nil {
		t.Errorf("spec.valuesContent is not valid YAML: %v", err)
	}
}

// The lock TTL must outlive the longest operation that holds the lock and no
// more, or a CLI that dies holding it stops every node's automatic reboot for
// ever (P1: no --lock-ttl is exactly that failure, indefinitely). It is
// derived from the manifest — node-drain plus node-reboot, the two deadlines a
// node reboot is bounded by — so both fixtures below move the value.
func TestKuredValuesSetALockTtlLongerThanTheRebootTimeout(t *testing.T) {
	// The released bundle's numbers.
	released := testBundle(t, "15m", "20m")
	reboot, err := released.Limits.Timeouts.For("node-reboot")
	if err != nil {
		t.Fatal(err)
	}
	conf := configuration(t, chartValues(t, "15m", "20m"))
	ttl, err := time.ParseDuration(strings.TrimSpace(toString(conf["lockTtl"])))
	if err != nil {
		t.Fatalf("configuration.lockTtl = %v, which kured cannot parse as a duration: %v", conf["lockTtl"], err)
	}
	if ttl <= reboot {
		t.Errorf("configuration.lockTtl = %s, which does not exceed limits.timeouts.node-reboot (%s): a node that never becomes Ready would then keep the lock past the point the platform gave up on it", ttl, reboot)
	}

	// A different manifest produces a different TTL. 25m + 45m is 70m, not the
	// 35m the released bundle derives: the value tracks the manifest.
	other := configuration(t, chartValues(t, "25m", "45m"))
	if got := toString(other["lockTtl"]); got != "1h10m0s" {
		t.Errorf("configuration.lockTtl = %v for node-drain 25m + node-reboot 45m, want 1h10m0s: the TTL is derived, not a constant", got)
	}
}

func toString(v any) string {
	s, _ := v.(string)
	if s != "" {
		return s
	}
	return ""
}

// day2API scripts the four commands holding a node can make, so a test can
// assert which of them happened and in what order. An unscripted command is
// fatal: answering it with an empty success would hide a call nobody wanted.
type day2API struct {
	t      *testing.T
	ds     string
	labels int
}

func (a *day2API) respond(cmd string) (sshx.Result, error) {
	switch {
	case strings.HasPrefix(cmd, "sudo -n k3s kubectl label node "):
		a.labels++
		return sshx.Result{}, nil
	case cmd == "sudo -n k3s kubectl get daemonset -n kube-system kured -o json":
		return sshx.Result{Stdout: a.ds}, nil
	case strings.HasPrefix(cmd, "sudo -n k3s kubectl uncordon "):
		return sshx.Result{}, nil
	case cmd == "sudo -n k3s kubectl replace -f -":
		return sshx.Result{}, nil
	default:
		a.t.Fatalf("unscripted command: %q", cmd)
		return sshx.Result{}, nil
	}
}

// daemonSetWithLock renders kured's DaemonSet with the lock annotation kured
// would have written, so the hold is tested against the wire document and not
// against a struct this package also owns.
func daemonSetWithLock(t *testing.T, rv string, lock interlock.Value) string {
	t.Helper()
	b, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	return `{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"kured","namespace":"kube-system",` +
		`"resourceVersion":` + strconv.Quote(rv) + `,"annotations":{` + strconv.Quote(interlock.LockAnnotation) + `:` + strconv.Quote(string(b)) + `}},` +
		`"spec":{"updateStrategy":{"type":"RollingUpdate"}}}`
}

// P1's third finding, as a test: a node labelled while it held kured's lock
// kept that lock AND stayed cordoned, and with kured's concurrency of 1 that
// stops every other node's reboot for ever. Holding a node must therefore
// clear the lock it owns and uncordon it — after the label (or kured could
// start a reboot in the gap), and with the lock's own recorded metadata
// deciding the uncordon (unschedulable false = the node was schedulable
// before kured cordoned it).
func TestHoldingANodeReleasesTheLockItOwnsAndUncordonsIt(t *testing.T) {
	api := &day2API{t: t, ds: daemonSetWithLock(t, "100", interlock.Value{
		NodeID:   "n1",
		Metadata: interlock.NodeMeta{Unschedulable: false},
		Created:  time.Now().UTC().Add(-time.Minute),
	})}
	r := &componenttest.FakeRunner{Respond: api.respond}

	if err := HoldAutomaticReboots(context.Background(), r, "n1"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"sudo -n k3s kubectl label node n1 " + NoAutoRebootLabel + "=" + NoAutoRebootValue + " --overwrite",
		"sudo -n k3s kubectl get daemonset -n kube-system kured -o json",
		"sudo -n k3s kubectl uncordon n1",
		// The release reads the DaemonSet a second time on purpose: its write
		// is a compare-and-swap, and the resourceVersion it compares against
		// must be one it observed in the same breath as the write.
		"sudo -n k3s kubectl get daemonset -n kube-system kured -o json",
		"sudo -n k3s kubectl replace -f -",
	}
	got := r.Commands()
	if len(got) != len(want) {
		t.Fatalf("commands =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d = %q, want %q (the label must be in place before the lock is cleared, and the node uncordoned before it is released)", i, got[i], want[i])
		}
	}
	inputs := r.Inputs()
	if len(inputs) != 1 {
		t.Fatalf("%d writes, want exactly 1", len(inputs))
	}
	var doc struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(inputs[0], &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Metadata.Annotations[interlock.LockAnnotation]; ok {
		t.Errorf("the orphaned lock survived the hold: %s", inputs[0])
	}
}

// And the half that must NOT happen: a lock naming another node belongs to
// that node, which may be mid-reboot. Holding a different node labels it and
// stops there — no uncordon, no release, nothing that touches the lock.
func TestHoldingANodeLeavesAnotherNodesLockAlone(t *testing.T) {
	api := &day2API{t: t, ds: daemonSetWithLock(t, "100", interlock.Value{
		NodeID:   "n2",
		Metadata: interlock.NodeMeta{Unschedulable: false},
		Created:  time.Now().UTC().Add(-time.Minute),
	})}
	r := &componenttest.FakeRunner{Respond: api.respond}

	if err := HoldAutomaticReboots(context.Background(), r, "n1"); err != nil {
		t.Fatal(err)
	}
	if api.labels != 1 {
		t.Errorf("labelled %d times, want the held node once", api.labels)
	}
	for _, cmd := range r.Commands() {
		if strings.Contains(cmd, "uncordon") || strings.Contains(cmd, "replace") {
			t.Errorf("holding n1 issued %q: another node's lock must be left exactly as it was", cmd)
		}
	}
	if len(r.Inputs()) != 0 {
		t.Errorf("%d writes to kured's DaemonSet while it held another node's lock", len(r.Inputs()))
	}
}
