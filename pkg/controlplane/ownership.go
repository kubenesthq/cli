package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"kubenest.io/cli/pkg/k3s"
)

// NO CHART-RENDERED FIELD MAY BE OWNED BY A NON-HELM MANAGER (T7.0, finding 5).
//
// helm-controller applies this chart SERVER-SIDE with --force-conflicts=false.
// If a second manager also owns a field the chart renders, the NEXT chart
// apply fails outright with `conflict with "kubectl-patch" using v1:
// .data.status` — observed on hardware on 2026-09-25, and the reason the chart
// no longer renders the checkpoint status document's data. Nothing then
// upgrades except by hand, with the release left failed under failurePolicy:
// abort.
//
// THE GATE IS A FORCED CHOICE BETWEEN TWO TERMS, which is what makes it
// decidable without naming a manager: a field is in conflict when MORE THAN
// ONE manager owns the SAME field path. That is exactly Server-Side Apply's
// own conflict condition, so it is the condition that will actually stop the
// apply, rather than a guess about who wrote what. It also means the gate
// cannot be defeated by a manager this build has never heard of, and cannot
// refuse an object merely because some tool once applied it.
const (
	// helmManager and related are the managers this gate expects to own the
	// chart's fields. They are recorded in the report so an operator can see
	// which manager was unexpected, not used to decide whether there is a
	// conflict.
	helmManager = "helm"

	// releaseObjectsCmd lists the objects the release's chart renders, by the
	// label every one of them carries (kubenest.labels). Only kinds helm
	// renders are listed: a CRD's objects, or the fence's own objects (which
	// the CLI writes and helm never touches), are deliberately not here.
	releaseObjectsCmd = "get configmap,secret,deployment,statefulset,service,serviceaccount,cronjob" +
		" -n " + Namespace + " -l app.kubernetes.io/instance=" + ReleaseName + " -o json"
)

// OwnershipConflict is one field path two managers both own.
type OwnershipConflict struct {
	// Object is the object's kind and name, e.g. "configmap/control-plane-checkpoint-status".
	Object string
	// Field is the field path both managers own, e.g. ".data.status".
	Field string
	// Managers are the managers that own it, sorted.
	Managers []string
}

func (c OwnershipConflict) String() string {
	return fmt.Sprintf("%s owns %s as well: %s", c.Object, c.Field, strings.Join(c.Managers, " and "))
}

// OwnershipReport is what the gate found.
type OwnershipReport struct {
	Conflicts []OwnershipConflict
	// Objects is how many objects were inspected, so a report of "no
	// conflicts" cannot be mistaken for "nothing was read".
	Objects int
	// Managers are every field manager seen, sorted, so an operator meeting a
	// conflict has the whole picture rather than one line of it.
	Managers []string
}

// CheckFieldOwnership reports the fields of the release's objects that more
// than one manager owns.
//
// It is a READ. A gate that changed anything would not be one.
func CheckFieldOwnership(ctx context.Context, r k3s.Runner) (OwnershipReport, error) {
	out, err := k3s.Kubectl(ctx, r, releaseObjectsCmd)
	if err != nil {
		return OwnershipReport{}, fmt.Errorf("reading the release's objects to check field ownership: %w", err)
	}
	return ownershipConflicts([]byte(out))
}

// ownershipConflicts is the analysis on its own, so the conflict terms are
// testable without a cluster.
func ownershipConflicts(objectsJSON []byte) (OwnershipReport, error) {
	report := OwnershipReport{}
	if strings.TrimSpace(string(objectsJSON)) == "" {
		return report, fmt.Errorf("no objects were read, so the field-ownership check would pass without looking at anything")
	}
	var list struct {
		Items []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name          string `json:"name"`
				ManagedFields []struct {
					Manager    string          `json:"manager"`
					Operation  string          `json:"operation"`
					FieldsType string          `json:"fieldsType"`
					FieldsV1   json.RawMessage `json:"fieldsV1"`
				} `json:"managedFields"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(objectsJSON, &list); err != nil {
		return report, fmt.Errorf("parsing the release's objects: %w", err)
	}

	managers := map[string]bool{}
	for _, item := range list.Items {
		report.Objects++
		object := strings.ToLower(item.Kind) + "/" + item.Metadata.Name
		// Field path -> the managers that own it.
		owners := map[string]map[string]bool{}
		for _, entry := range item.Metadata.ManagedFields {
			if entry.Manager == "" || len(entry.FieldsV1) == 0 {
				continue
			}
			managers[entry.Manager] = true
			var fields any
			if err := json.Unmarshal(entry.FieldsV1, &fields); err != nil {
				// A fieldsV1 this build cannot read is not a conflict it can
				// rule on. Skip the entry rather than inventing one.
				continue
			}
			for _, path := range fieldPaths(fields, "") {
				if owners[path] == nil {
					owners[path] = map[string]bool{}
				}
				owners[path][entry.Manager] = true
			}
		}
		for path, held := range owners {
			if len(held) < 2 {
				continue
			}
			names := make([]string, 0, len(held))
			for name := range held {
				names = append(names, name)
			}
			sort.Strings(names)
			conflict := OwnershipConflict{Object: object, Field: path, Managers: names}
			report.Conflicts = append(report.Conflicts, conflict)
		}
	}
	for name := range managers {
		report.Managers = append(report.Managers, name)
	}
	sort.Strings(report.Managers)
	sort.Slice(report.Conflicts, func(i, j int) bool {
		if report.Conflicts[i].Object != report.Conflicts[j].Object {
			return report.Conflicts[i].Object < report.Conflicts[j].Object
		}
		return report.Conflicts[i].Field < report.Conflicts[j].Field
	})
	return report, nil
}

// fieldPaths walks a fieldsV1 document and returns every leaf path it names.
//
// A fieldsV1 document is a tree of field names — `f:` for a struct field,
// `k:` for a list key, `v:` for a list value — ending in empty objects at the
// leaves. The path is built the way the conflict message prints it, so an
// operator can match the report against what helm said.
func fieldPaths(node any, prefix string) []string {
	mapping, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	if len(mapping) == 0 {
		if prefix == "" {
			return nil
		}
		return []string{prefix}
	}
	var out []string
	for key, child := range mapping {
		name := key
		switch {
		case strings.HasPrefix(key, "f:"):
			name = "." + strings.TrimPrefix(key, "f:")
		case strings.HasPrefix(key, "k:"):
			name = "[" + strings.TrimPrefix(key, "k:") + "]"
		case strings.HasPrefix(key, "v:"):
			name = "." + strings.TrimPrefix(key, "v:")
		case key == ".":
			// The marker for the field itself; its children carry the path.
			name = ""
		}
		out = append(out, fieldPaths(child, prefix+name)...)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// OwnershipRefusal is the gate's refusal, naming every conflicting field.
type OwnershipRefusal struct {
	Report OwnershipReport
}

func (r *OwnershipRefusal) Error() string {
	var b strings.Builder
	b.WriteString("this upgrade is refused: a chart-rendered field is owned by a writer other than helm, ")
	b.WriteString("so the chart apply that follows would fail with a field-manager conflict rather than upgrade the control plane.\n")
	b.WriteString("      Why: helm-controller applies this chart server-side with --force-conflicts=false. Two owners of the same field is Server-Side Apply's own conflict condition; with failurePolicy: abort the release is then left failed, waiting for an operator with helm on the host.\n")
	b.WriteString("      Conflicting fields:\n")
	for _, conflict := range r.Report.Conflicts {
		fmt.Fprintf(&b, "        - %s\n", conflict)
	}
	fmt.Fprintf(&b, "      Field managers on the release's objects: %s\n", strings.Join(r.Report.Managers, ", "))
	b.WriteString("      Fix: stop the writer that is not helm from owning that field — a runtime patch of a field the chart renders must instead be kept outside the chart's objects, as the checkpoint status document was — then run this upgrade again")
	return b.String()
}
