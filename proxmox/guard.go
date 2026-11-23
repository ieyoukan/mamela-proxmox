package proxmox

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// The guard (ADR 0006 of Mamela) is the provider's safety check around every apply on a shared
// cluster that also runs guests Mamela must never touch. Mamela calls it before (with the changes)
// and after (with the baseline it returned before), sends it only the reads declared in the
// manifest, with the guard's own token: a read-only audit token that sees the whole cluster.
//
// Before, it checks the cluster (quorum, nodes), the operating token (its permissions are exactly
// the grants this provider needs: no more, no less), the room the changes need (memory, storage),
// the boundary (no foreign guest in the pool or the VMID range, the number of VMs, the named
// guests where they are named) and the changes themselves (each stays inside the boundary the
// operator set, whatever the declaration says), and it records every guest outside the boundary.
// After, it checks that those guests are unchanged.
//
// The boundary is the pool and the VMID range on the node, and the guests the operator names one
// by one (Named): existing guests elsewhere (another node, no pool, a VMID out of the range) that
// Mamela may adopt where they are, without widening the boundary to their neighbours.

// GuardSettings are the operator's settings ([provider.proxmox.guard] in mamela.providers.toml):
// the lab boundary and the safety thresholds. They are not secret.
type GuardSettings struct {
	Node         string `json:"node"`
	Pool         string `json:"pool"`
	VMIDMin      int64  `json:"vmid_min"`
	VMIDMax      int64  `json:"vmid_max"`
	Tag          string `json:"tag"`
	DiskStorage  string `json:"disk_storage"`
	ImageStorage string `json:"image_storage"`
	Bridge       string `json:"bridge"`
	// OperatingToken is the token ID of the provider's API (token_id of [provider.proxmox]); the
	// guard reads its permissions with the audit token.
	OperatingToken string `json:"operating_token"`
	MaxVMs         int64  `json:"max_vms"`
	// MinHostMemAvailableMiB is the memory that must remain available on the node after the
	// changes; MaxStorageUsedPercent the most the disk storage may be used after them.
	MinHostMemAvailableMiB int64 `json:"min_host_mem_available_mib"`
	MaxStorageUsedPercent  int64 `json:"max_storage_used_percent"`
	// Named are existing guests the operator adds to the boundary one by one
	// ([[provider.proxmox.guard.named]]): outside the pool and the VMID range, each is inside only
	// where it is named (its node and type), and the operating token reaches only its /vms/<vmid>.
	Named []NamedGuest `json:"named,omitempty"`
}

// NamedGuest is one existing guest named into the boundary: a guest a person made, outside the pool
// and the VMID range, that Mamela may adopt where it is. It is never moved into the pool.
type NamedGuest struct {
	Node string `json:"node"`
	VMID int64  `json:"vmid"`
	Type string `json:"type"` // lxc; qemu is refused (a VM cannot be adopted by its VMID yet)
	// Access is what the operating token may do to it, a person's decision made in two steps:
	// AccessObserve (VM.Audit: Mamela sees it, plans and previews, and changes nothing), then
	// AccessChange (also what adopting and changing it need). The bootstrap script grants exactly
	// that on /vms/<vmid>, and the guard holds the token to it.
	Access string `json:"access"`
}

// The steps of a named guest's access.
const (
	AccessObserve = "observe"
	AccessChange  = "change"
)

// path is the ACL path of a named guest: the only place the operating token holds anything for it.
func (n NamedGuest) path() string { return "/vms/" + strconv.FormatInt(n.VMID, 10) }

// ParseGuardSettings reads the settings strictly and checks them.
func ParseGuardSettings(b []byte) (GuardSettings, error) {
	var s GuardSettings
	if len(b) == 0 {
		return s, errors.New("the guard has no settings: add [provider.proxmox.guard] to mamela.providers.toml")
	}
	if err := json.Unmarshal(b, &s, json.RejectUnknownMembers(true)); err != nil {
		return s, fmt.Errorf("[provider.proxmox.guard]: %w", err)
	}
	switch {
	case s.Node == "" || s.Pool == "" || s.Tag == "" || s.DiskStorage == "" || s.ImageStorage == "" || s.Bridge == "":
		return s, errors.New("[provider.proxmox.guard]: node, pool, tag, disk_storage, image_storage and bridge are required")
	case !strings.Contains(s.OperatingToken, "!"):
		return s, errors.New("[provider.proxmox.guard]: operating_token must be the operating token's ID (user@realm!name)")
	case s.VMIDMin < 100 || s.VMIDMax < s.VMIDMin || s.VMIDMax > 999999999:
		return s, fmt.Errorf("[provider.proxmox.guard]: VMID range %d..%d is not a Proxmox VMID range", s.VMIDMin, s.VMIDMax)
	case s.MaxVMs < 1 || s.MinHostMemAvailableMiB < 0 || s.MaxStorageUsedPercent < 1 || s.MaxStorageUsedPercent > 100:
		return s, errors.New("[provider.proxmox.guard]: max_vms (1 or more), min_host_mem_available_mib and max_storage_used_percent (1 to 100) are required")
	}
	seen := map[int64]bool{}
	for _, n := range s.Named {
		where := fmt.Sprintf("[[provider.proxmox.guard.named]] vmid %d", n.VMID)
		switch {
		case n.Node == "":
			return s, fmt.Errorf("%s: node is required (the node the guest is on)", where)
		case n.VMID < 100 || n.VMID > 999999999:
			return s, fmt.Errorf("%s: not a Proxmox VMID", where)
		case seen[n.VMID]:
			return s, fmt.Errorf("%s: named twice; name each guest once", where)
		case s.inRange(n.VMID):
			return s, fmt.Errorf("%s: it is in the VMID range %d..%d, which is inside the boundary by its pool: put the guest in pool %s "+
				"instead of naming it (name only guests outside the range)", where, s.VMIDMin, s.VMIDMax, s.Pool)
		case n.Type == "qemu":
			return s, fmt.Errorf("%s: naming a VM (qemu) is not supported yet: a ProxmoxVM is adopted by its VMID only when it was imported "+
				"from Terraform or made by Mamela's built-in provider, inside the pool and the range; remove the entry (only containers, "+
				"type = \"lxc\", can be named)", where)
		case n.Type != "lxc":
			return s, fmt.Errorf("%s: type %q is not lxc (a container; qemu is not supported yet)", where, n.Type)
		case n.Access != AccessObserve && n.Access != AccessChange:
			return s, fmt.Errorf("%s: access %q is not %q (VM.Audit only: Mamela sees it and changes nothing) or %q (also what adopting "+
				"and changing it need); a person decides, and the bootstrap script grants it", where, n.Access, AccessObserve, AccessChange)
		}
		seen[n.VMID] = true
	}
	return s, nil
}

func (s GuardSettings) inRange(vmid int64) bool { return vmid >= s.VMIDMin && vmid <= s.VMIDMax }

// named is the guest named with a VMID, if any.
func (s GuardSettings) named(vmid int64) (NamedGuest, bool) {
	i := slices.IndexFunc(s.Named, func(n NamedGuest) bool { return n.VMID == vmid })
	if i < 0 {
		return NamedGuest{}, false
	}
	return s.Named[i], true
}

// isNamed says whether a guest is a named one where it is named: its VMID, node and type, and out of
// the pool (a pool's grants would reach it: PVE-0302).
func (s GuardSettings) isNamed(g Guest) bool {
	n, ok := s.named(g.VMID)
	return ok && g.Node == n.Node && g.Type == n.Type && g.Pool != s.Pool
}

// IsLab reports whether a guest is inside the boundary: in the pool and in the VMID range, or a
// named guest where it is named. The tag is not required: a VM is tagged at the end of its create,
// and a half-made one is still inside; a named guest is tagged when it is adopted.
func (s GuardSettings) IsLab(g Guest) bool {
	return g.Pool == s.Pool && s.inRange(g.VMID) || s.isNamed(g)
}

// namedNodes are the nodes, other than the settings' node, that named guests are on (sorted).
func (s GuardSettings) namedNodes() []string {
	var out []string
	for _, n := range s.Named {
		if n.Node != s.Node && !slices.Contains(out, n.Node) {
			out = append(out, n.Node)
		}
	}
	slices.Sort(out)
	return out
}

// Guest is a VM or container as the audit token sees it.
type Guest struct {
	VMID         int64  `json:"vmid"`
	Type         string `json:"type"` // qemu or lxc
	Node         string `json:"node"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	Pool         string `json:"pool,omitempty"`
	Uptime       int64  `json:"uptime,omitempty"`
	ConfigDigest string `json:"configDigest,omitempty"` // empty when its node is offline
	// What a guest's configuration gives it, to count what a start or a resize adds (0 when
	// unknown): a VM's memory and disk (scsi0), a container's memory and root filesystem.
	MemoryMiB int64  `json:"memoryMiB,omitempty"`
	DiskGiB   int64  `json:"diskGiB,omitempty"`
	Network   string `json:"network,omitempty"` // network recorded in the provider ownership marker
	// DiskStorage is the storage of a container's root filesystem.
	DiskStorage string `json:"diskStorage,omitempty"`
}

// Facts are what the guard decides from. Gathering is apart from checking, so that the checks are
// pure.
type Facts struct {
	Quorate                 bool
	NodesTotal, NodesOnline int
	NodeOnline              bool // the settings' node
	Guests                  []Guest
	MemAvailable            int64 // bytes, on the node
	DiskTotal, DiskUsed     int64 // bytes, of the disk storage
	ImportStorages          []string
	// Permissions are the operating token's: path -> privilege -> 1 when held.
	Permissions map[string]map[string]int
	// NamedNodes are the room on each other node a named guest is on (GuardSettings.namedNodes):
	// a change of a named guest there needs its room there.
	NamedNodes map[string]Room
}

// Room is what a node has left: whether it is online, its memory available and its disk storage's
// size and use (bytes; DiskTotal is 0 when the storage is unknown on it).
type Room struct {
	Online                            bool
	MemAvailable, DiskTotal, DiskUsed int64
}

// Guard calls.
var (
	clusterStatus = protocol.Call{Method: "GET", Path: "/cluster/status"}
	nodeStatus    = protocol.Call{Method: "GET", Path: "/nodes/{node}/status"}
	storageStatus = protocol.Call{Method: "GET", Path: "/nodes/{node}/storage/{storage}/status"}
	storages      = protocol.Call{Method: "GET", Path: "/nodes/{node}/storage", Query: []string{"content"}}
	permissions   = protocol.Call{Method: "GET", Path: "/access/permissions", Query: []string{"userid", "path"}}
)

// GuardCalls are the reads the guard makes.
var GuardCalls = []protocol.Call{clusterStatus, listVMs, getConfig, getLXCConfig, nodeStatus, storageStatus, storages, permissions}

// Gather reads the facts through the host (the audit token).
func Gather(c *kit.Client, s GuardSettings) (*Facts, error) {
	f := &Facts{}
	var status []struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Quorate int    `json:"quorate"`
		Nodes   int    `json:"nodes"`
		Online  int    `json:"online"`
	}
	if err := get(c, clusterStatus, nil, nil, &status); err != nil {
		return nil, err
	}
	online := map[string]bool{}
	for _, e := range status {
		switch e.Type {
		case "cluster":
			f.Quorate, f.NodesTotal = e.Quorate == 1, e.Nodes
		case "node":
			if e.Online == 1 {
				f.NodesOnline++
				online[e.Name] = true
			}
		}
	}
	if f.NodesTotal == 0 { // one node, no cluster
		f.NodesTotal, f.Quorate = f.NodesOnline, true
	}
	f.NodeOnline = online[s.Node]

	var rs []struct {
		VMID     int64  `json:"vmid"`
		Type     string `json:"type"`
		Node     string `json:"node"`
		Name     string `json:"name"`
		Status   string `json:"status"`
		Pool     string `json:"pool"`
		Uptime   int64  `json:"uptime"`
		Template int    `json:"template"`
	}
	if err := get(c, listVMs, nil, map[string][]string{"type": {"vm"}}, &rs); err != nil {
		return nil, err
	}
	for _, r := range rs {
		if r.Type != "qemu" && r.Type != "lxc" {
			continue
		}
		g := Guest{VMID: r.VMID, Type: r.Type, Node: r.Node, Name: r.Name, Status: r.Status, Pool: r.Pool, Uptime: r.Uptime}
		switch {
		case !online[g.Node]:
		case g.Type == "lxc":
			var cfg ctConfig
			if err := get(c, getLXCConfig, ids(g.Node, g.VMID), nil, &cfg); err != nil {
				return nil, err
			}
			g.ConfigDigest, g.MemoryMiB, g.DiskGiB, g.DiskStorage = cfg.Digest, cfg.MemoryMiB, cfg.RootfsGiB, diskStorage(cfg.Rootfs)
		default:
			var cfg vmConfig
			if err := get(c, getConfig, ids(g.Node, g.VMID), nil, &cfg); err != nil {
				return nil, err
			}
			g.ConfigDigest, g.MemoryMiB, g.DiskGiB = cfg.Digest, cfg.Memory, cfg.DiskGiB
			if m, ok := markerOf(Kind, cfg.Description); ok {
				g.Network = m.Network
			} else if m, ok := parseOldMarker(cfg.Description); ok {
				g.Network = m.Network
			}
		}
		f.Guests = append(f.Guests, g)
	}
	slices.SortFunc(f.Guests, func(a, b Guest) int { return int(a.VMID - b.VMID) })

	var err error
	if f.NodeOnline {
		if f.MemAvailable, err = memAvailable(c, s.Node); err != nil {
			return nil, err
		}
		if f.DiskTotal, f.DiskUsed, err = storageUse(c, s.Node, s.DiskStorage); err != nil {
			return nil, err
		}
		var st []struct {
			Storage string `json:"storage"`
		}
		if err := get(c, storages, map[string]string{"node": s.Node}, map[string][]string{"content": {"import"}}, &st); err != nil {
			return nil, err
		}
		for _, x := range st {
			f.ImportStorages = append(f.ImportStorages, x.Storage)
		}
	}
	for _, node := range s.namedNodes() {
		r := Room{Online: online[node]}
		if r.Online {
			if r.MemAvailable, err = memAvailable(c, node); err != nil {
				return nil, err
			}
			// The disk storage may not be on that node: then its size is unknown, and only a change
			// that grows a disk there is blocked (PVE-0202).
			r.DiskTotal, r.DiskUsed, _ = storageUse(c, node, s.DiskStorage)
		}
		if f.NamedNodes == nil {
			f.NamedNodes = map[string]Room{}
		}
		f.NamedNodes[node] = r
	}
	if err := get(c, permissions, nil, map[string][]string{"userid": {s.OperatingToken}}, &f.Permissions); err != nil {
		return nil, fmt.Errorf("the operating token's permissions: %w", err)
	}
	return f, nil
}

// memAvailable is the memory available on a node (bytes).
func memAvailable(c *kit.Client, node string) (int64, error) {
	var ns struct {
		Memory struct {
			Free      int64 `json:"free"`
			Available int64 `json:"available"`
		} `json:"memory"`
	}
	if err := get(c, nodeStatus, map[string]string{"node": node}, nil, &ns); err != nil {
		return 0, err
	}
	if ns.Memory.Available == 0 { // older Proxmox VE reports only free memory
		return ns.Memory.Free, nil
	}
	return ns.Memory.Available, nil
}

// storageUse is a storage's size and use on a node (bytes).
func storageUse(c *kit.Client, node, storage string) (total, used int64, err error) {
	var ss struct {
		Total int64 `json:"total"`
		Used  int64 `json:"used"`
	}
	if err := get(c, storageStatus, map[string]string{"node": node, "storage": storage}, nil, &ss); err != nil {
		return 0, 0, err
	}
	return ss.Total, ss.Used, nil
}

// Need is what the changes of one apply need of the node and the storage, guest by guest (a VM or
// a container):
//
//   - a VM created and started: its memory and disk, once;
//   - a stopped guest started: its memory (its new memory if it is resized too);
//   - a running guest resized: what its memory grows by;
//   - a disk grown: what it grows by (a container's root filesystem only when it is on the disk
//     storage: one elsewhere is outside the boundary, PVE-0304).
//
// A guest the guard cannot read is counted at the size the change asks for (an upper bound).
type Need struct {
	Creates   int
	MemoryMiB int64
	DiskGiB   int64
}

// NeedOf sums the changes, with what the guests' configurations say now; diskStorage is the
// storage whose room PVE-0202 checks.
func NeedOf(changes []protocol.Change, guests []Guest, diskStorage string) Need {
	type vm struct {
		created, started bool
		memory, disk     int64 // asked by the changes (0: not changed)
	}
	byVMID := map[string]*vm{}
	var order []string
	for _, c := range changes {
		id := c.IDs["vmid"]
		v := byVMID[id]
		if v == nil {
			v = &vm{}
			byVMID[id] = v
			order = append(order, id)
		}
		v.created = v.created || c.Op == OpCreate || c.Op == OpCreateTemplate
		for _, r := range c.Requests {
			switch r.Path {
			case startVM.Path, startLXC.Path:
				v.started = true
			case createVM.Path, setConfig.Path, setLXCConfig.Path:
				if x := r.Params["memory"]; len(x) == 1 {
					v.memory, _ = strconv.ParseInt(x[0], 10, 64)
				}
			case resizeDisk.Path, resizeLXC.Path:
				if x := r.Params["size"]; len(x) == 1 {
					v.disk, _ = strconv.ParseInt(strings.TrimSuffix(x[0], "G"), 10, 64)
				}
			}
		}
	}
	now := map[string]Guest{}
	for _, g := range guests {
		if g.Type == "qemu" || g.Type == "lxc" {
			now[strconv.FormatInt(g.VMID, 10)] = g
		}
	}
	var n Need
	for _, id := range order {
		v := byVMID[id]
		g, known := now[id]
		switch {
		case v.created:
			n.Creates++
			n.MemoryMiB += v.memory
			n.DiskGiB += v.disk
			continue
		case !known:
			n.MemoryMiB += v.memory
			n.DiskGiB += v.disk
			continue
		}
		running := g.Status == "running"
		memory := g.MemoryMiB
		if v.memory > 0 {
			memory = v.memory
		}
		switch {
		case !running && v.started:
			n.MemoryMiB += memory
		case running && v.memory > g.MemoryMiB:
			n.MemoryMiB += v.memory - g.MemoryMiB
		}
		if v.disk > g.DiskGiB && (g.Type != "lxc" || g.DiskStorage == diskStorage) {
			n.DiskGiB += v.disk - g.DiskGiB
		}
	}
	return n
}

func finding(code, sev, format string, args ...any) protocol.Finding {
	return protocol.Finding{Code: code, Severity: sev, Message: fmt.Sprintf(format, args...)}
}

// Check is the guard before an apply.
func Check(s GuardSettings, m protocol.Manifest, f *Facts, changes []protocol.Change) []protocol.Finding {
	var out []protocol.Finding
	add := func(code, sev, format string, args ...any) { out = append(out, finding(code, sev, format, args...)) }
	ok, warn, block := protocol.SeverityOK, protocol.SeverityWarn, protocol.SeverityBlock

	// The cluster.
	if f.Quorate {
		add("PVE-0001", ok, "the cluster is quorate")
	} else {
		add("PVE-0001", block, "the cluster is not quorate: nothing may be changed until it is")
	}
	if need := f.NodesTotal/2 + 1; f.NodesTotal > 1 && f.NodesOnline <= need {
		add("PVE-0002", warn, "%d of %d nodes are online and %d are needed for quorum: losing one more node loses quorum", f.NodesOnline, f.NodesTotal, need)
	} else {
		add("PVE-0002", ok, "%d of %d nodes online", f.NodesOnline, f.NodesTotal)
	}
	if f.NodeOnline {
		add("PVE-0003", ok, "node %s is online", s.Node)
	} else {
		add("PVE-0003", block, "node %s is not online", s.Node)
	}

	// The operating token's reach.
	grants := Grants(m, s)
	if excess := Excess(grants, s, f.Permissions); len(excess) > 0 {
		add("PVE-0101", block, "the operating token %s has permissions this provider never needs: %s; run the bootstrap script again "+
			"(mamela provider script <pome-dir> proxmox bootstrap; it removes them)", s.OperatingToken, strings.Join(excess, ", "))
	} else {
		add("PVE-0101", ok, "the operating token's permissions are within the boundary")
	}
	if missing := Missing(grants, f.Permissions); len(missing) > 0 {
		add("PVE-0102", block, "the operating token %s lacks %s; review and run the bootstrap script again (mamela provider script <pome-dir> proxmox bootstrap)",
			s.OperatingToken, strings.Join(missing, ", "))
	} else {
		add("PVE-0102", ok, "the operating token has every permission this provider needs")
	}

	// Room on the node and the storage: the changes of named guests on another node need room there
	// (byNode), every other change on the settings' node.
	byNode := s.byNode(changes)
	n := NeedOf(byNode[s.Node], f.Guests, s.DiskStorage)
	left := (f.MemAvailable >> 20) - n.MemoryMiB
	if left < s.MinHostMemAvailableMiB {
		add("PVE-0201", block, "only %d MiB would remain available on %s after the changes' %d MiB (minimum %d MiB): free memory or lower min_host_mem_available_mib",
			left, s.Node, n.MemoryMiB, s.MinHostMemAvailableMiB)
	} else {
		add("PVE-0201", ok, "%d MiB would remain available on %s after the changes' %d MiB", left, s.Node, n.MemoryMiB)
	}
	if f.DiskTotal <= 0 {
		add("PVE-0202", block, "the size of storage %s is unknown", s.DiskStorage)
	} else {
		after := float64(f.DiskUsed+n.DiskGiB<<30) / float64(f.DiskTotal) * 100
		if after > float64(s.MaxStorageUsedPercent) {
			add("PVE-0202", block, "storage %s would be %.1f%% used after the changes' %d GiB (maximum %d%%)", s.DiskStorage, after, n.DiskGiB, s.MaxStorageUsedPercent)
		} else {
			add("PVE-0202", ok, "storage %s would be %.1f%% used after the changes' %d GiB", s.DiskStorage, after, n.DiskGiB)
		}
	}
	if slices.Contains(f.ImportStorages, s.ImageStorage) {
		add("PVE-0203", ok, "storage %s accepts cloud images to import", s.ImageStorage)
	} else {
		add("PVE-0203", block, "storage %s does not accept importable images (content type \"import\"); run the bootstrap script (it adds it)", s.ImageStorage)
	}
	for _, node := range slices.Sorted(maps.Keys(byNode)) {
		if node != s.Node {
			out = append(out, checkNamedNode(s, node, f.NamedNodes[node], NeedOf(byNode[node], f.Guests, s.DiskStorage))...)
		}
	}

	// The boundary.
	lab := 0
	for _, g := range f.Guests {
		_, named := s.named(g.VMID)
		switch {
		case s.inRange(g.VMID) && !s.IsLab(g):
			add("PVE-0301", block, "guest %d (%s) is in the VMID range %d..%d but not in pool %s (pool %q): someone else's guest, or a create still running",
				g.VMID, g.Name, s.VMIDMin, s.VMIDMax, s.Pool, g.Pool)
		case g.Pool == s.Pool && !s.IsLab(g) && named:
			add("PVE-0302", block, "guest %d (%s) is named in [provider.proxmox.guard] and is in pool %s, outside the VMID range %d..%d: the pool's grants "+
				"reach it beyond what its name gives; a named guest stays where it is, out of the pool: take it out of pool %s", g.VMID, g.Name, s.Pool,
				s.VMIDMin, s.VMIDMax, s.Pool)
		case g.Pool == s.Pool && !s.IsLab(g):
			add("PVE-0302", block, "guest %d (%s) is in pool %s but outside the VMID range %d..%d", g.VMID, g.Name, s.Pool, s.VMIDMin, s.VMIDMax)
		case s.IsLab(g):
			lab++
		}
	}
	creates := NeedOf(changes, f.Guests, s.DiskStorage).Creates
	if int64(lab+creates) > s.MaxVMs {
		add("PVE-0303", block, "the boundary has %d VM(s); %d more would exceed max_vms %d", lab, creates, s.MaxVMs)
	} else {
		add("PVE-0303", ok, "the boundary has %d VM(s) and the changes create %d; max_vms is %d", lab, creates, s.MaxVMs)
	}
	outside := 0
	for _, c := range changes {
		for _, why := range Outside(s, c, f.Guests) {
			add("PVE-0304", block, "%s %s: %s; the declaration (Proxmox.Site) and [provider.proxmox.guard] must agree", c.Op, c.Resource, why)
			outside++
		}
	}
	if outside == 0 {
		named := ""
		if len(s.Named) > 0 {
			named = fmt.Sprintf(", or on the %d named guest(s) where they are named", len(s.Named))
		}
		add("PVE-0304", ok, "every change stays on node %s, in pool %s and VMIDs %d..%d%s", s.Node, s.Pool, s.VMIDMin, s.VMIDMax, named)
	}
	out = append(out, checkNamed(s, f.Guests)...)
	out = append(out, checkStaticAddresses(s, f.Guests, changes)...)
	return out
}

// byNode groups changes by the node whose room they need: a change of a named guest on another
// node needs that node's; every other change is counted on the settings' node (one on another node
// is outside the boundary anyway: PVE-0304).
func (s GuardSettings) byNode(changes []protocol.Change) map[string][]protocol.Change {
	out := map[string][]protocol.Change{s.Node: nil}
	for _, c := range changes {
		node := s.Node
		if vmid, err := strconv.ParseInt(c.IDs["vmid"], 10, 64); err == nil {
			if n, ok := s.named(vmid); ok && n.Node == c.IDs["node"] {
				node = n.Node
			}
		}
		out[node] = append(out[node], c)
	}
	return out
}

// checkNamedNode checks the room the changes of named guests on another node need there: that node
// online, its memory and its disk storage (when a disk grows there), with the same thresholds.
func checkNamedNode(s GuardSettings, node string, r Room, n Need) []protocol.Finding {
	block := protocol.SeverityBlock
	if !r.Online {
		return []protocol.Finding{finding("PVE-0003", block, "node %s (of named guests) is not online: their changes cannot be made", node)}
	}
	var out []protocol.Finding
	if left := (r.MemAvailable >> 20) - n.MemoryMiB; left < s.MinHostMemAvailableMiB {
		out = append(out, finding("PVE-0201", block, "only %d MiB would remain available on %s after the named guests' changes' %d MiB (minimum %d MiB): "+
			"free memory or lower min_host_mem_available_mib", left, node, n.MemoryMiB, s.MinHostMemAvailableMiB))
	} else {
		out = append(out, finding("PVE-0201", protocol.SeverityOK, "%d MiB would remain available on %s after the named guests' changes' %d MiB",
			left, node, n.MemoryMiB))
	}
	switch {
	case n.DiskGiB == 0:
	case r.DiskTotal <= 0:
		out = append(out, finding("PVE-0202", block, "the size of storage %s on %s is unknown (is it on that node?): a disk cannot grow there", s.DiskStorage, node))
	default:
		after := float64(r.DiskUsed+n.DiskGiB<<30) / float64(r.DiskTotal) * 100
		if after > float64(s.MaxStorageUsedPercent) {
			out = append(out, finding("PVE-0202", block, "storage %s on %s would be %.1f%% used after the named guests' changes' %d GiB (maximum %d%%)",
				s.DiskStorage, node, after, n.DiskGiB, s.MaxStorageUsedPercent))
		} else {
			out = append(out, finding("PVE-0202", protocol.SeverityOK, "storage %s on %s would be %.1f%% used after the named guests' changes' %d GiB",
				s.DiskStorage, node, after, n.DiskGiB))
		}
	}
	return out
}

// checkNamed checks that each named guest is where it is named: a guest with its VMID, of its type,
// on its node. One that is not (gone, migrated, another guest at its VMID) is outside the boundary
// (read-only, Foreign), and the operating token still holds what its name gives on its VMID: with
// access "change", that blocks every apply until a person fixes the settings and the grants.
func checkNamed(s GuardSettings, guests []Guest) []protocol.Finding {
	if len(s.Named) == 0 {
		return nil
	}
	var out []protocol.Finding
	for _, n := range s.Named {
		var why string
		switch i := slices.IndexFunc(guests, func(g Guest) bool { return g.VMID == n.VMID }); {
		case i < 0:
			why = "no guest has that VMID now"
		case guests[i].Type != n.Type:
			why = fmt.Sprintf("guest %d (%s) is a %s, not a %s: another guest has its VMID now", n.VMID, guests[i].Name, typeName(guests[i].Type), typeName(n.Type))
		case guests[i].Node != n.Node:
			why = fmt.Sprintf("guest %d (%s) is on node %s, not %s: it was migrated", n.VMID, guests[i].Name, guests[i].Node, n.Node)
		default:
			continue // where it is named (in Mamela's pool: PVE-0302)
		}
		sev, reach := protocol.SeverityWarn, "read"
		if n.Access == AccessChange {
			sev, reach = protocol.SeverityBlock, "change"
		}
		out = append(out, finding("PVE-0305", sev, "guest %d is named in [provider.proxmox.guard] as a %s on %s, but %s. It is outside the boundary: "+
			"nothing of it is changed (read-only), yet the operating token may %s whatever has VMID %d (access %s). Name it where it is (node, type), "+
			"or remove its entry and run the bootstrap script again (it takes the grant on /vms/%d away)",
			n.VMID, typeName(n.Type), n.Node, why, reach, n.VMID, n.Access, n.VMID))
	}
	if len(out) == 0 {
		out = append(out, finding("PVE-0305", protocol.SeverityOK, "the %d named guest(s) are where they are named", len(s.Named)))
	}
	return out
}

func typeName(t string) string {
	switch t {
	case "lxc":
		return "container (lxc)"
	case "qemu":
		return "VM (qemu)"
	}
	return t
}

// Outside lists how a change leaves the boundary the operator set: another node, a VMID out of the
// range (and not a named guest's), another pool, storage, bridge or tag. A container's change is
// checked against the guest it names (guests, as the audit token sees them): a container in the
// pool, or the named container where it is named, whose root filesystem is on the disk storage,
// keeping the tag. A named guest's change must be a container's, on its node, and its access
// "change".
func Outside(s GuardSettings, c protocol.Change, guests []Guest) []string {
	var out []string
	vmid, err := strconv.ParseInt(c.IDs["vmid"], 10, 64)
	if n, named := s.named(vmid); err == nil && named {
		out = append(out, outsideNamed(n, c)...)
	} else {
		if c.IDs["node"] != s.Node {
			out = append(out, fmt.Sprintf("node %q is not %s", c.IDs["node"], s.Node))
		}
		if err != nil || !s.inRange(vmid) {
			out = append(out, fmt.Sprintf("VMID %q is not in %d..%d, and no guest is named with it in [provider.proxmox.guard]", c.IDs["vmid"], s.VMIDMin, s.VMIDMax))
		}
	}
	if st, ok := c.IDs["storage"]; ok && st != s.ImageStorage {
		out = append(out, fmt.Sprintf("storage %q is not %s", st, s.ImageStorage))
	}
	param := func(r protocol.Approved, k string) (string, bool) {
		v := r.Params[k]
		if len(v) != 1 {
			return "", false
		}
		return v[0], true
	}
	for _, r := range c.Requests {
		switch r.Path {
		case createVM.Path:
			if v, _ := param(r, "pool"); v != s.Pool {
				out = append(out, fmt.Sprintf("pool %q is not %s", v, s.Pool))
			}
			if v, _ := param(r, "scsi0"); !strings.HasPrefix(v, s.DiskStorage+":") {
				out = append(out, fmt.Sprintf("disk %q is not on %s", v, s.DiskStorage))
			}
			if v, _ := param(r, "net0"); !slices.Contains(strings.Split(v, ","), "bridge="+s.Bridge) {
				out = append(out, fmt.Sprintf("network %q is not on bridge %s", v, s.Bridge))
			}
			if v, ok := param(r, "ide2"); ok && !strings.HasPrefix(v, s.ImageStorage+":") {
				out = append(out, fmt.Sprintf("seed %q is not on %s", v, s.ImageStorage))
			}
		case setConfig.Path:
			if v, ok := param(r, "tags"); ok && v != s.Tag {
				out = append(out, fmt.Sprintf("tag %q is not %s", v, s.Tag))
			}
			if v, ok := param(r, "ide2"); ok && !strings.HasPrefix(v, s.ImageStorage+":") {
				out = append(out, fmt.Sprintf("seed %q is not on %s", v, s.ImageStorage))
			}
		case cloneVM.Path:
			if v, _ := param(r, "pool"); v != s.Pool {
				out = append(out, fmt.Sprintf("pool %q is not %s", v, s.Pool))
			}
			if v, _ := param(r, "newid"); v != c.IDs["vmid"] {
				out = append(out, fmt.Sprintf("the clone's VMID %q is not the change's %s", v, c.IDs["vmid"]))
			}
			if v, _ := param(r, "full"); v != "0" {
				out = append(out, "a clone is linked (full=0)")
			}
			if t, err := strconv.ParseInt(c.IDs["template"], 10, 64); err != nil || !s.inRange(t) {
				out = append(out, fmt.Sprintf("template %q is not in %d..%d", c.IDs["template"], s.VMIDMin, s.VMIDMax))
			}
		}
	}
	if c.Kind == KindLXC {
		out = append(out, outsideContainer(s, c, guests)...)
	}
	return out
}

// outsideNamed lists how a change of a named guest leaves the boundary: it is not a container's
// change (only containers can be named), it is one that never reaches a named guest (deleting it),
// it is on another node than the guest is named on, or the guest's access is only "observe".
func outsideNamed(n NamedGuest, c protocol.Change) []string {
	var out []string
	if slices.Contains(neverNamed, c.Op) {
		out = append(out, fmt.Sprintf("guest %d is a named guest (%s on %s): Mamela never deletes a named guest (a production guest the operator "+
			"named into the boundary); a person deletes it by hand (pct destroy %d on %s), after removing its declaration and its "+
			"[[provider.proxmox.guard.named]] entry", n.VMID, n.Type, n.Node, n.VMID, n.Node))
	}
	if c.Kind != KindLXC || n.Type != "lxc" {
		out = append(out, fmt.Sprintf("VMID %d is a named guest (%s on %s): only a container's (ProxmoxLXC) changes may reach it", n.VMID, n.Type, n.Node))
	}
	if c.IDs["node"] != n.Node {
		out = append(out, fmt.Sprintf("node %q is not %s, where guest %d is named", c.IDs["node"], n.Node, n.VMID))
	}
	if n.Access != AccessChange {
		out = append(out, fmt.Sprintf("guest %d is named with access %q: Mamela may only read it; when a person decides Mamela may change it, "+
			"set access = %q in its [[provider.proxmox.guard.named]] entry and run the bootstrap script again", n.VMID, n.Access, AccessChange))
	}
	return out
}

// outsideContainer lists how a container's change leaves the boundary: the guest it names is not a
// container in the pool on the node (or, named, out of the pool where it is named), its root
// filesystem is not on the disk storage, or the tags it sets lack the tag.
func outsideContainer(s GuardSettings, c protocol.Change, guests []Guest) []string {
	var out []string
	i := slices.IndexFunc(guests, func(g Guest) bool { return strconv.FormatInt(g.VMID, 10) == c.IDs["vmid"] })
	var g Guest
	switch {
	case i < 0:
		out = append(out, fmt.Sprintf("no guest has VMID %q", c.IDs["vmid"]))
	case guests[i].Type != "lxc":
		g = guests[i]
		out = append(out, fmt.Sprintf("guest %d (%s) is a VM (%s), not a container", g.VMID, g.Name, g.Type))
	default:
		g = guests[i]
		_, named := s.named(g.VMID)
		switch {
		case named && g.Pool == s.Pool:
			out = append(out, fmt.Sprintf("named container %d (%s) is in pool %s, outside the VMIDs %d..%d: a named guest stays out of the pool",
				g.VMID, g.Name, s.Pool, s.VMIDMin, s.VMIDMax))
		case !named && g.Pool != s.Pool:
			out = append(out, fmt.Sprintf("container %d (%s) is in pool %q, not %s", g.VMID, g.Name, g.Pool, s.Pool))
		}
		if g.Node != c.IDs["node"] {
			out = append(out, fmt.Sprintf("container %d (%s) is on node %s, not %s", g.VMID, g.Name, g.Node, c.IDs["node"]))
		}
	}
	for _, r := range c.Requests {
		switch r.Path {
		case resizeLXC.Path:
			if g.Type == "lxc" && g.DiskStorage != s.DiskStorage {
				out = append(out, fmt.Sprintf("the root filesystem of container %d is on storage %q, not %s", g.VMID, g.DiskStorage, s.DiskStorage))
			}
		case setLXCConfig.Path:
			if v, ok := r.Params["tags"]; ok && (len(v) != 1 || !slices.Contains(splitTags(v[0]), s.Tag)) {
				out = append(out, fmt.Sprintf("tags %q do not keep %s", strings.Join(v, ","), s.Tag))
			}
		}
	}
	return out
}

// Baseline is every guest outside the boundary, as it was before an apply.
type Baseline struct {
	Schema string  `json:"schema"`
	Guests []Guest `json:"guests"`
}

// BaselineSchema names the baseline's format.
const BaselineSchema = "mamela-proxmox-baseline/v1"

// NewBaseline records the guests outside the boundary.
func NewBaseline(s GuardSettings, f *Facts) Baseline {
	b := Baseline{Schema: BaselineSchema, Guests: []Guest{}}
	for _, g := range f.Guests {
		if !s.IsLab(g) {
			b.Guests = append(b.Guests, g)
		}
	}
	return b
}

// Compare is the guard after an apply: every guest outside the boundary is as it was.
func Compare(s GuardSettings, before Baseline, f *Facts) []protocol.Finding {
	var out []protocol.Finding
	add := func(code, sev, format string, args ...any) { out = append(out, finding(code, sev, format, args...)) }
	now := map[int64]Guest{}
	for _, g := range f.Guests {
		now[g.VMID] = g
	}
	changed := 0
	for _, b := range before.Guests {
		a, ok := now[b.VMID]
		switch {
		case !ok:
			add("PVE-0401", protocol.SeverityBlock, "guest %d (%s) outside the boundary has disappeared", b.VMID, b.Name)
		case a.Status != b.Status:
			add("PVE-0402", protocol.SeverityBlock, "guest %d (%s) outside the boundary changed status: %s -> %s", b.VMID, b.Name, b.Status, a.Status)
		case b.ConfigDigest != "" && a.ConfigDigest != "" && a.ConfigDigest != b.ConfigDigest:
			add("PVE-0403", protocol.SeverityBlock, "guest %d (%s) outside the boundary: its configuration changed", b.VMID, b.Name)
		case b.Status == "running" && a.Uptime < b.Uptime:
			add("PVE-0404", protocol.SeverityBlock, "guest %d (%s) outside the boundary restarted (uptime %ds -> %ds)", b.VMID, b.Name, b.Uptime, a.Uptime)
		default:
			continue
		}
		changed++
	}
	known := map[int64]bool{}
	for _, b := range before.Guests {
		known[b.VMID] = true
	}
	for _, g := range f.Guests {
		switch {
		case known[g.VMID] || s.IsLab(g):
		case s.inRange(g.VMID):
			add("PVE-0405", protocol.SeverityWarn, "guest %d (%s) is in the VMID range but not in pool %s: a create that has not finished, or someone else's guest", g.VMID, g.Name, s.Pool)
		default:
			add("PVE-0405", protocol.SeverityWarn, "a new guest %d (%s) appeared outside the boundary (not made by Mamela)", g.VMID, g.Name)
		}
	}
	if changed == 0 {
		add("PVE-0400", protocol.SeverityOK, "all %d guests outside the boundary are unchanged", len(before.Guests))
	}
	return out
}

// guard is the provider's guard (kit.Provider.GuardFunc), with the provider's manifest.
func guard(c *kit.Client, m protocol.Manifest, in protocol.GuardParams) (protocol.GuardResult, error) {
	s, err := ParseGuardSettings(in.Settings)
	if err != nil {
		return protocol.GuardResult{}, err
	}
	f, err := Gather(c, s)
	if err != nil {
		return protocol.GuardResult{}, err
	}
	switch in.Stage {
	case protocol.GuardBefore:
		b, _ := json.Marshal(NewBaseline(s, f), json.Deterministic(true))
		return protocol.GuardResult{Findings: Check(s, m, f, in.Changes), Baseline: b}, nil
	case protocol.GuardAfter:
		var b Baseline
		if err := json.Unmarshal(in.Baseline, &b, json.RejectUnknownMembers(true)); err != nil || b.Schema != BaselineSchema {
			return protocol.GuardResult{}, fmt.Errorf("the baseline is not a %s (%v): run the guard before the apply", BaselineSchema, err)
		}
		return protocol.GuardResult{Findings: Compare(s, b, f)}, nil
	}
	return protocol.GuardResult{}, fmt.Errorf("guard stage %q is not before or after", in.Stage)
}

// Grant is the privileges the operating token holds on one path, each with the operations that
// need it.
type Grant struct {
	Path    string
	Privs   []string
	Reasons map[string][]string // privilege -> operations
}

// Grants are every permission the operating token needs, from the manifest's needs (observing and
// every supported operation) with the settings filled in, and those of each named guest on its own
// /vms/<vmid> (NamedPrivileges). The pool's grant comes first.
func Grants(m protocol.Manifest, s GuardSettings) []Grant {
	refs := strings.NewReplacer("{pool}", s.Pool, "{diskStorage}", s.DiskStorage, "{imageStorage}", s.ImageStorage, "{bridge}", s.Bridge, "{node}", s.Node)
	by := map[string]*Grant{}
	add := func(why string, needs []protocol.Need) {
		for _, n := range needs {
			path := refs.Replace(n.Target.Ref)
			g := by[path]
			if g == nil {
				g = &Grant{Path: path, Reasons: map[string][]string{}}
				by[path] = g
			}
			if !slices.Contains(g.Privs, n.Capability) {
				g.Privs = append(g.Privs, n.Capability)
			}
			if !slices.Contains(g.Reasons[n.Capability], why) {
				g.Reasons[n.Capability] = append(g.Reasons[n.Capability], why)
			}
		}
	}
	add("observe", m.Observe)
	for _, k := range m.Kinds {
		for _, o := range k.Operations {
			if o.Support == protocol.Supported {
				add(string(o.Op), o.Needs)
			}
		}
	}
	for _, n := range s.Named {
		privs, reasons := NamedPrivileges(m, n)
		by[n.path()] = &Grant{Path: n.path(), Privs: privs, Reasons: reasons}
	}
	pool := "/pool/" + s.Pool
	paths := slices.SortedFunc(maps.Keys(by), func(a, b string) int {
		switch {
		case a == pool:
			return -1
		case b == pool:
			return 1
		}
		return strings.Compare(a, b)
	})
	var out []Grant
	for _, p := range paths {
		g := *by[p]
		slices.Sort(g.Privs)
		out = append(out, g)
	}
	return out
}

// neverNamed are the operations that never reach a named guest: Mamela never deletes one (a
// production guest the operator named; a person deletes it by hand). Its grant leaves them out, so
// that Proxmox refuses them too (VM.Allocate), and the guard blocks them (PVE-0304).
var neverNamed = []protocol.Op{OpDeleteLXC}

// NamedPrivileges are what the operating token holds on a named guest's /vms/<vmid>, each with the
// operations that need it: what a pool's grant gives its guests (the manifest's needs on the pool),
// narrowed to the guest's kind and its access. With access "observe", observing's VM.Audit only;
// with "change", also what every operation of its kind needs there (a container's: adopt-lxc,
// configure-lxc, grow-lxc-rootfs, start-lxc, shutdown-lxc, reboot-lxc, stop-lxc), but those that
// never reach a named guest (neverNamed: delete-lxc). Nothing of the pool itself (Pool.Audit), and
// no other path: the storage and the bridge are granted already.
func NamedPrivileges(m protocol.Manifest, n NamedGuest) (privs []string, reasons map[string][]string) {
	reasons = map[string][]string{}
	add := func(why string, needs []protocol.Need) {
		for _, x := range needs {
			if x.Target.Ref != AtPool || !strings.HasPrefix(x.Capability, "VM.") {
				continue
			}
			if !slices.Contains(privs, x.Capability) {
				privs = append(privs, x.Capability)
			}
			if !slices.Contains(reasons[x.Capability], why) {
				reasons[x.Capability] = append(reasons[x.Capability], why)
			}
		}
	}
	add("observe", m.Observe)
	if n.Access == AccessChange {
		kind := map[string]protocol.ResourceKind{"lxc": KindLXC}[n.Type]
		for _, k := range m.Kinds {
			if k.Kind != kind {
				continue
			}
			for _, o := range k.Operations {
				if o.Support == protocol.Supported && !slices.Contains(neverNamed, o.Op) {
					add(string(o.Op), o.Needs)
				}
			}
		}
	}
	slices.Sort(privs)
	return privs, reasons
}

// allowed is what the token may hold on a path: a grant's privileges, or for a VM in the range the
// pool's (a pool's ACL reaches its guests, /vms/<vmid>).
func allowed(grants []Grant, s GuardSettings, path string) ([]string, bool) {
	for _, g := range grants {
		if g.Path == path {
			return g.Privs, true
		}
	}
	if id, ok := strings.CutPrefix(path, "/vms/"); ok {
		if vmid, err := strconv.ParseInt(id, 10, 64); err == nil && strconv.FormatInt(vmid, 10) == id && s.inRange(vmid) {
			for _, g := range grants {
				if g.Path == "/pool/"+s.Pool {
					return g.Privs, true
				}
			}
		}
	}
	return nil, false
}

// Excess lists the (path, privilege) pairs the token holds and must not.
func Excess(grants []Grant, s GuardSettings, perms map[string]map[string]int) []string {
	var out []string
	for _, path := range slices.Sorted(maps.Keys(perms)) {
		privs, found := allowed(grants, s, path)
		for _, priv := range slices.Sorted(maps.Keys(perms[path])) {
			if perms[path][priv] != 0 && (!found || !slices.Contains(privs, priv)) {
				out = append(out, path+" "+priv)
			}
		}
	}
	return out
}

// Missing lists the granted privileges the token does not hold.
func Missing(grants []Grant, perms map[string]map[string]int) []string {
	var out []string
	for _, g := range grants {
		for _, priv := range g.Privs {
			if perms[g.Path][priv] == 0 {
				out = append(out, fmt.Sprintf("%s %s (for %s)", g.Path, priv, strings.Join(g.Reasons[priv], ", ")))
			}
		}
	}
	return out
}

// checkStaticAddresses compares the static addresses recorded for managed guests and every seed
// in the proposed changes. It does not claim to discover DHCP leases or unmanaged guest addresses.
func checkStaticAddresses(s GuardSettings, guests []Guest, changes []protocol.Change) []protocol.Finding {
	assigned := map[netip.Addr]int64{}
	for _, g := range guests {
		fields := strings.Fields(g.Network)
		if !s.IsLab(g) || len(fields) < 2 || fields[0] != "static" {
			continue
		}
		if p, err := netip.ParsePrefix(fields[1]); err == nil {
			assigned[p.Addr().Unmap()] = g.VMID
		}
	}
	var out []protocol.Finding
	for _, c := range changes {
		for _, a := range c.Artifacts {
			if a.Kind != protocol.ArtifactSeed || a.Network == nil || a.Network.Mode != "static" {
				continue
			}
			prefix, err := netip.ParsePrefix(a.Network.Address)
			id, idErr := strconv.ParseInt(c.IDs["vmid"], 10, 64)
			if err != nil || idErr != nil {
				out = append(out, finding("PVE-0306", protocol.SeverityBlock, "%s has an invalid static address or VMID", c.Resource))
				continue
			}
			addr := prefix.Addr().Unmap()
			if owner, used := assigned[addr]; used && owner != id {
				out = append(out, finding("PVE-0306", protocol.SeverityBlock, "%s requests static address %s already assigned to VM %d; choose another address", c.Resource, addr, owner))
			} else {
				assigned[addr] = id
			}
		}
	}
	return out
}
