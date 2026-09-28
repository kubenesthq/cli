package storage

import (
	"fmt"
	"sort"
	"strings"
)

// Devices is --storage-device resolved for one install: which block device
// kubenest-vg is created on, named once for every node or once per node.
//
// THE SINGLE FORM CANNOT DESCRIBE A REAL MULTI-NODE INSTALL. The device an
// operator must name is the stable /dev/disk/by-id/... path, and that path
// embeds the volume's serial, so it is different on every host that has its
// own volume — Hetzner names a volume scsi-0HC_Volume_<id>, AWS names one
// nvme-Amazon_Elastic_Block_Store_vol<id>. A per-host mapping is what lets a
// multi-node install follow the documented advice instead of creating
// kubenest-vg by hand on every node and omitting the flag.
//
// The zero value is Option 1: the operator created kubenest-vg on the nodes
// themselves, so the installer touches no block device at all.
type Devices struct {
	// All is the single form: one device, used on every node. ParseDevices
	// never combines it with PerHost, and PerHost wins if something else did.
	All string `json:"all,omitempty"`
	// PerHost is the per-host form: one device per node, keyed by the node's
	// address exactly as the operator wrote it with --server or --agent.
	PerHost map[string]string `json:"per_host,omitempty"`
}

// For returns the device kubenest-vg is created on for one node. Empty means
// the volume group already exists there and the installer must not touch a
// block device.
func (d Devices) For(host string) string {
	if d.PerHost != nil {
		return d.PerHost[host]
	}
	return d.All
}

// Empty reports whether this mapping names no device at all: Option 1 on every
// node.
func (d Devices) Empty() bool { return d.All == "" && len(d.PerHost) == 0 }

// IsZero lets the journal omit a record's storage mapping entirely when the
// operator created the volume group themselves, rather than writing an empty
// object into every such journal.
func (d Devices) IsZero() bool { return d.Empty() }

// Unnamed returns the nodes of hosts this mapping names no device for, in the
// order given. It is empty for the single form, which names every node by
// construction, and for the empty mapping (Option 1), which names none.
func (d Devices) Unnamed(hosts []string) []string {
	if d.PerHost == nil {
		return nil
	}
	var missing []string
	for _, host := range hosts {
		if _, named := d.PerHost[host]; !named {
			missing = append(missing, host)
		}
	}
	return missing
}

// RefuseUnnamed is the refusal a per-host mapping earns when it leaves one of
// the install's nodes out. It names the node and both ways to name it, and it
// is used by the flag parser AND by the installer immediately before it dials,
// so an operator reads the same sentence whichever path refused.
func (d Devices) RefuseUnnamed(hosts []string) error {
	for _, host := range d.Unnamed(hosts) {
		return fmt.Errorf("--storage-device names no device for %s: pass --storage-device %s=<blank device> as well, or leave the HOST= part off every value to use one device on every node. A per-host set has to name every node of the install (%s)",
			host, host, strings.Join(hosts, ", "))
	}
	return nil
}

// Identity renders the mapping as one line for the journal's resume identity.
// Nodes are sorted, so the order the flags were written in is never mistaken
// for a different install.
func (d Devices) Identity() string {
	if d.PerHost == nil {
		return d.All
	}
	hosts := make([]string, 0, len(d.PerHost))
	for host := range d.PerHost {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	parts := make([]string, 0, len(hosts))
	for _, host := range hosts {
		parts = append(parts, host+"="+d.PerHost[host])
	}
	return strings.Join(parts, " ")
}

// ParseDevices turns the values of --storage-device into the mapping one
// install uses. hosts is the install's node list in the order the operator
// gave it: the --server values, then the --agent values.
//
// EVERY REFUSAL HERE HAPPENS BEFORE A HOST IS TOUCHED, and each names the node
// concerned and the way out. A mapping that silently left a node unnamed would
// install on the other nodes and only then stop, and a value named twice would
// make which one wins a matter of flag order — so neither is accepted.
func ParseDevices(values []string, hosts []string) (Devices, error) {
	if len(values) == 0 {
		return Devices{}, nil
	}
	var single string
	var perHost []string
	for _, value := range values {
		host, _, isPerHost := strings.Cut(value, "=")
		if !isPerHost {
			if single != "" {
				return Devices{}, fmt.Errorf("--storage-device %q and %q both name one device for every node: pass that one device once, or name a device per node as HOST=DEV", single, value)
			}
			single = value
			continue
		}
		if host == "" {
			return Devices{}, fmt.Errorf("--storage-device %q names no host: write HOST=DEV, where HOST is a --server or --agent value exactly as you gave it", value)
		}
		perHost = append(perHost, value)
	}
	if single != "" && len(perHost) > 0 {
		return Devices{}, fmt.Errorf("--storage-device mixes the two forms: %q names one device for every node, and %q names one node's device. Pass one device with no HOST=, or one HOST=DEV value for each of %s",
			single, perHost[0], strings.Join(hosts, ", "))
	}

	d := Devices{All: single}
	if len(perHost) == 0 {
		return d, nil
	}

	d.PerHost = make(map[string]string, len(perHost))
	for _, value := range perHost {
		host, device, _ := strings.Cut(value, "=")
		if device == "" {
			return Devices{}, fmt.Errorf("--storage-device %q names no device for %s: write HOST=DEV with the blank device to create %s on, or leave the HOST= part off every value to use one device on every node",
				value, host, VolumeGroup)
		}
		if _, named := d.PerHost[host]; named {
			return Devices{}, fmt.Errorf("--storage-device names %s twice: each node of the install takes exactly one device", host)
		}
		d.PerHost[host] = device
	}

	known := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		known[host] = true
	}
	for _, value := range perHost {
		host, _, _ := strings.Cut(value, "=")
		if !known[host] {
			return Devices{}, fmt.Errorf("--storage-device %q names a host that is not one of this install's nodes: HOST has to be a --server or --agent value exactly as given (%s)",
				value, nodeList(hosts))
		}
	}
	if err := d.RefuseUnnamed(hosts); err != nil {
		return Devices{}, err
	}
	return d, nil
}

// nodeList renders a node list for a message. An install always has at least
// one --server by the time a device is resolved, so the empty case only
// reaches a caller that validated nothing.
func nodeList(hosts []string) string {
	if len(hosts) == 0 {
		return "this install has no nodes"
	}
	return strings.Join(hosts, ", ")
}
