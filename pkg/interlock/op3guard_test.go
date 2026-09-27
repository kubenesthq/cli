// The op3 regression guard (T5.1, PLAN 7.2 "Excluding kured").
//
// THE HAZARD. kured records its lock as an annotation on its own DaemonSet,
// and both kured and this CLI take it with a compare-and-swap on that object's
// resourceVersion (probe P1, recorded in kured_test.go). The lock therefore
// lives in an object SOMEBODY ELSE re-reconciles: k3s's Helm controller
// re-renders the DaemonSet whenever the HelmChart that installs kured is
// applied, and that render REPLACES the object — dropping every annotation
// including the lock. An operator reconcile that creates, replaces, patches or
// deletes the DaemonSet does the same. Either one hands the lock back to kured
// in the middle of a CLI operation, and then both actors believe they may take
// a node down, which is the lost update the interlock exists to prevent.
//
// WHY THIS IS A SOURCE-LEVEL GUARD. The op3 half of T5.1 is not code: T3.3's
// controller legitimately writes kured's HelmChartConfig (the maintenance
// window) and legitimately RELEASES the lock by updating the DaemonSet it read
// (P1 finding 3 — a node held out of kured's pool would otherwise keep the
// lock and the cordon for ever). The requirement here is the negative one:
// nothing in op3 may take OWNERSHIP of the DaemonSet or of the CLI's
// HelmChart. That is a property of the sources, so it is asserted against the
// sources.
//
// THE GUARD PROVES IT CAN SEE. A scan that finds nothing because it is looking
// in the wrong place, or because its patterns rotted, reads exactly like a
// clean tree — the failure mode scripts/ws-grep.sh was written for. So the
// detector is exercised against planted sources of each shape it must catch,
// and against the shapes it must NOT catch, in the same test that scans op3.
package interlock

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// op3Root is the operator repository as it is checked out beside this one
// (kubenest-cli/pkg/interlock -> kubenest/op3).
const op3Root = "../../../op3"

// goWriteVerbs are the client-go/controller-runtime calls that change an
// object. Apply is the server-side-apply call; DeleteAllOf is a bulk delete.
var goWriteVerbs = []string{"Create", "Update", "Patch", "Apply", "Replace", "Delete", "DeleteAllOf"}

// kubectlKuredPaths are the object paths a shell command would name. A write
// through one of these carries no resourceVersion unless the caller went and
// read the object first, which is exactly the unconditional update the
// interlock must not have.
var kubectlKuredPaths = []string{
	"daemonsets/kured",
	"daemonsets.apps/kured",
	"helmcharts/kured",
	"helmcharts.helm.cattle.io/kured",
}

// TestNoOp3PathWritesTheKuredDaemonSetOrChart is T5.1's op3 half: no file in
// the operator takes ownership of kured's DaemonSet, of its annotations, or of
// the k3s HelmChart the CLI installs.
//
// What it does NOT claim: that op3 does not TOUCH the DaemonSet. T3.3's
// releaseKuredLock updates it, deliberately and correctly, and that shape is
// asserted clean below.
func TestNoOp3PathWritesTheKuredDaemonSetOrChart(t *testing.T) {
	files, named, offences, err := scanForKuredWrites(op3Root)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("the operator repository is not checked out beside this one (looked in %s), so this guard did NOT run", op3Root)
		}
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatalf("no Go file under %s: this guard would pass without looking at anything", op3Root)
	}
	if named == 0 {
		t.Fatalf("none of the %d Go files under %s names kured at all, so this guard is looking in the wrong place", files, op3Root)
	}
	if len(offences) > 0 {
		t.Errorf("the operator takes ownership of kured's lock object in %d place(s): a reconcile that replaces the DaemonSet, or that writes the HelmChart k3s renders it from, drops the %s annotation and hands the lock back to kured mid-operation. A release is an Update of the object that was READ:\n  %s",
			len(offences), LockAnnotation, strings.Join(offences, "\n  "))
	}
}

// TestTheGuardFindsAPlantedWriteInATree walks a tree of its own holding the
// shapes the guard must catch and the ones it must ignore (a test fixture, a
// vendored dependency). It is the end-to-end control for the walk as well as
// the patterns: a scan that reads nothing reads exactly like a clean tree.
func TestTheGuardFindsAPlantedWriteInATree(t *testing.T) {
	root := t.TempDir()
	plant := func(rel, body string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plant("internal/controller/reconcile.go", `package controller

import appsv1 "k8s.io/api/apps/v1"

// kured's DaemonSet is rebuilt here, which drops its annotations.
func reconcileKured(ctx context.Context) error {
	return r.Create(ctx, &appsv1.DaemonSet{})
}
`)
	plant("internal/controller/reconcile_test.go", `package controller

func TestFixtureWritesKured(ctx context.Context) error {
	return r.Update(ctx, &appsv1.DaemonSet{})
}
`)
	plant("vendor/example.com/dep/setup.go", `package dep

func setUpKured(ctx context.Context) error {
	return r.Delete(ctx, &appsv1.DaemonSet{})
}
`)

	files, named, offences, err := scanForKuredWrites(root)
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 || named != 1 {
		t.Fatalf("scanned %d file(s), %d of them naming kured: a _test.go fixture is not a reconcile path and a vendor tree is not this repository's code", files, named)
	}
	if len(offences) != 1 {
		t.Fatalf("%d offence(s) in the planted tree, want exactly 1 (the Create):\n  %s", len(offences), strings.Join(offences, "\n  "))
	}
	if !strings.Contains(offences[0], "reconcileKured") || !strings.Contains(offences[0], "Create") {
		t.Errorf("the offence does not name the function and the call: %q", offences[0])
	}
}

// scanForKuredWrites walks a Go tree and reports how many files it read, how
// many of them name kured at all, and every construct that would take
// ownership of kured's DaemonSet or of the k3s HelmChart. It is the whole
// guard, so the planted-tree test exercises the same walk the op3 assertion
// does.
func scanForKuredWrites(root string) (files int, named int, offences []string, err error) {
	paths, err := op3GoSources(root)
	if err != nil {
		return 0, 0, nil, err
	}
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			return 0, 0, nil, err
		}
		files++
		if strings.Contains(strings.ToLower(string(src)), "kured") {
			named++
		}
		for _, o := range kuredOwnershipWrites(src) {
			offences = append(offences, path+": "+o)
		}
	}
	return files, named, offences, nil
}

// TestTheGuardFlagsEachShapeItMustCatch exercises the detector against planted
// sources: every shape it must catch, and the two shapes op3 legitimately has.
func TestTheGuardFlagsEachShapeItMustCatch(t *testing.T) {
	cases := []struct {
		what     string
		src      string
		mustFlag bool
	}{
		{
			what: "a reconcile that creates the kured DaemonSet",
			src: `package controller

import appsv1 "k8s.io/api/apps/v1"

func setUpKured(ctx context.Context) error {
	return r.Create(ctx, &appsv1.DaemonSet{})
}
`,
			mustFlag: true,
		},
		{
			what: "a reconcile that patches the kured DaemonSet",
			src: `package controller

func refreshKured(ctx context.Context, ds *appsv1.DaemonSet) error {
	return r.Patch(ctx, ds, client.MergeFrom(ds))
}
`,
			mustFlag: true,
		},
		{
			what: "an update of kured's DaemonSet with no read, so it carries no resourceVersion",
			src: `package controller

import appsv1 "k8s.io/api/apps/v1"

func releaseKured(ctx context.Context) error {
	ds := &appsv1.DaemonSet{}
	ds.Namespace = "kube-system"
	ds.Name = "kured"
	return r.Update(ctx, ds)
}
`,
			mustFlag: true,
		},
		{
			what: "a write of the k3s HelmChart that renders kured",
			src: `package controller

func setKuredWindow(ctx context.Context) error {
	chart := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.cattle.io/v1",
		"kind":       "HelmChart",
		"metadata":   map[string]any{"name": "kured", "namespace": "kube-system"},
	}}
	return r.Update(ctx, chart)
}
`,
			mustFlag: true,
		},
		{
			what: "a shell command that writes kured's DaemonSet by its kubectl path",
			src: `package controller

func replaceKured(ctx context.Context) (string, error) {
	return k3s.Kubectl(ctx, r, "replace -f - daemonsets/kured")
}
`,
			mustFlag: true,
		},
		{
			what: "T3.3's release: an Update of the object it read, deleting only the lock",
			src: `package controller

import appsv1 "k8s.io/api/apps/v1"

func releaseKuredLock(ctx context.Context, nodeName string) error {
	key := types.NamespacedName{Namespace: "kube-system", Name: "kured"}
	ds := &appsv1.DaemonSet{}
	if err := r.Get(ctx, key, ds); err != nil {
		return err
	}
	delete(ds.Annotations, kuredNodeLockAnnotation)
	return r.Update(ctx, ds)
}
`,
		},
		{
			what: "T3.3's window: a write of kured's HelmChartConfig",
			src: `package controller

func reconcileWindowConfig(ctx context.Context) error {
	write := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.cattle.io/v1",
		"kind":       "HelmChartConfig",
		"metadata":   map[string]any{"name": r.kuredName(), "namespace": r.kuredNamespace()},
	}}
	if err := r.Create(ctx, write); err != nil {
		return r.Update(ctx, write)
	}
	return nil
}
`,
		},
	}
	for _, c := range cases {
		got := kuredOwnershipWrites([]byte(c.src))
		if c.mustFlag && len(got) == 0 {
			t.Errorf("the guard did NOT flag %s, so it cannot be trusted to notice one in op3:\n%s", c.what, c.src)
		}
		if !c.mustFlag && len(got) > 0 {
			t.Errorf("the guard flagged %s, which is a shape op3 is supposed to have (%s)", c.what, strings.Join(got, "; "))
		}
	}
}

// kuredOwnershipWrites reports every construct in one Go source file that
// would take ownership of kured's DaemonSet or of the k3s HelmChart that
// renders it. Comments are stripped first: the operator's sources discuss
// kured at length, and prose is not a write.
func kuredOwnershipWrites(src []byte) []string {
	source := stripGoComments(string(src))
	var out []string

	// A shell command that names the object by path, in any function.
	for _, path := range kubectlKuredPaths {
		if strings.Contains(source, path) {
			out = append(out, "names the object by its kubectl path ("+path+"), which is how a write without a precondition is written")
		}
	}

	// The objects, function by function: which object a function owns, and
	// the precondition it writes with, are properties of that function. A
	// package-level type variable is not a write; a function that names kured
	// and writes a chart record is.
	for _, fn := range goFunctions(source) {
		if !strings.Contains(strings.ToLower(fn.body), "kured") {
			continue
		}
		if helms := stripHelmChartConfig(fn.body); strings.Contains(helms, "HelmChart") {
			if verbs := writeVerbsIn(fn.body); len(verbs) > 0 {
				out = append(out, fn.name+" writes an object while naming the k3s HelmChart kind ("+strings.Join(verbs, ", ")+"): the HelmChart belongs to the CLI, and k3s re-renders the DaemonSet whenever it is touched")
				continue
			}
		}
		if !mentionsDaemonSet(fn.body) {
			continue
		}
		for _, verb := range writeVerbsIn(fn.body) {
			switch {
			case verb == "Update" && !strings.Contains(fn.body, ".Get("):
				out = append(out, fn.name+" updates the DaemonSet without reading it first, so the write carries no resourceVersion and overwrites whatever kured wrote")
			case verb != "Update":
				out = append(out, fn.name+" calls "+verb+" on the DaemonSet: only an Update of the object that was read is a release, and anything else drops kured's lock")
			}
		}
	}
	return out
}

// stripHelmChartConfig removes mentions of the object this controller owns, so
// that whatever still names a HelmChart is a mention of k3s's chart record —
// the object the CLI owns and the operator must not write.
func stripHelmChartConfig(body string) string {
	return strings.ReplaceAll(strings.ReplaceAll(body, "HelmChartConfig", ""), "helmchartconfig", "")
}

// mentionsDaemonSet reports whether a fragment uses the DaemonSet TYPE. The
// List is deliberately not a match: reading every DaemonSet is how the
// operator reports health, and it is not a write to kured's.
func mentionsDaemonSet(body string) bool {
	body = strings.ReplaceAll(body, "DaemonSetList", "")
	return strings.Contains(body, "appsv1.DaemonSet") || strings.Contains(body, "DaemonSet{")
}

// writeVerbsIn returns which of the object-changing calls a fragment makes, in
// a stable order, so a message reads the same way twice.
func writeVerbsIn(body string) []string {
	var out []string
	for _, v := range goWriteVerbs {
		if strings.Contains(body, "."+v+"(") {
			out = append(out, v)
		}
	}
	return out
}

// goFunction is one top-level declaration.
type goFunction struct {
	name string
	body string
}

// goFunctions splits Go source at its top-level declarations. It is a line
// scan rather than a parse because the question (which function a write sits
// in, and what that function reads first) is answerable from the text, and a
// parse of another repository's package would need that package's build
// context.
func goFunctions(source string) []goFunction {
	var out []goFunction
	var cur *goFunction
	for _, line := range strings.Split(source, "\n") {
		if strings.HasPrefix(line, "func ") {
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &goFunction{name: funcName(line)}
		}
		if cur != nil {
			cur.body += line + "\n"
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

// funcName pulls the name out of a declaration line.
func funcName(line string) string {
	rest := strings.TrimPrefix(line, "func ")
	if strings.HasPrefix(rest, "(") { // a method: skip the receiver
		if end := strings.Index(rest, ")"); end >= 0 {
			rest = strings.TrimSpace(rest[end+1:])
		}
	}
	name, _, _ := strings.Cut(rest, "(")
	return strings.TrimSpace(name)
}

// stripGoComments removes line and block comments, and leaves string, rune and
// raw literals exactly as they are. The literals are the point: the kubectl
// paths this guard looks for live in them, and a URL inside a string must not
// be mistaken for the start of a comment.
func stripGoComments(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case strings.HasPrefix(src[i:], "/*"):
			i += 2
			for i < len(src) && !strings.HasPrefix(src[i:], "*/") {
				i++
			}
			if i < len(src) {
				i += 2
			}
		case src[i] == '"' || src[i] == '`' || src[i] == '\'':
			quote := src[i]
			b.WriteByte(src[i])
			i++
			for i < len(src) {
				c := src[i]
				b.WriteByte(c)
				i++
				if c == '\\' && quote != '`' && i < len(src) {
					b.WriteByte(src[i])
					i++
					continue
				}
				if c == quote {
					break
				}
			}
		default:
			b.WriteByte(src[i])
			i++
		}
	}
	return b.String()
}

// op3GoSources lists the non-test Go files under the operator's root. Test
// files are excluded on purpose: a test that writes kured's DaemonSet is a
// fixture, not a reconcile path.
func op3GoSources(root string) ([]string, error) {
	if _, err := os.Stat(root); err != nil {
		return nil, err
	}
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "vendor" || d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out, err
}
