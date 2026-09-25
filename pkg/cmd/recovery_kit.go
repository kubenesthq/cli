package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/config"
	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/s3"
)

// NewRecoveryKitCommand groups the recovery-kit operations.
//
// There is exactly one verb, and it checks rather than restores: a check needs
// no live control plane and no cluster, which is the whole point — the day
// someone needs it, the control plane may be the thing that is gone.
func NewRecoveryKitCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recovery-kit",
		Short: "Check that a cluster's recovery kit and its recovery set are usable",
	}
	cmd.AddCommand(newRecoveryKitCheckCommand())
	return cmd
}

// kitStore is the slice of the bucket a check reads. *s3.Client satisfies it.
type kitStore interface {
	Get(ctx context.Context, key string) ([]byte, error)
}

// kitAnswer is one answer, with the line that explains it. It is a struct
// rather than a bool because four booleans and a string are what stops one
// finding being reported as another — which is the failure mode that matters
// here: "does not decrypt" read as "the set is incomplete" sends an operator
// looking in the wrong place on the worst day of their year.
type kitAnswer struct {
	OK     bool
	Detail string
}

// kitAnswers is a check's four answers. They are deliberately NOT collapsed
// into a verdict: no aggregate means no way to report one finding as another.
type kitAnswers struct {
	// Upload is whether the bucket holds the bytes the fleet wrote.
	Upload kitAnswer
	// Decryption is whether the key supplied opens them.
	Decryption kitAnswer
	// Fingerprints is whether what opened is the key material the kit's header
	// was written for.
	Fingerprints kitAnswer
	// Set is whether the recovery set is complete, belongs to this instance,
	// organisation and cluster, and contains the named backup.
	Set kitAnswer
}

// AllGood reports whether every answer passed. The command exits non-zero when
// it is false; it is never used to hide which answer failed.
func (a kitAnswers) AllGood() bool {
	return a.Upload.OK && a.Decryption.OK && a.Fingerprints.OK && a.Set.OK
}

// kitCheck is one check's inputs.
type kitCheck struct {
	// Local is the local copy of the kit, when this machine has one. It is the
	// authority for the digest comparison, and the copy that is opened when
	// the upload does not match it.
	Local []byte
	// ArtifactID and Scope locate the kit and the set in the bucket.
	ArtifactID string
	Scope      string
	ClusterID  string
	Kind       recoverykit.Kind
	// Expected is what the operator says this instance, organisation and
	// cluster are. Anything the bucket holds that disagrees is refused.
	Expected recoverykit.Binding
	// FleetKey is the private key supplied. It is never logged, never written
	// and never sent anywhere.
	FleetKey string
	// Backup, when set, is a backup the set must contain as Completed.
	Backup string
	Store  kitStore
}

// runKitCheck answers the four questions separately, and refuses outright when
// what it found belongs to a different cluster: a kit or a set from elsewhere
// is not merely suspect, it is the wrong artifact, and continuing would report
// about the wrong cluster.
func runKitCheck(ctx context.Context, in kitCheck) (kitAnswers, error) {
	var out kitAnswers
	if in.Store == nil {
		return out, errors.New("a check needs the bucket: no S3 client was built")
	}
	if in.ArtifactID == "" || in.ClusterID == "" {
		return out, errors.New("a check needs to know which kit to look at: pass --kit-file, or --cluster with --artifact")
	}
	if in.FleetKey == "" {
		return out, errors.New("a check needs the fleet recovery key: pass --fleet-key-file or type it at the prompt")
	}
	if err := validateForCheck(in.Expected); err != nil {
		return out, err
	}

	kitKey := recoverykit.KitKey(in.Scope, in.ClusterID, in.Kind, in.ArtifactID)
	setKey := recoverykit.SetKey(in.Scope, in.ClusterID, in.Kind, in.ArtifactID)

	// The set first: it is the plaintext manifest, and it may be the only
	// digest of the kit that exists when no local copy does.
	setDoc, setErr := in.Store.Get(ctx, setKey)
	var set *recoverykit.Set
	switch {
	case setErr == nil:
		set, setErr = recoverykit.LoadSet(setDoc)
		if setErr != nil {
			out.Set = kitAnswer{Detail: fmt.Sprintf("%s is not a readable recovery set: %v", setKey, setErr)}
		} else if err := set.Verify(in.Expected); err != nil {
			// A refusal, not an answer: the set is about another cluster.
			return out, fmt.Errorf("%s: %w", setKey, err)
		}
	case errors.Is(setErr, s3.ErrNotFound):
		out.Set = kitAnswer{Detail: fmt.Sprintf("no recovery set at %s: nothing in the bucket says which kit and which backup a recovery of this cluster should start from", setKey)}
	default:
		out.Set = kitAnswer{Detail: fmt.Sprintf("could not read %s: %v", setKey, setErr)}
	}

	// The local copy: it is what a later install compares against, and it is
	// the authority on what was written.
	var local *recoverykit.Kit
	var localDigest string
	if len(in.Local) > 0 {
		loaded, err := recoverykit.Load(in.Local)
		if err != nil {
			return out, fmt.Errorf("the local kit copy is not a recovery kit: %w", err)
		}
		if err := loaded.Verify(in.Expected); err != nil {
			return out, fmt.Errorf("the local kit copy: %w", err)
		}
		if loaded.ArtifactID != in.ArtifactID {
			return out, fmt.Errorf("the local kit copy is artifact %s and this check is about %s: checking one kit while holding another would report about the wrong thing", loaded.ArtifactID, in.ArtifactID)
		}
		local = loaded
		localDigest = recoverykit.Digest(in.Local)
	}

	// 1. Is the upload intact?
	fetched, fetchErr := in.Store.Get(ctx, kitKey)
	var uploaded *recoverykit.Kit
	var loadErr error
	switch {
	case fetchErr == nil:
		uploadedDigest := recoverykit.Digest(fetched)
		switch {
		case local != nil && uploadedDigest != localDigest:
			out.Upload = kitAnswer{Detail: fmt.Sprintf("the object at %s is not the kit this machine wrote: uploaded %s, local %s", kitKey, uploadedDigest, localDigest)}
		case local != nil:
			out.Upload = kitAnswer{OK: true, Detail: fmt.Sprintf("the object at %s is byte-identical to the local copy (%s)", kitKey, localDigest)}
		case set != nil && set.Checksums["kit"] != "" && uploadedDigest != set.Checksums["kit"]:
			out.Upload = kitAnswer{Detail: fmt.Sprintf("the object at %s digests %s and the recovery set names %s: there is no local copy to compare with, so the set is the only record of what was written, and it disagrees", kitKey, uploadedDigest, set.Checksums["kit"])}
		case set != nil && set.Checksums["kit"] != "":
			out.Upload = kitAnswer{OK: true, Detail: fmt.Sprintf("the object at %s matches the digest the recovery set records (%s); there is no local copy", kitKey, uploadedDigest)}
		default:
			out.Upload = kitAnswer{Detail: fmt.Sprintf("the object at %s exists (%s) but neither a local copy nor a recovery set records what it should be, so nothing here can say whether it is intact", kitKey, uploadedDigest)}
		}
		uploaded, loadErr = recoverykit.Load(fetched)
	case errors.Is(fetchErr, s3.ErrNotFound):
		out.Upload = kitAnswer{Detail: fmt.Sprintf("there is no recovery kit at %s: the upload never completed, or it was deleted", kitKey)}
	default:
		out.Upload = kitAnswer{Detail: fmt.Sprintf("could not read %s: %v", kitKey, fetchErr)}
	}

	// 2. Does the key supplied open it? Every copy that exists is opened, and
	// the answer names the ones that did not — so "the key is wrong" and "the
	// upload is damaged" can never be reported as each other.
	//
	// A copy that cannot even be read back as a kit counts as NOT opening: the
	// question this answer must never get wrong is the dangerous direction —
	// saying the kit opens when the copy in the bucket is not the kit.
	type copyOf struct {
		name string
		kit  *recoverykit.Kit
		err  error
	}
	copies := []copyOf{}
	if local != nil {
		copies = append(copies, copyOf{name: "local", kit: local})
	}
	if fetched != nil {
		if loadErr != nil {
			copies = append(copies, copyOf{name: "uploaded", err: fmt.Errorf("it is not a readable recovery kit: %w", loadErr)})
		} else if uploaded != nil {
			copies = append(copies, copyOf{name: "uploaded", kit: uploaded})
		}
	}
	if len(copies) == 0 {
		out.Decryption = kitAnswer{Detail: "there is nothing to open: no local copy and no uploaded copy could be read"}
		out.Fingerprints = kitAnswer{Detail: "there is nothing to fingerprint: no copy could be read"}
	} else {
		var opened []string
		var decryptFail, fpFail []string
		for _, c := range copies {
			if c.err != nil {
				decryptFail = append(decryptFail, fmt.Sprintf("%s: %v", c.name, c.err))
				continue
			}
			secrets, err := c.kit.Secrets(in.FleetKey)
			if err != nil {
				decryptFail = append(decryptFail, fmt.Sprintf("%s: %v", c.name, err))
				continue
			}
			opened = append(opened, c.name)
			if err := c.kit.FingerprintsMatch(secrets); err != nil {
				fpFail = append(fpFail, fmt.Sprintf("%s: %v", c.name, err))
			}
		}
		switch {
		case len(decryptFail) == 0:
			out.Decryption = kitAnswer{OK: true, Detail: fmt.Sprintf("the fleet key supplied opens %s (keys carried: %s)", strings.Join(opened, " and "), strings.Join(copies[0].kit.KeyNames(), ", "))}
		case len(opened) == 0:
			out.Decryption = kitAnswer{Detail: fmt.Sprintf("the fleet key supplied does NOT open the kit — %s. This is not the key this kit was sealed to, or it was mistyped", strings.Join(decryptFail, "; "))}
		default:
			out.Decryption = kitAnswer{Detail: fmt.Sprintf("the fleet key supplied opens %s but not %s — the copies are not the same kit, and the bucket's copy is the one a recovery has, so this counts as not opening: %s", strings.Join(opened, ", "), strings.Join(decryptFail, "; "), strings.Join(decryptFail, "; "))}
		}
		switch {
		case len(opened) == 0:
			out.Fingerprints = kitAnswer{Detail: "fingerprints could not be checked: no copy opened"}
		case len(fpFail) == 0:
			out.Fingerprints = kitAnswer{OK: true, Detail: fmt.Sprintf("what opened is the key material the header was written for (%s)", strings.Join(copies[0].kit.KeyNames(), ", "))}
		default:
			out.Fingerprints = kitAnswer{Detail: fmt.Sprintf("the kit opened but the fingerprints disagree: %s", strings.Join(fpFail, "; "))}
		}
	}

	// 4. Is the set complete, is it about this cluster, and does it name the
	// backup the operator asked about?
	if set != nil {
		if !set.Complete {
			out.Set = kitAnswer{Detail: fmt.Sprintf("%s records an upload that never completed: a partial upload is never something to recover from", setKey)}
		} else if set.ArtifactID != in.ArtifactID {
			out.Set = kitAnswer{Detail: fmt.Sprintf("%s is about kit artifact %s, and this check is about %s", setKey, set.ArtifactID, in.ArtifactID)}
		} else if in.Backup != "" {
			var found *recoverykit.Backup
			for i := range set.Backups {
				if set.Backups[i].Name == in.Backup {
					found = &set.Backups[i]
				}
			}
			switch {
			case found == nil:
				out.Set = kitAnswer{Detail: fmt.Sprintf("the set does not name backup %q; it names %s", in.Backup, backupNames(set))}
			case !found.Completed():
				out.Set = kitAnswer{Detail: fmt.Sprintf("backup %q is recorded as %s, which is not something to recover from", in.Backup, found.Status)}
			default:
				out.Set = kitAnswer{OK: true, Detail: fmt.Sprintf("backup %q completed at %s and is recorded in the set, which belongs to %s", in.Backup, found.CompletedAt.UTC().Format("2006-01-02T15:04:05Z"), in.Expected)}
			}
		} else if latest, ok := set.Latest(); ok {
			out.Set = kitAnswer{OK: true, Detail: fmt.Sprintf("the set belongs to %s and names %d completed backup(s), the newest %q at %s; pass --backup to check a specific one", in.Expected, len(set.Backups), latest.Name, latest.CompletedAt.UTC().Format("2006-01-02T15:04:05Z"))}
		} else {
			out.Set = kitAnswer{Detail: "the set names no completed backup: it was written with the install baseline and no backup has been recorded in it yet"}
		}
	}
	return out, nil
}

func backupNames(set *recoverykit.Set) string {
	if len(set.Backups) == 0 {
		return "no backups"
	}
	names := make([]string, 0, len(set.Backups))
	for _, b := range set.Backups {
		names = append(names, b.Name)
	}
	return strings.Join(names, ", ")
}

// validateForCheck makes sure the operator said enough for the check to mean
// something: the whole value of it is comparing what is in the bucket with
// what this instance, organisation and cluster are.
func validateForCheck(b recoverykit.Binding) error {
	if b.InstanceID == "" {
		return errors.New("a check needs the instance id to compare the kit and the set against: pass --instance (or it is read from this machine's config after a --control-plane install)")
	}
	if b.Kind == recoverykit.KindCluster && (b.OrganisationID == "" || b.ClusterID == "") {
		return errors.New("a check needs the organisation and cluster ids to compare the kit and the set against: pass --organisation and --cluster")
	}
	return nil
}

// s3TargetFlag renders a kit's recorded S3 location as the --target flag
// string, so a check defaults to exactly where the install uploaded.
func s3TargetFlag(loc recoverykit.Location, prefix string) string {
	return backup.TargetURL(backup.Target{
		Endpoint: loc.Endpoint,
		Bucket:   loc.Bucket,
		Region:   loc.Region,
		Prefix:   strings.Trim(prefix, "/"),
	})
}

func newRecoveryKitCheckCommand() *cobra.Command {
	var (
		kitFile    string
		clusterID  string
		artifactID string
		kind       string
		target     string
		instance   string
		org        string
		backupName string
		keyFile    string
	)
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Prove a cluster's recovery kit and recovery set are usable, without a control plane",
		Long: `Report, separately, whether the kit's upload is intact, whether it decrypts with
the fleet key you supply, whether its fingerprints match, and whether the
recovery set is complete and belongs to this instance, organisation and cluster.

This needs NO live control plane and NO cluster: it reads the bucket, a local
copy of the kit if this machine has one, and the fleet recovery key. The key is
read from --fleet-key-file or typed at a prompt, and is never written anywhere.

It says plainly what it does not do: these checks say the artifact is present,
readable and about the right cluster. ONLY A COMPLETED RESTORE proves a restore
works.

A kit or a recovery set that belongs to another cluster is refused before
anything else is reported, and the refusal names the instance, organisation and
cluster it does belong to.`,
		Example: `  # From the machine that installed the cluster.
  kubenest recovery-kit check --cluster 01a02362-f8a3-7dd6-aa07-2f10ed7a5c17 \
    --organisation 0f3d… --instance 9c11… --fleet-key-file ~/fleet-key.txt

  # Checking the backup you actually mean to restore from.
  kubenest recovery-kit check --cluster 01a02362-… --organisation 0f3d… --instance 9c11… \
    --backup daily-20260925020000`,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()

			// The key first, from the file or the prompt. Never from a flag:
			// a secret on a command line lands in shell history and in ps.
			fleetKey, err := readFleetKey(cmd.InOrStdin(), out, keyFile)
			if err != nil {
				return err
			}

			// The local copy, if this machine has one.
			var local []byte
			var kit *recoverykit.Kit
			switch {
			case kitFile != "":
				local, err = os.ReadFile(kitFile)
				if err != nil {
					return fmt.Errorf("reading %s: %w", kitFile, err)
				}
				kit, err = recoverykit.Load(local)
				if err != nil {
					return err
				}
			case clusterID != "":
				kit, err = recoverykit.NewestLocal(clusterID, recoverykit.Kind(kind))
				if err == nil {
					path, pathErr := recoverykit.LocalPath(clusterID, recoverykit.Kind(kind), kit.ArtifactID)
					if pathErr != nil {
						return pathErr
					}
					local, err = os.ReadFile(path)
					if err != nil {
						return fmt.Errorf("reading %s: %w", path, err)
					}
				}
			}
			if kit == nil && artifactID == "" {
				return errors.New("a check needs to know which kit to look at: pass --kit-file, or --cluster (for this machine's newest local kit) with --artifact for a kit you do not have locally")
			}

			// Fill in what the local kit knows, so the operator only names
			// what they must.
			binding := recoverykit.Binding{Kind: recoverykit.Kind(kind)}
			if kit != nil {
				binding = kit.Binding
				if artifactID == "" {
					artifactID = kit.ArtifactID
				}
				if clusterID == "" {
					clusterID = kit.Binding.ClusterID
				}
				if target == "" && kit.S3Location.Bucket != "" {
					target = s3TargetFlag(kit.S3Location, kit.S3Location.Prefix)
				}
			}
			if instance != "" {
				binding.InstanceID = instance
			}
			if org != "" {
				binding.OrganisationID = org
			}
			if clusterID != "" {
				binding.ClusterID = clusterID
			}
			if binding.InstanceID == "" {
				if cfg, err := config.Load(); err == nil {
					binding.InstanceID = cfg.InstanceID
				}
			}
			if clusterID == "" && binding.ClusterID != "" {
				clusterID = binding.ClusterID
			}

			target2, err := backup.ParseTarget(target, envFirst("KUBENEST_BACKUP_ACCESS_KEY_ID", "AWS_ACCESS_KEY_ID"), envFirst("KUBENEST_BACKUP_SECRET_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY"))
			if err != nil {
				return err
			}
			client, err := target2.S3Client()
			if err != nil {
				return err
			}

			answers, err := runKitCheck(cmd.Context(), kitCheck{
				Local:      local,
				ArtifactID: artifactID,
				Scope:      strings.Trim(target2.Prefix, "/"),
				ClusterID:  clusterID,
				Kind:       binding.Kind,
				Expected:   binding,
				FleetKey:   fleetKey,
				Backup:     backupName,
				Store:      client,
			})
			// A refusal (a foreign kit or set) is reported as a refusal, not
			// as an answer: it is not that the artifact failed a check, it is
			// that it is not this cluster's artifact at all.
			if err != nil {
				return err
			}

			fmt.Fprintf(out, "recovery-kit check for %s %s (artifact %s)\n\n", binding.Kind, clusterID, artifactID)
			reportAnswer(out, "kit upload intact", answers.Upload)
			reportAnswer(out, "opens with the key supplied", answers.Decryption)
			reportAnswer(out, "fingerprints match", answers.Fingerprints)
			reportAnswer(out, "recovery set complete and belongs here", answers.Set)
			fmt.Fprintf(out, "\nOnly a completed restore proves restoration. These checks say the artifact is present, readable and about the right cluster; they do not prove a cluster comes back.\n")
			if !answers.AllGood() {
				return fmt.Errorf("recovery-kit check failed: at least one answer above is no (see each line; they are reported separately because they have different fixes)")
			}
			return nil
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&kitFile, "kit-file", "", "the local copy of the kit document to check against (default: this machine's newest local kit for --cluster)")
	fs.StringVar(&keyFile, "fleet-key-file", "", "file holding the fleet recovery key (the AGE-SECRET-KEY-1... printed once at control-plane install); prompted for when omitted")
	fs.StringVar(&clusterID, "cluster", "", "the cluster's immutable id; with no --kit-file this finds this machine's newest local kit for it")
	fs.StringVar(&artifactID, "artifact", "", "the kit artifact id to check (default: the local kit's)")
	fs.StringVar(&kind, "kind", string(recoverykit.KindCluster), "kit scope: cluster or control-plane")
	fs.StringVar(&target, "target", "", "the bucket: s3://<bucket>[/<prefix>]?endpoint=<host>&region=<region> (default: the S3 location the local kit records)")
	fs.StringVar(&instance, "instance", "", "the instance id the kit must belong to (default: this machine's config)")
	fs.StringVar(&org, "organisation", "", "the organisation id the kit must belong to")
	fs.StringVar(&backupName, "backup", "", "a backup the recovery set must record as completed")
	return cmd
}

func reportAnswer(w io.Writer, what string, a kitAnswer) {
	mark := "NO "
	if a.OK {
		mark = "yes"
	}
	fmt.Fprintf(w, "  [%s] %s: %s\n", mark, what, a.Detail)
}

// readFleetKey reads the key from a file, or prompts for it without echoing.
// It never comes from a flag: a fleet key on a command line is a fleet key in
// shell history and in `ps`.
func readFleetKey(in io.Reader, out io.Writer, path string) (string, error) {
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", path, err)
		}
		key := strings.TrimSpace(string(raw))
		if key == "" {
			return "", fmt.Errorf("%s is empty: it must hold the AGE-SECRET-KEY-1... the control-plane install printed once", path)
		}
		return key, nil
	}
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(out, "fleet recovery key (input is not echoed): ")
		raw, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return "", fmt.Errorf("reading the fleet recovery key: %w", err)
		}
		key := strings.TrimSpace(string(raw))
		if key == "" {
			return "", errors.New("no fleet recovery key was entered")
		}
		return key, nil
	}
	return "", errors.New("the fleet recovery key is required: pass --fleet-key-file, or run this where a terminal can prompt for it")
}
