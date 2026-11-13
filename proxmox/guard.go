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
// the boundary (no foreign guest in the pool or the VMID range, the number of VMs) and the changes
// themselves (each stays inside the boundary the operator set, whatever the declaration says), and
// it records every guest outside the boundary. After, it checks that those guests are unchanged.

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
}

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
	return s, nil
}

func (s GuardSettings) inRange(vmid int64) bool { return vmid >= s.VMIDMin && vmid <= s.VMIDMax }

// IsLab reports whether a guest is inside the boundary: in the pool and in the VMID range. The tag
// is not required: a VM is tagged at the end of its create, and a half-made one is still inside.
func (s GuardSettings) IsLab(g Guest) bool { return g.Pool == s.Pool && s.inRange(g.VMID) }

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
	// What a VM's configuration gives it, to count what a start or a resize adds (0 when unknown).
	MemoryMiB int64  `json:"memoryMiB,omitempty"`
	DiskGiB   int64  `json:"diskGiB,omitempty"`
	Network   string `json:"network,omitempty"` // network recorded in the provider ownership marker
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
}

// Guard calls.
var (
	clusterStatus = protocol.Call{Method: "GET", Path: "/cluster/status"}
	nodeStatus    = protocol.Call{Method: "GET", Path: "/nodes/{node}/status"}
	storageStatus = protocol.Call{Method: "GET", Path: "/nodes/{node}/storage/{storage}/status"}
	storages      = protocol.Call{Method: "GET", Path: "/nodes/{node}/storage", Query: []string{"content"}}
	permissions   = protocol.Call{Method: "GET", Path: "/access/permissions", Query: []string{"userid", "path"}}
	lxcConfig     = protocol.Call{Method: "GET", Path: "/nodes/{node}/lxc/{vmid}/config"}
)

// GuardCalls are the reads the guard makes.
var GuardCalls = []protocol.Call{clusterStatus, listVMs, getConfig, lxcConfig, nodeStatus, storageStatus, storages, permissions}

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
		if online[g.Node] {
			read := getConfig
			if g.Type == "lxc" {
				read = lxcConfig
			}
			var cfg vmConfig
			if err := get(c, read, ids(g.Node, g.VMID), nil, &cfg); err != nil {
				return nil, err
			}
			g.ConfigDigest = cfg.Digest
			if g.Type == "qemu" {
				g.MemoryMiB, g.DiskGiB = cfg.Memory, cfg.DiskGiB
				if m, ok := markerOf(Kind, cfg.Description); ok {
					g.Network = m.Network
				} else if m, ok := parseOldMarker(cfg.Description); ok {
					g.Network = m.Network
				}
			}
		}
		f.Guests = append(f.Guests, g)
	}
	slices.SortFunc(f.Guests, func(a, b Guest) int { return int(a.VMID - b.VMID) })

	if f.NodeOnline {
		var ns struct {
			Memory struct {
				Free      int64 `json:"free"`
				Available int64 `json:"available"`
			} `json:"memory"`
		}
		if err := get(c, nodeStatus, map[string]string{"node": s.Node}, nil, &ns); err != nil {
			return nil, err
		}
		f.MemAvailable = ns.Memory.Available
		if f.MemAvailable == 0 { // older Proxmox VE reports only free memory
			f.MemAvailable = ns.Memory.Free
		}
		var ss struct {
			Total int64 `json:"total"`
			Used  int64 `json:"used"`
		}
		if err := get(c, storageStatus, map[string]string{"node": s.Node, "storage": s.DiskStorage}, nil, &ss); err != nil {
			return nil, err
		}
		f.DiskTotal, f.DiskUsed = ss.Total, ss.Used
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
	if err := get(c, permissions, nil, map[string][]string{"userid": {s.OperatingToken}}, &f.Permissions); err != nil {
		return nil, fmt.Errorf("the operating token's permissions: %w", err)
	}
	return f, nil
}

// Need is what the changes of one apply need of the node and the storage, VM by VM:
//
//   - a VM created and started: its memory and disk, once;
//   - a stopped VM started: its memory (its new memory if it is resized too);
//   - a running VM resized: what its memory grows by;
//   - a disk grown: what it grows by.
//
// A VM the guard cannot read is counted at the size the change asks for (an upper bound).
type Need struct {
	Creates   int
	MemoryMiB int64
	DiskGiB   int64
}

// NeedOf sums the changes, with what the guests' configurations say now.
func NeedOf(changes []protocol.Change, guests []Guest) Need {
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
			case startVM.Path:
				v.started = true
			case createVM.Path, setConfig.Path:
				if x := r.Params["memory"]; len(x) == 1 {
					v.memory, _ = strconv.ParseInt(x[0], 10, 64)
				}
			case resizeDisk.Path:
				if x := r.Params["size"]; len(x) == 1 {
					v.disk, _ = strconv.ParseInt(strings.TrimSuffix(x[0], "G"), 10, 64)
				}
			}
		}
	}
	now := map[string]Guest{}
	for _, g := range guests {
		if g.Type == "qemu" {
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
		if v.disk > g.DiskGiB {
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

	// Room on the node and the storage.
	n := NeedOf(changes, f.Guests)
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

	// The boundary.
	lab := 0
	for _, g := range f.Guests {
		switch {
		case s.inRange(g.VMID) && !s.IsLab(g):
			add("PVE-0301", block, "guest %d (%s) is in the VMID range %d..%d but not in pool %s (pool %q): someone else's guest, or a create still running",
				g.VMID, g.Name, s.VMIDMin, s.VMIDMax, s.Pool, g.Pool)
		case g.Pool == s.Pool && !s.IsLab(g):
			add("PVE-0302", block, "guest %d (%s) is in pool %s but outside the VMID range %d..%d", g.VMID, g.Name, s.Pool, s.VMIDMin, s.VMIDMax)
		case s.IsLab(g):
			lab++
		}
	}
	if int64(lab+n.Creates) > s.MaxVMs {
		add("PVE-0303", block, "the boundary has %d VM(s); %d more would exceed max_vms %d", lab, n.Creates, s.MaxVMs)
	} else {
		add("PVE-0303", ok, "the boundary has %d VM(s) and the changes create %d; max_vms is %d", lab, n.Creates, s.MaxVMs)
	}
	outside := 0
	for _, c := range changes {
		for _, why := range Outside(s, c) {
			add("PVE-0304", block, "%s %s: %s; the declaration (Proxmox.Site) and [provider.proxmox.guard] must agree", c.Op, c.Resource, why)
			outside++
		}
	}
	if outside == 0 {
		add("PVE-0304", ok, "every change stays on node %s, in pool %s and VMIDs %d..%d", s.Node, s.Pool, s.VMIDMin, s.VMIDMax)
	}
	out = append(out, checkStaticAddresses(s, f.Guests, changes)...)
	return out
}

// Outside lists how a change leaves the boundary the operator set: another node, a VMID out of the
// range, another pool, storage, bridge or tag.
func Outside(s GuardSettings, c protocol.Change) []string {
	var out []string
	if c.IDs["node"] != s.Node {
		out = append(out, fmt.Sprintf("node %q is not %s", c.IDs["node"], s.Node))
	}
	if vmid, err := strconv.ParseInt(c.IDs["vmid"], 10, 64); err != nil || !s.inRange(vmid) {
		out = append(out, fmt.Sprintf("VMID %q is not in %d..%d", c.IDs["vmid"], s.VMIDMin, s.VMIDMax))
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
// every supported operation) with the settings filled in. The pool's grant comes first.
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
