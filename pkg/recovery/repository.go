package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/k3s"
)

// ErrWrongKit reports that the cluster is already pointed at a repository whose
// password is not the one this kit carries. It is never retried: kopia's
// repository password is a repository key, not a rotation candidate, and
// "retry until it works" is how a repository gets re-encrypted over.
var ErrWrongKit = errors.New("this is not the kit whose password opened the cluster's backup repository")

// Repository is the Velero backup repository a recovery set binds: the
// repository's identity, and the password that opens it.
type Repository struct {
	// ID is the repository's identity — the BackupRepository object's name,
	// which the install that wrote the kit derived from the managed storage
	// location and the repository type. A recovery opens THIS repository; it
	// never creates another.
	ID string
	// Password is the kit's velero-repo-password. It is a credential and is
	// never logged, echoed or written anywhere but the cluster Secret.
	Password string
}

// Validate refuses an incomplete repository before anything is read.
func (r Repository) Validate() error {
	if strings.TrimSpace(r.ID) == "" {
		return errors.New("a recovery needs the repository identity the recovery set names: without it there is no way to tell the repository this cluster must open from one Velero would initialise over it")
	}
	if r.Password == "" {
		return errors.New("a recovery needs the repository password the kit carries: creating a repository under a generated password would write new volume data into a repository the kit cannot open")
	}
	return nil
}

// RepositoryState is what the cluster said before and after the repository was
// opened, so the operation record can prove what was and was not changed.
type RepositoryState struct {
	// SecretExisted is whether the cluster already had the repository-password
	// Secret. False means this recovery created it — which is the fresh-host
	// case, and must happen BEFORE Velero starts, or Velero writes its vendored
	// default password and the repository opens under that instead.
	SecretExisted bool
	// PasswordMatches is whether the stored password is the kit's.
	PasswordMatches bool
	// RepositoryID is the id of the existing BackupRepository object for the
	// repository this recovery opens, or empty when the cluster has none yet
	// (Velero creates it on first connect, in the recovery's target).
	RepositoryID string
	// Existing lists every BackupRepository the cluster already had, in the
	// order kubectl returned them, so "nothing was reinitialised" is visible
	// rather than asserted.
	Existing []string
}

// Existed reports whether the cluster already held the repository-password
// Secret, which is what tells a fresh host (this recovery created it) from a
// cluster whose repository was already configured.
func (s RepositoryState) Existed() bool { return s.SecretExisted }

// OpenRepository points the cluster at the repository the recovery set names,
// using the kit's password, and refuses to initialise a new one.
//
// WHAT IT DELIBERATELY DOES NOT DO:
//
//   - it never generates a password. A created Secret carries the kit's own
//     bytes, because a repository is opened by the password it was encrypted
//     under — probe P3 question 2 measured Velero reopening an existing
//     repository with the supplied password and refusing a wrong one;
//   - it never deletes or re-creates a BackupRepository object. Velero creates
//     one the first time it connects, and an object that already exists is the
//     repository's identity: replacing it is how a restored cluster starts
//     writing into a NEW repository beside the old data;
//   - it refuses when the cluster's stored password is not the kit's, and names
//     that as a wrong kit rather than retrying. `BackupRepository` reading
//     Ready does not prove the password is right (probe P3 question 2: Velero
//     reports Ready under a wrong password and fails at the first read), so the
//     only honest place to catch it is here, before a restore runs.
//
// The Secret must exist before the Velero server starts, which is why the
// recovery plan runs this stage before the one that installs Velero.
func OpenRepository(ctx context.Context, r k3s.Runner, want Repository) (RepositoryState, error) {
	var state RepositoryState
	if err := want.Validate(); err != nil {
		return state, err
	}

	stored, err := backup.RepositoryPassword(ctx, r)
	switch {
	case err == nil:
		state.SecretExisted = true
		if stored != want.Password {
			return state, fmt.Errorf("%w: the cluster's %s Secret holds a different repository password than the recovery kit does. The kit belongs to another cluster, or to another artifact of this one; recovering with it would restore into a repository whose password is not the kit's. Nothing was changed",
				ErrWrongKit, backup.RepositorySecretName)
		}
		state.PasswordMatches = true
	case isMissingSecret(err):
		// The host is fresh and has no Secret: this is the whole reason the
		// stage runs before Velero. Creating it is the one write this function
		// performs.
		if err := writeRepositoryPassword(ctx, r, want.Password); err != nil {
			return state, err
		}
		state.PasswordMatches = true
	default:
		// The Secret is there and unusable — a missing, undecodable or empty
		// key. It is NOT regenerated: the repository already encrypted under
		// those bytes cannot be re-derived from a new password.
		return state, fmt.Errorf("the cluster's %s Secret cannot be used as it stands and is deliberately not replaced, because the repository already encrypted under it cannot be re-derived: %w", backup.RepositorySecretName, err)
	}

	existing, err := listRepositories(ctx, r)
	if err != nil {
		return state, err
	}
	state.Existing = existing
	for _, name := range existing {
		if name == want.ID {
			state.RepositoryID = name
		}
	}
	return state, nil
}

// isMissingSecret reports whether a repository-password read failed because the
// Secret is not on the cluster. pkg/backup keeps its sentinel unexported, and
// the distinction the caller must make is exactly this one: absent is the fresh
// host, anything else is a Secret that must not be replaced.
func isMissingSecret(err error) bool {
	return strings.Contains(err.Error(), "is not on this cluster yet")
}

// writeRepositoryPassword creates the repository-password Secret, and only the
// Secret: `kubectl create`, never `apply`, so two writers cannot silently
// overwrite each other's password. The document travels over STDIN because its
// content is a credential.
func writeRepositoryPassword(ctx context.Context, r k3s.Runner, password string) error {
	nsDoc := []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + backup.Namespace + "\n")
	if res, err := r.RunInput(ctx, "sudo -n k3s kubectl apply -f -", bytes.NewReader(nsDoc)); err != nil {
		return fmt.Errorf("creating namespace %s: %w", backup.Namespace, err)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("creating namespace %s: exit %d: %s", backup.Namespace, res.ExitCode, firstLine(res.Stderr))
	}
	doc, err := backup.RepositoryPasswordSecret(password)
	if err != nil {
		return fmt.Errorf("rendering secret %s/%s: %w", backup.Namespace, backup.RepositorySecretName, err)
	}
	res, err := r.RunInput(ctx, "sudo -n k3s kubectl create -f -", bytes.NewReader(doc))
	if err != nil {
		return fmt.Errorf("creating secret %s/%s: %w", backup.Namespace, backup.RepositorySecretName, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("creating secret %s/%s: exit %d: %s. The Secret is what opens the existing repository, so a recovery that cannot write it must not continue and let Velero create one under its vendored default password", backup.Namespace, backup.RepositorySecretName, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

// listRepositories reads the BackupRepository objects Velero has created. A
// cluster whose API has no Velero CRDs (this stage runs before Velero is
// installed) has none, which is not an error.
func listRepositories(ctx context.Context, r k3s.Runner) ([]string, error) {
	out, err := k3s.Kubectl(ctx, r, "get backuprepository -n "+backup.Namespace+" -o json")
	if err != nil {
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "not find") || strings.Contains(err.Error(), "the server doesn't have a resource") {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the cluster's Velero backup repositories: %w", err)
	}
	var listed struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		return nil, fmt.Errorf("the cluster's BackupRepository list is unparsable: %w", err)
	}
	names := make([]string, 0, len(listed.Items))
	for _, item := range listed.Items {
		if item.Metadata.Name != "" {
			names = append(names, item.Metadata.Name)
		}
	}
	return names, nil
}
