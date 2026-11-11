// Package proxmox is a Mamela provider of Proxmox VE virtual machines that mamelet runs on. It
// runs as a separate, isolated program (WASM): it reaches Proxmox only through the Mamela host,
// which holds the token and checks every request against Mamela's Proxmox catalog and the change's
// permit, and which makes the files a VM is created from (ADR 0004 of Mamela):
//
//   - the cloud image, a pinned download (URL and sha256 from Images), uploaded once per storage;
//   - the VM's mamelet seed, made by the host with a one-time enrollment token.
//
// Kind ProxmoxVM is a node kind: Mamela runs mamelet on these VMs, so packages, services and k3s
// can be declared on them. The provider creates VMs (stopped; start-vm starts them), starts them,
// grows their disks and changes their vCPUs and memory. It never shrinks a disk, replaces an image
// or changes a network, and it never touches a VM without its marker. Deleting, restarting and
// taking over a VM are explicit operations (explicit.go): a person asks for each by name.
//
// Its guard (guard.go) checks the cluster before and after every apply: the boundary of pool, VMID
// range and node; the operating token's permissions; the room on the node; and that no guest
// outside the boundary changed.
package proxmox

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/sdk"
)

// Name and Version are the provider's, as its manifest and Pome name them.
const (
	Name    = "proxmox"
	Version = "0.2.0"
	// API is the API it talks to (its catalog: pome/catalogs/proxmox.json, "pome:proxmox").
	API = "proxmox"
)

// Kind is the resource kind.
const Kind protocol.ResourceKind = "ProxmoxVM"

// Operations.
const (
	OpCreate protocol.Op = "create-vm"
	OpStart  protocol.Op = "start-vm"
	OpGrow   protocol.Op = "grow-vm-disk"
	OpResize protocol.Op = "resize-vm"
	OpTag    protocol.Op = "tag-vm"

	// Never carried out (the manifest says why); a difference is shown and denied.
	OpShrink        protocol.Op = "shrink-vm-disk"
	OpReplaceImage  protocol.Op = "replace-vm-image"
	OpChangeNetwork protocol.Op = "change-vm-network"
)

// Image is a pinned cloud image a VM can be created from. Changing an entry is a reviewed change
// of this provider (a new version).
type Image struct {
	URL        string
	SHA256     string
	MinDiskGiB int64 // the image's virtual size, rounded up
}

// Images are the images a declaration may name.
var Images = map[string]Image{
	// Ubuntu 24.04 LTS server cloud image, release 20261017 (qcow2, virtual size 3.5 GiB).
	// From https://cloud-images.ubuntu.com/releases/noble/release-20261017/SHA256SUMS
	"ubuntu-24.04": {
		URL:        "https://cloud-images.ubuntu.com/releases/noble/release-20261017/ubuntu-24.04-server-cloudimg-amd64.img",
		SHA256:     "04e8f2461a16a44e78a370284471f29616cdd9baf1893df25807b71f985b6c94",
		MinDiskGiB: 4,
	},
}

// VolumeName is an image's file name on the image storage: its checksum is in it, so a different
// file is never taken for it, and it ends in .qcow2 as Proxmox requires of importable images.
// It is the name Mamela's built-in provider uses too, so both share an uploaded image.
func VolumeName(name string, img Image) string {
	return fmt.Sprintf("mamela-%s-%s.qcow2", name, img.SHA256[:16])
}

// Spec is a declared VM: where it goes (the site's settings, which a declaration states in full)
// and what it is.
type Spec struct {
	Node         string           `json:"node"`
	Pool         string           `json:"pool"`
	DiskStorage  string           `json:"diskStorage"`
	ImageStorage string           `json:"imageStorage"`
	Bridge       string           `json:"bridge"`
	Tag          string           `json:"tag"`
	VMIDMin      int64            `json:"vmidMin"`
	VMIDMax      int64            `json:"vmidMax"`
	CPU          int64            `json:"cpu"`
	MemoryMiB    int64            `json:"memoryMiB"`
	DiskGiB      int64            `json:"diskGiB"`
	Image        string           `json:"image"`
	Network      protocol.Network `json:"network"`
}

// State is what is observed of a declared VM.
type State struct {
	CPU       int64  `json:"cpu,omitempty"`
	MemoryMiB int64  `json:"memoryMiB,omitempty"`
	DiskGiB   int64  `json:"diskGiB,omitempty"`
	Image     string `json:"image,omitempty"`
	Network   string `json:"network,omitempty"`
	Tagged    bool   `json:"tagged,omitempty"`
	Running   bool   `json:"running,omitempty"`
	// For a VM that does not exist yet: the free VMIDs of its range, and whether its image is
	// already on the image storage.
	FreeVMIDs     []int64 `json:"freeVmids,omitempty"`
	ImageUploaded bool    `json:"imageUploaded,omitempty"`
}

// Calls.
var (
	listVMs     = protocol.Call{Method: "GET", Path: "/cluster/resources", Query: []string{"type"}}
	getConfig   = protocol.Call{Method: "GET", Path: "/nodes/{node}/qemu/{vmid}/config"}
	getStatus   = protocol.Call{Method: "GET", Path: "/nodes/{node}/qemu/{vmid}/status/current"}
	listContent = protocol.Call{Method: "GET", Path: "/nodes/{node}/storage/{storage}/content", Query: []string{"content"}}
	getTask     = protocol.Call{Method: "GET", Path: "/nodes/{node}/tasks/{upid}/status"}
	upload      = protocol.Call{Method: "POST", Path: "/nodes/{node}/storage/{storage}/upload", Form: []string{"content", "checksum-algorithm", "checksum"}, File: "filename"}
	createVM    = protocol.Call{Method: "POST", Path: "/nodes/{node}/qemu", Form: []string{"vmid", "name", "pool", "description", "ostype",
		"cores", "memory", "scsihw", "scsi0", "boot", "serial0", "vga", "net0", "onboot", "ide2", "agent"}}
	resizeDisk = protocol.Call{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/resize", Form: []string{"disk", "size", "digest"}}
	setConfig  = protocol.Call{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/config"}
	// What each operation sends of the configuration: only what it changes.
	setSize   = protocol.Call{Method: "PUT", Path: setConfig.Path, Form: []string{"cores", "memory", "digest"}}
	setTags   = protocol.Call{Method: "PUT", Path: setConfig.Path, Form: []string{"tags", "digest"}}
	setMarker = protocol.Call{Method: "PUT", Path: setConfig.Path, Form: []string{"description", "tags", "digest"}}
	startVM   = protocol.Call{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/start"}
	rebootVM  = protocol.Call{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/reboot"}
)

// Provider implements sdk.Provider.
type Provider struct {
	// Poll is how long to wait between task status reads (default 2s); Timeout bounds a task
	// (default 2 hours: importing an image can take long).
	Poll, Timeout time.Duration
}

// Manifest implements sdk.Provider.
func (Provider) Manifest() protocol.Manifest {
	pool := func(caps ...string) []protocol.Need {
		var out []protocol.Need
		for _, c := range caps {
			out = append(out, protocol.Need{Capability: c, Target: protocol.Target{Type: "proxmox-acl", Ref: "/pool/{pool}"}, Always: true})
		}
		return out
	}
	storage := func(ref string, caps ...string) []protocol.Need {
		var out []protocol.Need
		for _, c := range caps {
			out = append(out, protocol.Need{Capability: c, Target: protocol.Target{Type: "proxmox-acl", Ref: "/storage/{" + ref + "}"}, Always: true})
		}
		return out
	}
	create := append(pool("VM.Allocate", "VM.Audit", "VM.Config.CPU", "VM.Config.Memory", "VM.Config.Disk", "VM.Config.HWType",
		"VM.Config.Network", "VM.Config.Options"),
		append(storage("diskStorage", "Datastore.AllocateSpace"), storage("imageStorage", "Datastore.Audit", "Datastore.AllocateTemplate")...)...)
	create = append(create, protocol.Need{Capability: "SDN.Use", Target: protocol.Target{Type: "proxmox-acl", Ref: "/sdn/zones/localnetwork/{bridge}"}, Always: true})
	reads := []protocol.Call{listVMs, getConfig, getStatus, listContent, getTask}
	with := func(calls ...protocol.Call) []protocol.Call { return append(slices.Clone(reads), calls...) }
	unsupported := func(op protocol.Op, why string) protocol.Operation {
		return protocol.Operation{Op: op, Support: protocol.Unsupported, Why: why}
	}
	return protocol.Manifest{
		Schema: protocol.Schema, Provider: Name, Version: Version, API: API,
		Observe:      append(pool("VM.Audit", "Pool.Audit"), storage("imageStorage", "Datastore.Audit")...),
		ObserveCalls: []protocol.Call{listVMs, getConfig, listContent},
		Guard:        &protocol.Guard{Calls: GuardCalls},
		Scripts:      Scripts,
		Kinds: []protocol.Kind{{Kind: Kind, Node: true, Operations: []protocol.Operation{
			{Op: OpCreate, Support: protocol.Supported, Needs: create, Calls: with(upload, createVM, resizeDisk, setTags)},
			{Op: OpStart, Support: protocol.Supported, Needs: pool("VM.PowerMgmt"), Calls: with(startVM)},
			{Op: OpGrow, Support: protocol.Supported, Needs: append(pool("VM.Config.Disk"), storage("diskStorage", "Datastore.AllocateSpace")...), Calls: with(resizeDisk)},
			{Op: OpResize, Support: protocol.Supported, Needs: pool("VM.Config.CPU", "VM.Config.Memory", "VM.PowerMgmt"), Calls: with(setSize, rebootVM)},
			{Op: OpTag, Support: protocol.Supported, Needs: pool("VM.Config.Options"), Calls: with(setTags)},
			unsupported(OpShrink, "shrinking a disk can destroy data"),
			unsupported(OpReplaceImage, "changing the image would mean replacing the VM and its data"),
			unsupported(OpChangeNetwork, "a VM gets its network once, when it is created"),
			{Op: OpDelete, Support: protocol.Supported, Explicit: true, Needs: pool("VM.PowerMgmt", "VM.Allocate"), Calls: with(stopVM, deleteVM)},
			{Op: OpReboot, Support: protocol.Supported, Explicit: true, Needs: pool("VM.PowerMgmt"), Calls: with(rebootVM)},
			{Op: OpReset, Support: protocol.Supported, Explicit: true, Needs: pool("VM.PowerMgmt"), Calls: with(resetVM)},
			{Op: OpAdopt, Support: protocol.Supported, Explicit: true, Adopts: true, Needs: pool("VM.Config.Options"), Calls: with(setMarker)},
		}}},
	}
}

// Marker is the first line of the description of the VMs this provider created; the lines after
// it record the VM's name, image and network.
const Marker = "Managed by Mamela (proxmox provider). Do not edit the lines below."

type marker struct{ Name, Image, Network string }

func (m marker) String() string {
	return fmt.Sprintf("%s\nmamela-name: %s\nmamela-image: %s\nmamela-network: %s\n", Marker, m.Name, m.Image, m.Network)
}

func parseMarker(description string) (marker, bool) {
	lines := strings.Split(strings.TrimSpace(description), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != Marker {
		return marker{}, false
	}
	var m marker
	for _, l := range lines[1:] {
		k, v, _ := strings.Cut(l, ":")
		switch strings.TrimSpace(k) {
		case "mamela-name":
			m.Name = strings.TrimSpace(v)
		case "mamela-image":
			m.Image = strings.TrimSpace(v)
		case "mamela-network":
			m.Network = strings.TrimSpace(v)
		}
	}
	return m, m.Name != ""
}

// NetworkString is a network as Mamela shows it ("dhcp", "static 192.0.2.10/24 via ... dns ...").
func NetworkString(n protocol.Network) string {
	if n.Mode != "static" {
		return n.Mode
	}
	return fmt.Sprintf("static %s via %s dns %s", n.Address, n.Gateway, strings.Join(n.DNS, ","))
}

type resource struct {
	VMID   int64  `json:"vmid"`
	Name   string `json:"name"`
	Node   string `json:"node"`
	Pool   string `json:"pool"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Uptime int64  `json:"uptime"`
}

// Observe implements sdk.Provider.
func (p Provider) Observe(ctx context.Context, h *sdk.Host, in protocol.ObserveParams) (protocol.ObserveResult, error) {
	vms, err := list(ctx, h)
	if err != nil {
		return protocol.ObserveResult{}, err
	}
	var out protocol.ObserveResult
	mine := map[int64]bool{}
	for _, r := range in.Resources {
		s, err := spec(r.Spec)
		if err != nil {
			return out, fmt.Errorf("%s: %w", r.ID, err)
		}
		o := protocol.Observed{ID: r.ID, Kind: Kind, Name: r.Name}
		found := false
		for _, vmid := range slices.Sorted(maps.Keys(vms)) {
			vm := vms[vmid]
			if vm.Pool != s.Pool || vm.Node != s.Node || vmid < s.VMIDMin || vmid > s.VMIDMax {
				continue
			}
			cfg, err := config(ctx, h, vm.Node, vmid)
			if err != nil {
				return out, err
			}
			if m, ok := parseMarker(cfg.Description); ok && m.Name == r.Name {
				fill(&o, vm, cfg, m, s.Tag)
				mine[vmid], found = true, true
				break
			}
		}
		if !found {
			// A VM of Mamela's built-in provider with this name: adoptable (adopt-vm), never
			// created again.
			for _, vmid := range slices.Sorted(maps.Keys(vms)) {
				vm := vms[vmid]
				if vm.Pool != s.Pool || vm.Node != s.Node || vmid < s.VMIDMin || vmid > s.VMIDMax || mine[vmid] {
					continue
				}
				cfg, err := config(ctx, h, vm.Node, vmid)
				if err != nil {
					return out, err
				}
				if m, ok := parseOldMarker(cfg.Description); ok && m.Name == r.Name {
					fill(&o, vm, cfg, m, s.Tag)
					o.Owned, o.Unfinished, o.Adoptable, o.Node = false, false, true, nil
					mine[vmid], found = true, true
					break
				}
			}
		}
		if !found {
			st := State{}
			for id := s.VMIDMin; id <= s.VMIDMax && len(st.FreeVMIDs) < 64; id++ {
				if _, taken := vms[id]; !taken {
					st.FreeVMIDs = append(st.FreeVMIDs, id)
				}
			}
			img, ok := Images[s.Image]
			if !ok {
				return out, fmt.Errorf("%s: image %q is not one of this provider's (%s)", r.ID, s.Image, strings.Join(slices.Sorted(maps.Keys(Images)), ", "))
			}
			if st.ImageUploaded, err = hasVolume(ctx, h, s.Node, s.ImageStorage, "import", VolumeName(s.Image, img)); err != nil {
				return out, err
			}
			o.State, _ = json.Marshal(st, json.Deterministic(true))
		}
		out.Resources = append(out.Resources, o)
	}
	// What it created and is no longer declared: reported, so that Mamela keeps it.
	for _, vmid := range slices.Sorted(maps.Keys(vms)) {
		vm := vms[vmid]
		if mine[vmid] || vm.Pool == "" {
			continue
		}
		cfg, err := config(ctx, h, vm.Node, vmid)
		if err != nil {
			return out, err
		}
		m, ok := parseMarker(cfg.Description)
		if !ok {
			continue
		}
		o := protocol.Observed{ID: string(Kind) + " " + m.Name, Kind: Kind, Name: m.Name}
		fill(&o, vm, cfg, m, "")
		out.Resources = append(out.Resources, o)
	}
	return out, nil
}

func fill(o *protocol.Observed, vm resource, cfg vmConfig, m marker, tag string) {
	running := vm.Status == "running"
	tagged := tag != "" && slices.Contains(cfg.Tags, tag)
	o.Exists, o.Version = true, cfg.Digest
	o.Owned, o.Unfinished = Ownership(tag == "" || tagged)
	o.IDs = ids(vm.Node, vm.VMID)
	o.Node = &protocol.ObservedNode{ID: vm.VMID, Running: running}
	if running {
		o.Node.UptimeSeconds = vm.Uptime
	}
	st := State{CPU: cfg.Cores, MemoryMiB: cfg.Memory, DiskGiB: cfg.DiskGiB, Image: m.Image, Network: m.Network,
		Tagged: tagged, Running: running}
	o.State, _ = json.Marshal(st, json.Deterministic(true))
}

// Ownership is the provider's claim on a VM that carries its marker, in the pool and the VMID
// range: owned when it also has the tag, unfinished when it does not (a create interrupted before
// tagging). Mamela keeps either claim only for a VM it recorded creating (ADR 0004 of Mamela).
// Undeclared VMs are found by the marker alone (their tag is not known here): tagged reports true.
func Ownership(tagged bool) (owned, unfinished bool) {
	return tagged, !tagged
}

func ids(node string, vmid int64) map[string]string {
	return map[string]string{"node": node, "vmid": strconv.FormatInt(vmid, 10)}
}

// startReason is the same whether the start comes with the create or the VM is found stopped: after
// a create whose answer was lost, the approved start is still exactly the change to make (Mamela
// carries on with an approved plan only while the provider's changes are the same). Starts are
// late: Mamela runs them after every create of the apply, so that no first boot competes with
// another VM's disk import.
const startReason = "start it (after every VM of the run is created)"

// Diff implements sdk.Provider.
func (Provider) Diff(ctx context.Context, in protocol.DiffParams) (protocol.DiffResult, error) {
	obs := map[string]protocol.Observed{}
	for _, o := range in.Observed {
		obs[o.ID] = o
	}
	var out protocol.DiffResult
	desired := slices.Clone(in.Desired)
	slices.SortFunc(desired, func(a, b protocol.Resource) int { return strings.Compare(a.ID, b.ID) })
	taken := map[int64]bool{}
	uploading := map[string]bool{} // images an earlier create of this diff uploads
	for _, r := range desired {
		s, err := spec(r.Spec)
		if err != nil {
			return out, fmt.Errorf("%s: %w", r.ID, err)
		}
		o := obs[r.ID]
		var st State
		if len(o.State) > 0 {
			if err := json.Unmarshal(o.State, &st); err != nil {
				return out, fmt.Errorf("%s: %w", r.ID, err)
			}
		}
		switch {
		case !o.Exists:
			vmid := int64(0)
			for _, id := range st.FreeVMIDs {
				if !taken[id] {
					vmid = id
					break
				}
			}
			if vmid == 0 {
				return out, fmt.Errorf("%s: no free VMID in %d..%d", r.ID, s.VMIDMin, s.VMIDMax)
			}
			taken[vmid] = true
			// An image is uploaded by one change only: another change that finds it on the storage
			// could not tell whether it uploaded it (lookup must tell its own effect).
			key := s.Node + "/" + s.ImageStorage + "/" + s.Image
			if uploading[key] {
				st.ImageUploaded = true
			}
			uploading[key] = true
			out.Changes = append(out.Changes, create(r, s, st, vmid), protocol.Change{Resource: r.ID, Kind: Kind, Op: OpStart,
				IDs: ids(s.Node, vmid), Requests: []protocol.Approved{{Method: startVM.Method, Path: startVM.Path}},
				Reasons: []string{startReason}, Late: true})
		case !o.Owned:
			continue
		default:
			id := o.IDs
			switch {
			case st.DiskGiB < s.DiskGiB:
				// One change at a time on the configuration: each needs the version it was planned on.
				out.Changes = append(out.Changes, protocol.Change{Resource: r.ID, Kind: Kind, Op: OpGrow, IDs: id, Precondition: o.Version,
					Reasons:  []string{fmt.Sprintf("disk %d -> %d GiB", st.DiskGiB, s.DiskGiB)},
					Requests: []protocol.Approved{{Method: resizeDisk.Method, Path: resizeDisk.Path, Params: map[string][]string{"disk": {"scsi0"}, "size": {fmt.Sprintf("%dG", s.DiskGiB)}, "digest": {o.Version}}}}})
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
				if st.Running {
					reqs = append(reqs, protocol.Approved{Method: rebootVM.Method, Path: rebootVM.Path})
					why = append(why, "restart it to apply the new size")
				}
				out.Changes = append(out.Changes, protocol.Change{Resource: r.ID, Kind: Kind, Op: OpResize, IDs: id, Precondition: o.Version, Reasons: why, Requests: reqs})
			case !st.Tagged:
				out.Changes = append(out.Changes, protocol.Change{Resource: r.ID, Kind: Kind, Op: OpTag, IDs: id, Precondition: o.Version,
					Reasons:  []string{"tag it " + s.Tag + " (a create was interrupted before tagging)"},
					Requests: []protocol.Approved{{Method: setConfig.Method, Path: setConfig.Path, Params: map[string][]string{"tags": {s.Tag}, "digest": {o.Version}}}}})
			}
			if !st.Running {
				out.Changes = append(out.Changes, protocol.Change{Resource: r.ID, Kind: Kind, Op: OpStart, IDs: id,
					Requests: []protocol.Approved{{Method: startVM.Method, Path: startVM.Path}}, Reasons: []string{startReason}, Late: true})
			}
			// What it never changes is still a difference: reported as a change of an unsupported
			// operation (nothing to send), which Mamela shows and denies.
			unsupported := func(op protocol.Op, why string) {
				out.Changes = append(out.Changes, protocol.Change{Resource: r.ID, Kind: Kind, Op: op, IDs: id, Reasons: []string{why}})
			}
			if st.DiskGiB > s.DiskGiB {
				unsupported(OpShrink, fmt.Sprintf("disk %d -> %d GiB", st.DiskGiB, s.DiskGiB))
			}
			if st.Image != s.Image {
				unsupported(OpReplaceImage, fmt.Sprintf("image %s -> %s", st.Image, s.Image))
			}
			if want := NetworkString(s.Network); st.Network != want {
				unsupported(OpChangeNetwork, fmt.Sprintf("network %s -> %s", st.Network, want))
			}
		}
	}
	return out, nil
}

// create is the change that makes a VM: upload the image (unless it is there) and the VM's seed,
// create the VM from them (stopped, with the marker), grow its disk, tag it.
func create(r protocol.Resource, s Spec, st State, vmid int64) protocol.Change {
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

// Apply implements sdk.Provider: it sends the approved requests, in order, each at most once, with
// the facts of the artifacts filled in, and skips what is already in effect (an earlier apply that
// was interrupted).
func (p Provider) Apply(ctx context.Context, h *sdk.Host, in protocol.ApplyParams) (protocol.ApplyResult, error) {
	c := in.Change
	node := c.IDs["node"]
	vmid, err := strconv.ParseInt(c.IDs["vmid"], 10, 64)
	if err != nil || node == "" {
		return protocol.ApplyResult{}, errors.New("a change names one VM on one node")
	}
	if explicit(c.Op) {
		if err := ensureOwn(ctx, h, c, node, vmid); err != nil {
			return protocol.ApplyResult{}, err
		}
	}
	ready := map[string]protocol.ArtifactInfo{}
	for _, a := range c.Artifacts {
		info, err := h.Artifact(ctx, a.ID)
		if err != nil {
			return protocol.ApplyResult{}, err
		}
		ready[a.ID] = info
	}
	var done []string
	for _, a := range c.Requests {
		params, err := protocol.FillArtifacts(a.Params, ready)
		if err != nil {
			return protocol.ApplyResult{}, err
		}
		skip, err := p.inEffect(ctx, h, node, c.IDs["storage"], vmid, a, params, ready)
		if err != nil {
			return protocol.ApplyResult{}, err
		}
		if skip {
			continue
		}
		path := strings.NewReplacer("{node}", node, "{vmid}", strconv.FormatInt(vmid, 10), "{storage}", c.IDs["storage"]).Replace(a.Path)
		req := protocol.HostRequest{Method: a.Method, Path: path, Form: params, Upload: a.Upload}
		if a.Method == "DELETE" { // Proxmox reads a DELETE's parameters from the query
			req.Form, req.Query = nil, params
		}
		var data jsontext.Value
		if err := call(ctx, h, req, &data); err != nil {
			return protocol.ApplyResult{}, err
		}
		// Proxmox answers a long operation with its task ID: wait for it to end.
		var upid string
		if json.Unmarshal(data, &upid) == nil && strings.HasPrefix(upid, "UPID:") {
			if err := p.wait(ctx, h, node, upid); err != nil {
				return protocol.ApplyResult{}, fmt.Errorf("%s %s: %w", a.Method, path, err)
			}
		}
		done = append(done, a.Method+" "+path)
	}
	return protocol.ApplyResult{Message: fmt.Sprintf("VM %d: %s", vmid, strings.Join(done, ", "))}, nil
}

// inEffect says whether an approved request's effect is already there.
func (p Provider) inEffect(ctx context.Context, h *sdk.Host, node, storage string, vmid int64, a protocol.Approved, params map[string][]string, ready map[string]protocol.ArtifactInfo) (bool, error) {
	switch {
	case a.Path == upload.Path:
		return hasVolume(ctx, h, node, storage, contentOf(params), ready[a.Upload].Name)
	case a.Path == createVM.Path:
		vms, err := list(ctx, h)
		if err != nil {
			return false, err
		}
		vm, exists := vms[vmid]
		if !exists {
			return false, nil
		}
		cfg, err := config(ctx, h, vm.Node, vmid)
		if err != nil {
			return false, err
		}
		if m, ok := parseMarker(cfg.Description); ok && m.Name == params["name"][0] {
			return true, nil // an earlier apply created it
		}
		return false, fmt.Errorf("VMID %d is taken by a VM this provider did not create; plan again", vmid)
	case a.Path == resizeDisk.Path && params["digest"] == nil:
		cfg, err := config(ctx, h, node, vmid)
		if err != nil {
			return false, err
		}
		want, _ := strconv.ParseInt(strings.TrimSuffix(params["size"][0], "G"), 10, 64)
		return cfg.DiskGiB >= want, nil
	case a.Path == setConfig.Path && params["digest"] == nil:
		cfg, err := config(ctx, h, node, vmid)
		if err != nil {
			return false, err
		}
		return slices.Contains(cfg.Tags, params["tags"][0]), nil
	case a.Path == startVM.Path, a.Path == stopVM.Path:
		vms, err := list(ctx, h)
		if err != nil {
			return false, err
		}
		return (vms[vmid].Status == "running") == (a.Path == startVM.Path), nil
	}
	return false, nil
}

func contentOf(params map[string][]string) string {
	if c := params["content"]; len(c) == 1 {
		return c[0]
	}
	return ""
}

// wait reads a task's status until it ends, and fails unless it ended OK.
func (p Provider) wait(ctx context.Context, h *sdk.Host, node, upid string) error {
	poll, timeout := p.Poll, p.Timeout
	if poll == 0 {
		poll = 2 * time.Second
	}
	if timeout == 0 {
		timeout = 2 * time.Hour
	}
	deadline := time.Now().Add(timeout)
	for {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if err := call(ctx, h, protocol.HostRequest{Method: "GET", Path: "/nodes/" + node + "/tasks/" + upid + "/status"}, &st); err != nil {
			return err
		}
		if st.Status == "stopped" {
			if st.ExitStatus != "OK" && !strings.HasPrefix(st.ExitStatus, "WARNINGS") {
				return fmt.Errorf("task %s ended: %s", upid, st.ExitStatus)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("task %s is still running after %s", upid, timeout)
		}
		time.Sleep(poll)
	}
}

// Lookup implements sdk.Provider.
func (p Provider) Lookup(ctx context.Context, h *sdk.Host, in protocol.LookupParams) (protocol.LookupResult, error) {
	c := in.Change
	node := c.IDs["node"]
	vmid, _ := strconv.ParseInt(c.IDs["vmid"], 10, 64)
	vms, err := list(ctx, h)
	if err != nil {
		return protocol.LookupResult{}, err
	}
	if explicit(c.Op) {
		status, err := lookupExplicit(ctx, h, c, node, vmid, vms)
		return protocol.LookupResult{Status: status}, err
	}
	vm, exists := vms[vmid]
	if !exists {
		// Not created; the uploads before the create may have landed (an image, the seed: Mamela
		// names seeds mamela-seed-<node>-<vmid>-...).
		if c.Op == OpCreate {
			for _, a := range c.Artifacts {
				content, prefix := "iso", fmt.Sprintf("mamela-seed-%s-%d", a.Node, a.NodeID)
				if a.Kind == protocol.ArtifactDownload {
					content, prefix = "import", a.Name
				}
				found, err := hasVolumePrefix(ctx, h, node, c.IDs["storage"], content, prefix)
				if err != nil {
					return protocol.LookupResult{}, err
				}
				if found && slices.ContainsFunc(c.Requests, func(r protocol.Approved) bool { return r.Upload == a.ID }) {
					return protocol.LookupResult{Status: protocol.LookupPartly}, nil
				}
			}
		}
		return protocol.LookupResult{Status: protocol.LookupNotDone}, nil
	}
	cfg, err := config(ctx, h, node, vmid)
	if err != nil {
		return protocol.LookupResult{}, err
	}
	if _, ok := parseMarker(cfg.Description); !ok {
		return protocol.LookupResult{Status: protocol.LookupNotDone}, nil
	}
	want, got := 0, 0
	check := func(ok bool) {
		want++
		if ok {
			got++
		}
	}
	for _, a := range c.Requests {
		switch a.Path {
		case createVM.Path:
			check(true)
		case resizeDisk.Path:
			n, _ := strconv.ParseInt(strings.TrimSuffix(a.Params["size"][0], "G"), 10, 64)
			check(cfg.DiskGiB >= n)
		case setConfig.Path:
			if t := a.Params["tags"]; t != nil {
				check(slices.Contains(cfg.Tags, t[0]))
			}
			if v := a.Params["cores"]; v != nil {
				check(strconv.FormatInt(cfg.Cores, 10) == v[0])
			}
			if v := a.Params["memory"]; v != nil {
				check(strconv.FormatInt(cfg.Memory, 10) == v[0])
			}
		case startVM.Path:
			check(vm.Status == "running")
		}
	}
	switch {
	case got == want:
		return protocol.LookupResult{Status: protocol.LookupDone}, nil
	case got == 0 && c.Op != OpCreate:
		return protocol.LookupResult{Status: protocol.LookupNotDone}, nil
	}
	return protocol.LookupResult{Status: protocol.LookupPartly}, nil
}

func spec(v jsontext.Value) (Spec, error) {
	var s Spec
	if err := json.Unmarshal(v, &s, json.RejectUnknownMembers(true)); err != nil {
		return s, err
	}
	switch {
	case s.Node == "" || s.Pool == "" || s.DiskStorage == "" || s.ImageStorage == "" || s.Bridge == "" || s.Tag == "":
		return s, errors.New("node, pool, diskStorage, imageStorage, bridge and tag are required")
	case s.VMIDMin < 100 || s.VMIDMax < s.VMIDMin || s.VMIDMax > 999999999:
		return s, fmt.Errorf("VMID range %d..%d is not a Proxmox VMID range", s.VMIDMin, s.VMIDMax)
	case s.CPU < 1 || s.MemoryMiB < 256 || s.DiskGiB < 1:
		return s, errors.New("cpu, memoryMiB (256 or more) and diskGiB must be positive")
	case s.Network.Mode != "dhcp" && s.Network.Mode != "static":
		return s, fmt.Errorf("network mode %q is not dhcp or static", s.Network.Mode)
	}
	img, ok := Images[s.Image]
	if !ok {
		return s, fmt.Errorf("image %q is not one of this provider's (%s)", s.Image, strings.Join(slices.Sorted(maps.Keys(Images)), ", "))
	}
	if s.DiskGiB < img.MinDiskGiB {
		return s, fmt.Errorf("image %s needs a disk of at least %d GiB", s.Image, img.MinDiskGiB)
	}
	return s, nil
}

type vmConfig struct {
	Digest      string
	Description string
	Cores       int64
	Memory      int64
	DiskGiB     int64
	Tags        []string
}

// UnmarshalJSON reads what it needs of a VM's configuration: numbers may come as strings.
func (c *vmConfig) UnmarshalJSON(b []byte) error {
	var raw map[string]jsontext.Value
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	text := func(k string) string {
		var s string
		if json.Unmarshal(raw[k], &s) != nil {
			s = strings.Trim(string(raw[k]), `"`)
		}
		return s
	}
	num := func(k string) int64 {
		n, _ := strconv.ParseInt(text(k), 10, 64)
		return n
	}
	c.Digest, c.Description = text("digest"), text("description")
	c.Cores, c.Memory = num("cores"), num("memory")
	if c.Cores == 0 {
		c.Cores = 1 // the Proxmox default
	}
	c.DiskGiB = diskBytes(text("scsi0")) >> 30

	for _, t := range strings.FieldsFunc(text("tags"), func(r rune) bool { return r == ';' || r == ',' || r == ' ' }) {
		c.Tags = append(c.Tags, t)
	}
	return nil
}

// diskBytes reads size=<n>[KMGT] from a drive option (Proxmox writes 3584M after an import, 10G
// after a resize).
func diskBytes(drive string) int64 {
	for _, part := range strings.Split(drive, ",") {
		v, ok := strings.CutPrefix(part, "size=")
		if !ok || v == "" {
			continue
		}
		mult := int64(1)
		switch v[len(v)-1] {
		case 'K':
			mult = 1 << 10
		case 'M':
			mult = 1 << 20
		case 'G':
			mult = 1 << 30
		case 'T':
			mult = 1 << 40
		}
		n, err := strconv.ParseInt(strings.TrimRight(v, "KMGT"), 10, 64)
		if err != nil {
			return 0
		}
		return n * mult
	}
	return 0
}

func config(ctx context.Context, h *sdk.Host, node string, vmid int64) (vmConfig, error) {
	var c vmConfig
	err := call(ctx, h, protocol.HostRequest{Method: "GET", Path: fmt.Sprintf("/nodes/%s/qemu/%d/config", node, vmid)}, &c)
	return c, err
}

func list(ctx context.Context, h *sdk.Host) (map[int64]resource, error) {
	var rs []resource
	if err := call(ctx, h, protocol.HostRequest{Method: "GET", Path: "/cluster/resources", Query: map[string][]string{"type": {"vm"}}}, &rs); err != nil {
		return nil, err
	}
	out := map[int64]resource{}
	for _, r := range rs {
		if r.Type == "qemu" {
			out[r.VMID] = r
		}
	}
	return out, nil
}

func hasVolume(ctx context.Context, h *sdk.Host, node, storage, content, file string) (bool, error) {
	vols, err := volumes(ctx, h, node, storage, content)
	return slices.Contains(vols, storage+":"+content+"/"+file), err
}

func hasVolumePrefix(ctx context.Context, h *sdk.Host, node, storage, content, prefix string) (bool, error) {
	vols, err := volumes(ctx, h, node, storage, content)
	return slices.ContainsFunc(vols, func(v string) bool { return strings.HasPrefix(v, storage+":"+content+"/"+prefix) }), err
}

func volumes(ctx context.Context, h *sdk.Host, node, storage, content string) ([]string, error) {
	var vols []struct {
		VolID string `json:"volid"`
	}
	if err := call(ctx, h, protocol.HostRequest{Method: "GET", Path: fmt.Sprintf("/nodes/%s/storage/%s/content", node, storage),
		Query: map[string][]string{"content": {content}}}, &vols); err != nil {
		return nil, err
	}
	var out []string
	for _, v := range vols {
		out = append(out, v.VolID)
	}
	return out, nil
}

// call sends a request and decodes Proxmox's {"data": ...}.
func call(ctx context.Context, h *sdk.Host, r protocol.HostRequest, out any) error {
	resp, err := h.Request(ctx, r)
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("%s %s: status %d %s: %.300s", r.Method, r.Path, resp.Status, resp.Reason, resp.Body)
	}
	var env struct {
		Data jsontext.Value `json:"data"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &env); err != nil {
		return fmt.Errorf("%s %s: %w", r.Method, r.Path, err)
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}
