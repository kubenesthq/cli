package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
)

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

	var completed DrillResult
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
			return false, converge.State{Object: "restore drill", Status: "running"}, nil
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
