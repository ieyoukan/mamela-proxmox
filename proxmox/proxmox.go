// Package proxmox is a Mamela provider of Proxmox VE virtual machines that mamelet runs on. It
// runs as a separate, isolated program (WASM): it reaches Proxmox only through the Mamela host,
// which holds the token and checks every request against the Proxmox catalog this Pome ships and
// the change's permit, and which makes the files a VM is created from (ADR 0004 of Mamela):
//
//   - the cloud image, a pinned download (URL and sha256 from Images), uploaded once per storage;
//   - the VM's mamelet seed, made by the host with a one-time enrollment token.
//
// It is written with Mamela's provider kit (provider/kit, ADR 0009 of Mamela): each kind (vm.go,
// template.go) says how to list its objects, what a declaration changes and whether a request is
// in effect, and kit does the protocol. provider.go joins them, with what kit does not do yet.
//
// Kind ProxmoxVM is a node kind: Mamela runs mamelet on these VMs, so packages, services and k3s
// can be declared on them. The provider creates VMs (as linked clones of a template of their
// image), starts them or shuts them down as declared, grows their disks and changes their vCPUs
// and memory. It never shrinks a disk, replaces an image, changes a network or moves a VM, and it
// never touches a VM without its marker. Deleting, restarting, stopping and taking over a VM are
// explicit operations (explicit.go): a person asks for each by name. A VM that Terraform made can
// be imported and taken over (imports.go).
//
// Its guard (guard.go) checks the cluster before and after every apply: the boundary of pool, VMID
// range and node; the operating token's permissions; the room on the node; and that no guest
// outside the boundary changed.
package proxmox

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// Name and Version are the provider's, as its manifest and Pome name them.
const (
	Name    = "proxmox"
	Version = "0.2.0"
	// API is the API it talks to (its catalog: pome/catalogs/proxmox.json, "pome:proxmox").
	API = "proxmox"
)

// Kind is the resource kind of a VM.
const Kind protocol.ResourceKind = "ProxmoxVM"

// Operations of a VM that a plan carries out.
const (
	OpCreate   protocol.Op = "create-vm"
	OpStart    protocol.Op = "start-vm"
	OpShutdown protocol.Op = "shutdown-vm" // a VM declared stopped that runs: ask its guest to power off
	OpGrow     protocol.Op = "grow-vm-disk"
	OpResize   protocol.Op = "resize-vm"
	OpTag      protocol.Op = "tag-vm"

	// Never carried out (the manifest says why); a difference is shown and denied.
	OpShrink        protocol.Op = "shrink-vm-disk"
	OpReplaceImage  protocol.Op = "replace-vm-image"
	OpChangeNetwork protocol.Op = "change-vm-network"
	OpMove          protocol.Op = "move-vm"
	OpCreateNoImage protocol.Op = "create-vm-without-image"
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
// and what it is. Its JSON is what the Pome's Proxmox.vm writes; the Pome's Proxmox.Kinds module is
// generated from this type (its tags: kit.SpecOf).
type Spec struct {
	Node         string `json:"node" doc:"the Proxmox VE node"`
	Pool         string `json:"pool" doc:"the pool Mamela's token may change"`
	DiskStorage  string `json:"diskStorage" doc:"the storage of its disk"`
	ImageStorage string `json:"imageStorage" doc:"the storage of the images and seeds it is made from"`
	Bridge       string `json:"bridge" doc:"the bridge of its network"`
	Tag          string `json:"tag" doc:"the tag Mamela puts on its VMs"`
	VMIDMin      int64  `json:"vmidMin" doc:"the first VMID Mamela may use"`
	VMIDMax      int64  `json:"vmidMax" doc:"the last VMID Mamela may use"`
	CPU          int64  `json:"cpu" doc:"vCPUs (cores of one socket)"`
	MemoryMiB    int64  `json:"memoryMiB" doc:"memory in MiB"`
	DiskGiB      int64  `json:"diskGiB" doc:"the disk (scsi0) in GiB"`
	// Image is one of Images; none for a VM imported from Terraform: Mamela never creates one
	// without an image.
	Image   string           `json:"image,omitempty" doc:"the pinned image it is made from (ubuntu-24.04); none for a VM imported to adopt"`
	Network protocol.Network `json:"network" doc:"how it gets its address (given once, when it is created)"`
	// Template is the ProxmoxTemplate (by name) the VM is a linked clone of; none: the image is
	// imported into the VM's own disk.
	Template string `json:"template,omitempty" mamela:"ref=ProxmoxTemplate" doc:"the template it is a linked clone of"`
	// Stopped keeps the VM stopped: a running one is shut down (its guest is asked to power off).
	Stopped bool `json:"stopped,omitzero" doc:"keep it stopped (a running one is shut down)"`
}

// State is what is observed of a VM.
type State struct {
	CPU       int64  `json:"cpu,omitempty"`
	MemoryMiB int64  `json:"memoryMiB,omitempty"`
	DiskGiB   int64  `json:"diskGiB,omitempty"`
	Image     string `json:"image,omitempty"`
	Network   string `json:"network,omitempty"`
	Tagged    bool   `json:"tagged,omitempty"` // it has the declared tag
	Running   bool   `json:"running,omitempty"`
	Seeded    bool   `json:"seeded,omitempty"` // its seed is attached
	// Pool is the pool it is in; Tags its tags.
	Pool string   `json:"pool,omitempty"`
	Tags []string `json:"tags,omitempty"`
	// Origin is, for a VM to adopt, who made it: Mamela's built-in provider ("builtin"), or another
	// tool ("unmarked": found by the IDs it was imported with). Sockets, DiskStorage and Bridge
	// are what adopting an unmarked one checks.
	Origin      string `json:"origin,omitempty"`
	Sockets     int64  `json:"sockets,omitempty"`
	DiskStorage string `json:"diskStorage,omitempty"`
	Bridge      string `json:"bridge,omitempty"`
	// For a VM that does not exist yet: the free VMIDs of its range, and whether its image is
	// already on the image storage.
	FreeVMIDs     []int64 `json:"freeVmids,omitempty"`
	ImageUploaded bool    `json:"imageUploaded,omitempty"`
	// Uptime is what List read, for the observed node; it is not reported in the state.
	Uptime int64 `json:"uptime,omitempty"`
}

// Origins of a VM to adopt.
const (
	originBuiltin  = "builtin"
	originUnmarked = "unmarked"
)

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
	setSize    = protocol.Call{Method: "PUT", Path: setConfig.Path, Form: []string{"cores", "memory", "digest"}}
	setTags    = protocol.Call{Method: "PUT", Path: setConfig.Path, Form: []string{"tags", "digest"}}
	setMarker  = protocol.Call{Method: "PUT", Path: setConfig.Path, Form: []string{"description", "tags", "digest"}}
	startVM    = protocol.Call{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/start"}
	shutdownVM = protocol.Call{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/shutdown"}
	rebootVM   = protocol.Call{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/reboot"}
)

// reads are what every kind lists its objects with (kit adds them to every operation).
var reads = []protocol.Call{listVMs, getConfig, getStatus, listContent, getTask}

// Marker is the first line of the description of the VMs this provider created; the lines after
// it record the VM's name, image and network.
const Marker = "Managed by Mamela (proxmox provider). Do not edit the lines below."

type marker struct{ Name, Image, Network string }

func (m marker) String() string {
	return fmt.Sprintf("%s\nmamela-name: %s\nmamela-image: %s\nmamela-network: %s\n", Marker, m.Name, m.Image, m.Network)
}

func parseMarker(description string) (marker, bool) { return parseMarkerAfter(Marker, description) }

func parseMarkerAfter(first, description string) (marker, bool) {
	lines := strings.Split(strings.TrimSpace(description), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != first {
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

// marked says whether a description starts with any marker of Mamela's (this provider's, a
// template's, the built-in provider's), finished or not.
func marked(description string) bool {
	first, _, _ := strings.Cut(strings.TrimSpace(description), "\n")
	first = strings.TrimSpace(first)
	return first == Marker || first == TemplateMarker || first == OldMarker
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
	case s.Image == "" && s.Template != "":
		return s, errors.New("a VM without an image (imported to adopt) is not a clone of a template: remove template")
	case s.Image == "":
		return s, nil
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

// inRange says whether a VMID is in a declaration's range.
func (s Spec) inRange(vmid int64) bool { return vmid >= s.VMIDMin && vmid <= s.VMIDMax }

type vmConfig struct {
	Digest      string
	Description string
	Cores       int64
	Sockets     int64
	Memory      int64
	DiskGiB     int64
	Tags        []string
	Template    bool   // a template
	SCSI0, IDE2 string // the disk (a linked clone's names its template's base disk) and the seed
	Net0        string
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
	c.Cores, c.Sockets, c.Memory = num("cores"), num("sockets"), num("memory")
	if c.Cores == 0 {
		c.Cores = 1 // the Proxmox default
	}
	if c.Sockets == 0 {
		c.Sockets = 1
	}
	c.DiskGiB = diskBytes(text("scsi0")) >> 30
	c.SCSI0, c.IDE2, c.Net0 = text("scsi0"), text("ide2"), text("net0")
	c.Template = text("template") == "1"

	for _, t := range strings.FieldsFunc(text("tags"), func(r rune) bool { return r == ';' || r == ',' || r == ' ' }) {
		c.Tags = append(c.Tags, t)
	}
	return nil
}

// diskStorage is the storage of a drive ("local-lvm:vm-7001-disk-0,size=10G": local-lvm).
func diskStorage(drive string) string {
	vol, _, _ := strings.Cut(drive, ",")
	st, _, ok := strings.Cut(vol, ":")
	if !ok {
		return ""
	}
	return st
}

// bridgeOf is the bridge of a network device ("virtio=BC:24:11:00:00:01,bridge=vmbr0": vmbr0).
func bridgeOf(net string) string {
	for part := range strings.SplitSeq(net, ",") {
		if b, ok := strings.CutPrefix(part, "bridge="); ok {
			return b
		}
	}
	return ""
}

// diskBytes reads size=<n>[KMGT] from a drive option (Proxmox writes 3584M after an import, 10G
// after a resize).
func diskBytes(drive string) int64 {
	for _, part := range strings.Split(drive, ",") {
		v, ok := strings.CutPrefix(part, "size=")
		if !ok || v == "" {
			continue
		}
		return sizeBytes(v)
	}
	return 0
}

// sizeBytes reads a size: a number with K, M, G or T after it (bytes without).
func sizeBytes(v string) int64 {
	if v == "" {
		return 0
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

// --- reads ---

// get sends a read and decodes Proxmox's {"data": ...} into out.
func get(c *kit.Client, call protocol.Call, ids map[string]string, params map[string][]string, out any) error {
	body, err := c.Read(call, ids, params)
	if err != nil {
		return fmt.Errorf("%s %s %v: %w", call.Method, call.Path, ids, err)
	}
	return data(call, body, out)
}

// data decodes Proxmox's {"data": ...} answer into out (nothing when out is nil).
func data(call protocol.Call, body []byte, out any) error {
	var env struct {
		Data jsontext.Value `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("%s %s: the answer is not Proxmox's {\"data\": ...}: %w", call.Method, call.Path, err)
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

func config(c *kit.Client, node string, vmid int64) (vmConfig, error) {
	var cfg vmConfig
	err := get(c, getConfig, ids(node, vmid), nil, &cfg)
	return cfg, err
}

// list is every VM (qemu) the token sees, by VMID.
func list(c *kit.Client) (map[int64]resource, error) {
	var rs []resource
	if err := get(c, listVMs, nil, map[string][]string{"type": {"vm"}}, &rs); err != nil {
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

func hasVolume(c *kit.Client, node, storage, content, file string) (bool, error) {
	vols, err := volumes(c, node, storage, content)
	return slices.Contains(vols, storage+":"+content+"/"+file), err
}

func hasVolumePrefix(c *kit.Client, node, storage, content, prefix string) (bool, error) {
	vols, err := volumes(c, node, storage, content)
	return slices.ContainsFunc(vols, func(v string) bool { return strings.HasPrefix(v, storage+":"+content+"/"+prefix) }), err
}

func volumes(c *kit.Client, node, storage, content string) ([]string, error) {
	var vols []struct {
		VolID string `json:"volid"`
	}
	if err := get(c, listContent, map[string]string{"node": node, "storage": storage},
		map[string][]string{"content": {content}}, &vols); err != nil {
		return nil, err
	}
	var out []string
	for _, v := range vols {
		out = append(out, v.VolID)
	}
	return out, nil
}

// reader reads VMs and their configurations: once per observation (the kinds' lists, what is
// missing, what is found), and afresh otherwise (applying, looking up).
type reader struct {
	c    *kit.Client
	vms  map[int64]resource
	cfgs map[int64]vmConfig
}

func (r *reader) list() (map[int64]resource, error) {
	if r.vms != nil {
		return r.vms, nil
	}
	vms, err := list(r.c)
	if err == nil && r.cfgs != nil {
		r.vms = vms // an observation's reader keeps what it read
	}
	return vms, err
}

func (r *reader) config(node string, vmid int64) (vmConfig, error) {
	if cfg, ok := r.cfgs[vmid]; ok {
		return cfg, nil
	}
	cfg, err := config(r.c, node, vmid)
	if err == nil && r.cfgs != nil {
		r.cfgs[vmid] = cfg
	}
	return cfg, err
}

// encode is a state as kit reports it.
func encode(v any) jsontext.Value {
	b, _ := json.Marshal(v, json.Deterministic(true))
	return b
}
