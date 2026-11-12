// Package catalogs is the catalog of the Proxmox VE API the provider calls (ADR 0008 of Mamela):
// what each call does (its effect, where the API reads each parameter, its precondition, what it
// must name, how big what it makes is).
//
// It is data. build.sh writes it into the Pome (pome/catalogs/proxmox.json, by
// cmd/proxmox-catalogs), and Mamela uses it only when the operator names it in
// mamela.providers.toml (catalog = "pome:proxmox"). The provider program never sees it: Mamela
// classifies every request the program sends with it. This Go code only writes it; the JSON is
// what is shipped and reviewed (mamela provider catalog pome).
package catalogs

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// All are the catalogs, by the name the Pome ships each under.
var All = map[string]protocol.Catalog{"proxmox": Proxmox}

// Encode writes a catalog file: the schema, then the catalog, indented and deterministic.
func Encode(c protocol.Catalog) ([]byte, error) {
	c.Schema = protocol.CatalogSchema
	if err := c.Check(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(c, json.Deterministic(true), jsontext.Multiline(true), jsontext.WithIndent("  "))
	return append(data, '\n'), err
}

// Proxmox is the part of the Proxmox VE API that the provider uses. Effects
// follow the API documentation: creating allocates a VM and its disks; a configuration change
// with digest refuses a configuration that changed since it was read; starting and stopping
// interrupt the guest; deleting destroys its disks.
var Proxmox = protocol.Catalog{API: "proxmox", Entries: []protocol.Entry{
	{Method: "GET", Path: "/cluster/resources", Effect: protocol.EffectRead, Query: []string{"type"}},
	{Method: "GET", Path: "/nodes/{node}/qemu/{vmid}/config", Effect: protocol.EffectRead},
	{Method: "GET", Path: "/nodes/{node}/qemu/{vmid}/status/current", Effect: protocol.EffectRead},
	{Method: "GET", Path: "/nodes/{node}/tasks/{upid}/status", Effect: protocol.EffectRead},
	{Method: "GET", Path: "/nodes/{node}/tasks", Effect: protocol.EffectRead, Query: []string{"vmid", "typefilter", "source", "limit"}},
	{Method: "GET", Path: "/nodes/{node}/storage/{storage}/content", Effect: protocol.EffectRead, Query: []string{"content"}},
	// What a guard reads (ADR 0006): the cluster's health, the node's memory, the storage's use,
	// which storages accept what, a token's permissions, every guest and its configuration.
	{Method: "GET", Path: "/cluster/status", Effect: protocol.EffectRead},
	{Method: "GET", Path: "/nodes/{node}/status", Effect: protocol.EffectRead},
	{Method: "GET", Path: "/nodes/{node}/storage/{storage}/status", Effect: protocol.EffectRead},
	{Method: "GET", Path: "/nodes/{node}/storage", Effect: protocol.EffectRead, Query: []string{"content"}},
	{Method: "GET", Path: "/access/permissions", Effect: protocol.EffectRead, Query: []string{"userid", "path"}},
	{Method: "GET", Path: "/nodes/{node}/lxc/{vmid}/config", Effect: protocol.EffectRead},
	// Uploading adds a file to a storage (an image to import, a cloud-init seed). Proxmox checks
	// the checksum before it keeps the file. The file itself is the multipart parameter "filename".
	{Method: "POST", Path: "/nodes/{node}/storage/{storage}/upload", Effect: protocol.EffectCreate,
		Form: []string{"content", "checksum-algorithm", "checksum"}, File: "filename"},
	{Method: "POST", Path: "/nodes/{node}/qemu", Effect: protocol.EffectCreate,
		// No tags: Proxmox checks them before the VM is in its pool.
		Form: []string{"vmid", "name", "cores", "memory", "pool", "scsi0", "scsihw", "ostype", "description",
			"boot", "serial0", "vga", "net0", "onboot", "ide2", "agent"},
		Measures: []protocol.Measure{{Param: "cores", Name: protocol.MeasureCPU, Format: protocol.FormatCount},
			{Param: "memory", Name: protocol.MeasureMemoryMiB, Format: protocol.FormatCount},
			{Param: "scsi0", Name: protocol.MeasureDiskGiB, Format: protocol.FormatDrive}}},
	{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/config", Effect: protocol.EffectChange,
		Form: []string{"cores", "memory", "tags", "description", "ide2", "digest"}, Precondition: "digest",
		Measures: []protocol.Measure{{Param: "cores", Name: protocol.MeasureCPU, Format: protocol.FormatCount},
			{Param: "memory", Name: protocol.MeasureMemoryMiB, Format: protocol.FormatCount}}},
	// A template is a VM converted once (its disk becomes a base the clones share); a linked clone
	// is a new VM whose disk is a thin snapshot of the template's. The clone's VMID is the change's
	// (bind), and only linked clones are made (full=0).
	{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/template", Effect: protocol.EffectChange},
	{Method: "POST", Path: "/nodes/{node}/qemu/{template}/clone", Effect: protocol.EffectCreate,
		Form: []string{"newid", "name", "pool", "full", "description"}, Bind: []string{"newid"},
		Values: map[string][]string{"full": {"0"}}},
	// Growing a disk keeps its data (Proxmox never shrinks one).
	{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/resize", Effect: protocol.EffectChange,
		Form: []string{"disk", "size", "digest"}, Precondition: "digest",
		Measures: []protocol.Measure{{Param: "size", Name: protocol.MeasureDiskGiB, Format: protocol.FormatSize}}},
	{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/start", Effect: protocol.EffectRestart},
	{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/reboot", Effect: protocol.EffectRestart},
	{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/reset", Effect: protocol.EffectRestart},
	{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/shutdown", Effect: protocol.EffectRestart},
	{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/stop", Effect: protocol.EffectRestart},
	{Method: "DELETE", Path: "/nodes/{node}/qemu/{vmid}", Effect: protocol.EffectLoseData, Query: []string{"purge", "destroy-unreferenced-disks"}},
}}
