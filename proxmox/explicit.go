package proxmox

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// Explicit operations (ADR 0006 of Mamela): never planned. A person asks for one by name (mamela
// act) and confirms it; Mamela carries it out only on a VM whose ownership it recorded (or, for
// adopt-vm, on a VM it observes as adoptable: one of the old built-in provider, or one imported
// from Terraform, inside the boundary).
const (
	OpDelete protocol.Op = "delete-vm" // stop it, then delete it and its disks
	OpReboot protocol.Op = "reboot-vm" // ask the guest to restart (ACPI)
	OpReset  protocol.Op = "reset-vm"  // restart it at once, as a reset button (a guest that does not respond)
	OpStop   protocol.Op = "stop-vm"   // stop it at once, as pulling its power (a guest that does not shut down)
	OpAdopt  protocol.Op = "adopt-vm"  // take over a VM Mamela's built-in provider made, or one imported from Terraform: write this provider's marker and tag
)

var (
	resetVM  = protocol.Call{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/reset"}
	stopVM   = protocol.Call{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/stop"}
	deleteVM = protocol.Call{Method: "DELETE", Path: "/nodes/{node}/qemu/{vmid}", Query: []string{"destroy-unreferenced-disks"}}
)

// OldMarker is the first line of the description of the VMs Mamela's built-in Proxmox provider
// made (before ADR 0006). Its lines: mamela-name, mamela-image, mamela-network, mamela-state.
const OldMarker = "Managed by Mamela. Do not edit the lines below."

// parseOldMarker reads the built-in provider's marker; only a VM it finished (state ready) is
// adoptable.
func parseOldMarker(description string) (marker, bool) {
	lines := strings.Split(strings.TrimSpace(description), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != OldMarker {
		return marker{}, false
	}
	var m marker
	state := ""
	for _, l := range lines[1:] {
		k, v, _ := strings.Cut(l, ":")
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "mamela-name":
			m.Name = v
		case "mamela-image":
			m.Image = v
		case "mamela-network":
			m.Network = v
		case "mamela-state":
			state = v
		}
	}
	if m.Network == "" {
		m.Network = "dhcp" // the built-in provider's VMs from before networks were declared
	}
	return m, m.Name != "" && m.Image != "" && state == "ready"
}

// prepareTemplate is the change of a template's explicit operation (delete-template).
func prepareTemplate(op protocol.Op, o protocol.Observed) (protocol.Change, error) {
	var ts TemplateState
	if err := decodeState(o, &ts); err != nil {
		return protocol.Change{}, err
	}
	vmid := o.IDs["vmid"]
	switch {
	case op != OpDeleteTemplate:
		return protocol.Change{}, fmt.Errorf("%s is not an explicit operation of a template (it has: %s)", op, OpDeleteTemplate)
	case len(ts.Clones) > 0:
		return protocol.Change{}, fmt.Errorf("%s: VMs %v are linked clones of template %s: delete them first (delete-vm)", o.ID, ts.Clones, vmid)
	}
	return protocol.Change{Resource: o.ID, Kind: o.Kind, Op: op, IDs: map[string]string{"node": o.IDs["node"], "vmid": vmid},
		Requests: []protocol.Approved{req(deleteVM, map[string][]string{"destroy-unreferenced-disks": {"1"}})},
		Reasons:  []string{fmt.Sprintf("delete template %s (%s) and its disk: no VM is cloned from it", vmid, o.Name)}}, nil
}

// prepareVM is the change of a VM's explicit operation, from what was observed.
func prepareVM(op protocol.Op, o protocol.Observed, desired *protocol.Resource) (protocol.Change, error) {
	var st State
	if err := decodeState(o, &st); err != nil {
		return protocol.Change{}, err
	}
	vmid := o.IDs["vmid"]
	c := protocol.Change{Resource: o.ID, Kind: o.Kind, Op: op, IDs: map[string]string{"node": o.IDs["node"], "vmid": vmid}}
	switch op {
	case OpDelete:
		if st.Running {
			c.Requests = append(c.Requests, req(stopVM, nil))
			c.Reasons = append(c.Reasons, fmt.Sprintf("stop VM %s (running)", vmid))
		}
		c.Requests = append(c.Requests, req(deleteVM, map[string][]string{"destroy-unreferenced-disks": {"1"}}))
		c.Reasons = append(c.Reasons, fmt.Sprintf("delete VM %s (%s) and its disks: their data is lost", vmid, o.Name))
	case OpReboot, OpReset:
		if !st.Running {
			return protocol.Change{}, fmt.Errorf("%s: VM %s is stopped; nothing to restart (plan and apply start it)", o.ID, vmid)
		}
		call, why := rebootVM, "ask the guest of VM %s to restart"
		if op == OpReset {
			call, why = resetVM, "reset VM %s at once (as a reset button: the guest's unsaved data is lost)"
		}
		c.Requests = []protocol.Approved{req(call, nil)}
		c.Reasons = []string{fmt.Sprintf(why, vmid)}
	case OpStop:
		if !st.Running {
			return protocol.Change{}, fmt.Errorf("%s: VM %s is stopped already", o.ID, vmid)
		}
		c.Requests = []protocol.Approved{req(stopVM, nil)}
		c.Reasons = []string{fmt.Sprintf("stop VM %s at once (as pulling its power: the guest's unsaved data is lost); "+
			"a VM declared running is started again by the next plan (declare it stopped to keep it so)", vmid)}
	case OpAdopt:
		return prepareAdopt(c, o, st, desired)
	default:
		return protocol.Change{}, fmt.Errorf("%s is not an explicit operation of this provider", op)
	}
	return c, nil
}

// prepareAdopt is the change that takes over a VM: write this provider's marker (its name, image
// and network) and the tag, based on the configuration observed (its digest). Nothing else of the
// VM changes. A VM of the built-in provider keeps the image and network its marker records; one
// imported from Terraform takes its declaration's, and must be inside the declared boundary and
// what Mamela can manage (one socket, its disk scsi0 on the disk storage, its network on the
// bridge).
func prepareAdopt(c protocol.Change, o protocol.Observed, st State, desired *protocol.Resource) (protocol.Change, error) {
	if !o.Adoptable || desired == nil || (st.Origin != originBuiltin && st.Origin != originUnmarked) {
		return protocol.Change{}, fmt.Errorf("%s: only a declared VM made by Mamela's built-in provider, or imported from Terraform "+
			"(mamela import terraform), can be adopted", o.ID)
	}
	s, err := spec(desired.Spec)
	if err != nil {
		return protocol.Change{}, fmt.Errorf("%s: %w", o.ID, err)
	}
	vmid := o.IDs["vmid"]
	m := marker{Name: o.Name, Image: st.Image, Network: st.Network}
	why := fmt.Sprintf("take over VM %s (%s), made by Mamela's built-in provider: write this provider's marker and tag it %s", vmid, o.Name, s.Tag)
	if st.Origin == originUnmarked {
		var refuse []string
		if moved := movedFrom(s.Node, s.Pool, s.VMIDMin, s.VMIDMax, o.IDs, st.Pool); moved != "" {
			refuse = append(refuse, moved+" (Mamela takes over only what is inside its boundary)")
		}
		if st.Sockets > 1 {
			refuse = append(refuse, fmt.Sprintf("it has %d sockets: Mamela declares vCPUs as cores of one socket (set sockets to 1 first)", st.Sockets))
		}
		if st.DiskStorage != s.DiskStorage {
			refuse = append(refuse, fmt.Sprintf("its disk scsi0 is on storage %q, not %s (Mamela manages one disk, scsi0, on its disk storage)", st.DiskStorage, s.DiskStorage))
		}
		if st.Bridge != s.Bridge {
			refuse = append(refuse, fmt.Sprintf("its network net0 is on bridge %q, not %s", st.Bridge, s.Bridge))
		}
		if s.Template != "" {
			refuse = append(refuse, "its declaration names a template, and it is not a clone of Mamela's")
		}
		if len(refuse) > 0 {
			return protocol.Change{}, fmt.Errorf("%s: VM %s cannot be adopted: %s; nothing was done", o.ID, vmid, strings.Join(refuse, "; "))
		}
		m = marker{Name: o.Name, Image: s.Image, Network: NetworkString(s.Network)}
		why = fmt.Sprintf("take over VM %s (%s), imported from Terraform: write this provider's marker into its description and tag it %s "+
			"(its description and its tags %v are replaced); nothing else of it changes. Then remove it from Terraform's state (terraform state rm)",
			vmid, o.Name, s.Tag, st.Tags)
	}
	c.Precondition = o.Version
	c.Requests = []protocol.Approved{req(setMarker, map[string][]string{"description": {m.String()}, "tags": {s.Tag}, "digest": {o.Version}})}
	c.Reasons = []string{why}
	return c, nil
}

// nameOf is the declared name in a resource ID ("ProxmoxVM web", "ProxmoxTemplate ubuntu-24.04-pve1").
func nameOf(id string) string {
	_, name, _ := strings.Cut(id, " ")
	return name
}

// markerOf reads the marker of the kind a change is for.
func markerOf(k protocol.ResourceKind, description string) (marker, bool) {
	if k == KindTemplate {
		return parseTemplateMarker(description)
	}
	return parseMarker(description)
}

// ensureOwn refuses to act on a VMID that is not, or no longer, the VM the change was prepared for:
// the VM there must carry this provider's marker with the change's name (or, to adopt it, the old
// marker with that name, or no marker at all) and, when the change says so, the configuration's
// version.
func ensureOwn(c *kit.Client, ch protocol.Change, node string, vmid int64) error {
	cfg, err := config(c, node, vmid)
	if err != nil {
		return err
	}
	m, ok := markerOf(ch.Kind, cfg.Description)
	if ch.Op == OpAdopt {
		switch m, ok = parseOldMarker(cfg.Description); {
		case ok:
		case !marked(cfg.Description) && ch.Precondition != "":
			// Imported (no marker): the version it was prepared on says it is the same VM.
			m, ok = marker{Name: nameOf(ch.Resource)}, true
		}
	}
	switch {
	case !ok || m.Name != nameOf(ch.Resource):
		return fmt.Errorf("VM %d is not %s's any more (its marker differs); nothing was done, observe and prepare again", vmid, nameOf(ch.Resource))
	case ch.Precondition != "" && cfg.Digest != ch.Precondition:
		return fmt.Errorf("VM %d's configuration changed since it was prepared; nothing was done, prepare again", vmid)
	}
	return nil
}

// lookupExplicit tells whether an explicit operation is in effect.
func lookupExplicit(c *kit.Client, ch protocol.Change, node string, vmid int64, vms map[int64]resource) (protocol.LookupStatus, error) {
	vm, exists := vms[vmid]
	var cfg vmConfig
	if exists {
		var err error
		if cfg, err = config(c, node, vmid); err != nil {
			return "", err
		}
	}
	m, mine := markerOf(ch.Kind, cfg.Description)
	mine = exists && mine && m.Name == nameOf(ch.Resource)
	switch ch.Op {
	case OpDelete, OpDeleteTemplate:
		switch {
		case !mine:
			return protocol.LookupDone, nil // gone (or the VMID was taken again by another VM)
		case vm.Status != "running" && len(ch.Requests) > 1:
			return protocol.LookupPartly, nil // stopped, not deleted
		}
		return protocol.LookupNotDone, nil
	case OpReboot, OpReset:
		// A restart leaves no trace this change can be tied to (a running VM may have restarted or
		// not): unknown, never done. Mamela keeps the outcome unknown, and the person looks.
		if !mine {
			return protocol.LookupNotDone, nil // the VM is not there to restart
		}
		return protocol.LookupUnknown, nil
	case OpStop:
		if mine && vm.Status != "running" {
			return protocol.LookupDone, nil
		}
		return protocol.LookupNotDone, nil
	case OpAdopt:
		s := ""
		for _, r := range ch.Requests {
			if t := r.Params["tags"]; len(t) == 1 {
				s = t[0]
			}
		}
		if mine && (s == "" || slices.Contains(cfg.Tags, s)) {
			return protocol.LookupDone, nil
		}
		return protocol.LookupNotDone, nil
	}
	return "", errors.New("not an explicit operation")
}

func explicit(op protocol.Op) bool {
	return op == OpDelete || op == OpReboot || op == OpReset || op == OpStop || op == OpAdopt || op == OpDeleteTemplate
}
