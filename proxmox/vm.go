package proxmox

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// vmKind is the kind ProxmoxVM: a VM that mamelet runs on.
type vmKind struct{ run *run }

func (*vmKind) Kind() protocol.ResourceKind { return Kind }
func (*vmKind) IsNode() bool                { return true }
func (*vmKind) Spec() any                   { return Spec{} }
func (*vmKind) Reads() []protocol.Call      { return reads }

func (*vmKind) Doc() string {
	return "A virtual machine on Proxmox VE that Mamela's agent (mamelet) runs on: a linked clone of the template of its image\n" +
		"(template), or made from its image directly. Declare packages and services on it as a node."
}

func (*vmKind) Operations() []protocol.Operation {
	create := needs(atPool("VM.Allocate", "VM.Audit", "VM.Config.CPU", "VM.Config.Memory", "VM.Config.Disk", "VM.Config.HWType",
		"VM.Config.Network", "VM.Config.Options"),
		atStorage("diskStorage", "Datastore.AllocateSpace"), atStorage("imageStorage", "Datastore.Audit", "Datastore.AllocateTemplate"),
		atBridge("SDN.Use"),
		// A VM cloned from a template: VM.Clone on the template, VM.Config.CDROM for its seed.
		atPool("VM.Clone", "VM.Config.CDROM"))
	finishClone := needs(atPool("VM.Config.CPU", "VM.Config.Memory", "VM.Config.CDROM"),
		atStorage("imageStorage", "Datastore.Audit", "Datastore.AllocateTemplate"))
	return []protocol.Operation{
		kit.Op(OpCreate, create, upload, createVM, cloneVM, configureNew, resizeDisk),
		kit.Op(OpFinishClone, finishClone, upload, setupClone),
		kit.Op(OpStart, atPool("VM.PowerMgmt"), startVM),
		kit.Op(OpGrow, needs(atPool("VM.Config.Disk"), atStorage("diskStorage", "Datastore.AllocateSpace")), resizeDisk),
		kit.Op(OpResize, atPool("VM.Config.CPU", "VM.Config.Memory", "VM.PowerMgmt"), setSize, rebootVM),
		kit.Op(OpTag, atPool("VM.Config.Options"), setTags),
		kit.Op(OpShutdown, atPool("VM.PowerMgmt"), shutdownVM),
		kit.Unsupported(OpShrink, "shrinking a disk can destroy data"),
		kit.Unsupported(OpReplaceImage, "changing the image would mean replacing the VM and its data"),
		kit.Unsupported(OpChangeNetwork, "a VM gets its network once, when it is created"),
		kit.Unsupported(OpMove, "Mamela never moves a VM to another node, pool or VMID: migrate it by hand (qm migrate) and declare where it is, "+
			"or declare it where it is"),
		kit.Unsupported(OpCreateNoImage, "a VM without an image was imported to adopt, and it is not there: Mamela creates a VM only from "+
			"one of its pinned images; declare one, or remove the declaration"),
		kit.ExplicitOp(OpDelete, atPool("VM.PowerMgmt", "VM.Allocate"), stopVM, deleteVM),
		kit.ExplicitOp(OpReboot, atPool("VM.PowerMgmt"), rebootVM),
		kit.ExplicitOp(OpReset, atPool("VM.PowerMgmt"), resetVM),
		kit.ExplicitOp(OpStop, atPool("VM.PowerMgmt"), stopVM),
		kit.AdoptOp(OpAdopt, atPool("VM.Config.Options"), setMarker),
	}
}

// List is every VM in a pool that carries this provider's marker (Mamela's), then those that carry
// the built-in provider's (to adopt, never listed as Mamela's).
func (k *vmKind) List(c *kit.Client) ([]kit.Object, error) {
	rd := k.run.reader(c)
	vms, err := rd.list()
	if err != nil {
		return nil, err
	}
	var mine, old []kit.Object
	for _, vmid := range slices.Sorted(maps.Keys(vms)) {
		vm := vms[vmid]
		if vm.Pool == "" {
			continue
		}
		cfg, err := rd.config(vm.Node, vmid)
		if err != nil {
			return nil, err
		}
		if m, ok := parseMarker(cfg.Description); ok {
			mine = append(mine, vmObject(vm, cfg, m, ""))
		} else if m, ok := parseOldMarker(cfg.Description); ok {
			x := vmObject(vm, cfg, m, originBuiltin)
			x.Adoptable = true // a VM of Mamela's built-in provider: adopt-vm, never created again
			old = append(old, x)
		}
	}
	return append(mine, old...), nil
}

func vmObject(vm resource, cfg vmConfig, m marker, origin string) kit.Object {
	running := vm.Status == "running"
	st := State{CPU: cfg.Cores, MemoryMiB: cfg.Memory, DiskGiB: cfg.DiskGiB, Image: m.Image, Network: m.Network,
		Running: running, Seeded: cfg.IDE2 != "", Pool: vm.Pool, Tags: cfg.Tags, Origin: origin}
	if running {
		st.Uptime = vm.Uptime
	}
	return kit.Object{Name: m.Name, IDs: ids(vm.Node, vm.VMID), Version: cfg.Digest, State: st}
}

// claim makes what kit observed of a VM what Mamela is told: a node with its state, owned with the
// declared tag (unfinished without it) and its seeds' storage among its IDs, or adoptable only
// inside the declared pool, node and VMID range.
func (k *vmKind) claim(o *protocol.Observed, r *protocol.Resource) error {
	if !o.Exists {
		return nil
	}
	var st State
	if err := decodeState(*o, &st); err != nil {
		return err
	}
	vmid, _ := strconv.ParseInt(o.IDs["vmid"], 10, 64)
	o.Node = &protocol.ObservedNode{ID: vmid, Running: st.Running}
	if st.Running {
		o.Node.UptimeSeconds = st.Uptime
	}
	st.Uptime = 0
	if r != nil {
		s, err := spec(r.Spec)
		if err != nil {
			return err
		}
		switch {
		case o.Adoptable:
			o.Node = nil
			switch {
			case st.Origin != originBuiltin && st.Origin != originUnmarked:
				o.Adoptable = false // Mamela's own VM (another declaration's): never taken over by this one
			case o.IDs["node"] != s.Node || st.Pool != s.Pool || !s.inRange(vmid):
				o.Adoptable = false // outside the declared boundary: never Mamela's to take over
			}
		default:
			st.Tagged = slices.Contains(st.Tags, s.Tag)
			o.Owned, o.Unfinished = Ownership(st.Tagged)
			o.IDs["storage"] = s.ImageStorage // its seeds' storage
		}
	}
	o.State = encode(st)
	return nil
}

// Missing is what creating a declared VM needs: the free VMIDs of its range, and whether its image
// is on the image storage already.
func (k *vmKind) Missing(c *kit.Client, r protocol.Resource) (any, error) {
	s, err := spec(r.Spec)
	if err != nil {
		return nil, err
	}
	vms, err := k.run.reader(c).list()
	if err != nil {
		return nil, err
	}
	st := State{FreeVMIDs: freeVMIDs(vms, c.Claimed, s.Node, s.VMIDMin, s.VMIDMax, false)}
	if s.Image != "" {
		if st.ImageUploaded, err = hasVolume(c, s.Node, s.ImageStorage, "import", VolumeName(s.Image, Images[s.Image])); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// Diff implements kit.Kind.
func (k *vmKind) Diff(d kit.Desired) ([]protocol.Change, error) {
	r, o := d.Resource, d.Observed
	s, err := spec(r.Spec)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r.ID, err)
	}
	var st State
	if err := decodeState(o, &st); err != nil {
		return nil, err
	}
	switch {
	case !o.Exists:
		return k.create(d, s, st)
	case !o.Owned:
		return nil, nil
	}
	return vmChanges(r, s, o, st), nil
}

// create is what makes a VM that does not exist: a linked clone of its template, or a VM from its
// image; then its start (unless it is declared stopped).
func (k *vmKind) create(d kit.Desired, s Spec, st State) ([]protocol.Change, error) {
	r := d.Resource
	if s.Image == "" {
		return []protocol.Change{{Resource: r.ID, Kind: Kind, Op: OpCreateNoImage,
			Reasons: []string{"no VM is there to adopt, and the declaration names no image to create one from"}}}, nil
	}
	vmid := int64(0)
	for _, id := range st.FreeVMIDs {
		if k.run.take(id) {
			vmid = id
			break
		}
	}
	if vmid == 0 {
		return nil, fmt.Errorf("%s: no free VMID in %d..%d", r.ID, s.VMIDMin, s.VMIDMax)
	}
	var made protocol.Change
	if s.Template != "" {
		t, ok := d.Dependencies[string(KindTemplate)+" "+s.Template]
		if !ok || !t.Exists {
			return nil, fmt.Errorf("%s: its template %s is not among its dependencies (the Pome declares it; plan with a Proxmox Pome that does)", r.ID, s.Template)
		}
		var ts TemplateState
		json.Unmarshal(t.State, &ts)
		if ts.Image != s.Image || t.IDs["node"] != s.Node {
			return nil, fmt.Errorf("%s: template %s is of %s on %s, the VM is of %s on %s", r.ID, s.Template, ts.Image, t.IDs["node"], s.Image, s.Node)
		}
		tid, _ := strconv.ParseInt(t.IDs["vmid"], 10, 64)
		made = cloneChange(r, s, vmid, tid)
	} else {
		if k.run.upload(s.Node, s.ImageStorage, s.Image) {
			st.ImageUploaded = true
		}
		made = createFromImage(r, s, st, vmid)
	}
	out := []protocol.Change{made}
	if !s.Stopped {
		out = append(out, startOf(r.ID, ids(s.Node, vmid)))
	}
	return out, nil
}

func startOf(resource string, id map[string]string) protocol.Change {
	return protocol.Change{Resource: resource, Kind: Kind, Op: OpStart, IDs: id,
		Requests: []protocol.Approved{req(startVM, nil)}, Reasons: []string{startReason}, Late: true}
}

// vmChanges are the changes of a VM that is Mamela's, one change of its configuration at a time
// (each needs the version it was planned on), and its power as declared.
func vmChanges(r protocol.Resource, s Spec, o protocol.Observed, st State) []protocol.Change {
	id := o.IDs
	var out []protocol.Change
	// What it never changes is still a difference: reported as a change of an unsupported
	// operation (nothing to send), which Mamela shows and denies.
	unsupported := func(op protocol.Op, why string) {
		out = append(out, protocol.Change{Resource: r.ID, Kind: Kind, Op: op, IDs: id, Reasons: []string{why}})
	}
	if moved := movedFrom(s.Node, s.Pool, s.VMIDMin, s.VMIDMax, id, st.Pool); moved != "" {
		unsupported(OpMove, moved)
		return out
	}
	switch {
	case !st.Seeded && s.Template != "":
		// A clone interrupted before its seed and size: one change, on its configuration.
		out = append(out, finishClone(r, s, o))
	case st.DiskGiB < s.DiskGiB:
		out = append(out, protocol.Change{Resource: r.ID, Kind: Kind, Op: OpGrow, IDs: id, Precondition: o.Version,
			Reasons:  []string{fmt.Sprintf("disk %d -> %d GiB", st.DiskGiB, s.DiskGiB)},
			Requests: []protocol.Approved{req(resizeDisk, map[string][]string{"disk": {"scsi0"}, "size": {fmt.Sprintf("%dG", s.DiskGiB)}, "digest": {o.Version}})}})
	case st.CPU != s.CPU || st.MemoryMiB != s.MemoryMiB:
		params := map[string][]string{"digest": {o.Version}}
		var why []string
		if st.CPU != s.CPU {
			params["cores"] = []string{strconv.FormatInt(s.CPU, 10)}
			why = append(why, fmt.Sprintf("vCPUs %d -> %d", st.CPU, s.CPU))
		}
		if st.MemoryMiB != s.MemoryMiB {
			params["memory"] = []string{strconv.FormatInt(s.MemoryMiB, 10)}
			why = append(why, fmt.Sprintf("memory %d -> %d MiB", st.MemoryMiB, s.MemoryMiB))
		}
		reqs := []protocol.Approved{{Method: setConfig.Method, Path: setConfig.Path, Params: params}}
		if st.Running && !s.Stopped {
			reqs = append(reqs, req(rebootVM, nil))
			why = append(why, "restart it to apply the new size")
		}
		out = append(out, protocol.Change{Resource: r.ID, Kind: Kind, Op: OpResize, IDs: id, Precondition: o.Version, Reasons: why, Requests: reqs})
	case !st.Tagged:
		out = append(out, protocol.Change{Resource: r.ID, Kind: Kind, Op: OpTag, IDs: id, Precondition: o.Version,
			Reasons:  []string{"tag it " + s.Tag + " (a create was interrupted before tagging)"},
			Requests: []protocol.Approved{{Method: setConfig.Method, Path: setConfig.Path, Params: map[string][]string{"tags": {s.Tag}, "digest": {o.Version}}}}})
	}
	switch {
	case !st.Running && !s.Stopped:
		out = append(out, startOf(r.ID, id))
	case st.Running && s.Stopped:
		out = append(out, protocol.Change{Resource: r.ID, Kind: Kind, Op: OpShutdown, IDs: id,
			Requests: []protocol.Approved{req(shutdownVM, nil)}, Late: true,
			Reasons: []string{"shut it down, as declared (stopped): its guest is asked to power off; one that does not is stopped by hand (stop-vm)"}})
	}
	if st.DiskGiB > s.DiskGiB {
		unsupported(OpShrink, fmt.Sprintf("disk %d -> %d GiB", st.DiskGiB, s.DiskGiB))
	}
	if st.Image != s.Image {
		unsupported(OpReplaceImage, fmt.Sprintf("image %s -> %s", orNone(st.Image), orNone(s.Image)))
	}
	if want := NetworkString(s.Network); st.Network != want {
		unsupported(OpChangeNetwork, fmt.Sprintf("network %s -> %s", st.Network, want))
	}
	return out
}

// movedFrom says how an observed VM or template is not where its declaration puts it (another node,
// pool, or a VMID outside the range): "" when it is.
func movedFrom(node, pool string, vmidMin, vmidMax int64, id map[string]string, observedPool string) string {
	var out []string
	if id["node"] != node {
		out = append(out, fmt.Sprintf("it is on node %s, declared on %s", id["node"], node))
	}
	if observedPool != pool {
		out = append(out, fmt.Sprintf("it is in pool %q, declared in %s", observedPool, pool))
	}
	if vmid, err := strconv.ParseInt(id["vmid"], 10, 64); err != nil || vmid < vmidMin || vmid > vmidMax {
		out = append(out, fmt.Sprintf("its VMID %s is outside the declared %d..%d", id["vmid"], vmidMin, vmidMax))
	}
	return strings.Join(out, "; ")
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// createFromImage is the change that makes a VM from its image: upload the image (unless it is there)
// and the VM's seed, create the VM from them (stopped, with the marker), grow its disk, tag it.
func createFromImage(r protocol.Resource, s Spec, st State, vmid int64) protocol.Change {
	img := Images[s.Image]
	vol := VolumeName(s.Image, img)
	c := protocol.Change{Resource: r.ID, Kind: Kind, Op: OpCreate,
		IDs: map[string]string{"node": s.Node, "vmid": strconv.FormatInt(vmid, 10), "storage": s.ImageStorage},
		Reasons: []string{fmt.Sprintf("VM %d: %d vCPU, %d MiB, %d GiB from %s, network %s, in pool %s", vmid, s.CPU, s.MemoryMiB, s.DiskGiB,
			s.Image, NetworkString(s.Network), s.Pool)},
		Artifacts: []protocol.Artifact{{ID: "seed", Kind: protocol.ArtifactSeed, Node: r.Name, NodeID: vmid, Network: &s.Network}}}
	if !st.ImageUploaded {
		c.Artifacts = append([]protocol.Artifact{{ID: "image", Kind: protocol.ArtifactDownload, URL: img.URL, SHA256: img.SHA256, Name: vol}}, c.Artifacts...)
		c.Requests = append(c.Requests, protocol.Approved{Method: upload.Method, Path: upload.Path, Upload: "image",
			Params: map[string][]string{"content": {"import"}, "checksum-algorithm": {"sha256"}, "checksum": {"{artifact:image.sha256}"}}})
	}
	c.Requests = append(c.Requests,
		protocol.Approved{Method: upload.Method, Path: upload.Path, Upload: "seed",
			Params: map[string][]string{"content": {"iso"}, "checksum-algorithm": {"sha256"}, "checksum": {"{artifact:seed.sha256}"}}},
		protocol.Approved{Method: createVM.Method, Path: createVM.Path, Params: map[string][]string{
			"vmid": {strconv.FormatInt(vmid, 10)}, "name": {r.Name}, "pool": {s.Pool},
			"description": {marker{Name: r.Name, Image: s.Image, Network: NetworkString(s.Network)}.String()},
			"ostype":      {"l26"}, "cores": {strconv.FormatInt(s.CPU, 10)}, "memory": {strconv.FormatInt(s.MemoryMiB, 10)},
			"scsihw": {"virtio-scsi-single"}, "scsi0": {s.DiskStorage + ":0,import-from=" + s.ImageStorage + ":import/" + vol + ",discard=on"},
			"boot": {"order=scsi0"}, "serial0": {"socket"}, "vga": {"serial0"}, "net0": {"virtio,bridge=" + s.Bridge}, "onboot": {"0"},
			"ide2": {s.ImageStorage + ":iso/{artifact:seed.name},media=cdrom"}, "agent": {"enabled=1"}}},
		protocol.Approved{Method: resizeDisk.Method, Path: resizeDisk.Path, Params: map[string][]string{"disk": {"scsi0"}, "size": {fmt.Sprintf("%dG", s.DiskGiB)}}},
		protocol.Approved{Method: setConfig.Method, Path: setConfig.Path, Params: map[string][]string{"tags": {s.Tag}}},
	)
	return c
}

func (*vmKind) Done(c *kit.Client, ch protocol.Change, a protocol.Approved) (bool, error) {
	return done(c, ch, a)
}

func (*vmKind) Lookup(c *kit.Client, ch protocol.Change) (protocol.LookupStatus, bool, error) {
	status, err := lookup(c, ch)
	return status, true, err
}

func (*vmKind) Prepare(op protocol.Op, o protocol.Observed, desired *protocol.Resource) (protocol.Change, error) {
	return prepareVM(op, o, desired)
}

// freeVMIDs are up to 64 VMIDs of a range that no guest has and that no other plan is to create on
// the node (Mamela's claims): from the bottom of the range (VMs) or from the top (templates).
func freeVMIDs(vms map[int64]resource, claimed []map[string]string, node string, min, max int64, fromTop bool) []int64 {
	taken := map[int64]bool{}
	for _, ids := range claimed {
		if ids["node"] != node {
			continue
		}
		for _, k := range []string{"vmid", "newid"} {
			if n, err := strconv.ParseInt(ids[k], 10, 64); err == nil {
				taken[n] = true
			}
		}
	}
	var out []int64
	for i := int64(0); i <= max-min && len(out) < 64; i++ {
		id := min + i
		if fromTop {
			id = max - i
		}
		if _, exists := vms[id]; !exists && !taken[id] {
			out = append(out, id)
		}
	}
	return out
}
