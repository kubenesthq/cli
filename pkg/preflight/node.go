package preflight

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/storage"
)

// checkNode runs every per-node check. It runs them ALL even after one has
// failed: an operator who fixes one condition, re-runs, and hits the next has
// been failed by the installer, not by their infrastructure.
func checkNode(ctx context.Context, opts Options, node Node, rep *Report) {
	// SSH reachability is the dial, which the caller already attempted —
	// every other check needs the connection, so a failed dial ends this
	// node's checks rather than producing eight identical failures.
	if node.Runner == nil || node.DialErr != nil {
		detail := "could not connect"
		if node.DialErr != nil {
			detail = node.DialErr.Error()
		}
		rep.add(Result{
			Check: CheckSSH, Node: node.Address, Outcome: Fail,
			Detail: detail,
			Fix:    "check the address, --ssh-user and --ssh-key (or your ssh-agent and ~/.ssh/config); the installer uses your existing SSH setup and key material never leaves this machine",
		})
		return
	}
	rep.add(Result{Check: CheckSSH, Node: node.Address, Outcome: Pass, Detail: "connected"})

	checkOS(ctx, opts, node, rep)
	checkPrivilege(ctx, node, rep)
	checkExistingKubernetes(ctx, node, rep)
	checkResources(ctx, opts, node, rep)
	checkVolumeGroup(ctx, opts, node, rep)
	checkEgress(ctx, opts, node, rep)
}

// checkOS refuses anything outside the bundle's tested matrix. Locking the
// OS is what makes the bundle testable: a known kernel and a deterministic
// LVM layout are preconditions for storage and OS patching.
func checkOS(ctx context.Context, opts Options, node Node, rep *Report) {
	out, err := run(ctx, node.Runner, "cat /etc/os-release")
	if err != nil {
		rep.add(Result{
			Check: CheckOS, Node: node.Address, Outcome: Fail,
			Detail: "could not read /etc/os-release: " + err.Error(),
			Fix:    "the platform installs on Ubuntu LTS only: " + strings.Join(opts.Bundle.OS.Supported, ", "),
		})
		return
	}
	id, versionID, pretty := parseOSRelease(out)
	if opts.Bundle.OS.Supports(id, versionID) {
		rep.add(Result{Check: CheckOS, Node: node.Address, Outcome: Pass, Detail: pretty})
		return
	}
	rep.add(Result{
		Check: CheckOS, Node: node.Address, Outcome: Fail,
		Detail: fmt.Sprintf("this node runs %s", pretty),
		Fix: fmt.Sprintf("bundle %s is tested on %s only, and the installer refuses anything else rather than half-installing it",
			opts.Bundle.Bundle, strings.Join(opts.Bundle.OS.Supported, ", ")),
	})
}

func parseOSRelease(out string) (id, versionID, pretty string) {
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"'`)
		switch key {
		case "ID":
			id = value
		case "VERSION_ID":
			versionID = value
		case "PRETTY_NAME":
			pretty = value
		}
	}
	if pretty == "" {
		pretty = id + " " + versionID
	}
	return id, versionID, pretty
}

// checkPrivilege is `sudo -n true`, exactly as documented, because every
// later stage runs the k3s installer and writes to /var/lib/rancher.
func checkPrivilege(ctx context.Context, node Node, rep *Report) {
	if _, err := run(ctx, node.Runner, "sudo -n true"); err != nil {
		rep.add(Result{
			Check: CheckPrivilege, Node: node.Address, Outcome: Fail,
			Detail: "sudo -n true failed: this user cannot use sudo without a password",
			Fix:    "grant passwordless sudo (the default `ubuntu` user on official Ubuntu cloud images already has it), or install as a user that has it",
		})
		return
	}
	rep.add(Result{Check: CheckPrivilege, Node: node.Address, Outcome: Pass, Detail: "passwordless sudo works"})
}

// existingKubernetes lists the binaries whose presence means this host is not
// a new cluster. Adopting an existing cluster means inheriting whatever
// ingress, CSI and cert manager are already on it — the untested component
// combination the bundle exists to eliminate.
var existingKubernetes = []string{"k3s", "rke2", "kubelet", "containerd"}

func checkExistingKubernetes(ctx context.Context, node Node, rep *Report) {
	script := "for b in " + strings.Join(existingKubernetes, " ") + `; do command -v "$b" >/dev/null 2>&1 && echo "$b"; done; true`
	out, err := run(ctx, node.Runner, script)
	if err != nil {
		rep.add(Result{
			Check: CheckExistingK8s, Node: node.Address, Outcome: Fail,
			Detail: "could not check for an existing cluster: " + err.Error(),
			Fix:    "the installer builds new clusters only and must be able to prove this host is one",
		})
		return
	}
	found := strings.Fields(out)
	if len(found) == 0 {
		rep.add(Result{Check: CheckExistingK8s, Node: node.Address, Outcome: Pass, Detail: "no existing Kubernetes on this host"})
		return
	}
	// A resumed install always re-runs preflight, and by then WE have
	// installed k3s. Without this, the check that forbids adopting someone
	// else's cluster would refuse our own half-finished one — and resume is
	// the documented recovery path.
	if node.ExistingK3sIsOurs {
		rep.add(Result{
			Check: CheckExistingK8s, Node: node.Address, Outcome: Pass,
			Detail: "k3s is present and this install's journal says it put it there — resuming, not adopting",
		})
		return
	}
	rep.add(Result{
		Check: CheckExistingK8s, Node: node.Address, Outcome: Fail,
		Detail: fmt.Sprintf("already present: %s", strings.Join(found, ", ")),
		Fix:    "the installer builds new clusters only — use a host with no Kubernetes on it, or remove the existing one first. Adopting a cluster means inheriting its ingress, CSI and cert manager, which is the untested combination the bundle exists to eliminate",
	})
}

// checkResources compares against limits.resources IN BINARY UNITS, failing
// below the floor and warning below the recommendation.
//
// The warn/fail split is the entire reason a provisional number is safe to
// ship: the floor is k3s's documented minimum plus headroom and is a
// refusal; the recommendation is a guess and is advice.
func checkResources(ctx context.Context, opts Options, node Node, rep *Report) {
	floor, err := opts.Bundle.Limits.Floor()
	if err != nil {
		rep.add(Result{Check: CheckResources, Node: node.Address, Outcome: Fail, Detail: err.Error(),
			Fix: "the bundle manifest must carry limits.resources.floor"})
		return
	}
	recommended, err := opts.Bundle.Limits.Recommended()
	if err != nil {
		rep.add(Result{Check: CheckResources, Node: node.Address, Outcome: Fail, Detail: err.Error(),
			Fix: "the bundle manifest must carry limits.resources.recommended"})
		return
	}

	// One round trip: cores, MemTotal in KiB, and free bytes on the
	// filesystem that will hold /var/lib/rancher.
	const script = `echo "cpu=$(nproc)"; ` +
		`echo "memkb=$(awk '/^MemTotal:/{print $2}' /proc/meminfo)"; ` +
		`echo "diskbytes=$(df -B1 -P /var/lib | awk 'NR==2{print $4}')"`
	out, err := run(ctx, node.Runner, script)
	if err != nil {
		rep.add(Result{Check: CheckResources, Node: node.Address, Outcome: Fail,
			Detail: "could not measure this node: " + err.Error(),
			Fix:    "the node must answer nproc, /proc/meminfo and df"})
		return
	}
	values := parseKeyValues(out)
	cpu := atoi(values["cpu"])
	// MemTotal is KiB — the kernel's own unit, which is why the manifest's
	// thresholds are binary.
	memory := manifest.Quantity(atoi64(values["memkb"]) * 1024)
	disk := manifest.Quantity(atoi64(values["diskbytes"]))
	measured := fmt.Sprintf("%d vCPU, %s RAM, %s free on /var/lib", cpu, memory, disk)

	var short []string
	if cpu < floor.CPU {
		short = append(short, fmt.Sprintf("%d vCPU (floor %d)", cpu, floor.CPU))
	}
	if memory < floor.Memory {
		short = append(short, fmt.Sprintf("%s RAM (floor %s)", memory, floor.Memory))
	}
	if disk < floor.Disk {
		short = append(short, fmt.Sprintf("%s free disk (floor %s)", disk, floor.Disk))
	}
	if len(short) > 0 {
		rep.add(Result{
			Check: CheckResources, Node: node.Address, Outcome: Fail,
			Detail: "below the hard floor: " + strings.Join(short, ", ") + " — measured " + measured,
			Fix: fmt.Sprintf("the floor is %d vCPU / %s / %s, in binary units as the kernel reports them; buy a machine advertised as 4 GB / 40 GB or larger",
				floor.CPU, floor.Memory, floor.Disk),
		})
		return
	}

	var under []string
	if cpu < recommended.CPU {
		under = append(under, fmt.Sprintf("%d vCPU (recommended %d)", cpu, recommended.CPU))
	}
	if memory < recommended.Memory {
		under = append(under, fmt.Sprintf("%s RAM (recommended %s)", memory, recommended.Memory))
	}
	if disk < recommended.Disk {
		under = append(under, fmt.Sprintf("%s free disk (recommended %s)", disk, recommended.Disk))
	}
	if len(under) > 0 {
		rep.add(Result{
			Check: CheckResources, Node: node.Address, Outcome: Warn,
			Detail: measured + ", below the recommendation: " + strings.Join(under, ", "),
			Fix:    "the install will proceed; the recommendation is what to buy if this cluster is going to carry real workloads",
		})
		return
	}
	rep.add(Result{Check: CheckResources, Node: node.Address, Outcome: Pass, Detail: measured})
}

// checkVolumeGroup is the two supported storage paths, and nothing else:
// either the customer created kubenest-vg (the default, and the installer
// never touches their block devices), or --storage-device names a blank
// device. There is no auto-detection, because guessing which disk is
// disposable on someone else's infrastructure is not a risk worth taking.
func checkVolumeGroup(ctx context.Context, opts Options, node Node, rep *Report) {
	// A resumed install re-runs preflight after stage 7 already created the
	// volume group; without this the "device must be blank" rule would
	// refuse the install's own work.
	if node.StorageIsOurs {
		rep.add(Result{
			Check: CheckVolumeGroup, Node: node.Address, Outcome: Pass,
			Detail: storage.VolumeGroup + " exists and this install's journal says it created it — resuming",
		})
		return
	}
	ownership, err := storage.PreflightVolumeGroup(ctx, node.Runner, opts.StorageDevice)
	if err != nil {
		fix := "create it before installing: `sudo vgcreate " + storage.VolumeGroup + " <device>` (lvm2 ships on the stock Ubuntu 24.04 cloud image), or pass --storage-device with a blank device for the installer to create it"
		if opts.StorageDevice != "" {
			fix = "--storage-device must name a device with no partition table, filesystem or existing volume group; use the stable /dev/disk/by-id/... path"
		}
		rep.add(Result{Check: CheckVolumeGroup, Node: node.Address, Outcome: Fail, Detail: err.Error(), Fix: fix})
		return
	}
	detail := storage.VolumeGroup + " exists with free extents (you created it; the installer will not touch your block devices)"
	if ownership == storage.InstallerCreated {
		detail = opts.StorageDevice + " is blank; the installer will create " + storage.VolumeGroup + " on it"
	}
	rep.add(Result{Check: CheckVolumeGroup, Node: node.Address, Outcome: Pass, Detail: detail})
}

// checkEgress proves the node can reach the registries and chart repositories
// the install pulls from. Air-gapped installs are not supported, and finding
// that out at stage 5 rather than stage 1 is what preflight exists to prevent.
//
// ANY HTTP response counts as reachable: a 401 from a registry means egress
// works and authentication is a different subject. Only a failed connection,
// DNS or TLS handshake is a refusal.
func checkEgress(ctx context.Context, opts Options, node Node, rep *Report) {
	if len(opts.Egress) == 0 {
		return
	}
	var urls []string
	byURL := map[string]string{}
	for _, t := range opts.Egress {
		urls = append(urls, t.URL)
		byURL[t.URL] = t.Name
	}
	script := "for u in " + strings.Join(urls, " ") + `; do ` +
		`code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 "$u" 2>/dev/null); ` +
		`echo "$u ${code:-000}"; done; true`
	out, err := run(ctx, node.Runner, script)
	if err != nil {
		rep.add(Result{
			Check: CheckEgress, Node: node.Address, Outcome: Fail,
			Detail: "could not test outbound egress: " + err.Error(),
			Fix:    "curl must be present on the node",
		})
		return
	}

	var unreachable []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		url, code, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if code == "000" {
			unreachable = append(unreachable, fmt.Sprintf("%s (%s)", byURL[url], url))
		}
	}
	if len(unreachable) > 0 {
		rep.add(Result{
			Check: CheckEgress, Node: node.Address, Outcome: Fail,
			Detail: "no HTTPS egress to " + strings.Join(unreachable, ", "),
			Fix:    "both the install and the running cluster require outbound internet access — air-gapped installs are not supported. Open HTTPS egress to the container registries and Helm repositories above",
		})
		return
	}
	rep.add(Result{
		Check: CheckEgress, Node: node.Address, Outcome: Pass,
		Detail: fmt.Sprintf("HTTPS egress to %d registries and chart repositories", len(urls)),
	})
}

// run executes one command and returns stdout, treating a non-zero exit as an
// error carrying stderr.
func run(ctx context.Context, r k3s.Runner, command string) (string, error) {
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

func parseKeyValues(out string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			values[key] = strings.TrimSpace(value)
		}
	}
	return values
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
