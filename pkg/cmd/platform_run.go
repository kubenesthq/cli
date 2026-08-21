package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/storage"
	"kubenest.io/cli/pkg/uninstall"
)

// controlPlaneClient builds an authenticated client from the stored config.
// Install requires a control plane: the cluster is registered before anything
// is written to a machine, because there is no safe upgrade without a record
// of what was installed, and fleet telemetry has to start at install rather
// than at the first support call.
func controlPlaneClient() (*api.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if cfg.ControlPlaneURL == "" {
		return nil, fmt.Errorf("no control plane configured: run `kubenest login --control-plane https://...` first")
	}
	creds, err := config.LoadCredentials()
	if err != nil {
		return nil, err
	}
	token := creds.TokenFor(cfg.ControlPlaneURL)
	if token == "" {
		return nil, fmt.Errorf("not logged in to %s: run `kubenest login` first", cfg.ControlPlaneURL)
	}
	return api.New(cfg.ControlPlaneURL, api.WithToken(token))
}

// runInstall is `kubenest platform install`.
func runInstall(ctx context.Context, out io.Writer, f InstallFlags) error {
	client, err := controlPlaneClient()
	if err != nil {
		return err
	}

	// The bundle manifest comes from the control plane, not from a file on
	// this machine: it is a versioned artifact of the release, and the record
	// written in stage 12 has to be checkable against the same document.
	raw, err := client.BundleManifest(ctx, f.Bundle)
	if err != nil {
		return err
	}
	bundle, err := manifest.Parse(raw)
	if err != nil {
		return fmt.Errorf("bundle %s from the control plane is not a valid manifest: %w", f.Bundle, err)
	}

	opts := install.Options{
		Bundle:        f.Bundle,
		Name:          f.Name,
		Org:           f.Org,
		Servers:       f.Servers,
		Agents:        f.Agents,
		HATier:        f.HATier,
		Profiles:      f.Profiles,
		SSHUser:       f.SSHUser,
		SSHKey:        f.SSHKey,
		StorageDevice: f.StorageDevice,
		BackupTarget:  f.BackupTarget,
	}

	journalPath, err := install.JournalPath(f.Name)
	if err != nil {
		return err
	}
	journal, err := install.OpenJournal(journalPath, opts.Identity())
	if err != nil {
		return err
	}
	if entry, resuming := journal.LastFailure(); resuming {
		fmt.Fprintf(out, "Resuming: the previous run stopped at stage %s (%s).\nCompleted stages will be skipped.\n\n",
			entry.Stage, entry.At.Format(time.RFC3339))
	}

	session := &install.Session{
		ID:       install.NewRunID(),
		Opts:     opts,
		Bundle:   bundle,
		Jnl:      journal,
		Reporter: converge.NewTextReporter(out),
		Out:      out,
		API:      client,
	}
	defer session.Close()

	// Printed locally AND published to the control plane, from the same
	// transition: the operator at the terminal and the console watching the
	// install see the same thirteen stages.
	session.Emit = install.Emitters{
		install.TextEmitter{W: out},
		install.NewControlPlaneEmitter(client, func() string { return journal.ClusterID }),
	}

	fmt.Fprintf(out, "Installing platform bundle %s on %d node(s), %s tier.\n",
		f.Bundle, len(f.Servers)+len(f.Agents), f.HATier)
	fmt.Fprintf(out, "Nothing is written to any machine until stage 3.\n\n")

	result, err := install.Execute(ctx, session, install.Plan(session))
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "\nInstalled in %s.\n", result.Elapsed.Round(time.Second))
	if len(result.Skipped) > 0 {
		fmt.Fprintf(out, "Skipped %d stage(s) completed by an earlier run: %s\n",
			len(result.Skipped), strings.Join(result.Skipped, ", "))
	}
	fmt.Fprintf(out, "Journal: %s\n", journal.Path())
	return nil
}

// runUninstall is `kubenest platform uninstall --confirm`.
//
// It reads the journal for the node list and the volume-group ownership. It
// also works WITHOUT one — a lost journal must not strand an operator on a
// machine they want back — but then it refuses to remove any volume group,
// because ownership it cannot establish is treated as the customer's.
func runUninstall(ctx context.Context, out io.Writer, name string, destroyData bool, f InstallFlags) error {
	journal, journalPath, err := findJournal(name)
	if err != nil {
		return err
	}

	servers, agents := f.Servers, f.Agents
	var ownership storage.Ownership
	device := ""
	if journal != nil {
		recorded, err := install.Recorded(journal)
		if err != nil {
			return err
		}
		if len(servers) == 0 && len(agents) == 0 {
			servers, agents = install.NodesFromJournal(journal)
		}
		ownership = recorded.Ownership
		device = recorded.Device
		if f.SSHUser == "" {
			fmt.Fprintf(out, "Using the journal at %s.\n", journalPath)
		}
	} else {
		fmt.Fprintf(out, "No install journal found%s.\n"+
			"Uninstalling from the nodes given on the command line; no volume group will be removed, because there is no record of who created it.\n",
			forCluster(name))
	}
	if len(servers) == 0 && len(agents) == 0 {
		return fmt.Errorf("no nodes to uninstall: pass --server (and --agent) for the hosts to clean, or --name for a cluster with an install journal")
	}

	sshOpts := sshx.Options{User: f.SSHUser, KeyPath: f.SSHKey, DialTimeout: 15 * time.Second}
	var nodes []uninstall.Node
	var closers []io.Closer
	defer func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}()
	dial := func(address string, role uninstall.Role) error {
		endpoint, err := sshx.Resolve(address, sshOpts)
		if err != nil {
			return fmt.Errorf("%s: %w", address, err)
		}
		client, err := sshx.Dial(ctx, endpoint, sshOpts)
		if err != nil {
			return fmt.Errorf("%s: %w", address, err)
		}
		closers = append(closers, client)
		nodes = append(nodes, uninstall.Node{Address: address, Role: role, Runner: client})
		return nil
	}
	for _, address := range servers {
		if err := dial(address, uninstall.RoleServer); err != nil {
			return err
		}
	}
	for _, address := range agents {
		if err := dial(address, uninstall.RoleAgent); err != nil {
			return err
		}
	}

	if err := uninstall.Run(ctx, uninstall.Options{
		Nodes:       nodes,
		DestroyData: destroyData,
		Ownership:   ownership,
		Device:      device,
		Out:         out,
	}); err != nil {
		return err
	}

	if journal != nil {
		// The journal outliving its cluster would make the next install on
		// these hosts refuse to run, for a cluster that no longer exists.
		if err := journal.Remove(); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "\nDone. The machines are back to a known state.\n")
	return nil
}

func forCluster(name string) string {
	if name == "" {
		return ""
	}
	return " for cluster " + name
}

// findJournal locates the install journal. With a name, it is that cluster's.
// Without one, a single journal is used and several is a question rather than
// a guess — uninstalling the wrong cluster is not recoverable.
func findJournal(name string) (*install.Journal, string, error) {
	if name != "" {
		path, err := install.JournalPath(name)
		if err != nil {
			return nil, "", err
		}
		if _, err := os.Stat(path); err != nil {
			return nil, path, nil
		}
		journal, err := install.ReadJournal(path)
		return journal, path, err
	}

	dir, err := install.JournalDir()
	if err != nil {
		return nil, "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", nil // no journals at all
	}
	var paths []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(paths)
	switch len(paths) {
	case 0:
		return nil, "", nil
	case 1:
		journal, err := install.ReadJournal(paths[0])
		return journal, paths[0], err
	default:
		var names []string
		for _, p := range paths {
			names = append(names, strings.TrimSuffix(filepath.Base(p), ".json"))
		}
		return nil, "", fmt.Errorf("this machine has install journals for %s: pass --name to say which cluster to uninstall",
			strings.Join(names, ", "))
	}
}
