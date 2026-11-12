package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

const (
	DrillResultName        = "kubenest-restore-drill-result"
	DrillResultDataKey     = "result.json"
	DrillRequestDataKey    = "run-now"
	DrillRequestAnnotation = "kubenest.io/restore-drill-request"

	// DrillDeclinedAnnotation and DrillDeclinedDetailAnnotation carry the
	// operator's refusal of this request, written in the same patch that marks
	// the request done. They are the operator's side of
	// op3/pkg/restoredrill/result.go's DeclinedKey and DeclinedDetailKey.
	DrillDeclinedAnnotation       = "kubenest.io/restore-drill-declined"
	DrillDeclinedDetailAnnotation = "kubenest.io/restore-drill-declined-detail"

	// The reasons a drill request is declined. Each one means no restore was
	// attempted, so the drill proved nothing and must not be reported as a pass.
	DrillDeclineNoCompletedBackup   = "NO_COMPLETED_BACKUP"
	DrillDeclineBackupPredatesProof = "BACKUP_PREDATES_PROOF"
	DrillDeclineNodeNotReady        = "NODE_NOT_READY"
)

// DrillDeclinedError is the operator refusing a drill request before it starts
// any restore. It is separate from a failed drill: nothing ran, so there is no
// drill result to read and nothing was proved.
type DrillDeclinedError struct {
	Code string
	// Detail is the operator's own sentence, naming what it found and the fix.
	Detail string
}

func (e *DrillDeclinedError) Error() string {
	detail := e.Detail
	if detail == "" {
		detail = "The operator refused the request and said nothing about why."
	}
	message := fmt.Sprintf("restore drill declined (%s): %s No restore was attempted, so the drill proved nothing.", e.Code, detail)
	if !strings.Contains(detail, "kubenest backup") {
		message += " Run `kubenest backup now`, then `kubenest backup drill`."
	}
	return message
}

type DrillResult struct {
	Status          string             `json:"status"`
	CompletedAt     string             `json:"completed_at"`
	Backup          string             `json:"backup"`
	DurationSeconds float64            `json:"duration_seconds"`
	Verification    *DrillVerification `json:"verification,omitempty"`
	Failure         *struct {
		Stage      string `json:"stage"`
		ReasonCode string `json:"reason_code"`
		Detail     string `json:"detail"`
	} `json:"failure,omitempty"`
}

type DrillVerification struct {
	Mode    string           `json:"mode"`
	Objects DrillMatchCounts `json:"objects"`
	PVCData DrillMatchCounts `json:"pvc_data"`
}

type DrillMatchCounts struct {
	Restored int `json:"restored"`
	Matched  int `json:"matched"`
}

type drillWaitRestoreRecord struct {
	Name    string
	Phase   string
	Created time.Time
}

func restorePickedUp(phase string) bool {
	return phase != "" && phase != "New"
}

// otherUnsettledRestore finds the Restore most likely to hold Velero's single
// worker: a non-drill Restore with an active phase takes precedence over one
// Velero has not picked up, and the oldest of either kind wins.
func otherUnsettledRestore(ctx context.Context, r k3s.Runner) (*drillWaitRestoreRecord, error) {
	out, err := k3s.Kubectl(ctx, r, "get restores.velero.io -n "+Namespace+" -o json")
	if err != nil {
		return nil, fmt.Errorf("reading Velero Restores during the drill wait: %w", err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				CreationTimestamp string            `json:"creationTimestamp"`
				Labels            map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parsing Velero Restores during the drill wait: %w", err)
	}
	var selected *drillWaitRestoreRecord
	for _, item := range list.Items {
		name, phase := item.Metadata.Name, item.Status.Phase
		if item.Metadata.Labels[DrillRestoreLabelKey] == DrillRestoreLabelValue ||
			strings.HasPrefix(name, DrillRestoreNamePrefix) || restorePhaseTerminal(phase) {
			continue
		}
		created, _ := time.Parse(time.RFC3339Nano, item.Metadata.CreationTimestamp)
		candidate := &drillWaitRestoreRecord{Name: name, Phase: phase, Created: created}
		if selected == nil || (restorePickedUp(phase) && !restorePickedUp(selected.Phase)) ||
			(restorePickedUp(phase) == restorePickedUp(selected.Phase) && !created.IsZero() &&
				(selected.Created.IsZero() || created.Before(selected.Created))) {
			selected = candidate
		}
	}
	return selected, nil
}

func drillRestoreAge(created time.Time) string {
	if created.IsZero() {
		return "unknown"
	}
	age := time.Since(created)
	if age < 0 {
		age = 0
	}
	return age.Round(time.Minute).String()
}

func unfinishedDrillVolumes(volumes []VolumeRestoreState) []string {
	var unfinished []string
	for _, volume := range volumes {
		switch volume.Phase {
		case "Completed", "Failed", "Canceled":
			continue
		}
		phase := volume.Phase
		if phase == "" {
			phase = "not started"
		}
		state := volume.Name + " (" + phase
		if volume.Pod != "" {
			state += ", pod " + volume.Pod
		}
		unfinished = append(unfinished, state+")")
	}
	return unfinished
}

// drillWaitState observes the objects behind a pending result token. The
// operator publishes no intermediate stage, so the CLI reads Velero directly.
func drillWaitState(ctx context.Context, r k3s.Runner) (converge.State, error) {
	cluster := NewK3sCluster(r)
	drill, err := cluster.DrillRestore(ctx)
	if err != nil {
		return converge.State{Object: "restore drill", Status: "stage unavailable"}, err
	}
	var drillVolumes []string
	if drill != nil {
		volumes, err := cluster.VolumeRestores(ctx, drill.Name)
		if err != nil {
			return converge.State{Object: "Velero Restore " + drill.Name, Status: drill.Phase}, err
		}
		drillVolumes = unfinishedDrillVolumes(volumes)
	}
	other, err := otherUnsettledRestore(ctx, r)
	if err != nil {
		return converge.State{Object: "restore drill", Status: "Velero state unavailable"}, err
	}
	if other != nil {
		volumes, err := cluster.VolumeRestores(ctx, other.Name)
		if err != nil {
			return converge.State{Object: "Velero Restore " + other.Name, Status: other.Phase}, err
		}
		phase := other.Phase
		if phase == "" {
			phase = "not yet picked up"
		}
		status := "waiting for the operator"
		object := "restore drill request"
		if drill != nil {
			object = "Velero Restore " + drill.Name
			status = drill.Phase
			if status == "" || status == "New" {
				status = "queued; Velero has not picked up this Restore"
			}
		}
		detail := fmt.Sprintf("Velero Restore %s is %s (age %s)",
			other.Name, phase, drillRestoreAge(other.Created))
		if restorePickedUp(other.Phase) {
			detail += " and holds Velero's single restore worker"
		} else {
			detail += "; Velero has not picked it up either, so the worker holder is not identified"
		}
		unfinished := unfinishedDrillVolumes(volumes)
		if len(unfinished) > 0 {
			detail += "; unfinished PodVolumeRestores: " + strings.Join(unfinished, ", ")
			detail += ". Settle that Restore or delete its stuck PodVolumeRestore, then run kubenest backup drill again."
		} else {
			detail += ". Settle that Restore, then run kubenest backup drill again."
		}
		return converge.State{Object: object, Status: status, Detail: detail}, nil
	}
	if drill == nil {
		return converge.State{
			Object: "restore drill request", Status: "waiting for the operator",
			Detail: "No drill Restore exists yet. Check the operator and the restore proof workload, then run kubenest backup drill again if the request does not settle.",
		}, nil
	}
	status := drill.Phase
	if status == "" || status == "New" {
		status = "queued; Velero has not picked up this Restore"
	}
	detail := ""
	if len(drillVolumes) > 0 {
		detail = "unfinished PodVolumeRestores: " + strings.Join(drillVolumes, ", ") + ". Check those volumes and Velero, then run kubenest backup drill again if the request does not settle."
	} else if status == "queued; Velero has not picked up this Restore" {
		detail = "No other unsettled Restore was found. Check Velero's restore worker and logs, then run kubenest backup drill again if the request does not settle."
	} else if restorePhaseTerminal(drill.Phase) {
		detail = "Velero finished restoring; the operator may be verifying proof or cleaning up. Check the operator if the request does not settle."
	} else {
		detail = "Velero is restoring. Check its logs if this phase does not advance, then run kubenest backup drill again."
	}
	return converge.State{Object: "Velero Restore " + drill.Name, Status: status, Detail: detail}, nil
}

// RequestDrill asks the always-on operator to run the latest-backup drill and
// waits for the exact request token to be recorded on the result. It does not
// duplicate the runner in the CLI: scheduled and manual drills use one path.
func RequestDrill(
	ctx context.Context,
	r k3s.Runner,
	bundle *manifest.Manifest,
	rep converge.Reporter,
) (DrillResult, error) {
	deadline, err := bundle.Limits.Timeouts.For("restore-drill")
	if err != nil {
		return DrillResult{}, err
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return DrillResult{}, fmt.Errorf("create restore drill request: %w", err)
	}
	token := time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(random)
	patch := fmt.Sprintf(`'{"data":{"%s":"%s"}}'`, DrillRequestDataKey, token)
	if _, err := k3s.Kubectl(ctx, r,
		"patch configmap "+DrillConfigName+" -n "+Namespace+" --type=merge -p "+patch); err != nil {
		return DrillResult{}, fmt.Errorf("request restore drill: %w", err)
	}

	// Say what the request is waiting for before the wait starts. The operator
	// decides whether it can run at all on its next poll, and a user who is told
	// nothing has no way to learn that a backup has to complete after the proof
	// workload before a drill means anything.
	if rep != nil {
		rep.Report(converge.Event{
			Check:   "restore-drill",
			Outcome: converge.Converging,
			State: converge.State{
				Object: "restore drill request accepted",
				Status: "waiting for the operator",
				Detail: "the operator declines immediately when no backup completed after the restore proof became ready",
			},
			Deadline: deadline,
		})
	}

	var completed DrillResult
	var declined *DrillDeclinedError
	settled, err := converge.Wait(ctx, func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, r, "get configmap "+DrillResultName+" -n "+Namespace+" -o json")
		if err != nil {
			return false, converge.State{Object: "restore drill result", Status: "not available yet"}, err
		}
		var cm struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &cm); err != nil {
			return false, converge.State{Object: "restore drill result", Status: "unparsable"}, err
		}
		if cm.Metadata.Annotations[DrillRequestAnnotation] != token {
			state, err := drillWaitState(ctx, r)
			return false, state, err
		}
		// The token is on the result, so the operator has seen this request. A
		// refusal is its verdict and settles here: there is no drill result to
		// read, and waiting for one that will never be written is the defect.
		if code := cm.Metadata.Annotations[DrillDeclinedAnnotation]; code != "" {
			declined = &DrillDeclinedError{Code: code, Detail: cm.Metadata.Annotations[DrillDeclinedDetailAnnotation]}
			return true, converge.State{Object: "restore drill request", Status: "declined", Detail: declined.Detail}, nil
		}
		if err := json.Unmarshal([]byte(cm.Data[DrillResultDataKey]), &completed); err != nil {
			return false, converge.State{Object: "restore drill result", Status: "unparsable"}, err
		}
		detail := ""
		if completed.Failure != nil {
			detail = completed.Failure.Stage + ": " + completed.Failure.ReasonCode
		}
		return true, converge.State{Object: "restore drill for backup " + completed.Backup, Status: completed.Status, Detail: detail}, nil
	}, converge.Options{Name: "restore-drill", Deadline: deadline, Reporter: rep})
	if err != nil {
		return DrillResult{}, err
	}
	if declined != nil {
		return DrillResult{}, declined
	}
	if err := settled.Err(); err != nil {
		return DrillResult{}, err
	}
	if completed.Status != "passed" {
		if completed.Failure == nil {
			return completed, fmt.Errorf("restore drill settled as %s without failure evidence", completed.Status)
		}
		return completed, fmt.Errorf("restore drill failed at %s (%s): %s",
			completed.Failure.Stage, completed.Failure.ReasonCode, completed.Failure.Detail)
	}
	return completed, nil
}
