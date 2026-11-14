package proxmox

import "github.com/ieyoukan/mamela/provider/protocol"

// The permission catalog: for every call the provider makes with the operating token, the
// privileges Proxmox VE checks, and where. The manifest's needs (and so the bootstrap's grants and
// the guard's check of the token) must cover them; a test holds them to it, so that a new call
// cannot be added without saying what it needs.
//
// Verified against Proxmox VE 9.2 (pve-manager 9.2.26): the API schema's "permissions" and the
// checks inside the handlers (PVE/API2/Qemu.pm create_vm, check_storage_access,
// check_vm_modify_config_perm, check_vm_create_serial_perm, check_bridge_access; PVE/Storage.pm
// check_volume_access; PVE/GuestHelpers.pm assert_tag_permissions; PVE/API2/Cluster.pm resources).
// Moved here from Mamela's internal/proxmox (ADR 0006 of Mamela).

// Where a privilege is checked, as a manifest target's ref. A VM's privileges (/vms/<vmid>) come
// from its pool's ACL.
const (
	AtPool         = "/pool/{pool}"
	AtDiskStorage  = "/storage/{diskStorage}"
	AtImageStorage = "/storage/{imageStorage}"
	AtBridge       = "/sdn/zones/localnetwork/{bridge}"
)

// Privilege is one check: one of Privs at Ref (Proxmox's "any": the first, the narrowest, is
// granted).
type Privilege struct {
	Ref   string
	Privs []string
	Why   string
}

func at(ref, why string, privs ...string) Privilege {
	return Privilege{Ref: ref, Privs: privs, Why: why}
}

// Catalog is the privileges of each call (by method and path), apart from its parameters.
var Catalog = map[string][]Privilege{
	"GET /cluster/resources": {at(AtPool, "list the pool's VMs (only VMs with VM.Audit are listed)", "VM.Audit"),
		at(AtPool, "be told which pool each VM is in (left out without Pool.Audit)", "Pool.Audit")},
	"GET /nodes/{node}/storage/{storage}/content":    {at(AtImageStorage, "find uploaded images and seeds", "Datastore.Audit", "Datastore.AllocateSpace")},
	"POST /nodes/{node}/storage/{storage}/upload":    {at(AtImageStorage, "upload the pinned image (import) and seeds (iso)", "Datastore.AllocateTemplate")},
	"GET /nodes/{node}/tasks/{upid}/status":          nil, // the caller's own tasks are open to it
	"GET /nodes/{node}/qemu/{vmid}/config":           {at(AtPool, "read a VM's configuration and marker", "VM.Audit")},
	"GET /nodes/{node}/qemu/{vmid}/status/current":   {at(AtPool, "read a VM's state", "VM.Audit")},
	"POST /nodes/{node}/qemu":                        {at(AtPool, "create a VM in the pool", "VM.Allocate")},
	"PUT /nodes/{node}/qemu/{vmid}/config":           nil, // each parameter's (ConfigParams)
	"PUT /nodes/{node}/qemu/{vmid}/resize":           {at(AtPool, "grow a VM's disk", "VM.Config.Disk"), at(AtDiskStorage, "grow a disk on the disk storage", "Datastore.AllocateSpace")},
	"POST /nodes/{node}/qemu/{vmid}/status/start":    {at(AtPool, "start a VM", "VM.PowerMgmt")},
	"POST /nodes/{node}/qemu/{vmid}/status/reboot":   {at(AtPool, "restart a VM", "VM.PowerMgmt")},
	"POST /nodes/{node}/qemu/{vmid}/status/reset":    {at(AtPool, "reset a VM", "VM.PowerMgmt")},
	"POST /nodes/{node}/qemu/{vmid}/status/stop":     {at(AtPool, "stop a VM (before deleting it, or a guest that does not shut down)", "VM.PowerMgmt")},
	"POST /nodes/{node}/qemu/{vmid}/status/shutdown": {at(AtPool, "shut down a VM declared stopped", "VM.PowerMgmt")},
	"DELETE /nodes/{node}/qemu/{vmid}":               {at(AtPool, "delete a VM and its disks", "VM.Allocate")},
	"POST /nodes/{node}/qemu/{vmid}/template":        {at(AtPool, "convert a VM to a template", "VM.Allocate")},
	"POST /nodes/{node}/qemu/{template}/clone": {at(AtPool, "clone the template (in the pool)", "VM.Clone"),
		at(AtPool, "make the clone in the pool", "VM.Allocate"), at(AtDiskStorage, "the clone's disk", "Datastore.AllocateSpace")},
}

// CreateParams are the privileges of each parameter of POST /nodes/{node}/qemu, checked with the
// pool (so the pool's grants suffice).
var CreateParams = map[string][]Privilege{
	"vmid": nil, "pool": nil,
	"name":        {at(AtPool, "name", "VM.Config.Options")},
	"description": {at(AtPool, "ownership marker", "VM.Config.Options")},
	"ostype":      {at(AtPool, "guest OS type", "VM.Config.Options")},
	"onboot":      {at(AtPool, "start on boot (off)", "VM.Config.Options")},
	"agent":       {at(AtPool, "qemu-guest-agent", "VM.Config.Options")},
	"cores":       {at(AtPool, "vCPUs", "VM.Config.CPU")},
	"memory":      {at(AtPool, "memory", "VM.Config.Memory")},
	"scsihw":      {at(AtPool, "SCSI controller", "VM.Config.HWType")},
	"vga":         {at(AtPool, "display (serial)", "VM.Config.HWType")},
	"serial0":     {at(AtPool, "serial console (check_vm_create_serial_perm)", "VM.Config.HWType")},
	"boot":        {at(AtPool, "boot order", "VM.Config.Disk")},
	// Drives are not checked against VM.Config.Disk/CDROM at create; only their storage is.
	"scsi0": {at(AtDiskStorage, "allocate the VM's disk", "Datastore.AllocateSpace"),
		at(AtImageStorage, "read the image to import (check_volume_access)", "Datastore.Audit", "Datastore.AllocateSpace")},
	"ide2": {at(AtImageStorage, "attach the seed ISO (check_volume_access)", "Datastore.Audit", "Datastore.AllocateSpace")},
	"net0": {at(AtPool, "network device", "VM.Config.Network"), at(AtBridge, "connect to the bridge (check_bridge_access)", "SDN.Use")},
	// Never "tags": at create, Proxmox checks tags on /vms/<vmid> before the VM is in the pool.
}

// ConfigParams are the privileges of each parameter of PUT /nodes/{node}/qemu/{vmid}/config.
var ConfigParams = map[string][]Privilege{
	"digest":      nil,
	"description": {at(AtPool, "write the marker", "VM.Config.Options")},
	"tags":        {at(AtPool, "tag the VM (once it is in the pool)", "VM.Config.Options")},
	"cores":       {at(AtPool, "change the vCPUs", "VM.Config.CPU")},
	"memory":      {at(AtPool, "change the memory", "VM.Config.Memory")},
	"ide2":        {at(AtPool, "attach a clone's seed", "VM.Config.CDROM"), at(AtImageStorage, "the seed ISO (check_volume_access)", "Datastore.Audit", "Datastore.AllocateSpace")},
}

// PrivilegesOf is every privilege a call needs, with the parameters it may send. ok is false for a
// call or parameter the catalog does not know.
func PrivilegesOf(c protocol.Call) (out []Privilege, ok bool) {
	ps, known := Catalog[c.Method+" "+c.Path]
	if !known {
		return nil, false
	}
	out = append(out, ps...)
	var params map[string][]Privilege
	switch c.Method + " " + c.Path {
	case "POST /nodes/{node}/qemu":
		params = CreateParams
	case "PUT /nodes/{node}/qemu/{vmid}/config":
		params = ConfigParams
	default:
		return out, true
	}
	for _, p := range append(append([]string{}, c.Query...), c.Form...) {
		pp, known := params[p]
		if !known {
			return nil, false
		}
		out = append(out, pp...)
	}
	return out, true
}
