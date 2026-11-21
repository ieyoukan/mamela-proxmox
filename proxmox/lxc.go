package proxmox

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// A container (LXC) on Proxmox VE that a person made by hand, which Mamela manages once a person
// adopts it. Mamela never creates a container, and so never deletes one.
//
//   - Declaring: the declaration names the container by its VMID, inside the site's boundary
//     (node, pool, VMID range). Until a person adopts it, observe reports it as one to adopt
//     (kit.Adopter), and the plan shows what would change (previews) and changes nothing.
//   - Adopting (adopt-lxc, explicit): one change of its configuration, based on the configuration
//     observed (its digest), appends this provider's marker line to its description and the site's
//     tag to its tags; what people wrote there stays. Nothing else changes, and it is not
//     restarted.
//   - Managing: only the values its declaration gives (cores, memory, swap, start at boot, the
//     size of its root filesystem, running or stopped). An omitted value is never changed. Proxmox
//     applies cores, memory and swap to a running container at once, without a restart. The root
//     filesystem is grown, never shrunk. A container is never moved.
//   - Ownership: a container is Mamela's when it carries the marker line with its declared name
//     (List), in a pool; a VM's marker is never taken for a container's, nor a container's for a
//     VM's. Restarting and stopping it are explicit operations (reboot-lxc, stop-lxc).
//
// A container is a node: mamelet can run in it later (it is not installed in it by Mamela).

// KindLXC is the resource kind of a container.
const KindLXC protocol.ResourceKind = "ProxmoxLXC"

// Operations of a container.
const (
	OpConfigureLXC protocol.Op = "configure-lxc"   // cores, memory, swap, start at boot (and its tag): applied at once
	OpGrowLXC      protocol.Op = "grow-lxc-rootfs" // grow its root filesystem
	OpStartLXC     protocol.Op = "start-lxc"
	OpShutdownLXC  protocol.Op = "shutdown-lxc" // a container declared stopped that runs: ask it to shut down

	// Never carried out (the manifest says why); a difference is shown and denied.
	OpShrinkLXC protocol.Op = "shrink-lxc-rootfs"
	OpMoveLXC   protocol.Op = "move-lxc"
	OpCreateLXC protocol.Op = "create-lxc"

	// Explicit: a person asks for each by name (mamela act).
	OpAdoptLXC  protocol.Op = "adopt-lxc"  // write this provider's marker line and the site's tag, nothing else
	OpRebootLXC protocol.Op = "reboot-lxc" // ask it to restart
	OpStopLXC   protocol.Op = "stop-lxc"   // stop it at once
)

// Calls of containers.
var (
	getLXCConfig = protocol.Call{Method: "GET", Path: "/nodes/{node}/lxc/{vmid}/config"}
	// What each operation sends of the configuration: only what it changes.
	setLXCConfig = protocol.Call{Method: "PUT", Path: "/nodes/{node}/lxc/{vmid}/config", Form: []string{"cores", "memory", "swap", "onboot", "tags", "digest"}}
	setLXCMarker = protocol.Call{Method: "PUT", Path: setLXCConfig.Path, Form: []string{"description", "tags", "digest"}}
	resizeLXC    = protocol.Call{Method: "PUT", Path: "/nodes/{node}/lxc/{vmid}/resize", Form: []string{"disk", "size", "digest"}}
	startLXC     = protocol.Call{Method: "POST", Path: "/nodes/{node}/lxc/{vmid}/status/start"}
	shutdownLXC  = protocol.Call{Method: "POST", Path: "/nodes/{node}/lxc/{vmid}/status/shutdown"}
	rebootLXC    = protocol.Call{Method: "POST", Path: "/nodes/{node}/lxc/{vmid}/status/reboot"}
	stopLXC      = protocol.Call{Method: "POST", Path: "/nodes/{node}/lxc/{vmid}/status/stop"}
)

// lxcReads are what the kind lists containers with (and waits for tasks with).
var lxcReads = []protocol.Call{listVMs, getLXCConfig, getTask}

// The marker of a container is one line of its description, appended to what people wrote there:
// "Managed by Mamela (proxmox provider): container <name>. Do not edit this line."
const (
	lxcMarkerPrefix = "Managed by Mamela (proxmox provider): container "
	lxcMarkerSuffix = ". Do not edit this line."
	// mamelaMark starts every marker line of Mamela's (a VM's, a template's, the built-in
	// provider's, a container's).
	mamelaMark = "Managed by Mamela"
	// maxDescription is the longest description Proxmox VE keeps (its API schema's maxLength).
	maxDescription = 8192
)

// lxcName is what a name in a container's marker may be: a resource name of Mamela's.
var lxcName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,252}$`)

// LXCMarker is the marker line of the container declared with a name.
func LXCMarker(name string) string { return lxcMarkerPrefix + name + lxcMarkerSuffix }

// parseLXCMarker finds the container's marker line, wherever it is in the description: the name
// on it. There is none when no line is one, when one is malformed, or when there are several.
func parseLXCMarker(description string) (string, bool) {
	var names []string
	for line := range strings.SplitSeq(description, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), lxcMarkerPrefix)
		if !ok {
			continue
		}
		name, ok := strings.CutSuffix(rest, lxcMarkerSuffix)
		if !ok || !lxcName.MatchString(name) {
			return "", false
		}
		names = append(names, name)
	}
	if len(names) != 1 {
		return "", false
	}
	return names[0], true
}

// anyMark says whether a description carries a mark of Mamela's on any line: a marker of a VM, a
// template, the built-in provider or a container, whole or not.
func anyMark(description string) bool {
	for line := range strings.SplitSeq(description, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), mamelaMark) {
			return true
		}
	}
	return false
}

// withMarker is a description with the container's marker line appended: the description as it
// is, byte for byte, then the marker on a line of its own.
func withMarker(description, name string) string {
	if description != "" && !strings.HasSuffix(description, "\n") {
		description += "\n"
	}
	return description + LXCMarker(name) + "\n"
}

// withTag is a container's tags with one more, unless it has it already; what it has is kept.
func withTag(tags []string, tag string) []string {
	if slices.Contains(tags, tag) {
		return slices.Clone(tags)
	}
	return append(slices.Clone(tags), tag)
}

// LXCSpec is a declared container: the site's boundary, the VMID of the container (made by hand)
// to adopt, and the values Mamela manages. A value that is omitted is not managed: Mamela never
// changes it, and it makes no difference. Its JSON is what the Pome's Proxmox.lxc writes.
type LXCSpec struct {
	Node    string `json:"node" doc:"the Proxmox VE node the container is on"`
	Pool    string `json:"pool" doc:"the pool Mamela's token may change (the container is in it)"`
	Tag     string `json:"tag" doc:"the tag Mamela adds to the container's tags"`
	VMIDMin int64  `json:"vmidMin" doc:"the first VMID Mamela may use"`
	VMIDMax int64  `json:"vmidMax" doc:"the last VMID Mamela may use"`
	VMID    int64  `json:"vmid" doc:"the VMID of the existing container to adopt (made by hand; Mamela creates none)"`
	// The managed values: nil when not managed.
	Cores     *int64 `json:"cores,omitzero" doc:"its cores (not managed when omitted)"`
	MemoryMib *int64 `json:"memoryMib,omitzero" doc:"its memory in MiB (not managed when omitted)"`
	SwapMib   *int64 `json:"swapMib,omitzero" doc:"its swap in MiB (not managed when omitted)"`
	RootfsGib *int64 `json:"rootfsGib,omitzero" doc:"its root filesystem in GiB, only ever grown (not managed when omitted)"`
	Onboot    *bool  `json:"onboot,omitzero" doc:"start it when the node boots (not managed when omitted)"`
	Power     string `json:"power,omitempty" values:"running|stopped" doc:"keep it running or stopped (not managed when omitted)"`
}

func lxcSpec(v jsontext.Value) (LXCSpec, error) {
	var s LXCSpec
	if err := json.Unmarshal(v, &s, json.RejectUnknownMembers(true)); err != nil {
		return s, err
	}
	switch {
	case s.Node == "" || s.Pool == "" || s.Tag == "":
		return s, errors.New("node, pool and tag are required")
	case s.VMIDMin < 100 || s.VMIDMax < s.VMIDMin || s.VMIDMax > 999999999:
		return s, fmt.Errorf("VMID range %d..%d is not a Proxmox VMID range", s.VMIDMin, s.VMIDMax)
	case s.VMID < s.VMIDMin || s.VMID > s.VMIDMax:
		return s, fmt.Errorf("vmid %d is outside the declared VMIDs %d..%d: Mamela adopts only a container inside its boundary "+
			"(a VMID cannot be changed in place: restore a backup of the container to a free VMID of the range, in pool %s, and declare that)",
			s.VMID, s.VMIDMin, s.VMIDMax, s.Pool)
	case s.Cores != nil && *s.Cores < 1:
		return s, errors.New("cores must be 1 or more")
	case s.MemoryMib != nil && *s.MemoryMib < 16:
		return s, errors.New("memoryMib must be 16 or more (Proxmox's least)")
	case s.SwapMib != nil && *s.SwapMib < 0:
		return s, errors.New("swapMib must not be negative")
	case s.RootfsGib != nil && *s.RootfsGib < 1:
		return s, errors.New("rootfsGib must be 1 or more")
	case s.Power != "" && s.Power != "running" && s.Power != "stopped":
		return s, fmt.Errorf("power %q is not running or stopped", s.Power)
	}
	return s, nil
}

func (s LXCSpec) inRange(vmid int64) bool { return vmid >= s.VMIDMin && vmid <= s.VMIDMax }

// LXCState is what is observed of a container.
type LXCState struct {
	Cores         int64    `json:"cores,omitzero"` // 0: all of the node's (none set)
	MemoryMiB     int64    `json:"memoryMiB,omitzero"`
	SwapMiB       int64    `json:"swapMiB,omitzero"`
	RootfsGiB     int64    `json:"rootfsGiB,omitzero"`
	RootfsStorage string   `json:"rootfsStorage,omitempty"`
	Onboot        bool     `json:"onboot,omitzero"`
	Running       bool     `json:"running,omitzero"`
	Pool          string   `json:"pool,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Tagged        bool     `json:"tagged,omitzero"` // it has the declared tag
	// Description is, for a container to adopt only, its description: adopting it appends the
	// marker to it.
	Description string `json:"description,omitempty"`
	// Missing is, for a declared container that is not there to adopt, what is at its VMID.
	Missing string `json:"missing,omitempty"`
	// Uptime is what List read, for the observed node; it is not reported in the state.
	Uptime int64 `json:"uptime,omitzero"`
}

// ctConfig is what is read of a container's configuration.
type ctConfig struct {
	Digest, Description, Hostname string
	Cores, MemoryMiB, SwapMiB     int64
	Rootfs                        string
	RootfsGiB                     int64
	Onboot, Template              bool
	Tags                          []string
}

// UnmarshalJSON reads what it needs of a container's configuration ("rootfs":
// "local-lvm:vm-7005-disk-0,size=8G"): numbers may come as strings; memory and swap are 512 MiB
// when not set (Proxmox's defaults), and no cores is all of the node's.
func (c *ctConfig) UnmarshalJSON(b []byte) error {
	raw, err := readConfig(b)
	if err != nil {
		return err
	}
	c.Digest, c.Description, c.Hostname = raw.text("digest"), raw.text("description"), raw.text("hostname")
	c.Cores, c.MemoryMiB, c.SwapMiB = raw.num("cores"), raw.num("memory"), raw.num("swap")
	if !raw.has("memory") {
		c.MemoryMiB = 512
	}
	if !raw.has("swap") {
		c.SwapMiB = 512
	}
	c.Rootfs = raw.text("rootfs")
	c.RootfsGiB = diskBytes(c.Rootfs) >> 30
	c.Onboot = raw.text("onboot") == "1" || raw.text("onboot") == "true"
	c.Template = raw.text("template") == "1"
	c.Tags = splitTags(raw.text("tags"))
	return nil
}

func containerConfig(c *kit.Client, node string, vmid int64) (ctConfig, error) {
	var cfg ctConfig
	err := get(c, getLXCConfig, ids(node, vmid), nil, &cfg)
	return cfg, err
}

// lxcKind is the kind ProxmoxLXC.
type lxcKind struct{ run *run }

func (*lxcKind) Kind() protocol.ResourceKind { return KindLXC }
func (*lxcKind) IsNode() bool                { return true }
func (*lxcKind) Spec() any                   { return LXCSpec{} }
func (*lxcKind) Reads() []protocol.Call      { return lxcReads }

func (*lxcKind) Doc() string {
	return "A container (LXC) on Proxmox VE that a person made, which Mamela manages once adopted (adopt-lxc): only the values\n" +
		"declared; an omitted value is never changed. Mamela never creates, moves or deletes a container, and never shrinks its disk."
}

func (*lxcKind) Operations() []protocol.Operation {
	return []protocol.Operation{
		kit.Op(OpConfigureLXC, atPool("VM.Config.CPU", "VM.Config.Memory", "VM.Config.Options"), setLXCConfig),
		kit.Op(OpGrowLXC, needs(atPool("VM.Config.Disk"), atStorage("diskStorage", "Datastore.AllocateSpace")), resizeLXC),
		kit.Op(OpStartLXC, atPool("VM.PowerMgmt"), startLXC),
		kit.Op(OpShutdownLXC, atPool("VM.PowerMgmt"), shutdownLXC),
		kit.Unsupported(OpShrinkLXC, "shrinking a container's root filesystem can destroy data (Proxmox refuses it too)"),
		kit.Unsupported(OpMoveLXC, "Mamela never moves a container to another node, pool or VMID: migrate it by hand (pct migrate) and declare "+
			"where it is, or declare it where it is"),
		kit.Unsupported(OpCreateLXC, "Mamela does not create containers: it adopts one a person made (declare its VMID, then adopt-lxc)"),
		kit.ExplicitOp(OpRebootLXC, atPool("VM.PowerMgmt"), rebootLXC),
		kit.ExplicitOp(OpStopLXC, atPool("VM.PowerMgmt"), stopLXC),
		kit.AdoptOp(OpAdoptLXC, atPool("VM.Config.Options"), setLXCMarker),
	}
}

// List is every container in a pool that carries this provider's container marker. A template is
// never one (it is not a container that runs).
func (k *lxcKind) List(c *kit.Client) ([]kit.Object, error) {
	rd := k.run.reader(c)
	cts, err := rd.containers()
	if err != nil {
		return nil, err
	}
	var out []kit.Object
	for _, vmid := range slices.Sorted(maps.Keys(cts)) {
		g := cts[vmid]
		if g.Pool == "" || g.Template == 1 {
			continue
		}
		cfg, err := rd.ct(g.Node, vmid)
		if err != nil {
			return nil, err
		}
		if name, ok := parseLXCMarker(cfg.Description); ok && !cfg.Template {
			out = append(out, lxcObject(g, cfg, name))
		}
	}
	return out, nil
}

func lxcObject(g resource, cfg ctConfig, name string) kit.Object {
	running := g.Status == "running"
	st := LXCState{Cores: cfg.Cores, MemoryMiB: cfg.MemoryMiB, SwapMiB: cfg.SwapMiB, RootfsGiB: cfg.RootfsGiB,
		RootfsStorage: diskStorage(cfg.Rootfs), Onboot: cfg.Onboot, Running: running, Pool: g.Pool, Tags: cfg.Tags}
	if running {
		st.Uptime = g.Uptime
	}
	return kit.Object{Name: name, IDs: ids(g.Node, g.VMID), Version: cfg.Digest, State: st}
}

// Adopts implements kit.Adopter: a declaration names the container it adopts by its VMID, on its
// node.
func (*lxcKind) Adopts(r protocol.Resource) (map[string]string, error) {
	s, err := lxcSpec(r.Spec)
	if err != nil {
		return nil, err
	}
	return ids(s.Node, s.VMID), nil
}

// Find implements kit.Finder: the container with the VMID on the node, if it is a container (lxc,
// not a template) that carries no mark of Mamela's. Whether it is inside the declared boundary is
// the declaration's (claim).
func (k *lxcKind) Find(c *kit.Client, id map[string]string) (*kit.Object, error) {
	vmid, err := strconv.ParseInt(id["vmid"], 10, 64)
	if err != nil || id["node"] == "" {
		return nil, nil
	}
	rd := k.run.reader(c)
	cts, err := rd.containers()
	if err != nil {
		return nil, err
	}
	g, ok := cts[vmid]
	if !ok || g.Node != id["node"] || g.Template == 1 {
		return nil, nil
	}
	cfg, err := rd.ct(g.Node, vmid)
	if err != nil || anyMark(cfg.Description) || cfg.Template {
		return nil, err
	}
	x := lxcObject(g, cfg, g.Name)
	st := x.State.(LXCState)
	st.Description = cfg.Description
	x.State, x.Adoptable = st, true
	return &x, nil
}

// claim makes what kit observed of a container what Mamela is told: a node with its state, owned
// with its marker (tagged or not: a missing tag is put back), or adoptable only inside the
// declared boundary (node, pool, VMID range, the declared VMID).
func (k *lxcKind) claim(o *protocol.Observed, r *protocol.Resource) error {
	if !o.Exists {
		return nil
	}
	var st LXCState
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
		s, err := lxcSpec(r.Spec)
		if err != nil {
			return err
		}
		switch {
		case o.Adoptable:
			o.Node = nil
			if o.IDs["node"] != s.Node || st.Pool != s.Pool || !s.inRange(vmid) || vmid != s.VMID || anyMark(st.Description) {
				// Outside the declared boundary: never Mamela's to take over.
				o.Adoptable, st.Description = false, ""
			}
		default:
			st.Tagged = slices.Contains(st.Tags, s.Tag)
		}
	}
	o.State = encode(st)
	return nil
}

// Missing says what is at a declared container's VMID when there is no container to adopt there:
// Mamela does not create one, and the plan says why.
func (k *lxcKind) Missing(c *kit.Client, r protocol.Resource) (any, error) {
	s, err := lxcSpec(r.Spec)
	if err != nil {
		return nil, err
	}
	rd := k.run.reader(c)
	gs, err := rd.guests()
	if err != nil {
		return nil, err
	}
	g, ok := gs[s.VMID]
	why := ""
	switch {
	case !ok:
		why = fmt.Sprintf("no container has VMID %d (among the guests Mamela's token sees: those of pool %s)", s.VMID, s.Pool)
	case g.Type != "lxc":
		why = fmt.Sprintf("VMID %d is a VM (qemu), not a container: declare it as a ProxmoxVM", s.VMID)
	case g.Node != s.Node:
		why = fmt.Sprintf("container %d is on node %s, and the declaration puts it on %s: declare it where it is (Mamela never moves a container)",
			s.VMID, g.Node, s.Node)
	case g.Template == 1:
		why = fmt.Sprintf("container %d is a template, not a container that runs", s.VMID)
	default:
		cfg, err := rd.ct(g.Node, s.VMID)
		if err != nil {
			return nil, err
		}
		switch name, ok := parseLXCMarker(cfg.Description); {
		case ok && name != r.Name:
			why = fmt.Sprintf("container %d is Mamela's already, for ProxmoxLXC %s (its marker): a container is one declaration's; "+
				"remove one of the two declarations", s.VMID, name)
		case ok:
			why = fmt.Sprintf("container %d carries this declaration's marker, and it is in no pool (pool %q): put it back in pool %s",
				s.VMID, g.Pool, s.Pool)
		case anyMark(cfg.Description):
			why = fmt.Sprintf("container %d carries a mark of Mamela's that is not a container's marker (in its description): "+
				"it is not one to adopt as it is; remove that line by hand if Mamela never managed it", s.VMID)
		case cfg.Template:
			why = fmt.Sprintf("container %d is a template, not a container that runs", s.VMID)
		default:
			why = fmt.Sprintf("container %d is not one to adopt", s.VMID)
		}
	}
	return LXCState{Missing: why}, nil
}

// Diff implements kit.Kind: the changes of a container Mamela has, and the same changes, as
// previews, for one to adopt (except the tag, which adopting writes).
func (k *lxcKind) Diff(d kit.Desired) ([]protocol.Change, error) {
	r, o := d.Resource, d.Observed
	s, err := lxcSpec(r.Spec)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r.ID, err)
	}
	var st LXCState
	if err := decodeState(o, &st); err != nil {
		return nil, err
	}
	switch {
	case !o.Exists:
		why := st.Missing
		if why == "" {
			why = fmt.Sprintf("no container with VMID %d is there to adopt", s.VMID)
		}
		return []protocol.Change{{Resource: r.ID, Kind: KindLXC, Op: OpCreateLXC,
			Reasons: []string{why + "; Mamela does not create containers: it adopts one that exists (adopt-lxc)"}}}, nil
	case o.Adoptable:
		return lxcChanges(r, s, o, st, true), nil
	case !o.Owned && !o.Unfinished:
		return nil, nil // not Mamela's: nothing is planned for it
	}
	return lxcChanges(r, s, o, st, false), nil
}

func boolText(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// lxcChanges are the changes that make a container what its declaration says: one change of its
// configuration at a time (each needs the version it was planned on: the root filesystem first,
// then the rest), and its power, late. A preview (a container to adopt) has every change of its
// configuration, and not its tag.
func lxcChanges(r protocol.Resource, s LXCSpec, o protocol.Observed, st LXCState, preview bool) []protocol.Change {
	id := map[string]string{"node": o.IDs["node"], "vmid": o.IDs["vmid"]}
	var out []protocol.Change
	unsupported := func(op protocol.Op, why string) {
		out = append(out, protocol.Change{Resource: r.ID, Kind: KindLXC, Op: op, IDs: id, Reasons: []string{why}})
	}
	moved := movedFrom(s.Node, s.Pool, s.VMIDMin, s.VMIDMax, id, st.Pool)
	if want := strconv.FormatInt(s.VMID, 10); id["vmid"] != want {
		moved = strings.Join(slices.DeleteFunc([]string{moved, fmt.Sprintf("its VMID %s is not the declared %s", id["vmid"], want)},
			func(x string) bool { return x == "" }), "; ")
	}
	if moved != "" {
		unsupported(OpMoveLXC, moved)
		return out
	}

	params := map[string][]string{}
	var why []string
	live := false
	if s.Cores != nil && *s.Cores != st.Cores {
		was := strconv.FormatInt(st.Cores, 10)
		if st.Cores == 0 {
			was = "all of the node's"
		}
		params["cores"] = []string{strconv.FormatInt(*s.Cores, 10)}
		why, live = append(why, fmt.Sprintf("cores %s -> %d", was, *s.Cores)), true
	}
	if s.MemoryMib != nil && *s.MemoryMib != st.MemoryMiB {
		params["memory"] = []string{strconv.FormatInt(*s.MemoryMib, 10)}
		why, live = append(why, fmt.Sprintf("memory %d -> %d MiB", st.MemoryMiB, *s.MemoryMib)), true
		if st.Running && *s.MemoryMib < st.MemoryMiB {
			why = append(why, "less memory for a running container: if it uses more, its kernel reclaims memory and may stop processes in it")
		}
	}
	if s.SwapMib != nil && *s.SwapMib != st.SwapMiB {
		params["swap"] = []string{strconv.FormatInt(*s.SwapMib, 10)}
		why, live = append(why, fmt.Sprintf("swap %d -> %d MiB", st.SwapMiB, *s.SwapMib)), true
	}
	if s.Onboot != nil && *s.Onboot != st.Onboot {
		params["onboot"] = []string{boolText(*s.Onboot)}
		why = append(why, fmt.Sprintf("start at boot %s -> %s", onOff(st.Onboot), onOff(*s.Onboot)))
	}
	if !preview && !st.Tagged {
		tags := withTag(st.Tags, s.Tag)
		params["tags"] = []string{strings.Join(tags, ";")}
		why = append(why, fmt.Sprintf("tag it %s again (its tags %v are kept)", s.Tag, st.Tags))
	}
	var configure, grow *protocol.Change
	if len(params) > 0 {
		params["digest"] = []string{o.Version}
		if live && st.Running {
			why = append(why, "Proxmox applies cores, memory and swap to the running container at once: it is not restarted")
		}
		configure = &protocol.Change{Resource: r.ID, Kind: KindLXC, Op: OpConfigureLXC, IDs: id, Precondition: o.Version, Reasons: why,
			Requests: []protocol.Approved{req(setLXCConfig, params)}}
	}
	if s.RootfsGib != nil && *s.RootfsGib > st.RootfsGiB {
		reasons := []string{fmt.Sprintf("root filesystem %d -> %d GiB, grown in place (a running container keeps running)", st.RootfsGiB, *s.RootfsGib)}
		if configure != nil && !preview {
			// The rest is shown too (an observe-only container's plan shows every difference), though
			// it is the next plan's change: each change is based on the configuration it was planned on.
			var next []string
			for _, w := range configure.Reasons {
				if !strings.HasPrefix(w, "Proxmox applies") {
					next = append(next, w)
				}
			}
			reasons = append(reasons, "then, in the next plan (each change is based on the configuration it was planned on): "+strings.Join(next, "; "))
		}
		grow = &protocol.Change{Resource: r.ID, Kind: KindLXC, Op: OpGrowLXC, IDs: id, Precondition: o.Version, Reasons: reasons,
			Requests: []protocol.Approved{req(resizeLXC, map[string][]string{"disk": {"rootfs"}, "size": {fmt.Sprintf("%dG", *s.RootfsGib)},
				"digest": {o.Version}})}}
	}
	switch {
	case preview:
		for _, c := range []*protocol.Change{grow, configure} {
			if c != nil {
				out = append(out, *c)
			}
		}
	case grow != nil:
		out = append(out, *grow)
	case configure != nil:
		out = append(out, *configure)
	}
	switch {
	case s.Power == "running" && !st.Running:
		out = append(out, protocol.Change{Resource: r.ID, Kind: KindLXC, Op: OpStartLXC, IDs: id, Late: true,
			Requests: []protocol.Approved{req(startLXC, nil)}, Reasons: []string{"start it, as declared (running)"}})
	case s.Power == "stopped" && st.Running:
		out = append(out, protocol.Change{Resource: r.ID, Kind: KindLXC, Op: OpShutdownLXC, IDs: id, Late: true,
			Requests: []protocol.Approved{req(shutdownLXC, nil)},
			Reasons:  []string{"shut it down, as declared (stopped): it is asked to stop its processes; one that does not is stopped by hand (stop-lxc)"}})
	}
	if s.RootfsGib != nil && *s.RootfsGib < st.RootfsGiB {
		unsupported(OpShrinkLXC, fmt.Sprintf("root filesystem %d -> %d GiB", st.RootfsGiB, *s.RootfsGib))
	}
	return out
}

// Prepare implements kit.Explicit: adopt-lxc, reboot-lxc and stop-lxc, from what was observed.
func (*lxcKind) Prepare(op protocol.Op, o protocol.Observed, desired *protocol.Resource) (protocol.Change, error) {
	var st LXCState
	if err := decodeState(o, &st); err != nil {
		return protocol.Change{}, err
	}
	vmid := o.IDs["vmid"]
	c := protocol.Change{Resource: o.ID, Kind: o.Kind, Op: op, IDs: map[string]string{"node": o.IDs["node"], "vmid": vmid}}
	switch op {
	case OpAdoptLXC:
		return prepareAdoptLXC(c, o, st, desired)
	case OpRebootLXC, OpStopLXC:
		switch {
		case !o.Owned:
			return protocol.Change{}, fmt.Errorf("%s: container %s is not Mamela's (adopt it first: adopt-lxc); nothing was done", o.ID, vmid)
		case !st.Running && op == OpRebootLXC:
			return protocol.Change{}, fmt.Errorf("%s: container %s is stopped; nothing to restart (declare it running, or start it by hand)", o.ID, vmid)
		case !st.Running:
			return protocol.Change{}, fmt.Errorf("%s: container %s is stopped already", o.ID, vmid)
		}
		if op == OpRebootLXC {
			c.Requests = []protocol.Approved{req(rebootLXC, nil)}
			c.Reasons = []string{fmt.Sprintf("ask container %s (%s) to restart: its processes are stopped and it starts again", vmid, o.Name)}
		} else {
			c.Requests = []protocol.Approved{req(stopLXC, nil)}
			c.Reasons = []string{fmt.Sprintf("stop container %s (%s) at once (as pulling its power: what its processes did not save is lost); "+
				"a container declared running is started again by the next plan (declare it stopped to keep it so)", vmid, o.Name)}
		}
		return c, nil
	}
	return protocol.Change{}, fmt.Errorf("%s is not an explicit operation of a container (it has %s, %s, %s)", op, OpAdoptLXC, OpRebootLXC, OpStopLXC)
}

// prepareAdoptLXC is the change that takes over a container: append this provider's marker line to
// its description and the tag to its tags, based on the configuration observed (its digest).
// Nothing else of it changes, and it is not restarted. It is refused, with nothing sent, for a
// container outside the declared boundary or one that carries a mark of Mamela's.
func prepareAdoptLXC(c protocol.Change, o protocol.Observed, st LXCState, desired *protocol.Resource) (protocol.Change, error) {
	vmid := o.IDs["vmid"]
	if !o.Adoptable || desired == nil {
		return protocol.Change{}, fmt.Errorf("%s: only a declared container observed as one to adopt (its VMID declared, inside the boundary, "+
			"without a mark of Mamela's) can be adopted; container %s is not; nothing was done", o.ID, vmid)
	}
	s, err := lxcSpec(desired.Spec)
	if err != nil {
		return protocol.Change{}, fmt.Errorf("%s: %w", o.ID, err)
	}
	var refuse []string
	if moved := movedFrom(s.Node, s.Pool, s.VMIDMin, s.VMIDMax, o.IDs, st.Pool); moved != "" {
		refuse = append(refuse, moved+" (Mamela takes over only what is inside its boundary)")
	}
	if vmid != strconv.FormatInt(s.VMID, 10) {
		refuse = append(refuse, fmt.Sprintf("its VMID is not the declared %d", s.VMID))
	}
	if anyMark(st.Description) {
		refuse = append(refuse, "its description carries a mark of Mamela's: it is Mamela's already (another declaration's), or was")
	}
	if name, ok := parseLXCMarker(LXCMarker(o.Name)); !ok || name != o.Name {
		refuse = append(refuse, fmt.Sprintf("its name %q cannot be written into the marker", o.Name))
	}
	description := withMarker(st.Description, o.Name)
	if len(description) > maxDescription {
		refuse = append(refuse, fmt.Sprintf("its description would be %d bytes with the marker, and Proxmox keeps at most %d: shorten it first",
			len(description), maxDescription))
	}
	if len(refuse) > 0 {
		return protocol.Change{}, fmt.Errorf("%s: container %s cannot be adopted: %s; nothing was done", o.ID, vmid, strings.Join(refuse, "; "))
	}
	tags := withTag(st.Tags, s.Tag)
	c.Precondition = o.Version
	c.Requests = []protocol.Approved{req(setLXCMarker, map[string][]string{"description": {description}, "tags": {strings.Join(tags, ";")},
		"digest": {o.Version}})}
	c.Reasons = []string{fmt.Sprintf("take over container %s (%s): append this provider's marker line to its description and tag %s to its tags "+
		"(what is there stays: tags %v); nothing else of it changes, and it is not restarted", vmid, o.Name, s.Tag, st.Tags)}
	return c, nil
}

// container reads the container a change names: it must be a container on the change's node.
func container(c *kit.Client, ch protocol.Change) (resource, ctConfig, bool, error) {
	node, vmid, err := target(ch)
	if err != nil {
		return resource{}, ctConfig{}, false, err
	}
	gs, err := guests(c)
	if err != nil {
		return resource{}, ctConfig{}, false, err
	}
	g, ok := gs[vmid]
	if !ok || g.Type != "lxc" || g.Node != node {
		return g, ctConfig{}, false, nil
	}
	cfg, err := containerConfig(c, node, vmid)
	return g, cfg, err == nil, err
}

// mine says whether a container carries the marker of the change's resource.
func mine(ch protocol.Change, cfg ctConfig) bool {
	name, ok := parseLXCMarker(cfg.Description)
	return ok && name == nameOf(ch.Resource)
}

// Done implements kit.Kind: whether an approved request is in effect already. Right before sending,
// it makes sure the VMID is still the container the change was made for: the one to adopt (no
// mark of Mamela's, the configuration observed), or the one that carries its marker.
func (*lxcKind) Done(c *kit.Client, ch protocol.Change, a protocol.Approved) (bool, error) {
	g, cfg, found, err := container(c, ch)
	switch {
	case err != nil:
		return false, err
	case !found:
		return false, fmt.Errorf("VMID %s on %s is not a container any more; nothing was done, observe and plan again", ch.IDs["vmid"], ch.IDs["node"])
	}
	vmid := ch.IDs["vmid"]
	if ch.Op == OpAdoptLXC {
		switch {
		case mine(ch, cfg) && hasTags(cfg.Tags, a.Params["tags"]):
			return true, nil // an earlier apply adopted it
		case anyMark(cfg.Description):
			return false, fmt.Errorf("container %s carries a mark of Mamela's now; nothing was done, observe and prepare again", vmid)
		case cfg.Template || g.Template == 1:
			return false, fmt.Errorf("container %s is a template now; nothing was done", vmid)
		case ch.Precondition == "" || cfg.Digest != ch.Precondition:
			return false, fmt.Errorf("container %s's configuration changed since it was prepared; nothing was done, prepare again", vmid)
		}
		return false, nil
	}
	if !mine(ch, cfg) {
		return false, fmt.Errorf("container %s is not %s's any more (its marker differs); nothing was done, observe and plan again", vmid, nameOf(ch.Resource))
	}
	return inEffect(g, cfg, a), nil
}

// inEffect says whether a request's effect is there.
func inEffect(g resource, cfg ctConfig, a protocol.Approved) bool {
	switch a.Path {
	case setLXCConfig.Path:
		return configHas(cfg, a.Params)
	case resizeLXC.Path:
		want, err := strconv.ParseInt(strings.TrimSuffix(first(a.Params["size"]), "G"), 10, 64)
		return err == nil && cfg.RootfsGiB >= want
	case startLXC.Path:
		return g.Status == "running"
	case shutdownLXC.Path, stopLXC.Path:
		return g.Status != "running"
	}
	return false // a restart leaves no trace to tell
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// hasTags says whether a container has every tag of a tags parameter.
func hasTags(have []string, param []string) bool {
	for _, t := range splitTags(first(param)) {
		if !slices.Contains(have, t) {
			return false
		}
	}
	return true
}

// configHas says whether a configuration has every value a request sets.
func configHas(cfg ctConfig, params map[string][]string) bool {
	for k, v := range params {
		var ok bool
		switch k {
		case "digest":
			continue
		case "cores":
			ok = strconv.FormatInt(cfg.Cores, 10) == first(v)
		case "memory":
			ok = strconv.FormatInt(cfg.MemoryMiB, 10) == first(v)
		case "swap":
			ok = strconv.FormatInt(cfg.SwapMiB, 10) == first(v)
		case "onboot":
			ok = boolText(cfg.Onboot) == first(v)
		case "tags":
			ok = hasTags(cfg.Tags, v)
		}
		if !ok {
			return false
		}
	}
	return true
}

// Lookup implements kit.Looker: how much of a change is in effect, on the container that carries
// its marker (on any other, nothing is).
func (*lxcKind) Lookup(c *kit.Client, ch protocol.Change) (protocol.LookupStatus, bool, error) {
	g, cfg, found, err := container(c, ch)
	switch {
	case err != nil:
		return "", true, err
	case !found || !mine(ch, cfg):
		return protocol.LookupNotDone, true, nil
	}
	switch ch.Op {
	case OpAdoptLXC:
		for _, a := range ch.Requests {
			if !hasTags(cfg.Tags, a.Params["tags"]) {
				return protocol.LookupNotDone, true, nil
			}
		}
		return protocol.LookupDone, true, nil
	case OpRebootLXC:
		// A restart leaves no trace this change can be tied to: unknown, never done.
		return protocol.LookupUnknown, true, nil
	}
	n := 0
	for _, a := range ch.Requests {
		if inEffect(g, cfg, a) {
			n++
		}
	}
	switch {
	case len(ch.Requests) > 0 && n == len(ch.Requests):
		return protocol.LookupDone, true, nil
	case n == 0:
		return protocol.LookupNotDone, true, nil
	}
	return protocol.LookupPartly, true, nil
}
