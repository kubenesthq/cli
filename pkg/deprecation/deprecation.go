// Package deprecation is the pre-flight API deprecation scan: the gate that
// separates an upgrade product from a cron job.
//
// Kubernetes removes APIs. When a minor version drops an API group a
// customer's workloads use, the upgrade succeeds, the cluster comes up
// healthy, every platform component reports Ready — and their application
// stops working, because the manifests it deploys from now reference an API
// that no longer exists. An upgrade that cleanly upgrades the cluster and
// takes down the customer's product has actively harmed them. It is worse
// than no upgrade at all.
//
// So the scan runs before anything else and BLOCKS on a finding, naming the
// resource, its namespace, the API version it uses and the one it should move
// to. Fixing them is the customer's work in their own manifests; we cannot
// rewrite their application for them.
//
// ADOPT, DO NOT REBUILD (decision D, 2026-08-20). The scanner is pluto, and
// both its version and its DATASET are pinned in the bundle manifest. The
// dataset pin is the load-bearing half: a scanner whose deprecation data has
// drifted will report a customer clean against removals it has never heard
// of, and they will believe it. That is confidence it has not earned.
//
// THIS SCAN FAILS CLOSED. If pluto cannot be fetched, cannot run, or emits
// output this package does not understand, the gate FAILS — it never reports
// "no deprecations found". A scan that silently degrades into a pass is the
// exact failure mode the pinning exists to prevent, and it would be
// indistinguishable from a clean cluster at the moment it mattered most.
package deprecation

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

// Finding is one resource using an API that is deprecated or removed in the
// target Kubernetes version.
type Finding struct {
	Namespace  string
	Kind       string
	Name       string
	APIVersion string
	// Replacement is the API version to move to, when pluto knows one.
	Replacement string
	// RemovedIn is the Kubernetes version that removes this API, empty if it
	// is only deprecated so far.
	RemovedIn string
	// DeprecatedIn is where it was first deprecated.
	DeprecatedIn string
	// Removed reports whether the API is gone in the TARGET version, which
	// is the difference between blocking and warning.
	Removed bool
}

// Ref is the acknowledgement form: namespace/Kind/name, or Kind/name for
// cluster-scoped resources. It is what an operator types to accept one
// specific finding, and what an audit reads back.
func (f Finding) Ref() string {
	if f.Namespace == "" {
		return f.Kind + "/" + f.Name
	}
	return f.Namespace + "/" + f.Kind + "/" + f.Name
}

func (f Finding) String() string {
	line := fmt.Sprintf("%-20s %s %s\n      %-26s", "namespace/"+f.Namespace, f.Kind, f.Name, f.APIVersion)
	if f.Replacement != "" {
		line += " →  " + f.Replacement
	} else {
		line += " →  (no replacement; this resource kind is gone)"
	}
	return line
}

// Report is one scan.
type Report struct {
	// TargetVersion is the Kubernetes version scanned against.
	TargetVersion string
	// Blocking are resources using APIs REMOVED in the target version.
	Blocking []Finding
	// Warnings are resources using APIs deprecated but not yet removed.
	// They do not block: an upgrade that refused every deprecation would
	// refuse most real clusters, and deprecation is a warning by design.
	Warnings []Finding
	// Acknowledged are blocking findings an operator accepted by name.
	Acknowledged []Finding
}

// Err is the refusal, in the shape install.mdx documents: what was found,
// where, and what it should become.
func (r Report) Err() error {
	if len(r.Blocking) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "BLOCKED: %d resource(s) use APIs removed in Kubernetes %s.\n\n", len(r.Blocking), r.TargetVersion)
	for _, f := range r.Blocking {
		fmt.Fprintf(&b, "  %s\n\n", f)
	}
	b.WriteString("Fix these, then re-run. Nothing has been changed.\n\n")
	b.WriteString("If a resource is genuinely safe — it is inert, or you are removing it in the same\n")
	b.WriteString("window — accept it by name and only by name:\n")
	for _, f := range r.Blocking {
		fmt.Fprintf(&b, "  --acknowledge %s\n", f.Ref())
	}
	return fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
}

// Runner runs a command on the cluster's server node.
type Runner = k3s.Runner

// Scan runs the pinned scanner against the cluster's LIVE workloads and
// reports what the target Kubernetes version would break.
//
// acknowledged holds refs an operator has accepted individually. There is
// deliberately no blanket override: a --force is how a customer takes their
// own product down and then calls us, whereas naming each resource is tedious
// enough to prevent reflexive use and specific enough to audit afterwards.
func Scan(ctx context.Context, r Runner, bundle *manifest.Manifest, acknowledged []string, targetK8s string) (Report, error) {
	scanner, err := bundle.Upgrade.Scanner()
	if err != nil {
		return Report{}, err
	}
	if scanner.Tool != "pluto" {
		return Report{}, fmt.Errorf("bundle pins deprecation scanner %q, but this build only knows how to drive pluto (decision D)", scanner.Tool)
	}
	target := KubernetesVersion(targetK8s)
	if target == "" {
		return Report{}, fmt.Errorf("cannot scan without a target Kubernetes version")
	}

	binary, err := ensurePluto(ctx, r, scanner)
	if err != nil {
		// Fail closed, loudly. Not being able to scan is not the same as
		// finding nothing, and must never be reported as if it were.
		return Report{}, fmt.Errorf("the deprecated-API scan could not run, so the upgrade is refused: %w\n\nThis gate is not optional — an upgrade that skips it can take your workloads down while reporting success", err)
	}

	raw, err := run(ctx, r, fmt.Sprintf(
		"sudo -n env KUBECONFIG=/etc/rancher/k3s/k3s.yaml %s detect-all-in-cluster -o json --target-versions k8s=%s",
		binary, shellQuote(target)))
	if err != nil {
		return Report{}, fmt.Errorf("the deprecated-API scan could not run, so the upgrade is refused: %w", err)
	}

	report, err := parse(raw, target)
	if err != nil {
		return Report{}, fmt.Errorf("the deprecated-API scan produced output this build does not understand, so the upgrade is refused: %w\n\nThe scanner is pinned at %s with dataset %s; a mismatch here means the pin and the parser have diverged",
			err, scanner.Version, scanner.Dataset)
	}

	acked := map[string]bool{}
	for _, ref := range acknowledged {
		acked[strings.TrimSpace(ref)] = true
	}
	var blocking []Finding
	for _, f := range report.Blocking {
		if acked[f.Ref()] {
			report.Acknowledged = append(report.Acknowledged, f)
			continue
		}
		blocking = append(blocking, f)
	}
	report.Blocking = blocking
	return report, nil
}

// plutoOutput is pluto's JSON, of which this reads only the fields the gate
// needs. Unknown fields are ignored; a MISSING items key is not, because an
// empty document and a clean cluster must never look the same.
type plutoOutput struct {
	Items *[]plutoItem `json:"items"`
}

type plutoItem struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	Kind       string `json:"kind"`
	APIVersion string `json:"api_version"`
	Deprecated bool   `json:"deprecated"`
	Removed    bool   `json:"removed"`
	// Version metadata, whose key names differ across pluto's output modes;
	// both spellings are read so a pin bump cannot silently blank them.
	Replacement  string `json:"replacement_api_version"`
	DeprecatedIn string `json:"deprecated_in"`
	RemovedIn    string `json:"removed_in"`
}

func parse(raw, target string) (Report, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Report{}, fmt.Errorf("the scanner produced no output at all")
	}
	var out plutoOutput
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return Report{}, fmt.Errorf("parsing scanner output: %w", err)
	}
	if out.Items == nil {
		// A document with no `items` key is not a clean cluster — it is a
		// shape this parser does not recognise.
		return Report{}, fmt.Errorf("scanner output has no items field")
	}

	report := Report{TargetVersion: target}
	for _, item := range *out.Items {
		f := Finding{
			Namespace:    item.Namespace,
			Kind:         item.Kind,
			Name:         item.Name,
			APIVersion:   item.APIVersion,
			Replacement:  item.Replacement,
			DeprecatedIn: item.DeprecatedIn,
			RemovedIn:    item.RemovedIn,
			Removed:      item.Removed,
		}
		switch {
		case item.Removed:
			report.Blocking = append(report.Blocking, f)
		case item.Deprecated:
			report.Warnings = append(report.Warnings, f)
		}
	}
	sort.Slice(report.Blocking, func(i, j int) bool { return report.Blocking[i].Ref() < report.Blocking[j].Ref() })
	sort.Slice(report.Warnings, func(i, j int) bool { return report.Warnings[i].Ref() < report.Warnings[j].Ref() })
	return report, nil
}

// KubernetesVersion turns a k3s pin into the version pluto wants:
// "v1.35.7+k3s1" -> "v1.35.7".
func KubernetesVersion(k3sPin string) string {
	v := strings.TrimSpace(k3sPin)
	if i := strings.IndexByte(v, '+'); i > 0 {
		v = v[:i]
	}
	if v != "" && !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

func run(ctx context.Context, r Runner, command string) (string, error) {
	res, err := r.Run(ctx, command)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		return "", fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(msg))
	}
	return res.Stdout, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
