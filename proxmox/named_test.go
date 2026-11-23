package proxmox

import (
	"encoding/json/v2"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela-proxmox/fakepve"
	"github.com/ieyoukan/mamela/provider/protocol"
)

var update = flag.Bool("update", false, "write the golden files in testdata")

// namedSettings are the cases' settings with container 150 on pve2 named, with an access.
func namedSettings(access string) GuardSettings {
	s := settings
	s.Named = []NamedGuest{{Node: "pve2", VMID: 150, Type: "lxc", Access: access}}
	return s
}

// namedFacts are healthy facts for settings: the token holds exactly their grants, named container
// 150 is on pve2 (in no pool, stopped, 2 GiB), and someone else's container 151 is beside it.
func namedFacts(s GuardSettings) *Facts {
	f := healthy()
	f.Permissions = map[string]map[string]int{}
	for _, g := range Grants(New().Manifest(), s) {
		f.Permissions[g.Path] = map[string]int{}
		for _, p := range g.Privs {
			f.Permissions[g.Path][p] = 1
		}
	}
	f.Guests = append(f.Guests,
		Guest{VMID: 150, Type: "lxc", Node: "pve2", Name: "named", Status: "stopped", ConfigDigest: "n", MemoryMiB: 2048, DiskGiB: 4, DiskStorage: "local-lvm"},
		Guest{VMID: 151, Type: "lxc", Node: "pve2", Name: "neighbour", Status: "running", Uptime: 7000, ConfigDigest: "u", MemoryMiB: 512, DiskGiB: 4,
			DiskStorage: "local-lvm"})
	f.NamedNodes = map[string]Room{"pve2": {Online: true, MemAvailable: 32 << 30, DiskTotal: 1000 << 30, DiskUsed: 100 << 30}}
	return f
}

func namedChange(op protocol.Op, node, vmid string, reqs ...protocol.Approved) protocol.Change {
	return protocol.Change{Resource: "ProxmoxLXC named", Kind: KindLXC, Op: op, IDs: map[string]string{"node": node, "vmid": vmid}, Requests: reqs}
}

func adoptNamed(node, vmid string) protocol.Change {
	return namedChange(OpAdoptLXC, node, vmid, req(setLXCMarker, map[string][]string{"description": {"x"}, "tags": {"mamela-lab"}, "digest": {"n"}}))
}

func blocks(fs []protocol.Finding, code, part string) bool {
	return slices.ContainsFunc(fs, func(f protocol.Finding) bool {
		return f.Code == code && f.Severity == protocol.SeverityBlock && strings.Contains(f.Message, part)
	})
}

// The settings name existing guests strictly: a container outside the range, on a node, with an
// access a person chose; anything else is refused with what to do.
func TestParseNamedSettings(t *testing.T) {
	base := strings.TrimSuffix(goodSettings, "}")
	ok := base + `,"named":[{"node":"pve2","vmid":150,"type":"lxc","access":"observe"},{"node":"pve1","vmid":160,"type":"lxc","access":"change"}]}`
	s, err := ParseGuardSettings([]byte(ok))
	if err != nil || len(s.Named) != 2 || s.Named[1] != (NamedGuest{Node: "pve1", VMID: 160, Type: "lxc", Access: "change"}) {
		t.Fatalf("%+v %v", s.Named, err)
	}
	if got := s.namedNodes(); !slices.Equal(got, []string{"pve2"}) {
		t.Errorf("named nodes other than the settings': %v", got)
	}
	for name, c := range map[string]struct{ named, why string }{
		"in the range":   {`{"node":"pve2","vmid":7005,"type":"lxc","access":"observe"}`, "put the guest in pool lab"},
		"a VM":           {`{"node":"pve2","vmid":150,"type":"qemu","access":"observe"}`, "naming a VM (qemu) is not supported yet"},
		"another type":   {`{"node":"pve2","vmid":150,"type":"vm","access":"observe"}`, `type "vm" is not lxc`},
		"no access":      {`{"node":"pve2","vmid":150,"type":"lxc"}`, `access "" is not "observe"`},
		"another access": {`{"node":"pve2","vmid":150,"type":"lxc","access":"admin"}`, `access "admin"`},
		"no node":        {`{"vmid":150,"type":"lxc","access":"observe"}`, "node is required"},
		"not a VMID":     {`{"node":"pve2","vmid":5,"type":"lxc","access":"observe"}`, "not a Proxmox VMID"},
		"twice": {`{"node":"pve2","vmid":150,"type":"lxc","access":"observe"},{"node":"pve3","vmid":150,"type":"lxc","access":"change"}`,
			"named twice"},
		"unknown member": {`{"node":"pve2","vmid":150,"type":"lxc","access":"observe","pool":"prod"}`, `unknown object member name "pool"`},
	} {
		if _, err := ParseGuardSettings([]byte(base + `,"named":[` + c.named + `]}`)); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: %v, want %q", name, err, c.why)
		}
	}
}

// A named guest's grants are on its /vms/<vmid> only, in two steps: VM.Audit to observe it, then
// what every operation of a container needs there (from the manifest, which the permission catalog
// holds to Proxmox's checks). Nothing on /, /vms, or another guest.
func TestNamedPermissions(t *testing.T) {
	m := New().Manifest()
	byPath := func(s GuardSettings) map[string]Grant {
		out := map[string]Grant{}
		for _, g := range Grants(m, s) {
			out[g.Path] = g
		}
		return out
	}
	plain := byPath(settings)
	observe := byPath(namedSettings(AccessObserve))
	change := byPath(namedSettings(AccessChange))
	if g := observe["/vms/150"]; !slices.Equal(g.Privs, []string{"VM.Audit"}) || !slices.Equal(g.Reasons["VM.Audit"], []string{"observe"}) {
		t.Errorf("observe: %+v", g)
	}
	want := []string{"VM.Audit", "VM.Config.CPU", "VM.Config.Disk", "VM.Config.Memory", "VM.Config.Options", "VM.PowerMgmt"}
	if g := change["/vms/150"]; !slices.Equal(g.Privs, want) ||
		!slices.Equal(g.Reasons["VM.Config.Options"], []string{"configure-lxc", "adopt-lxc"}) ||
		!slices.Equal(g.Reasons["VM.PowerMgmt"], []string{"start-lxc", "shutdown-lxc", "reboot-lxc", "stop-lxc"}) {
		t.Errorf("change: %+v", g)
	}
	for _, grants := range []map[string]Grant{observe, change} {
		if len(grants) != len(plain)+1 {
			t.Errorf("a named guest adds one grant: %v", slices.Sorted(maps.Keys(grants)))
		}
		for path, g := range grants {
			if path != "/vms/150" && !slices.Equal(g.Privs, plain[path].Privs) {
				t.Errorf("%s changed with a named guest: %v", path, g.Privs)
			}
			if path == "/" || path == "/vms" || path == "/nodes" {
				t.Errorf("granted on %s", path)
			}
		}
	}
	// Every privilege a container's call checks on its guest (the permission catalog) is in the
	// change step (Pool.Audit is the pool's: which pool a guest is in, told only for the pool), but
	// deleting's: Mamela never deletes a named guest (VM.Allocate is not granted on it).
	if slices.Contains(change["/vms/150"].Privs, "VM.Allocate") {
		t.Errorf("a named guest's grant holds VM.Allocate (delete-lxc): %v", change["/vms/150"].Privs)
	}
	for _, k := range m.Kinds {
		if k.Kind != KindLXC {
			continue
		}
		for _, o := range k.Operations {
			if slices.Contains(neverNamed, o.Op) {
				continue
			}
			for _, c := range o.Calls {
				ps, _ := PrivilegesOf(c)
				for _, p := range ps {
					if p.Ref == AtPool && strings.HasPrefix(p.Privs[0], "VM.") && !slices.Contains(change["/vms/150"].Privs, p.Privs[0]) {
						t.Errorf("%s: %s %s needs %s on the guest", o.Op, c.Method, c.Path, p.Privs[0])
					}
				}
			}
		}
	}
	// The guard holds the token to the step: more than it gives, or less, blocks.
	for _, c := range []struct {
		name   string
		access string
		edit   func(perms map[string]map[string]int)
		code   string
	}{
		{"observed, and the token may change it", AccessObserve, func(p map[string]map[string]int) { p["/vms/150"]["VM.Config.Options"] = 1 }, "PVE-0101"},
		{"to change, and the token may only read it", AccessChange, func(p map[string]map[string]int) { delete(p["/vms/150"], "VM.Config.Options") }, "PVE-0102"},
		{"a guest not named", AccessChange, func(p map[string]map[string]int) { p["/vms/151"] = map[string]int{"VM.Audit": 1} }, "PVE-0101"},
		{"on /vms", AccessChange, func(p map[string]map[string]int) { p["/vms"] = map[string]int{"VM.Audit": 1} }, "PVE-0101"},
	} {
		s := namedSettings(c.access)
		f := namedFacts(s)
		c.edit(f.Permissions)
		if fs := Check(s, m, f, nil); !slices.Contains(codes(fs, protocol.SeverityBlock), c.code) {
			t.Errorf("%s: want %s: %+v", c.name, c.code, fs)
		}
	}
}

// The guard takes a named container in as inside the boundary where it is named, out of the pool
// and the range, for a container's changes, once its access is "change"; everything else about it
// stays as it was: unnamed guests outside are someone else's, and a named guest that is not where
// it is named is refused.
func TestNamedGuard(t *testing.T) {
	m := New().Manifest()
	s := namedSettings(AccessChange)
	configure := namedChange(OpConfigureLXC, "pve2", "150", req(setLXCConfig, map[string][]string{"memory": {"4096"}, "digest": {"n"}}))
	grow := namedChange(OpGrowLXC, "pve2", "150", req(resizeLXC, map[string][]string{"disk": {"rootfs"}, "size": {"8G"}, "digest": {"n"}}))
	start := namedChange(OpStartLXC, "pve2", "150", req(startLXC, nil))
	inside := []protocol.Change{adoptNamed("pve2", "150"), configure, grow, start}
	fs := Check(s, m, namedFacts(s), inside)
	if b := codes(fs, protocol.SeverityBlock); len(b) > 0 {
		t.Fatalf("a named container's changes block: %+v", fs)
	}
	for _, want := range []string{"or on the 1 named guest(s)", "the 1 named guest(s) are where they are named", "on pve2 after the named guests' changes' 4096 MiB",
		"storage local-lvm on pve2 would be"} {
		if !slices.ContainsFunc(fs, func(f protocol.Finding) bool { return strings.Contains(f.Message, want) }) {
			t.Errorf("no finding says %q: %+v", want, fs)
		}
	}
	if !slices.ContainsFunc(fs, func(f protocol.Finding) bool {
		return f.Code == "PVE-0303" && strings.Contains(f.Message, "the boundary has 2 VM(s)")
	}) {
		t.Errorf("the named container is counted in the boundary: %+v", fs)
	}

	for _, c := range []struct {
		name     string
		settings GuardSettings
		edit     func(f *Facts)
		changes  []protocol.Change
		code     string
		why      string
	}{
		{"someone else's container beside it", s, nil, []protocol.Change{adoptNamed("pve2", "151")}, "PVE-0304", "no guest is named with it"},
		{"observed only", namedSettings(AccessObserve), nil, []protocol.Change{adoptNamed("pve2", "150")}, "PVE-0304", `set access = "change"`},
		{"on another node than named", s, nil, []protocol.Change{adoptNamed("pve3", "150")}, "PVE-0304", `node "pve3" is not pve2, where guest 150 is named`},
		{"a VM's change", s, nil, []protocol.Change{{Resource: "ProxmoxVM x", Kind: Kind, Op: OpStart, IDs: map[string]string{"node": "pve2", "vmid": "150"},
			Requests: []protocol.Approved{req(startVM, nil)}}}, "PVE-0304", "only a container's (ProxmoxLXC) changes"},
		{"tags without the tag", s, nil, []protocol.Change{namedChange(OpAdoptLXC, "pve2", "150",
			req(setLXCMarker, map[string][]string{"description": {"x"}, "tags": {"handmade"}, "digest": {"n"}}))}, "PVE-0304", "do not keep mamela-lab"},
		{"root filesystem elsewhere", s, func(f *Facts) { f.Guests[3].DiskStorage = "local-zfs" }, []protocol.Change{grow}, "PVE-0304", `on storage "local-zfs"`},
		{"migrated", s, func(f *Facts) { f.Guests[3].Node = "pve3" }, []protocol.Change{configure}, "PVE-0305", "it was migrated"},
		{"migrated: its change", s, func(f *Facts) { f.Guests[3].Node = "pve3" }, []protocol.Change{configure}, "PVE-0304", "is on node pve3, not pve2"},
		{"another guest at its VMID", s, func(f *Facts) { f.Guests[3].Type = "qemu" }, nil, "PVE-0305", "is a VM (qemu), not a container (lxc)"},
		{"gone", s, func(f *Facts) { f.Guests = slices.DeleteFunc(f.Guests, func(g Guest) bool { return g.VMID == 150 }) }, nil, "PVE-0305",
			"no guest has that VMID now"},
		{"in Mamela's pool", s, func(f *Facts) { f.Guests[3].Pool = "lab" }, nil, "PVE-0302", "a named guest stays where it is, out of the pool"},
		{"in Mamela's pool: its change", s, func(f *Facts) { f.Guests[3].Pool = "lab" }, []protocol.Change{configure}, "PVE-0304", "a named guest stays out of the pool"},
		{"memory on its node", s, func(f *Facts) {
			f.NamedNodes["pve2"] = Room{Online: true, MemAvailable: 5 << 30, DiskTotal: 1000 << 30}
		},
			[]protocol.Change{start}, "PVE-0201", "on pve2 after the named guests' changes' 2048 MiB"},
		{"its node offline", s, func(f *Facts) { f.NamedNodes["pve2"] = Room{} }, []protocol.Change{configure}, "PVE-0003", "node pve2 (of named guests)"},
		{"its disk storage unknown there", s, func(f *Facts) { f.NamedNodes["pve2"] = Room{Online: true, MemAvailable: 32 << 30} }, []protocol.Change{grow},
			"PVE-0202", "on pve2 is unknown"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := namedFacts(c.settings)
			if c.edit != nil {
				c.edit(f)
			}
			if fs := Check(c.settings, m, f, c.changes); !blocks(fs, c.code, c.why) {
				t.Errorf("want %s to block with %q: %+v", c.code, c.why, fs)
			}
		})
	}
	// A named guest observed only, not where it is named, is a warning: the token may only read it.
	f := namedFacts(namedSettings(AccessObserve))
	f.Guests[3].Node = "pve3"
	if fs := Check(namedSettings(AccessObserve), m, f, nil); !slices.Contains(codes(fs, protocol.SeverityWarn), "PVE-0305") ||
		slices.Contains(codes(fs, protocol.SeverityBlock), "PVE-0305") {
		t.Errorf("migrated, observed only: %+v", fs)
	}
	// The memory a named guest's start needs is counted on its node, not on the settings' node.
	f = namedFacts(s)
	f.MemAvailable = 5 << 30
	if fs := Check(s, m, f, []protocol.Change{start}); slices.Contains(codes(fs, protocol.SeverityBlock), "PVE-0201") {
		t.Errorf("the start on pve2 is counted on pve1: %+v", fs)
	}
}

// After an apply, the guard treats a named container as inside (it changed: it was adopted), and
// still checks every other guest outside the boundary, its neighbour on the same node included.
func TestNamedGuardAfter(t *testing.T) {
	s := namedSettings(AccessChange)
	before := NewBaseline(s, namedFacts(s))
	var outside []int64
	for _, g := range before.Guests {
		outside = append(outside, g.VMID)
	}
	if !slices.Equal(outside, []int64{100, 101, 151}) {
		t.Fatalf("the baseline holds %v, want the guests outside the boundary (not named 150)", outside)
	}
	adopted := namedFacts(s)
	adopted.Guests[3].ConfigDigest, adopted.Guests[3].Status = "adopted", "running"
	if fs := Compare(s, before, adopted); len(codes(fs, protocol.SeverityBlock)) > 0 || !slices.Contains(codes(fs, protocol.SeverityOK), "PVE-0400") {
		t.Errorf("the named container changed: %+v", fs)
	}
	neighbour := namedFacts(s)
	neighbour.Guests[4].ConfigDigest = "changed"
	if fs := Compare(s, before, neighbour); !blocks(fs, "PVE-0403", "guest 151 (neighbour)") {
		t.Errorf("the neighbour changed: %+v", fs)
	}
	// Not where it is named (migrated before the apply), it is outside, and checked.
	migrated := namedFacts(s)
	migrated.Guests[3].Node = "pve3"
	before = NewBaseline(s, migrated)
	if !slices.ContainsFunc(before.Guests, func(g Guest) bool { return g.VMID == 150 }) {
		t.Errorf("a named guest not where it is named is outside: %+v", before.Guests)
	}
}

// The bootstrap script grants a named guest exactly its step, on its /vms/<vmid> only, and says
// what each step gives; the permissions script explains them. The scripts are golden: what a
// person runs as root is reviewed as text (go test ./proxmox -run TestNamedScripts -update).
func TestNamedScripts(t *testing.T) {
	for _, c := range []struct{ access, golden string }{
		{AccessObserve, "bootstrap-named-observe.sh"},
		{AccessChange, "bootstrap-named-change.sh"},
		{AccessChange, "permissions-named.txt"},
	} {
		named := `,"named":[{"node":"pve2","vmid":150,"type":"lxc","access":"` + c.access + `"}]}`
		name := "bootstrap"
		if strings.HasPrefix(c.golden, "permissions") {
			name = "permissions"
		}
		res, err := New().Script(t.Context(), protocol.ScriptParams{Name: name, Settings: []byte(strings.TrimSuffix(goodSettings, "}") + named),
			Credentials: goodCredentials})
		if err != nil {
			t.Fatal(err)
		}
		golden(t, c.golden, res.Text)
		if name != "bootstrap" {
			continue
		}
		role := map[string]string{AccessObserve: RoleNamedObserve, AccessChange: RoleNamedChange}[c.access]
		for _, want := range []string{
			"pveum acl modify /vms/150 --users mamela@pve --roles " + role,
			"pveum acl modify /vms/150 --tokens 'mamela@pve!lab' --roles " + role,
			"keep=\"/pool/lab MamelaLabVM",
			"/vms/150 " + role + "\"",
		} {
			if !strings.Contains(res.Text, want) {
				t.Errorf("%s: no %q", c.access, want)
			}
		}
		for _, l := range strings.Split(res.Text, "\n") {
			if strings.Contains(l, "mamela@pve") && strings.HasPrefix(l, "pveum acl modify") &&
				(strings.Contains(l, "modify / ") || strings.Contains(l, "modify /vms ") || strings.Contains(l, " /nodes") ||
					strings.Contains(l, "/vms/") && !strings.Contains(l, "/vms/150 ")) {
				t.Errorf("%s: grants the operating token too much: %s", c.access, l)
			}
		}
	}
}

// golden compares text with testdata/<name>, or writes it there with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (write it with -update)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from what the provider writes now (review it, then -update):\n%s", path, got)
	}
}

// namedDecl declares named container name on pve2 by its VMID (Proxmox.named_lxc).
func namedDecl(name, node string, vmid int64, rest string) protocol.Resource {
	r := lxcDecl(name, vmid, `"named":true`)
	r.Spec = []byte(strings.Replace(string(r.Spec), `"node":"pve1"`, `"node":"`+node+`"`, 1))
	if rest != "" {
		r.Spec = []byte(strings.TrimSuffix(string(r.Spec), "}") + "," + rest + "}")
	}
	return r
}

// namedWorld is a cluster with container 7005 (made by hand, in the pool and the range) and
// someone else's container 105 on pve1, and on node pve2, in no pool: container 150 (made by
// hand, to name), container 151 beside it (someone else's, never named) and VM 152.
func namedWorld() *fakepve.Server {
	f := fakepve.New()
	f.Put(handmade(7005, "dns"))
	f.Put(fakepve.VM{VMID: 105, Name: "someone-else", Node: "pve1", Type: "lxc", Running: true, Memory: 2048, DiskGiB: 8})
	named := handmade(150, "named")
	named.Node, named.Pool = "pve2", ""
	f.Put(named)
	neighbour := handmade(151, "neighbour")
	neighbour.Node, neighbour.Pool = "pve2", ""
	f.Put(neighbour)
	f.Put(fakepve.VM{VMID: 152, Name: "vm", Node: "pve2", Cores: 1, Memory: 1024, DiskGiB: 10, Running: true})
	return f
}

func guardRPC(t *testing.T, api *fakepve.Server, s GuardSettings, stage string, baseline []byte, changes []protocol.Change) protocol.GuardResult {
	t.Helper()
	b, _ := json.Marshal(s)
	var res protocol.GuardResult
	rpc(t, api, protocol.MethodGuard, protocol.GuardParams{Stage: stage, Settings: b, Baseline: baseline, Changes: changes}, &res)
	return res
}

// A named container is declared on its node by its VMID, outside the pool and the range: observed
// as one to adopt (with what would change), adopted where it is with the guard before and after,
// then managed as declared, and never put in the pool. What is not named stays someone else's.
func TestNamedLXCAdoptedWhereItIs(t *testing.T) {
	world := namedWorld()
	s := namedSettings(AccessChange)
	world.Permissions = namedFacts(s).Permissions
	decl := namedDecl("named", "pve2", 150, `"cores":2,"memoryMib":1024`)
	in := protocol.ObserveParams{Resources: []protocol.Resource{decl}}
	o := observe(t, world, in)["ProxmoxLXC named"]
	if !o.Exists || !o.Adoptable || o.Owned || !maps.Equal(o.IDs, ids("pve2", 150)) {
		t.Fatalf("named: %+v", o)
	}
	var res protocol.DiffResult
	rpc(t, world, protocol.MethodDiff, protocol.DiffParams{Desired: in.Resources, Observed: []protocol.Observed{o}}, &res)
	if len(res.Changes) > 0 || !slices.Equal(ops(res.Previews), []protocol.Op{OpConfigureLXC}) {
		t.Fatalf("a named container to adopt: changes %+v, previews %v", res.Changes, ops(res.Previews))
	}

	var prep protocol.PrepareResult
	rpc(t, world, protocol.MethodPrepare, protocol.PrepareParams{Op: OpAdoptLXC, Observed: o, Desired: &decl}, &prep)
	// Observed only, the guard refuses to adopt it; a person gives "change" first.
	observeOnly := namedSettings(AccessObserve)
	world.Permissions = namedFacts(observeOnly).Permissions
	if g := guardRPC(t, world, observeOnly, protocol.GuardBefore, nil, []protocol.Change{prep.Change}); !blocks(g.Findings, "PVE-0304", `access "observe"`) {
		t.Fatalf("adopting a container observed only: %+v", g.Findings)
	}
	world.Permissions = namedFacts(s).Permissions
	before := guardRPC(t, world, s, protocol.GuardBefore, nil, []protocol.Change{prep.Change})
	if b := codes(before.Findings, protocol.SeverityBlock); len(b) > 0 {
		t.Fatalf("adopting the named container blocks: %+v", before.Findings)
	}
	if err := applyLXC(t, world, prep.Change); err != nil {
		t.Fatal(err)
	}
	after := guardRPC(t, world, s, protocol.GuardAfter, before.Baseline, nil)
	if b := codes(after.Findings, protocol.SeverityBlock); len(b) > 0 || !slices.Contains(codes(after.Findings, protocol.SeverityOK), "PVE-0400") {
		t.Fatalf("after adopting: %+v", after.Findings)
	}
	if v := world.VMs()[150]; v.Pool != "" || v.Node != "pve2" || v.Tags != "handmade;mamela-lab" || !strings.HasSuffix(v.Description, LXCMarker("named")+"\n") {
		t.Fatalf("adopted: %+v", v)
	}

	// Mamela's now (listed by its marker though it is in no pool), and changed as declared.
	o = observe(t, world, in)["ProxmoxLXC named"]
	if !o.Owned || o.Adoptable || o.Node == nil || o.Node.ID != 150 {
		t.Fatalf("adopted named: %+v", o)
	}
	rpc(t, world, protocol.MethodDiff, protocol.DiffParams{Desired: in.Resources, Observed: []protocol.Observed{o}}, &res)
	if !slices.Equal(ops(res.Changes), []protocol.Op{OpConfigureLXC}) {
		t.Fatalf("changes %v", ops(res.Changes))
	}
	before = guardRPC(t, world, s, protocol.GuardBefore, nil, res.Changes)
	if b := codes(before.Findings, protocol.SeverityBlock); len(b) > 0 {
		t.Fatalf("configuring the named container blocks: %+v", before.Findings)
	}
	if err := applyLXC(t, world, res.Changes[0]); err != nil {
		t.Fatal(err)
	}
	// Someone changes its neighbour meanwhile: the guard after says so.
	neighbour := world.VMs()[151]
	neighbour.Memory, neighbour.Serial = 4096, neighbour.Serial+1
	world.Put(neighbour)
	if after := guardRPC(t, world, s, protocol.GuardAfter, before.Baseline, nil); !blocks(after.Findings, "PVE-0403", "guest 151 (neighbour)") {
		t.Errorf("the neighbour changed during the apply: %+v", after.Findings)
	}
	if v := world.VMs()[150]; v.Cores != 2 || v.Memory != 1024 || v.Pool != "" {
		t.Errorf("named: %+v", v)
	}
	o = observe(t, world, in)["ProxmoxLXC named"]
	rpc(t, world, protocol.MethodDiff, protocol.DiffParams{Desired: in.Resources, Observed: []protocol.Observed{o}}, &res)
	if len(res.Changes) > 0 {
		t.Errorf("not converged: %+v", res.Changes)
	}

	// Migrated by a person: not the declared container any more; the guard refuses it too.
	moved := world.VMs()[150]
	moved.Node = "pve3"
	world.Put(moved)
	o = observe(t, world, in)["ProxmoxLXC named"]
	if o.Exists || o.Owned || !strings.Contains(lxcState(t, o).Missing, "on node pve3, and the declaration names it on pve2 (was it migrated?)") {
		t.Errorf("migrated: %+v %s", o, lxcState(t, o).Missing)
	}
	if g := guardRPC(t, world, s, protocol.GuardBefore, nil, nil); !blocks(g.Findings, "PVE-0305", "it was migrated") {
		t.Errorf("the guard on a migrated named container: %+v", g.Findings)
	}
}

// What a named declaration cannot be: in the range, a VM, another node, someone else's container
// (not named in the settings: the guard refuses to adopt it); and a container outside the range is
// never adopted without being named.
func TestNamedLXCRefused(t *testing.T) {
	world := namedWorld()
	got := observe(t, world, protocol.ObserveParams{Resources: []protocol.Resource{namedDecl("vm", "pve2", 152, ""),
		namedDecl("elsewhere", "pve3", 150, ""), namedDecl("none", "pve2", 153, "")}})
	for id, why := range map[string]string{
		"ProxmoxLXC vm":        "a VM cannot be named and adopted by its VMID (yet)",
		"ProxmoxLXC elsewhere": "on node pve2, and the declaration names it on pve3",
		"ProxmoxLXC none":      `[[provider.proxmox.guard.named]]: node "pve2", vmid 153, type "lxc"`,
	} {
		if o := got[id]; o.Exists || o.Adoptable || !strings.Contains(lxcState(t, o).Missing, why) {
			t.Errorf("%s: %+v %q, want %q", id, o, lxcState(t, o).Missing, why)
		}
	}
	for name, c := range map[string]struct {
		decl protocol.Resource
		why  string
	}{
		"named in the range":   {namedDecl("dns", "pve1", 7005, ""), "declare it with Proxmox.lxc"},
		"outside, not named":   {lxcDecl("neighbour", 151, ""), "declare it with Proxmox.named_lxc"},
		"named, not a VMID":    {namedDecl("x", "pve2", 99, ""), "not a Proxmox VMID"},
		"named, without node":  {namedDecl("x", "", 150, ""), "node, pool and tag are required"},
		"named, not a boolean": {declare(KindLXC, "x", `{`+lxcSite+`,"vmid":150,"named":"yes"}`), "named"},
	} {
		var res protocol.ObserveResult
		if err := call(t, world, protocol.MethodObserve, protocol.ObserveParams{Resources: []protocol.Resource{c.decl}}, &res); err == nil ||
			!strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: %v, want %q", name, err, c.why)
		}
	}

	// Declared named, but the operator did not name it: the provider sees it (the fake shows every
	// guest; Proxmox would not, without the grant), and the guard refuses to adopt it.
	decl := namedDecl("neighbour", "pve2", 151, "")
	o := observe(t, world, protocol.ObserveParams{Resources: []protocol.Resource{decl}})["ProxmoxLXC neighbour"]
	var prep protocol.PrepareResult
	rpc(t, world, protocol.MethodPrepare, protocol.PrepareParams{Op: OpAdoptLXC, Observed: o, Desired: &decl}, &prep)
	s := namedSettings(AccessChange)
	world.Permissions = namedFacts(s).Permissions
	if g := guardRPC(t, world, s, protocol.GuardBefore, nil, []protocol.Change{prep.Change}); !blocks(g.Findings, "PVE-0304", "no guest is named with it") {
		t.Errorf("adopting a container the settings do not name: %+v", g.Findings)
	}
	// Named in Mamela's pool, outside the range: not adoptable.
	inPool := world.VMs()[150]
	inPool.Pool = "lab"
	world.Put(inPool)
	decl = namedDecl("named", "pve2", 150, "")
	o = observe(t, world, protocol.ObserveParams{Resources: []protocol.Resource{decl}})["ProxmoxLXC named"]
	if o.Adoptable || o.Owned {
		t.Errorf("a named container in the pool is adoptable: %+v", o)
	}
	var adopt protocol.PrepareResult
	if err := call(t, world, protocol.MethodPrepare, protocol.PrepareParams{Op: OpAdoptLXC, Observed: o, Desired: &decl}, &adopt); err == nil {
		t.Errorf("adopting a named container in the pool: %+v", adopt.Change)
	}
	// A named container, once Mamela's, that someone put in the pool is not moved out: shown and denied.
	owned := ownedLXC(LXCState{Pool: "lab", Cores: 1, Tags: []string{"mamela-lab"}, Tagged: true})
	owned.IDs = ids("pve2", 150)
	cs, err := New().Diff(t.Context(), protocol.DiffParams{Desired: []protocol.Resource{namedDecl("app", "pve2", 150, `"cores":2`)}, Observed: []protocol.Observed{owned}})
	if err != nil || !slices.Equal(ops(cs.Changes), []protocol.Op{OpMoveLXC}) || !strings.Contains(cs.Changes[0].Reasons[0], "a named container stays out of the pool") {
		t.Errorf("a named container in the pool: %+v %v", cs.Changes, err)
	}
	// And a container in the range is still declared without being named, as before.
	if o := observe(t, world, protocol.ObserveParams{Resources: []protocol.Resource{lxcDecl("dns", 7005, "")}})["ProxmoxLXC dns"]; !o.Adoptable {
		t.Errorf("dns, in the range: %+v", o)
	}
}
