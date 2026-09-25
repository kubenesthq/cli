package cmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/bundles"
	"kubenest.io/cli/pkg/config"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/storage"
	"kubenest.io/cli/pkg/uninstall"
)

// controlPlaneClient builds an authenticated client from the stored config,
// trusting the control plane's own CA when one is stored.
//
// A control plane is what makes a cluster part of a fleet: the console, the
// health view and the multi-cluster day-2 surface. A cluster joins a fleet in
// one of two ways, and the flags say which: it installs the control plane
// itself (`kubenest platform install --control-plane`, the first cluster), or
// it is added to a fleet this machine is already logged in to
// (`kubenest login --control-plane https://api.<domain>`).
func controlPlaneClient() (*api.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if cfg.ControlPlaneURL == "" {
		return nil, fmt.Errorf("no control plane configured: run `kubenest login --control-plane https://api.<domain>` first, " +
			"or install the first cluster with `kubenest platform install --control-plane`, which installs the control plane and logs this machine in to it")
	}
	creds, err := config.LoadCredentials()
	if err != nil {
		return nil, err
	}
	token := creds.TokenFor(cfg.ControlPlaneURL)
	if token == "" {
		return nil, fmt.Errorf("not logged in to %s: run `kubenest login --control-plane %s` first, "+
			"or install the first cluster with `kubenest platform install --control-plane`", cfg.ControlPlaneURL, cfg.ControlPlaneURL)
	}
	opts := []api.Option{api.WithToken(token)}
	if cfg.ControlPlaneCA != "" {
		// A self-hosted control plane issues its own CA, which no system
		// trust store has ever seen.
		opts = append(opts, api.WithCACert([]byte(cfg.ControlPlaneCA)))
	}
	return api.New(cfg.ControlPlaneURL, opts...)
}

// controlPlaneConfigured reports whether this machine has a control plane to
// talk to.
//
// A day-2 command has no flag for this because the cluster already exists and
// its own record says what it is. The only thing left to decide is where to
// read that record from, and that is answered by whether a control plane was
// ever logged in to.
func controlPlaneConfigured() (bool, error) {
	cfg, err := config.Load()
	if err != nil {
		return false, err
	}
	return cfg.ControlPlaneURL != "", nil
}

// installSources resolves the two things an install needs before it starts:
// the control plane, if there is one, and the bundle manifest.
//
// THE MANIFEST IS THE POINT. Every version pin and every deadline in the
// installer comes out of it (a missing timeout is an error, never a default),
// so where it comes from decides whether an install can happen at all. A
// first cluster (--control-plane) reads the copy built into this binary — see
// pkg/bundles for why the pins travel with the binary that installs them —
// and gets NO client: the control plane it would talk to is the one this
// install is still putting on the cluster, so the control-plane stage builds
// that client once it exists. A cluster added to a fleet fetches the manifest
// from its control plane, because the record written in stage 12 has to be
// checkable against the same document the control plane validates against.
func installSources(ctx context.Context, f InstallFlags) (*api.Client, *manifest.Manifest, error) {
	if f.ControlPlane {
		bundle, err := bundles.Manifest(f.Bundle)
		if err != nil {
			return nil, nil, err
		}
		return nil, bundle, nil
	}

	client, err := controlPlaneClientChecked(ctx, "platform install")
	if err != nil {
		return nil, nil, err
	}
	raw, err := client.BundleManifest(ctx, f.Bundle)
	if err != nil {
		return nil, nil, err
	}
	bundle, err := manifest.Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("bundle %s from the control plane is not a valid manifest: %w", f.Bundle, err)
	}
	return client, bundle, nil
}

// runInstall is `kubenest platform install`.
func runInstall(ctx context.Context, out io.Writer, f InstallFlags) error {
	client, bundle, err := installSources(ctx, f)
	if err != nil {
		return err
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
		// The fleet's identity: normally from this machine's config (written
		// by the install that created the control plane), overridable by flag
		// for a machine that never ran one.
		FleetRecipient: f.FleetRecipient,
		InstanceID:     f.InstanceID,
	}

	// The two install shapes differ in where the cluster's record lives and in
	// how this machine reaches it. A first cluster carries its own control
	// plane and has its domain settled before a byte is written. A cluster
	// added to a fleet records into the control plane this machine is logged
	// in to, and verifies that fleet's self-issued CA, which no system trust
	// store has.
	domain := ""
	if f.ControlPlane {
		domain = f.controlPlaneDomain()
		opts.ControlPlaneInstall = true
		opts.Domain = domain
		opts.AdminEmail = f.controlPlaneAdminEmail(domain)
	} else {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		opts.ControlPlaneCA = []byte(cfg.ControlPlaneCA)
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
	// What earlier runs remembered, chiefly who created kubenest-vg. A resume
	// skips the storage stage that records it, so without this preflight
	// refuses the install's own volume group and the record stage reports no
	// ownership for it.
	record, err := install.Recorded(journal)
	if err != nil {
		return err
	}

	session := &install.Session{
		ID:       install.NewRunID(),
		Opts:     opts,
		Bundle:   bundle,
		Jnl:      journal,
		Record:   record,
		Reporter: converge.NewTextReporter(out),
		Out:      out,
		API:      client,
	}
	defer session.Close()

	// Printed locally AND, when there is one, published to the control plane
	// from the same transition: the operator at the terminal and the console
	// watching the install see the same stages. A first cluster has only the
	// terminal, because the control plane it would publish to is the one this
	// install is still putting on the cluster — the control-plane stage
	// builds that client, and a nil-client emitter would be a publisher that
	// silently drops everything.
	emitters := install.Emitters{install.TextEmitter{W: out}}
	if client != nil {
		emitters = append(emitters, install.NewControlPlaneEmitter(client, func() string { return journal.ClusterID }))
	}
	session.Emit = emitters

	fmt.Fprintf(out, "Installing platform bundle %s on %d node(s), %s tier.\n",
		f.Bundle, len(f.Servers)+len(f.Agents), f.HATier)
	if f.ControlPlane {
		fmt.Fprintf(out, "This is the first cluster: it gets the KubeNest control plane, serving %s.\n", domain)
	}
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

	if f.ControlPlane {
		fmt.Fprintf(out, "\nConsole:      https://app.%s\n", domain)
		fmt.Fprintf(out, "Logged in to https://api.%s\n", domain)
		warnIfControlPlaneUnreachable(ctx, out, domain)
	}
	return nil
}

// warnIfControlPlaneUnreachable makes ONE request to the control plane's
// health endpoint and warns rather than failing.
//
// The install has already succeeded on the cluster; this machine not being
// able to open api.<domain> is a fact about where the operator is sitting and
// what DNS and the firewall allow, not about whether the platform came up.
// The same reachability is what every later cluster needs, which is why the
// warning points at hub.<domain> as well.
func warnIfControlPlaneUnreachable(ctx context.Context, out io.Writer, domain string) {
	apiURL := "https://api." + domain
	var caPEM []byte
	if cfg, err := config.Load(); err == nil {
		// The control-plane stage saved the CA it generated; without a CA
		// the request falls back to the system roots, which is the honest
		// check for a domain that has a public certificate.
		caPEM = []byte(cfg.ControlPlaneCA)
	}
	if err := checkControlPlaneHealth(ctx, apiURL, caPEM); err != nil {
		fmt.Fprintf(out, "\nWarning: this machine cannot reach %s on 443 (%v).\n"+
			"The cluster is up and was not affected by this. The agents of every cluster you add\n"+
			"later must reach hub.%s on 443 as well, so check DNS and the firewall before adding one.\n",
			apiURL, err, domain)
	}
}

// checkControlPlaneHealth makes one short-timeout GET of the control plane's
// health endpoint, trusting the platform CA the same way api.WithCACert does.
//
// It is a plain HTTP client rather than pkg/api's: every call pkg/api exposes
// is an authenticated control-plane operation, and /api/v1/health is
// deliberately open. The only thing this shares with them is the trust
// decision, so the CA is folded into a pool seeded from the system roots —
// exactly as the api client's transport does it.
func checkControlPlaneHealth(ctx context.Context, apiURL string, caPEM []byte) error {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if len(caPEM) > 0 && !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("the control plane CA stored in the config is not valid PEM")
	}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(apiURL, "/")+"/api/v1/health", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("the health endpoint answered %s", resp.Status)
	}
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
