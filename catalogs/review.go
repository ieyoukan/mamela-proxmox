package catalogs

import "github.com/ieyoukan/mamela/provider/protocol"

// Review is what a person decided about one call (by its method and path, as the provider
// declares it), beyond what the API schema says. Every call that is not a GET has one: a call that
// changes something is never shipped without a person having looked at it.
type Review struct {
	// Handler is the name of the call's handler in Proxmox VE's API schema ("create_vm") this
	// review was made for: a schema where it is another fails the check (the call may do something
	// else now), and a person reviews the call again.
	Handler string
	// Effect classifies the call where no rule does (generate.go), or overrules one; Why says why.
	// It is never a read for a call that is not a GET.
	Effect protocol.Effect
	Why    string
	// Precondition, Measures, Values and Bind are the catalog entry's (protocol.Entry): the
	// parameter that carries the version a change is based on, the parameters that size what it
	// makes, the only values a parameter may have, and the parameters that must name the change's
	// object.
	Precondition string
	Measures     []protocol.Measure
	Values       map[string][]string
	Bind         []string
}

var (
	cpu    = protocol.Measure{Param: "cores", Name: protocol.MeasureCPU, Format: protocol.FormatCount}
	memory = protocol.Measure{Param: "memory", Name: protocol.MeasureMemoryMiB, Format: protocol.FormatCount}
)

// Reviewed is the reviewed table. Changing it is a reviewed change of the catalog: the generated
// pome/catalogs/proxmox.json shows what it changes.
var Reviewed = map[string]Review{
	"POST /nodes/{node}/storage/{storage}/upload": {Handler: "upload", Effect: protocol.EffectCreate,
		Why: "adds a file to a storage (an image to import, a cloud-init seed); Proxmox checks its checksum before it keeps it"},
	"POST /nodes/{node}/qemu": {Handler: "create_vm", Effect: protocol.EffectCreate,
		Why:      "allocates a VM and its disks",
		Measures: []protocol.Measure{cpu, memory, {Param: "scsi0", Name: protocol.MeasureDiskGiB, Format: protocol.FormatDrive}}},
	"PUT /nodes/{node}/qemu/{vmid}/config": {Handler: "update_vm", Effect: protocol.EffectChange,
		Why: "changes the configuration in place; only the parameters listed can be sent (vCPUs, memory, tags, the description " +
			"and the seed's CD-ROM): none removes or replaces a disk",
		Precondition: "digest", Measures: []protocol.Measure{cpu, memory}},
	"POST /nodes/{node}/qemu/{vmid}/template": {Handler: "template", Effect: protocol.EffectChange,
		Why: "converts a VM into a template in place: its disk becomes the read-only base of linked clones; no data is lost"},
	"POST /nodes/{node}/qemu/{template}/clone": {Handler: "clone_vm", Effect: protocol.EffectCreate,
		Why:    "makes a new VM (the change's newid) whose disk is a thin snapshot of the template's; only linked clones (full=0)",
		Values: map[string][]string{"full": {"0"}}, Bind: []string{"newid"}},
	"PUT /nodes/{node}/qemu/{vmid}/resize": {Handler: "resize_vm", Effect: protocol.EffectChange,
		Why:          "grows a disk and keeps its data: Proxmox refuses a smaller size",
		Precondition: "digest", Measures: []protocol.Measure{{Param: "size", Name: protocol.MeasureDiskGiB, Format: protocol.FormatSize}}},
	"POST /nodes/{node}/qemu/{vmid}/status/start":    {Handler: "vm_start"},
	"POST /nodes/{node}/qemu/{vmid}/status/shutdown": {Handler: "vm_shutdown"},
	"POST /nodes/{node}/qemu/{vmid}/status/stop":     {Handler: "vm_stop"},
	"POST /nodes/{node}/qemu/{vmid}/status/reboot":   {Handler: "vm_reboot"},
	"POST /nodes/{node}/qemu/{vmid}/status/reset":    {Handler: "vm_reset"},
	// Deleting destroys the VM's disks (destroy-unreferenced-disks); purge, which also changes the
	// cluster's backup and replication jobs, is not declared.
	"DELETE /nodes/{node}/qemu/{vmid}": {Handler: "destroy_vm"},
}
