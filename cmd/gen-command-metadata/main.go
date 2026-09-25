// Command gen-command-metadata writes the CLI's command tree as JSON.
//
// WHY THIS EXISTS (PLAN section 8, G0.7). docs.kubenest.io is checked against
// the CLI's own command tree, not against a second hand-written list: the first
// is the thing that actually runs, and a list is one more document that can
// disagree with it. Cobra already holds the tree, its flags and its
// availability, so this walks that and emits it.
//
// The metadata is attached to every GitHub release by
// .github/workflows/build.yml, so the docs repository reads the RELEASED CLI's
// command tree without building Go: the public site is checked against the
// release it documents, and the candidate site against the candidate channel
// (PLAN section 7.12).
//
// Availability is machine-readable, not inferred from a string: a registered
// command that is not built yet carries cmd.AnnotationUnavailable, and every
// path under it inherits that -- a stub's subcommand cannot be runnable while
// its parent is not.
//
// Usage:
//
//	go run ./cmd/gen-command-metadata > command-metadata.json
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"kubenest.io/cli/pkg/cmd"
	"kubenest.io/cli/pkg/version"
)

// flag is one flag as it applies to one command path.
type flag struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Shorthand string `json:"shorthand"`
	Inherited bool   `json:"inherited"`
}

// command is one node of the tree, keyed by the full path a user types.
type command struct {
	Path      string `json:"path"`
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
	Hidden    bool   `json:"hidden"`
	Flags     []flag `json:"flags"`
}

// metadata is the document the docs checker reads.
type metadata struct {
	CLIVersion  string    `json:"cli_version"`
	Commit      string    `json:"commit"`
	GeneratedAt string    `json:"generated_at"`
	Commands    []command `json:"commands"`
}

func main() {
	root := cmd.NewRootCommand()

	doc := metadata{
		CLIVersion:  version.Version,
		Commit:      version.Commit,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Commands:    collect(root),
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		fmt.Fprintf(os.Stderr, "gen-command-metadata: %v\n", err)
		os.Exit(1)
	}
}

// collect walks the tree depth-first and returns every command reachable from
// c, root included. Cobra's own defaults are materialized on the way, because
// `--help` (and `--version` on the root) are accepted on the command line even
// though nothing registered them in source.
func collect(c *cobra.Command) []command {
	out := []command{}
	var walk func(*cobra.Command, string)
	walk = func(c *cobra.Command, unavailable string) {
		c.InitDefaultHelpFlag()
		c.InitDefaultVersionFlag()

		reason := unavailable
		if why, ok := c.Annotations[cmd.AnnotationUnavailable]; ok {
			reason = why
		}

		entry := command{
			Path:      c.CommandPath(),
			Available: reason == "",
			Reason:    reason,
			Hidden:    c.Hidden,
			Flags:     flagsOf(c),
		}
		if entry.Available {
			entry.Reason = description(c)
		}
		out = append(out, entry)

		for _, sub := range c.Commands() {
			walk(sub, reason)
		}
	}
	walk(c, "")
	return out
}

// description is the human-readable reason field for a command that IS
// available: its Long text's first line, falling back to Short.
func description(c *cobra.Command) string {
	for _, s := range []string{c.Long, c.Short} {
		if line := firstLine(s); line != "" {
			return line
		}
	}
	return ""
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

// flagsOf lists the flags a user may pass at this path: the command's own,
// then the ones it inherits from its parents' persistent flags. Inherited
// flags are reported with inherited: true because the docs checker accepts a
// flag at a subcommand only when the CLI does, and "the CLI does" means either
// set.
func flagsOf(c *cobra.Command) []flag {
	out := []flag{}
	seen := map[string]bool{}
	add := func(fs *pflag.FlagSet, inherited bool) {
		fs.VisitAll(func(f *pflag.Flag) {
			if seen[f.Name] {
				return
			}
			seen[f.Name] = true
			out = append(out, flag{
				Name:      "--" + f.Name,
				Type:      f.Value.Type(),
				Shorthand: f.Shorthand,
				Inherited: inherited,
			})
		})
	}
	add(c.NonInheritedFlags(), false)
	add(c.InheritedFlags(), true)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Inherited != out[j].Inherited {
			return !out[i].Inherited
		}
		return out[i].Name < out[j].Name
	})
	return out
}
