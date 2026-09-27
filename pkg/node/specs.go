package node

import (
	"strings"

	"kubenest.io/cli/pkg/component/day2"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/storage"
)

// nodeSpecs decides which of the commands a node operation submits are
// ACTIONS — written down before submission, with a postcondition a successor
// can check — and what a successor can establish about each.
//
// READS ARE NOT ACTIONS. The postcondition of a read is the read; recording
// every observation would fill the record with steps a resume could not safely
// skip, which is the opposite of what the record is for (PLAN 7.2). What IS
// recorded is everything that changes a machine or the cluster: the join, the
// hold, the volume group, the cordon, the drain, the uninstall, the node's own
// deletion.
//
// The stage in the spec is the stage name in the journal, so a successor reads
// one vocabulary.
func nodeSpecs(stage, command string) (operation.Spec, bool) {
	switch stage {
	case "join":
		if !strings.Contains(command, "get.k3s.io") {
			return operation.Spec{}, false
		}
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: "k3s-agent is installed and active on the new host",
			// `is-active` and not `kubectl`: the question the join's successor
			// asks is whether the agent is running on THAT host, which is
			// answerable without the cluster's API.
			Observe: `test "$(sudo -n systemctl is-active k3s-agent)" = active`,
		}, true
	case "hold":
		if !strings.Contains(command, "label node ") {
			return operation.Spec{}, false
		}
		name := labelNodeName(command)
		if name == "" {
			return operation.Spec{}, false
		}
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: name + " is held out of kured's pool (" + day2.NoAutoRebootLabel + "=" + day2.NoAutoRebootValue + ")",
			Observe:       `test "$(sudo -n k3s kubectl get node ` + name + ` -o jsonpath='{.metadata.labels.kubenest\.io/auto-reboot}')" = ` + day2.NoAutoRebootValue,
		}, true
	case "lift-hold":
		if !strings.Contains(command, "label node ") {
			return operation.Spec{}, false
		}
		name := labelNodeName(command)
		if name == "" {
			return operation.Spec{}, false
		}
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: "the reboot hold is lifted from " + name,
			Observe:       `test "$(sudo -n k3s kubectl get node ` + name + ` -o jsonpath='{.metadata.labels.kubenest\.io/auto-reboot}')" != ` + day2.NoAutoRebootValue,
		}, true
	case "storage":
		if !strings.Contains(command, "pvcreate") && !strings.Contains(command, "vgcreate") {
			return operation.Spec{}, false
		}
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: "the volume group " + storage.VolumeGroup + " exists on the new host",
			Observe:       `sudo -n vgs ` + storage.VolumeGroup + ` --noheadings -o vg_name | grep -q ` + storage.VolumeGroup,
		}, true
	case "cordon":
		if !strings.Contains(command, "kubectl cordon ") {
			return operation.Spec{}, false
		}
		name := strings.TrimSpace(strings.TrimPrefix(command, "sudo -n k3s kubectl cordon "))
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: name + " is unschedulable",
			Observe:       `test "$(sudo -n k3s kubectl get node ` + name + ` -o jsonpath='{.spec.unschedulable}')" = true`,
		}, true
	case "drain":
		if !strings.Contains(command, "kubectl drain ") {
			return operation.Spec{}, false
		}
		name := secondWord(command, "drain")
		if name == "" {
			return operation.Spec{}, false
		}
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: "no pod that is not a DaemonSet remains on " + name,
			Observe: `test -z "$(sudo -n k3s kubectl get pods -A --field-selector spec.nodeName=` + name +
				` -o custom-columns=O:.metadata.ownerReferences[0].kind --no-headers 2>/dev/null | grep -v '^DaemonSet$')"`,
		}, true
	case "uninstall":
		if !strings.Contains(command, "k3s-agent-uninstall.sh") && !strings.Contains(command, "k3s-uninstall.sh") {
			return operation.Spec{}, false
		}
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: "k3s is uninstalled from the host (its datastore directory is gone)",
			Observe:       `test ! -e /var/lib/rancher/k3s`,
		}, true
	case "delete-node":
		if !strings.Contains(command, "kubectl delete node ") {
			return operation.Spec{}, false
		}
		name := strings.TrimSpace(strings.TrimPrefix(command, "sudo -n k3s kubectl delete node "))
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: "the Node object " + name + " is deleted",
			Observe:       `! sudo -n k3s kubectl get node ` + name + ` >/dev/null 2>&1`,
		}, true
	}
	return operation.Spec{}, false
}

// labelNodeName pulls the node a `kubectl label node <name> ...` command names
// out of the command string, so a postcondition names the node the command
// named rather than a name the caller remembered.
func labelNodeName(command string) string {
	return secondWord(command, "node")
}

// secondWord returns the argument that follows a word, e.g. the node name
// after `cordon`.
func secondWord(command, verb string) string {
	fields := strings.Fields(command)
	for i, f := range fields {
		if f == verb && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}
