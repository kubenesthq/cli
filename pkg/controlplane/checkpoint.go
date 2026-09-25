// The control plane's own recovery target (kn-t47).
//
// WHY IT IS SEPARATE FROM A CLUSTER'S BACKUP TARGET. The control plane's
// checkpoints are the fleet's recovery material: they hold every organisation,
// member, role and token floor of the installation. A cluster's backup
// credential, meanwhile, is read access to THAT cluster — Velero's resource
// archives carry workload Secrets in plaintext, which is why `backup
// set-target` refuses a credential that can reach outside its own prefix (see
// pkg/backup/target.go::VerifyScope).
//
// A single credential that may write both is therefore the worst of both: one
// leaked backup credential becomes the fleet's recovery history. So the
// checkpoints get a principal of their own, and this file is where the CLI
// proves it is one BEFORE it is handed to the chart: the credential must write,
// read and list <bucket>/control-plane/ and be REFUSED, by name, on every
// cluster prefix the install knows about.
//
// The CLI cannot create storage credentials — the customer's policy does that —
// so everything here either verifies what a credential may do or records it in
// the cluster exactly as the chart's environment reads it.
package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/s3"
)

const (
	// CheckpointCredentialsSecret is the Secret the chart reads the control
	// plane's object-store credentials from (checkpoint.credentialsSecret).
	// Named here because the installer CREATES it, and a chart default the
	// installer disagrees with is a CronJob whose pods cannot start.
	CheckpointCredentialsSecret = "kubenest-cp-checkpoint-credentials"

	// The keys inside that Secret. They are the environment the chart wires
	// into every checkpoint container, and the two optional ones are omitted
	// rather than written empty: an empty AWS_ENDPOINT_URL would send the
	// upload at the AWS default endpoint instead of the customer's store.
	keyCheckpointAccessKeyID     = "AWS_ACCESS_KEY_ID"
	keyCheckpointSecretAccessKey = "AWS_SECRET_ACCESS_KEY"
	keyCheckpointRegion          = "AWS_DEFAULT_REGION"
	keyCheckpointEndpoint        = "AWS_ENDPOINT_URL"

	// The environment the CONTROL-PLANE principal's credentials are read from.
	//
	// Separate variables from KUBENEST_BACKUP_* on purpose, and not a
	// convenience: those name the cluster's credential, and accepting them here
	// would make "the checkpoints use their own principal" a claim nothing
	// enforces. A run that supplies only the cluster's credential is refused by
	// VerifyScope, which says which variable to set.
	EnvCheckpointAccessKeyID     = "KUBENEST_CHECKPOINT_ACCESS_KEY_ID"
	EnvCheckpointSecretAccessKey = "KUBENEST_CHECKPOINT_SECRET_ACCESS_KEY"

	// keyCheckpointProbe is the object the scope check writes inside the
	// control-plane prefix to prove the credential can write, read and list
	// there. It is overwritten on every run rather than accumulated.
	keyCheckpointProbe = "scope-check.json"
)

// CheckpointTarget is where the control plane's checkpoints are written, and
// the credential that writes them.
type CheckpointTarget struct {
	// Recipient is the FLEET recovery key's public half, in age's `age1...`
	// form. The dump is encrypted to it and only the private half — which
	// never touches a host — opens it.
	Recipient string
	// Bucket and Prefix are where the checkpoint objects live. The prefix is
	// the control plane's own directory inside the bucket, and it is the thing
	// the principal's policy is scoped to.
	Bucket string
	Prefix string
	// Region and Endpoint locate the store; Endpoint is empty on AWS.
	Region   string
	Endpoint string
	// AccessKeyID and SecretAccessKey are the CONTROL PLANE's own principal.
	AccessKeyID     string
	SecretAccessKey string
	// CredentialsSecret is the chart value `checkpoint.credentialsSecret`.
	CredentialsSecret string
}

// NewCheckpointTarget builds the control plane's target from the install's
// backup target, the fleet recipient that install holds, and the checkpoint
// principal's credentials.
//
// The BUCKET is the backup target's: a control plane has one store, and the
// separation between a cluster's backups and the fleet's recovery material is
// the PREFIX plus the principal, not a second bucket. The prefix is not the
// target's either — it is the control plane's directory, fixed here so the CLI
// and the chart cannot disagree about where a recovery looks.
func NewCheckpointTarget(target backup.Target, recipient, accessKeyID, secretAccessKey string) CheckpointTarget {
	return CheckpointTarget{
		Recipient:         recipient,
		Bucket:            target.Bucket,
		Prefix:            backup.ControlPlanePrefix,
		Region:            target.Region,
		Endpoint:          target.Endpoint,
		AccessKeyID:       accessKeyID,
		SecretAccessKey:   secretAccessKey,
		CredentialsSecret: CheckpointCredentialsSecret,
	}
}

// ownPrefix is the control-plane directory without surrounding slashes.
func (t CheckpointTarget) ownPrefix() string { return strings.Trim(t.Prefix, "/") }

// Validate refuses a target the chart could not render, or one whose scope
// could not be proved.
func (t CheckpointTarget) Validate() error {
	if t.Bucket == "" {
		return errors.New("a control-plane checkpoint needs a bucket: it is the store the recovery objects are uploaded to")
	}
	if t.Recipient == "" {
		return errors.New("a control-plane checkpoint needs the fleet recipient (age1...) to seal the dump to: a checkpoint nobody can open is not a recovery point")
	}
	if t.AccessKeyID == "" || t.SecretAccessKey == "" {
		return fmt.Errorf("a control-plane checkpoint needs its OWN principal: set %s and %s to a credential that may write <bucket>/%s and nothing else. The cluster's KUBENEST_BACKUP_* credential is deliberately not accepted here — read access to a cluster's backups is cluster-admin on that cluster, and one credential that reaches both is one leak away from the fleet's recovery history",
			EnvCheckpointAccessKeyID, EnvCheckpointSecretAccessKey, backup.ControlPlanePrefix)
	}
	if strings.ContainsAny(t.Recipient, "\n\r") {
		return errors.New("the fleet recipient contains a newline: a value that cannot be a YAML scalar verbatim is one the chart would read differently from how it was written")
	}
	if t.ownPrefix() == "" {
		return fmt.Errorf("the control-plane prefix is empty, so it is the bucket root: the cluster prefixes are inside it and no policy can grant one without the other. It is %q", backup.ControlPlanePrefix)
	}
	return nil
}

// S3Client builds the client the scope check probes with: the same
// coordinates and credentials the chart hands the checkpoint containers.
func (t CheckpointTarget) S3Client() (*s3.Client, error) {
	return s3.New(s3.Config{
		Endpoint:        t.Endpoint,
		Bucket:          t.Bucket,
		Region:          t.Region,
		AccessKeyID:     t.AccessKeyID,
		SecretAccessKey: t.SecretAccessKey,
	})
}

// VerifyScope proves the credential IS the control plane's principal: it can do
// its job inside the control-plane prefix and CANNOT reach a cluster's.
//
// This is the mirror of backup.Target.VerifyScope, and it exists for the same
// reason. Read access to a cluster's backups is cluster-admin on that cluster;
// read access to the control-plane prefix is the fleet's recovery material. A
// credential that can reach both is a single leaked key for everything, so the
// two checks are each other's refusal.
//
// "Refused" is the distinction that matters. A credential that reaches a
// cluster's prefix and finds no key there has still reached it, so an absent
// object is a failure of this check, not a pass — only AccessDenied is.
//
// clusterPrefixes are the prefixes the caller KNOWS belong to a cluster. The
// CLI knows one per install (the backup target's own prefix); it is the exact
// question, rather than guessing at a layout.
func (t CheckpointTarget) VerifyScope(ctx context.Context, probe backup.Bucket, clusterPrefixes []string) error {
	if probe == nil {
		return errors.New("the checkpoint credential's scope check needs an S3 client")
	}
	if err := t.Validate(); err != nil {
		return err
	}
	own := t.ownPrefix()

	probeKey := path.Join(own, keyCheckpointProbe)
	body := []byte(fmt.Sprintf("{\"written_by\":\"kubenest platform install --control-plane\",\"scope\":%q,\"at\":%q}\n", own, time.Now().UTC().Format(time.RFC3339)))
	if err := probe.Put(ctx, probeKey, body); err != nil {
		return fmt.Errorf("the checkpoint credential cannot write the control-plane prefix %q (PutObject %s): grant it write on <bucket>/%s*: %w", own, probeKey, own, err)
	}
	got, err := probe.Get(ctx, probeKey)
	if err != nil {
		return fmt.Errorf("the checkpoint credential cannot read back what it just wrote (%s, GetObject %s): the weekly drill restores from this prefix and must be able to read it: %w", own, probeKey, err)
	}
	if !bytes.Equal(got, body) {
		return fmt.Errorf("the object read back from %s is not the one written: the store or the credential is doing something other than what this check assumes", probeKey)
	}
	if _, _, err := probe.List(ctx, own+"/"); err != nil {
		return fmt.Errorf("the checkpoint credential cannot list the control-plane prefix %q (ListObjectsV2 %s/): listing is how the restore path finds the newest checkpoint: %w", own, own, err)
	}

	for _, cluster := range clusterPrefixes {
		foreign := strings.Trim(cluster, "/") + "/"
		if foreign == "/" {
			continue
		}
		if _, _, err := probe.List(ctx, foreign); err == nil {
			return outsideClusterRefusal(t, foreign, "ListObjectsV2", "it returned a result")
		} else if !errors.Is(err, s3.ErrAccessDenied) {
			return outsideClusterRefusal(t, foreign, "ListObjectsV2", "it returned "+err.Error()+"; a refusal is AccessDenied, and anything else means the credential reached a cluster's prefix")
		}
		key := path.Join(foreign, keyCheckpointProbe)
		if _, err := probe.Get(ctx, key); err == nil {
			return outsideClusterRefusal(t, foreign, "GetObject", "it returned an object")
		} else if !errors.Is(err, s3.ErrAccessDenied) {
			return outsideClusterRefusal(t, foreign, "GetObject", "it returned "+err.Error()+"; a refusal is AccessDenied")
		}
	}
	return nil
}

// outsideClusterRefusal is the refusal a checkpoint credential that reaches a
// cluster's prefix gets, naming the operation that succeeded and the policy the
// customer must apply.
func outsideClusterRefusal(t CheckpointTarget, cluster, op, reached string) error {
	return fmt.Errorf(
		"the control-plane checkpoint credential can read the workload cluster prefix %q in bucket %q: %s succeeded — %s. Refused: read access to a cluster's backups is cluster-admin on that cluster, so a credential that can reach one is not the control plane's principal. Apply a policy granting this credential s3:GetObject, s3:PutObject and s3:ListBucket on <bucket>/%s/* only, and nothing on any cluster prefix",
		cluster, t.Bucket, op, reached, t.ownPrefix())
}

// CredentialsManifest renders the Secret the chart's checkpoint environment
// reads. stringData keeps the values out of the document as base64 noise.
func (t CheckpointTarget) CredentialsManifest() ([]byte, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	stringData := map[string]any{
		keyCheckpointAccessKeyID:     t.AccessKeyID,
		keyCheckpointSecretAccessKey: t.SecretAccessKey,
	}
	// Omitted when unset, never written empty: the chart marks both optional,
	// and an empty AWS_ENDPOINT_URL would send the upload at AWS instead of the
	// store it came from.
	if t.Region != "" {
		stringData[keyCheckpointRegion] = t.Region
	}
	if t.Endpoint != "" {
		stringData[keyCheckpointEndpoint] = t.Endpoint
	}
	doc := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      t.CredentialsSecret,
			"namespace": Namespace,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": "kubenest-cli",
			},
		},
		"type":       "Opaque",
		"stringData": stringData,
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("rendering secret %s/%s: %w", Namespace, t.CredentialsSecret, err)
	}
	return out, nil
}

// EnsureCredentials writes the control-plane principal's credentials into the
// cluster, so the chart's checkpoint containers can upload.
//
// APPLIED, NOT CREATED. The install stage is idempotent and re-runs on resume,
// and a customer who rotates the principal's keys must be able to fix it by
// re-running the install rather than by deleting a Secret first. The document
// travels over stdin and never in the command string: it is a credential, and a
// command string is the argv of the shell on the target host.
func EnsureCredentials(ctx context.Context, r k3s.Runner, t CheckpointTarget) error {
	manifest, err := t.CredentialsManifest()
	if err != nil {
		return err
	}
	res, err := r.RunInput(ctx, "sudo -n k3s kubectl apply -f -", bytes.NewReader(manifest))
	if err != nil {
		return fmt.Errorf("creating secret %s/%s: %w", Namespace, t.CredentialsSecret, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("creating secret %s/%s: exit %d: %s", Namespace, t.CredentialsSecret, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}
