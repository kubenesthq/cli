package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"kubenest.io/cli/pkg/cmd"
)

// These tests are the CLI half of the docs check (PLAN section 8, G0.7): the
// document they assert on is what kubenest-docs' scripts/check_examples_against_cli.py
// compares the pages to, so a command, a flag or an availability bit that goes
// missing from it makes every page that uses the real thing fail.

// treePaths walks the tree the way a user reaches it, root included. It does
// not call collect: the metadata must be checked against the tree, not against
// its own walk.
func treePaths(c *cobra.Command) []string {
	paths := []string{c.CommandPath()}
	for _, sub := range c.Commands() {
		paths = append(paths, treePaths(sub)...)
	}
	return paths
}

func emitted(t *testing.T) []command {
	t.Helper()
	return collect(cmd.NewRootCommand())
}

func byPath(commands []command) map[string]command {
	out := map[string]command{}
	for _, c := range commands {
		out[c.Path] = c
	}
	return out
}

func flagsByName(t *testing.T, c command) map[string]flag {
	t.Helper()
	out := map[string]flag{}
	for _, f := range c.Flags {
		out[f.Name] = f
	}
	return out
}

func TestEmittedTreeCarriesEveryReachableCommand(t *testing.T) {
	root := cmd.NewRootCommand()
	got := collect(root)

	want := treePaths(root)
	have := make([]string, 0, len(got))
	seen := map[string]bool{}
	for _, c := range got {
		have = append(have, c.Path)
		if seen[c.Path] {
			t.Errorf("%s appears twice in the metadata", c.Path)
		}
		seen[c.Path] = true
		if c.Flags == nil {
			t.Errorf("%s has no flags array; `--help` is accepted at every path, so it has at least one", c.Path)
		}
	}
	sort.Strings(want)
	sort.Strings(have)
	if !reflect.DeepEqual(want, have) {
		t.Errorf("the metadata does not carry the command tree.\nmissing: %v\nextra:   %v",
			missing(want, have), missing(have, want))
	}
	if len(got) == 0 || got[0].Path != "kubenest" {
		t.Fatalf("the first entry is %+v, want the root `kubenest` first", first(got))
	}
}

// A command added to the tree appears in the metadata without anyone touching
// the generator: that is the property that makes this metadata a view of Cobra
// rather than a second hand-written list that a new verb can fall behind.
func TestNewCommandsArePickedUp(t *testing.T) {
	root := cmd.NewRootCommand()
	// Nested two levels down, and hidden, because a hidden verb is still
	// reachable and the docs check must be able to say so.
	nested := &cobra.Command{Use: "fixture-verb", Short: "test-local", Hidden: true}
	for _, c := range root.Commands() {
		if c.Name() == "backup" {
			c.AddCommand(nested)
		}
	}

	got := byPath(collect(root))
	entry, ok := got["kubenest backup fixture-verb"]
	if !ok {
		t.Fatalf("a command added under `kubenest backup` is absent from the metadata; keys are %v",
			sortedKeys(got))
	}
	if !entry.Hidden {
		t.Errorf("`kubenest backup fixture-verb` has hidden: false, want true")
	}
}

func TestRegisteredStubIsUnavailable(t *testing.T) {
	got := byPath(emitted(t))

	stub, ok := got["kubenest backup restore"]
	if !ok {
		t.Fatal("`kubenest backup restore` is missing from the metadata")
	}
	if stub.Available {
		t.Error("`kubenest backup restore` is reported available: true, but running it exits with " +
			"\"not yet implemented\": the docs check would pass a page that offers a stub")
	}
	if strings.TrimSpace(stub.Reason) == "" {
		t.Error("`kubenest backup restore` is unavailable with no reason; a page cannot say why")
	}
	if cmd.AnnotationUnavailable != "kubenest.io/unavailable" {
		t.Errorf("AnnotationUnavailable is %q; the metadata key is the contract between the CLI and "+
			"the docs check", cmd.AnnotationUnavailable)
	}

	// The implemented siblings must NOT come out unavailable, or the flag is
	// noise a reader learns to ignore.
	for _, path := range []string{
		"kubenest backup set-target",
		"kubenest backup now",
		"kubenest backup drill",
		"kubenest platform install",
	} {
		if entry, ok := got[path]; !ok || !entry.Available {
			t.Errorf("%s is %+v, want available: true", path, entry)
		}
	}
}

func TestInstallFlagSurface(t *testing.T) {
	got := byPath(emitted(t))

	install, ok := got["kubenest platform install"]
	if !ok {
		t.Fatal("`kubenest platform install` is missing from the metadata")
	}
	flags := flagsByName(t, install)
	for _, name := range []string{"--ha", "--server", "--bundle", "--name"} {
		f, ok := flags[name]
		if !ok {
			t.Fatalf("`%s` is not on `kubenest platform install`; the install page passes it. It has: %v",
				name, sortedFlagKeys(flags))
		}
		if f.Inherited {
			t.Errorf("`%s` on `kubenest platform install` is inherited: true; it is registered on the "+
				"command itself", name)
		}
	}
	if flags["--ha"].Shorthand != "" {
		t.Errorf("--ha has shorthand %q, want none", flags["--ha"].Shorthand)
	}
	if flags["--server"].Type != "stringArray" {
		t.Errorf("--server has type %q, want stringArray: the docs check reads the type, and an "+
			"example may repeat it", flags["--server"].Type)
	}
	if _, ok := flags["--help"]; !ok {
		t.Error("`--help` is not reported on `kubenest platform install`; Cobra accepts it at every path")
	}
}

// A flag registered on a parent's persistent flags IS available on the
// subcommand, and the metadata has to say so -- otherwise the docs check
// rejects a valid example. No command in the released CLI has a persistent flag
// today, so this is asserted by registering one on the tree the generator is
// pointed at; the second half pins that today's tree reports none.
func TestInheritedFlagIsReportedAsInherited(t *testing.T) {
	for _, c := range collect(cmd.NewRootCommand()) {
		for _, f := range c.Flags {
			if f.Inherited {
				t.Errorf("%s reports %s as inherited: true, but no command in the released CLI "+
					"registers a persistent flag", c.Path, f.Name)
			}
		}
	}

	root := cmd.NewRootCommand()
	root.PersistentFlags().Bool("fixture-global", false, "test-local persistent flag")
	got := byPath(collect(root))

	if f := flagsByName(t, got["kubenest"])["--fixture-global"]; f.Inherited {
		t.Error("`--fixture-global` is inherited: true on the root, where it is registered")
	}
	for _, path := range []string{"kubenest platform install", "kubenest backup restore"} {
		f, ok := flagsByName(t, got[path])["--fixture-global"]
		if !ok {
			t.Errorf("`%s` does not carry `--fixture-global`, which a parent registers as a "+
				"persistent flag and Cobra therefore accepts there", path)
			continue
		}
		if !f.Inherited {
			t.Errorf("`--fixture-global` on `%s` is inherited: false; it comes from the root", path)
		}
	}
}

// The generator, run exactly as .github/workflows/build.yml runs it, emits JSON
// the docs check can read. This is also the drift check: kubenest-docs keeps
// cli-metadata/command-metadata.json as the metadata its CI and deploy read, so
// a command tree that moves without that copy moving is a divergence the
// umbrella workspace can see. Same rule as pkg/bundles' contract test, which
// skips where kubenest-contracts is not checked out beside this repo.
func TestGeneratorOutputMatchesTheCommittedDocsCopy(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go is not on PATH, so the generator cannot be run in-process: %v", err)
	}
	run := exec.Command("go", "run", "./cmd/gen-command-metadata")
	run.Dir = filepath.Join("..", "..")
	raw, err := run.Output()
	if err != nil {
		t.Fatalf("go run ./cmd/gen-command-metadata: %v", err)
	}

	var emittedDoc metadata
	if err := json.Unmarshal(raw, &emittedDoc); err != nil {
		t.Fatalf("the generator's stdout is not the metadata document: %v", err)
	}
	if emittedDoc.CLIVersion == "" || emittedDoc.GeneratedAt == "" {
		t.Errorf("the document carries cli_version %q and generated_at %q; the docs check reports "+
			"which CLI it checked against", emittedDoc.CLIVersion, emittedDoc.GeneratedAt)
	}
	if !reflect.DeepEqual(emittedDoc.Commands, collect(cmd.NewRootCommand())) {
		t.Fatal("the generator's stdout and collect() disagree; the metadata the release ships is " +
			"not the tree the tests assert on")
	}

	path := filepath.Join("..", "..", "..", "kubenest-docs", "cli-metadata", "command-metadata.json")
	committedRaw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("kubenest-docs is not checked out beside this repo, so the committed metadata cannot "+
			"be checked for drift here: %v", err)
	}
	var committed metadata
	if err := json.Unmarshal(committedRaw, &committed); err != nil {
		t.Fatalf("%s is not the metadata document: %v", path, err)
	}
	// The version stamps move with the build; the tree is what the docs check
	// compares the pages to.
	if !reflect.DeepEqual(emittedDoc.Commands, committed.Commands) {
		t.Errorf("%s does not match the command tree in this repository. Regenerate it with\n"+
			"    go run ./cmd/gen-command-metadata > %s\n"+
			"and commit it: kubenest-docs' CI and deploy workflows read that copy.",
			path, filepath.Join("..", "..", "..", "kubenest-docs", "cli-metadata", "command-metadata.json"))
	}
}

func missing(want, have []string) []string {
	has := map[string]bool{}
	for _, s := range have {
		has[s] = true
	}
	var out []string
	for _, s := range want {
		if !has[s] {
			out = append(out, s)
		}
	}
	return out
}

func first(commands []command) any {
	if len(commands) == 0 {
		return "nothing"
	}
	return commands[0]
}

func sortedKeys(m map[string]command) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedFlagKeys(m map[string]flag) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
