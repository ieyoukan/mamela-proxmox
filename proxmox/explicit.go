package proxmox

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/sdk"
)

// Explicit operations (ADR 0006 of Mamela): never planned. A person asks for one by name (mamela
// act) and confirms it; Mamela carries it out only on a VM whose ownership it recorded (or, for
// adopt-vm, on a VM of the old built-in provider).
const (
	OpDelete protocol.Op = "delete-vm" // stop it, then delete it and its disks
	OpReboot protocol.Op = "reboot-vm" // ask the guest to restart (ACPI)
	OpReset  protocol.Op = "reset-vm"  // restart it at once, as a reset button (a guest that does not respond)
	OpAdopt  protocol.Op = "adopt-vm"  // take over a VM Mamela's built-in provider made: write this provider's marker and tag
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

// Prepare implements sdk.Preparer: the change of an explicit operation, from what was observed.
func (Provider) Prepare(_ context.Context, in protocol.PrepareParams) (protocol.PrepareResult, error) {
	o := in.Observed
	if !o.Exists {
		return protocol.PrepareResult{}, fmt.Errorf("%s: there is no VM to %s", o.ID, in.Op)
	}
	var st State
	if len(o.State) > 0 {
		if err := json.Unmarshal(o.State, &st); err != nil {
			return protocol.PrepareResult{}, fmt.Errorf("%s: %w", o.ID, err)
		}
	}
	vmid := o.IDs["vmid"]
	c := protocol.Change{Resource: o.ID, Kind: Kind, Op: in.Op, IDs: map[string]string{"node": o.IDs["node"], "vmid": vmid}}
	req := func(call protocol.Call, params map[string][]string) protocol.Approved {
		return protocol.Approved{Method: call.Method, Path: call.Path, Params: params}
	}
	switch in.Op {
	case OpDelete:
		if st.Running {
			c.Requests = append(c.Requests, req(stopVM, nil))
			c.Reasons = append(c.Reasons, fmt.Sprintf("stop VM %s (running)", vmid))
		}
		c.Requests = append(c.Requests, req(deleteVM, map[string][]string{"destroy-unreferenced-disks": {"1"}}))
		c.Reasons = append(c.Reasons, fmt.Sprintf("delete VM %s (%s) and its disks: their data is lost", vmid, o.Name))
	case OpReboot, OpReset:
		if !st.Running {
			return protocol.PrepareResult{}, fmt.Errorf("%s: VM %s is stopped; nothing to restart (plan and apply start it)", o.ID, vmid)
		}
		call, why := rebootVM, "ask the guest of VM %s to restart"
		if in.Op == OpReset {
			call, why = resetVM, "reset VM %s at once (as a reset button: the guest's unsaved data is lost)"
		}
		c.Requests = []protocol.Approved{req(call, nil)}
		c.Reasons = []string{fmt.Sprintf(why, vmid)}
	case OpAdopt:
		if !o.Adoptable || in.Desired == nil {
			return protocol.PrepareResult{}, fmt.Errorf("%s: only a declared VM made by Mamela's built-in provider can be adopted", o.ID)
		}
		s, err := spec(in.Desired.Spec)
		if err != nil {
			return protocol.PrepareResult{}, fmt.Errorf("%s: %w", o.ID, err)
		}
		m := marker{Name: o.Name, Image: st.Image, Network: st.Network}
		c.Precondition = o.Version
		c.Requests = []protocol.Approved{req(setConfig, map[string][]string{"description": {m.String()}, "tags": {s.Tag}, "digest": {o.Version}})}
		c.Reasons = []string{fmt.Sprintf("take over VM %s (%s), made by Mamela's built-in provider: write this provider's marker and tag it %s", vmid, o.Name, s.Tag)}
	default:
		return protocol.PrepareResult{}, fmt.Errorf("%s is not an explicit operation of this provider", in.Op)
	}
	return protocol.PrepareResult{Change: c}, nil
}

// nameOf is the declared name in a resource ID ("ProxmoxVM web").
func nameOf(id string) string { return strings.TrimPrefix(id, string(Kind)+" ") }

// ensureOwn refuses to act on a VMID that is not, or no longer, the VM the change was prepared for:
// the VM there must carry this provider's marker with the change's name (or, to adopt it, the old
// marker) and, when the change says so, the configuration's version.
func ensureOwn(ctx context.Context, h *sdk.Host, c protocol.Change, node string, vmid int64) error {
	cfg, err := config(ctx, h, node, vmid)
	if err != nil {
		return err
	}
	m, ok := parseMarker(cfg.Description)
	if c.Op == OpAdopt {
		m, ok = parseOldMarker(cfg.Description)
	}
	switch {
	case !ok || m.Name != nameOf(c.Resource):
		return fmt.Errorf("VM %d is not %s's any more (its marker differs); nothing was done, observe and prepare again", vmid, nameOf(c.Resource))
	case c.Precondition != "" && cfg.Digest != c.Precondition:
		return fmt.Errorf("VM %d's configuration changed since it was prepared; nothing was done, prepare again", vmid)
	}
	return nil
}

// lookupExplicit tells whether an explicit operation is in effect.
func lookupExplicit(ctx context.Context, h *sdk.Host, c protocol.Change, node string, vmid int64, vms map[int64]resource) (protocol.LookupStatus, error) {
	vm, exists := vms[vmid]
	var cfg vmConfig
	if exists {
		var err error
		if cfg, err = config(ctx, h, node, vmid); err != nil {
			return "", err
		}
	}
	m, mine := parseMarker(cfg.Description)
	mine = exists && mine && m.Name == nameOf(c.Resource)
	switch c.Op {
	case OpDelete:
		switch {
		case !mine:
			return protocol.LookupDone, nil // gone (or the VMID was taken again by another VM)
		case vm.Status != "running" && len(c.Requests) > 1:
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
	case OpAdopt:
		s := ""
		for _, r := range c.Requests {
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
	return op == OpDelete || op == OpReboot || op == OpReset || op == OpAdopt
}
