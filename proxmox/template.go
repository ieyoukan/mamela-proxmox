package proxmox

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/sdk"
)

// A template is a VM made once from a pinned image and converted to a Proxmox template: a VM of
// the same image is then a linked clone of it (a snapshot of its disk on the thin pool), made in
// seconds, instead of importing the image again (minutes on a slow disk).
//
// The Pome declares the template of each VM's image and node by itself (Proxmox.vm), the same way
// for every VM, which Mamela takes as one resource; a VM waits until its template is finished.
// Like a VM, a template is in the pool, in the VMID range (from the top of it), tagged and marked;
// it is never started, never changed, and deleted only by the explicit delete-template, which is
// refused while a VM is a linked clone of it.

// KindTemplate is the template's resource kind.
const KindTemplate protocol.ResourceKind = "ProxmoxTemplate"

// Operations of templates, and of VMs cloned from them.
const (
	OpCreateTemplate protocol.Op = "create-template" // import the image into a new VM, tag it, convert it
	OpFinishTemplate protocol.Op = "finish-template" // tag and convert it, after a create that was interrupted
	OpDeleteTemplate protocol.Op = "delete-template" // explicit: delete it and its disk, when no VM is cloned from it

	OpFinishClone protocol.Op = "finish-clone" // a VM: attach its seed and size it, after a clone that was interrupted
)

// TemplateMarker is the first line of a template's description.
const TemplateMarker = "Managed by Mamela (proxmox provider): a template. Do not edit the lines below."

func templateMarker(name, image string) string {
	return fmt.Sprintf("%s\nmamela-name: %s\nmamela-image: %s\n", TemplateMarker, name, image)
}

func parseTemplateMarker(description string) (marker, bool) {
	return parseMarkerAfter(TemplateMarker, description)
}

var (
	createTemplateVM = protocol.Call{Method: "POST", Path: "/nodes/{node}/qemu", Form: []string{"vmid", "name", "pool", "description", "ostype",
		"scsihw", "scsi0", "boot", "serial0", "vga", "net0", "agent"}}
	convertTemplate = protocol.Call{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/template"}
	cloneVM         = protocol.Call{Method: "POST", Path: "/nodes/{node}/qemu/{template}/clone", Form: []string{"newid", "name", "pool", "full", "description"}}
	setupClone      = protocol.Call{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/config", Form: []string{"cores", "memory", "ide2", "digest"}}
	// configureNew is what a create sets of the VM it made: a clone's size and seed, and the tag.
	configureNew = protocol.Call{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/config", Form: []string{"cores", "memory", "ide2", "tags", "digest"}}
)

// TemplateSpec is a declared template: where it goes (the site's) and its image.
type TemplateSpec struct {
	Node         string `json:"node"`
	Pool         string `json:"pool"`
	DiskStorage  string `json:"diskStorage"`
	ImageStorage string `json:"imageStorage"`
	Bridge       string `json:"bridge"`
	Tag          string `json:"tag"`
	VMIDMin      int64  `json:"vmidMin"`
	VMIDMax      int64  `json:"vmidMax"`
	Image        string `json:"image"`
}

// TemplateState is what is observed of a template.
type TemplateState struct {
	Image    string  `json:"image,omitempty"`
	Template bool    `json:"template,omitempty"` // converted
	Tagged   bool    `json:"tagged,omitempty"`
	Clones   []int64 `json:"clones,omitempty"` // the VMs that are linked clones of it
	// For one that does not exist yet: the free VMIDs from the top of its range, and whether its
	// image is already on the image storage.
	FreeVMIDs     []int64 `json:"freeVmids,omitempty"`
	ImageUploaded bool    `json:"imageUploaded,omitempty"`
}

func templateSpec(r protocol.Resource) (TemplateSpec, error) {
	var s TemplateSpec
	if err := json.Unmarshal(r.Spec, &s, json.RejectUnknownMembers(true)); err != nil {
		return s, err
	}
	switch {
	case s.Node == "" || s.Pool == "" || s.DiskStorage == "" || s.ImageStorage == "" || s.Bridge == "" || s.Tag == "":
		return s, errors.New("node, pool, diskStorage, imageStorage, bridge and tag are required")
	case s.VMIDMin < 100 || s.VMIDMax < s.VMIDMin || s.VMIDMax > 999999999:
		return s, fmt.Errorf("VMID range %d..%d is not a Proxmox VMID range", s.VMIDMin, s.VMIDMax)
	}
	if _, ok := Images[s.Image]; !ok {
		return s, fmt.Errorf("image %q is not one of this provider's (%s)", s.Image, strings.Join(slices.Sorted(maps.Keys(Images)), ", "))
	}
	return s, nil
}

// baseOf is the template a disk is a linked clone of ("local-lvm:base-7099-disk-0/vm-7001-disk-0").
func baseOf(drive string) int64 {
	vol, _, _ := strings.Cut(drive, ",")
	_, rest, ok := strings.Cut(vol, ":base-")
	if !ok || !strings.Contains(rest, "/") {
		return 0
	}
	id, _, _ := strings.Cut(rest, "-")
	n, _ := strconv.ParseInt(id, 10, 64)
	return n
}

// configs reads the configuration of each VM of a pool on a node, once per observation.
type configs struct {
	ctx  context.Context
	h    *sdk.Host
	seen map[int64]vmConfig
}

func (c *configs) of(node string, vmid int64) (vmConfig, error) {
	if cfg, ok := c.seen[vmid]; ok {
		return cfg, nil
	}
	cfg, err := config(c.ctx, c.h, node, vmid)
	if err == nil {
		c.seen[vmid] = cfg
	}
	return cfg, err
}

// observeTemplate finds a declared template, or tells what creating it needs.
func observeTemplate(cs *configs, r protocol.Resource, vms map[int64]resource, mine map[int64]bool) (protocol.Observed, error) {
	s, err := templateSpec(r)
	if err != nil {
		return protocol.Observed{}, err
	}
	o := protocol.Observed{ID: r.ID, Kind: KindTemplate, Name: r.Name}
	for _, vmid := range slices.Sorted(maps.Keys(vms)) {
		vm := vms[vmid]
		if vm.Pool != s.Pool || vm.Node != s.Node || vmid < s.VMIDMin || vmid > s.VMIDMax {
			continue
		}
		cfg, err := cs.of(vm.Node, vmid)
		if err != nil {
			return o, err
		}
		if m, ok := parseTemplateMarker(cfg.Description); ok && m.Name == r.Name {
			mine[vmid] = true
			return o, fillTemplate(cs, &o, vm, cfg, m, s.Tag, vms)
		}
	}
	st := TemplateState{}
	for id := s.VMIDMax; id >= s.VMIDMin && len(st.FreeVMIDs) < 64; id-- {
		if _, taken := vms[id]; !taken {
			st.FreeVMIDs = append(st.FreeVMIDs, id)
		}
	}
	img := Images[s.Image]
	var err2 error
	if st.ImageUploaded, err2 = hasVolume(cs.ctx, cs.h, s.Node, s.ImageStorage, "import", VolumeName(s.Image, img)); err2 != nil {
		return o, err2
	}
	o.State, _ = json.Marshal(st, json.Deterministic(true))
	return o, nil
}

// fillTemplate fills in an observed template: owned when tagged and converted (unfinished when
// not), with the VMs cloned from it.
func fillTemplate(cs *configs, o *protocol.Observed, vm resource, cfg vmConfig, m marker, tag string, vms map[int64]resource) error {
	tagged := tag == "" || slices.Contains(cfg.Tags, tag)
	o.Exists, o.Version = true, cfg.Digest
	o.Owned, o.Unfinished = tagged && cfg.Template, !(tagged && cfg.Template)
	o.IDs = ids(vm.Node, vm.VMID)
	st := TemplateState{Image: m.Image, Template: cfg.Template, Tagged: tag != "" && slices.Contains(cfg.Tags, tag)}
	for _, id := range slices.Sorted(maps.Keys(vms)) {
		other := vms[id]
		if id == vm.VMID || other.Node != vm.Node {
			continue
		}
		c, err := cs.of(other.Node, id)
		if err != nil {
			return err
		}
		if baseOf(c.SCSI0) == vm.VMID {
			st.Clones = append(st.Clones, id)
		}
	}
	o.State, _ = json.Marshal(st, json.Deterministic(true))
	return nil
}

// diffTemplate is what makes a template that does not exist, or finishes one.
func diffTemplate(r protocol.Resource, o protocol.Observed, uploading map[string]bool) ([]protocol.Change, error) {
	s, err := templateSpec(r)
	if err != nil {
		return nil, err
	}
	var st TemplateState
	if len(o.State) > 0 {
		if err := json.Unmarshal(o.State, &st); err != nil {
			return nil, err
		}
	}
	img := Images[s.Image]
	vol := VolumeName(s.Image, img)
	switch {
	case !o.Exists:
		if len(st.FreeVMIDs) == 0 {
			return nil, fmt.Errorf("no free VMID in %d..%d", s.VMIDMin, s.VMIDMax)
		}
		vmid := st.FreeVMIDs[0]
		key := s.Node + "/" + s.ImageStorage + "/" + s.Image
		if uploading[key] {
			st.ImageUploaded = true
		}
		uploading[key] = true
		c := protocol.Change{Resource: r.ID, Kind: KindTemplate, Op: OpCreateTemplate,
			IDs: map[string]string{"node": s.Node, "vmid": strconv.FormatInt(vmid, 10), "storage": s.ImageStorage},
			Reasons: []string{fmt.Sprintf("template %d from %s on %s, in pool %s: the VMs of this image are linked clones of it (made in seconds)",
				vmid, s.Image, s.DiskStorage, s.Pool)}}
		if !st.ImageUploaded {
			c.Artifacts = []protocol.Artifact{{ID: "image", Kind: protocol.ArtifactDownload, URL: img.URL, SHA256: img.SHA256, Name: vol}}
			c.Requests = append(c.Requests, protocol.Approved{Method: upload.Method, Path: upload.Path, Upload: "image",
				Params: map[string][]string{"content": {"import"}, "checksum-algorithm": {"sha256"}, "checksum": {"{artifact:image.sha256}"}}})
		}
		c.Requests = append(c.Requests,
			protocol.Approved{Method: createTemplateVM.Method, Path: createTemplateVM.Path, Params: map[string][]string{
				"vmid": {strconv.FormatInt(vmid, 10)}, "name": {r.Name}, "pool": {s.Pool}, "description": {templateMarker(r.Name, s.Image)},
				"ostype": {"l26"}, "scsihw": {"virtio-scsi-single"}, "scsi0": {s.DiskStorage + ":0,import-from=" + s.ImageStorage + ":import/" + vol + ",discard=on"},
				"boot": {"order=scsi0"}, "serial0": {"socket"}, "vga": {"serial0"}, "net0": {"virtio,bridge=" + s.Bridge}, "agent": {"enabled=1"}}},
			protocol.Approved{Method: setConfig.Method, Path: setConfig.Path, Params: map[string][]string{"tags": {s.Tag}}},
			protocol.Approved{Method: convertTemplate.Method, Path: convertTemplate.Path},
		)
		return []protocol.Change{c}, nil
	case !o.Owned && !o.Unfinished:
		return nil, nil // not Mamela's: nothing is planned for it
	}
	var out []protocol.Change
	if !st.Tagged || !st.Template {
		c := protocol.Change{Resource: r.ID, Kind: KindTemplate, Op: OpFinishTemplate, IDs: o.IDs, Precondition: o.Version}
		if !st.Tagged {
			c.Reasons = append(c.Reasons, "tag it "+s.Tag)
			c.Requests = append(c.Requests, protocol.Approved{Method: setConfig.Method, Path: setConfig.Path,
				Params: map[string][]string{"tags": {s.Tag}, "digest": {o.Version}}})
		}
		if !st.Template {
			c.Reasons = append(c.Reasons, "convert it to a template (a create was interrupted)")
			c.Requests = append(c.Requests, protocol.Approved{Method: convertTemplate.Method, Path: convertTemplate.Path})
		}
		out = append(out, c)
	}
	if st.Image != s.Image {
		out = append(out, protocol.Change{Resource: r.ID, Kind: KindTemplate, Op: OpReplaceImage, IDs: o.IDs,
			Reasons: []string{fmt.Sprintf("image %s -> %s: declare a template of the new image (the VMs name it)", st.Image, s.Image)}})
	}
	return out, nil
}

// cloneChange makes a VM as a linked clone of its template: the seed, the clone (with its marker),
// its size and seed, its disk, its tag.
func cloneChange(r protocol.Resource, s Spec, vmid, template int64) protocol.Change {
	id, tid := strconv.FormatInt(vmid, 10), strconv.FormatInt(template, 10)
	return protocol.Change{Resource: r.ID, Kind: Kind, Op: OpCreate,
		IDs: map[string]string{"node": s.Node, "vmid": id, "storage": s.ImageStorage, "template": tid, "newid": id},
		Reasons: []string{fmt.Sprintf("VM %d: a linked clone of template %d (%s), %d vCPU, %d MiB, %d GiB, network %s, in pool %s", vmid, template,
			s.Image, s.CPU, s.MemoryMiB, s.DiskGiB, NetworkString(s.Network), s.Pool)},
		Artifacts: []protocol.Artifact{{ID: "seed", Kind: protocol.ArtifactSeed, Node: r.Name, NodeID: vmid, Network: &s.Network}},
		Requests: []protocol.Approved{
			{Method: upload.Method, Path: upload.Path, Upload: "seed",
				Params: map[string][]string{"content": {"iso"}, "checksum-algorithm": {"sha256"}, "checksum": {"{artifact:seed.sha256}"}}},
			{Method: cloneVM.Method, Path: cloneVM.Path, Params: map[string][]string{"newid": {id}, "name": {r.Name}, "pool": {s.Pool},
				"full": {"0"}, "description": {marker{Name: r.Name, Image: s.Image, Network: NetworkString(s.Network)}.String()}}},
			{Method: setupClone.Method, Path: setupClone.Path, Params: map[string][]string{"cores": {strconv.FormatInt(s.CPU, 10)},
				"memory": {strconv.FormatInt(s.MemoryMiB, 10)}, "ide2": {s.ImageStorage + ":iso/{artifact:seed.name},media=cdrom"}}},
			{Method: resizeDisk.Method, Path: resizeDisk.Path, Params: map[string][]string{"disk": {"scsi0"}, "size": {fmt.Sprintf("%dG", s.DiskGiB)}}},
			{Method: setConfig.Method, Path: setConfig.Path, Params: map[string][]string{"tags": {s.Tag}}},
		}}
}

// finishClone attaches the seed and sets the size of a VM whose clone was interrupted before.
func finishClone(r protocol.Resource, s Spec, o protocol.Observed) protocol.Change {
	vmid, _ := strconv.ParseInt(o.IDs["vmid"], 10, 64)
	return protocol.Change{Resource: r.ID, Kind: Kind, Op: OpFinishClone, IDs: o.IDs, Precondition: o.Version,
		Reasons:   []string{"attach its seed and set its size (a clone was interrupted)"},
		Artifacts: []protocol.Artifact{{ID: "seed", Kind: protocol.ArtifactSeed, Node: r.Name, NodeID: vmid, Network: &s.Network}},
		Requests: []protocol.Approved{
			{Method: upload.Method, Path: upload.Path, Upload: "seed",
				Params: map[string][]string{"content": {"iso"}, "checksum-algorithm": {"sha256"}, "checksum": {"{artifact:seed.sha256}"}}},
			{Method: setupClone.Method, Path: setupClone.Path, Params: map[string][]string{"cores": {strconv.FormatInt(s.CPU, 10)},
				"memory": {strconv.FormatInt(s.MemoryMiB, 10)}, "ide2": {s.ImageStorage + ":iso/{artifact:seed.name},media=cdrom"},
				"digest": {o.Version}}},
		}}
}
