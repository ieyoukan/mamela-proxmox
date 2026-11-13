package proxmox

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// Moving from Terraform (ADR 0009 of Mamela): `mamela import terraform` gives this provider the VMs
// of a Terraform state (proxmox_virtual_environment_vm of bpg/proxmox, proxmox_vm_qemu of
// Telmate/proxmox), and it declares each one inside the guard's boundary as it is. Importing
// changes nothing and sends no request. A person then adopts each VM (adopt-vm, explicit), which
// writes this provider's marker and tag on it and nothing else, and removes it from Terraform's
// state.
//
// Mamela's boundary is strict: only a VM on the guard's node, in its pool and its VMID range is
// imported, never a template, and only one Mamela can manage as it manages its own (vCPUs as cores
// of one socket, one disk scsi0 on the disk storage, its network on the bridge). Anything else is
// skipped with why and what to do; it stays Terraform's, and Mamela never touches it.

// Terraform resource types the VM kind imports.
const (
	TypeBPG     = "proxmox_virtual_environment_vm" // bpg/proxmox
	TypeTelmate = "proxmox_vm_qemu"                // Telmate/proxmox
)

func (*vmKind) Imports() []string { return []string{TypeBPG, TypeTelmate} }

// Import implements kit.Importer.
func (*vmKind) Import(o protocol.ImportObject, r kit.Resolver) (kit.Imported, error) {
	var raw jsontext.Value
	if ok, err := r.Settings(&raw); !ok || err != nil {
		return kit.Imported{}, kit.Skip("[provider.proxmox.guard] is not set (mamela.providers.toml): it is the boundary (node, pool, VMID range, tag) " +
			"imported VMs go into; set it, and import again")
	}
	s, err := ParseGuardSettings(raw)
	if err != nil {
		return kit.Imported{}, kit.Skip(err.Error())
	}
	var v tfVM
	switch o.Type {
	case TypeBPG:
		v, err = readBPG(o.Values)
	case TypeTelmate:
		v, err = readTelmate(o.Values)
	default:
		return kit.Imported{}, kit.Skip(fmt.Sprintf("type %s is not a VM this provider imports", o.Type))
	}
	if err != nil {
		return kit.Imported{}, kit.Skip(fmt.Sprintf("its attributes do not read as a VM: %v", err))
	}
	if why := v.outside(s); why != "" {
		return kit.Imported{}, kit.Skip(why)
	}
	return kit.Imported{IDs: ids(v.Node, v.VMID), Spec: Spec{Node: s.Node, Pool: s.Pool, DiskStorage: s.DiskStorage, ImageStorage: s.ImageStorage,
		Bridge: s.Bridge, Tag: s.Tag, VMIDMin: s.VMIDMin, VMIDMax: s.VMIDMax,
		CPU: v.Cores, MemoryMiB: v.MemoryMiB, DiskGiB: v.Disks["scsi0"].GiB, Network: v.Network, Stopped: !v.Running}}, nil
}

// Find implements kit.Finder: the VM with the IDs a declaration was imported with, though it
// carries no marker (List gives only VMs with one). A VM with a marker of Mamela's, or a template,
// is never found: it is not one to adopt. Whether it is inside the boundary is the declaration's
// (claim).
func (k *vmKind) Find(c *kit.Client, id map[string]string) (*kit.Object, error) {
	vmid, err := strconv.ParseInt(id["vmid"], 10, 64)
	if err != nil || id["node"] == "" {
		return nil, nil
	}
	rd := k.run.reader(c)
	vms, err := rd.list()
	if err != nil {
		return nil, err
	}
	vm, ok := vms[vmid]
	if !ok || vm.Node != id["node"] {
		return nil, nil
	}
	cfg, err := rd.config(vm.Node, vmid)
	if err != nil || marked(cfg.Description) || cfg.Template {
		return nil, err
	}
	x := vmObject(vm, cfg, marker{}, originUnmarked)
	st := x.State.(State)
	st.Sockets, st.DiskStorage, st.Bridge = cfg.Sockets, diskStorage(cfg.SCSI0), bridgeOf(cfg.Net0)
	x.State = st
	return &x, nil
}

// tfVM is what the import reads of a VM, from either provider's attributes.
type tfVM struct {
	VMID              int64
	Node, Pool        string
	Description       string
	Template, Running bool
	Cores, Sockets    int64
	MemoryMiB         int64
	Disks             map[string]tfDisk // by interface: scsi0, virtio0, ...
	Bridges           []string          // of net0, net1, ...
	Network           protocol.Network
}

type tfDisk struct {
	Storage string
	GiB     int64
}

// outside says why a VM is not imported, with what to do (empty: it is imported).
func (v tfVM) outside(s GuardSettings) string {
	disks := slices.Sorted(maps.Keys(v.Disks))
	switch {
	case v.Template:
		return "it is a template: Mamela makes its own templates from pinned images (ProxmoxTemplate); this one stays Terraform's"
	case marked(v.Description):
		return "it carries a marker of Mamela's: it is Mamela's already; remove it from Terraform's state (terraform state rm)"
	case v.Node != s.Node:
		return fmt.Sprintf("it is on node %s, and Mamela's boundary is node %s ([provider.proxmox.guard] node): Mamela never touches a guest "+
			"outside it; keep it in Terraform, or migrate it to %s first and import again", v.Node, s.Node, s.Node)
	case !s.inRange(v.VMID):
		return fmt.Sprintf("VMID %d is outside Mamela's VMIDs %d..%d ([provider.proxmox.guard]): Mamela never touches a guest outside its "+
			"range, and a VMID cannot be changed in place; keep it in Terraform, or restore a backup of it to a free VMID in the range, "+
			"in pool %s, and import that", v.VMID, s.VMIDMin, s.VMIDMax, s.Pool)
	case v.Pool != s.Pool:
		return fmt.Sprintf("it is in pool %q, not Mamela's pool %s: Mamela touches only guests in its pool (and the guard blocks every "+
			"apply while a guest in the VMID range is outside it); move it into %s with Terraform (pool_id, or pool), apply, and import again",
			v.Pool, s.Pool, s.Pool)
	case v.Sockets > 1:
		return fmt.Sprintf("it has %d sockets: Mamela declares vCPUs as cores of one socket; set sockets = 1 (and cores = %d) in Terraform, "+
			"apply, and import again", v.Sockets, v.Sockets*v.Cores)
	case v.Disks["scsi0"] == (tfDisk{}):
		return fmt.Sprintf("it has no disk scsi0 (its disks: %s): Mamela manages one disk, scsi0, and grows only that one",
			strings.Join(disks, ", "))
	case v.Disks["scsi0"].Storage != s.DiskStorage:
		return fmt.Sprintf("its disk scsi0 is on storage %s, not Mamela's disk storage %s ([provider.proxmox.guard] disk_storage): "+
			"move the disk there (Move Storage), apply it in Terraform, and import again", v.Disks["scsi0"].Storage, s.DiskStorage)
	case v.Disks["scsi0"].GiB < 1:
		return "its disk scsi0 has no size Mamela can read"
	case len(v.Bridges) == 0:
		return "it has no network device: Mamela's VMs have one, net0"
	case v.Bridges[0] != s.Bridge:
		return fmt.Sprintf("its network net0 is on bridge %s, not Mamela's bridge %s ([provider.proxmox.guard] bridge)", v.Bridges[0], s.Bridge)
	case v.Cores < 1 || v.MemoryMiB < 256:
		return fmt.Sprintf("%d cores and %d MiB of memory: Mamela declares at least 1 core and 256 MiB", v.Cores, v.MemoryMiB)
	}
	return ""
}

// readBPG reads a proxmox_virtual_environment_vm of bpg/proxmox.
func readBPG(raw jsontext.Value) (tfVM, error) {
	var a struct {
		ID          string `json:"id"`
		VMID        *int64 `json:"vm_id"`
		NodeName    string `json:"node_name"`
		PoolID      string `json:"pool_id"`
		Description string `json:"description"`
		Template    bool   `json:"template"`
		Started     *bool  `json:"started"`
		CPUs        []struct {
			Cores   *number `json:"cores"`
			Sockets *number `json:"sockets"`
		} `json:"cpu"`
		Memory []struct {
			Dedicated *number `json:"dedicated"`
		} `json:"memory"`
		Disk []struct {
			Interface   string  `json:"interface"`
			DatastoreID string  `json:"datastore_id"`
			Size        *number `json:"size"`
		} `json:"disk"`
		NetworkDevice []struct {
			Bridge string `json:"bridge"`
		} `json:"network_device"`
		Initialization []struct {
			IPConfig []struct {
				IPv4 []struct {
					Address string `json:"address"`
					Gateway string `json:"gateway"`
				} `json:"ipv4"`
			} `json:"ip_config"`
			DNS []struct {
				Servers []string `json:"servers"`
				Server  string   `json:"server"`
			} `json:"dns"`
		} `json:"initialization"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return tfVM{}, err
	}
	v := tfVM{Node: a.NodeName, Pool: a.PoolID, Description: a.Description, Template: a.Template, Running: a.Started == nil || *a.Started,
		Cores: 1, Sockets: 1, MemoryMiB: 512, Disks: map[string]tfDisk{}}
	switch {
	case a.VMID != nil:
		v.VMID = *a.VMID
	default:
		n, err := strconv.ParseInt(a.ID, 10, 64)
		if err != nil {
			return v, fmt.Errorf("no vm_id")
		}
		v.VMID = n
	}
	if len(a.CPUs) > 0 {
		v.Cores, v.Sockets = a.CPUs[0].Cores.or(1), a.CPUs[0].Sockets.or(1)
	}
	if len(a.Memory) > 0 {
		v.MemoryMiB = a.Memory[0].Dedicated.or(512)
	}
	for _, d := range a.Disk {
		v.Disks[d.Interface] = tfDisk{Storage: d.DatastoreID, GiB: d.Size.or(0)}
	}
	for _, n := range a.NetworkDevice {
		v.Bridges = append(v.Bridges, n.Bridge)
	}
	v.Network = protocol.Network{Mode: "dhcp"}
	if len(a.Initialization) > 0 {
		in := a.Initialization[0]
		if len(in.IPConfig) > 0 && len(in.IPConfig[0].IPv4) > 0 {
			ip := in.IPConfig[0].IPv4[0]
			if ip.Address != "" && ip.Address != "dhcp" {
				v.Network = protocol.Network{Mode: "static", Address: ip.Address, Gateway: ip.Gateway}
			}
		}
		if v.Network.Mode == "static" && len(in.DNS) > 0 {
			v.Network.DNS = in.DNS[0].Servers
			if len(v.Network.DNS) == 0 && in.DNS[0].Server != "" {
				v.Network.DNS = strings.Fields(in.DNS[0].Server)
			}
		}
	}
	return v, nil
}

// readTelmate reads a proxmox_vm_qemu of Telmate/proxmox (2.x and 3.x).
func readTelmate(raw jsontext.Value) (tfVM, error) {
	var a struct {
		ID          string         `json:"id"` // <node>/qemu/<vmid>
		VMID        *number        `json:"vmid"`
		TargetNode  string         `json:"target_node"`
		Pool        string         `json:"pool"`
		Desc        string         `json:"desc"`        // 2.x
		Description string         `json:"description"` // 3.x
		Cores       *number        `json:"cores"`
		Sockets     *number        `json:"sockets"`
		CPU         jsontext.Value `json:"cpu"` // the CPU type (a string), or a block of cores and sockets (3.x)
		Memory      *number        `json:"memory"`
		Disks       []tmBuses      `json:"disks"` // 3.x
		Disk        []struct {     // 2.x, and 3.x's disk block
			Type    string         `json:"type"`
			Slot    jsontext.Value `json:"slot"`
			Storage string         `json:"storage"`
			Size    jsontext.Value `json:"size"`
		} `json:"disk"`
		Network []struct {
			ID     *number `json:"id"`
			Bridge string  `json:"bridge"`
		} `json:"network"`
		IPConfig0  string `json:"ipconfig0"`
		Nameserver string `json:"nameserver"`
		VMState    string `json:"vm_state"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return tfVM{}, err
	}
	v := tfVM{Node: a.TargetNode, Pool: a.Pool, Description: a.Description, Running: a.VMState != "stopped",
		Cores: a.Cores.or(1), Sockets: a.Sockets.or(1), MemoryMiB: a.Memory.or(512), Disks: map[string]tfDisk{}}
	if v.Description == "" {
		v.Description = a.Desc
	}
	parts := strings.Split(a.ID, "/")
	if v.Node == "" && len(parts) == 3 {
		v.Node = parts[0]
	}
	switch {
	case a.VMID != nil && *a.VMID > 0:
		v.VMID = int64(*a.VMID)
	case len(parts) == 3:
		n, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return v, fmt.Errorf("no vmid")
		}
		v.VMID = n
	default:
		return v, fmt.Errorf("no vmid")
	}
	var block []struct {
		Cores   *number `json:"cores"`
		Sockets *number `json:"sockets"`
	}
	if json.Unmarshal(a.CPU, &block) == nil && len(block) > 0 {
		v.Cores, v.Sockets = block[0].Cores.or(v.Cores), block[0].Sockets.or(v.Sockets)
	}
	for _, b := range a.Disks {
		b.add(v.Disks)
	}
	count := map[string]int{}
	for _, d := range a.Disk {
		if d.Type == "cdrom" || d.Type == "cloudinit" {
			continue
		}
		var slot string
		var n number
		onBus := slices.Contains([]string{"ide", "sata", "scsi", "virtio"}, d.Type)
		switch {
		case json.Unmarshal(d.Slot, &slot) == nil && busSlot.MatchString(slot): // "scsi0" (3.x)
		case onBus && len(d.Slot) > 0 && json.Unmarshal(d.Slot, &n) == nil: // type scsi, slot 0 (2.x)
			slot = d.Type + strconv.FormatInt(int64(n), 10)
		case onBus: // type scsi, no slot: the next of its bus
			slot = d.Type + strconv.Itoa(count[d.Type])
		default:
			continue
		}
		count[d.Type]++
		v.Disks[slot] = tfDisk{Storage: d.Storage, GiB: telmateSize(d.Size)}
	}
	slices.SortStableFunc(a.Network, func(x, y struct {
		ID     *number `json:"id"`
		Bridge string  `json:"bridge"`
	}) int {
		return int(x.ID.or(0) - y.ID.or(0))
	})
	for _, n := range a.Network {
		v.Bridges = append(v.Bridges, n.Bridge)
	}
	v.Network = protocol.Network{Mode: "dhcp"}
	for part := range strings.SplitSeq(a.IPConfig0, ",") {
		k, val, _ := strings.Cut(part, "=")
		switch k {
		case "ip":
			if val != "dhcp" && val != "" {
				v.Network.Mode, v.Network.Address = "static", val
			}
		case "gw":
			v.Network.Gateway = val
		}
	}
	if v.Network.Mode == "static" {
		v.Network.DNS = strings.Fields(a.Nameserver)
	} else {
		v.Network.Gateway = ""
	}
	return v, nil
}

var busSlot = regexp.MustCompile(`^(ide|sata|scsi|virtio)[0-9]+$`)

// tmBuses is Telmate 3.x's disks block: a bus (ide, sata, scsi, virtio), then a slot (scsi0), then
// what is in it (a disk, a CD-ROM, a cloud-init drive).
type tmBuses map[string][]map[string][]struct {
	Disk []struct {
		Storage string         `json:"storage"`
		Size    jsontext.Value `json:"size"`
	} `json:"disk"`
}

func (b tmBuses) add(disks map[string]tfDisk) {
	for _, bus := range b {
		for _, slots := range bus {
			for slot, in := range slots {
				if len(in) > 0 && len(in[0].Disk) > 0 {
					disks[slot] = tfDisk{Storage: in[0].Disk[0].Storage, GiB: telmateSize(in[0].Disk[0].Size)}
				}
			}
		}
	}
}

// telmateSize reads a disk size: "20G", "20480M", or a number of GiB.
func telmateSize(v jsontext.Value) int64 {
	var s string
	if json.Unmarshal(v, &s) == nil {
		if strings.TrimRight(s, "0123456789") == "" {
			s += "G"
		}
		return sizeBytes(s) >> 30
	}
	var n number
	if json.Unmarshal(v, &n) == nil {
		return int64(n)
	}
	return 0
}

// number is a whole number that a state may write as a number or as a string.
type number int64

func (n *number) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		x, err := strconv.ParseInt(s, 10, 64)
		*n = number(x)
		return err
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*n = number(f)
	return nil
}

func (n *number) or(d int64) int64 {
	if n == nil || *n == 0 {
		return d
	}
	return int64(*n)
}
