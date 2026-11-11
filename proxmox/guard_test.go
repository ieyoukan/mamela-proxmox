package proxmox

import (
	"context"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/provider/protocol"
)

var settings = GuardSettings{Node: "pve1", Pool: "lab", VMIDMin: 7000, VMIDMax: 7009, Tag: "mamela-lab", DiskStorage: "local-lvm",
	ImageStorage: "local", Bridge: "vmbr0", OperatingToken: "mamela@pve!lab", MaxVMs: 3, MinHostMemAvailableMiB: 4096, MaxStorageUsedPercent: 80}

// healthy are the facts of a cluster where nothing blocks: the token holds exactly its grants.
func healthy() *Facts {
	f := &Facts{Quorate: true, NodesTotal: 3, NodesOnline: 3, NodeOnline: true, MemAvailable: 32 << 30,
		DiskTotal: 1000 << 30, DiskUsed: 100 << 30, ImportStorages: []string{"local"}, Permissions: map[string]map[string]int{}}
	for _, g := range Grants(Provider{}.Manifest(), settings) {
		f.Permissions[g.Path] = map[string]int{}
		for _, p := range g.Privs {
			f.Permissions[g.Path][p] = 1
		}
	}
	f.Guests = []Guest{
		{VMID: 100, Type: "qemu", Node: "pve1", Name: "prod-db", Status: "running", Uptime: 5000, ConfigDigest: "a"},
		{VMID: 101, Type: "lxc", Node: "pve2", Name: "dns", Status: "running", Uptime: 9000, ConfigDigest: "b"},
		{VMID: 7000, Type: "qemu", Node: "pve1", Name: "k3s-s", Pool: "lab", Status: "running"},
	}
	return f
}

func codes(fs []protocol.Finding, sev string) []string {
	var out []string
	for _, f := range fs {
		if f.Severity == sev {
			out = append(out, f.Code)
		}
	}
	return out
}

func createChange(vmid, pool, memory, disk string) protocol.Change {
	return protocol.Change{Resource: "ProxmoxVM web", Kind: Kind, Op: OpCreate, IDs: map[string]string{"node": "pve1", "vmid": vmid, "storage": "local"},
		Requests: []protocol.Approved{
			{Method: "POST", Path: createVM.Path, Params: map[string][]string{"pool": {pool}, "memory": {memory}, "scsi0": {"local-lvm:0,import-from=local:import/x.qcow2"},
				"net0": {"virtio,bridge=vmbr0"}, "ide2": {"local:iso/seed.iso,media=cdrom"}}},
			{Method: "PUT", Path: resizeDisk.Path, Params: map[string][]string{"disk": {"scsi0"}, "size": {disk + "G"}}},
			{Method: "PUT", Path: setConfig.Path, Params: map[string][]string{"tags": {"mamela-lab"}}},
		}}
}

func TestCheckHealthy(t *testing.T) {
	fs := Check(settings, Provider{}.Manifest(), healthy(), []protocol.Change{createChange("7001", "lab", "2048", "20")})
	if b := codes(fs, protocol.SeverityBlock); len(b) > 0 {
		t.Fatalf("blocks %v: %+v", b, fs)
	}
}

// Every unsafe state blocks, with its own code.
func TestCheckBlocks(t *testing.T) {
	m := Provider{}.Manifest()
	ok := []protocol.Change{createChange("7001", "lab", "2048", "20")}
	for _, c := range []struct {
		name    string
		mutate  func(f *Facts)
		changes []protocol.Change
		want    string
	}{
		{"no quorum", func(f *Facts) { f.Quorate = false }, ok, "PVE-0001"},
		{"node offline", func(f *Facts) { f.NodeOnline = false }, ok, "PVE-0003"},
		{"token can do more", func(f *Facts) { f.Permissions["/vms"] = map[string]int{"VM.Allocate": 1} }, ok, "PVE-0101"},
		{"token reaches a VM out of the range", func(f *Facts) { f.Permissions["/vms/100"] = map[string]int{"VM.Audit": 1} }, ok, "PVE-0101"},
		{"token lacks a grant", func(f *Facts) { delete(f.Permissions["/pool/lab"], "VM.Allocate") }, ok, "PVE-0102"},
		{"memory", func(f *Facts) { f.MemAvailable = 5 << 30 }, ok, "PVE-0201"},
		{"storage", func(f *Facts) { f.DiskUsed = 790 << 30 }, ok, "PVE-0202"},
		{"no import", func(f *Facts) { f.ImportStorages = nil }, ok, "PVE-0203"},
		{"foreign guest in the range", func(f *Facts) {
			f.Guests = append(f.Guests, Guest{VMID: 7005, Type: "qemu", Node: "pve1", Name: "intruder", Status: "running"})
		}, ok, "PVE-0301"},
		{"pool guest out of the range", func(f *Facts) {
			f.Guests = append(f.Guests, Guest{VMID: 200, Type: "qemu", Node: "pve1", Name: "stray", Pool: "lab", Status: "running"})
		}, ok, "PVE-0302"},
		{"too many VMs", func(f *Facts) {}, []protocol.Change{createChange("7001", "lab", "1024", "10"), createChange("7002", "lab", "1024", "10"),
			createChange("7003", "lab", "1024", "10")}, "PVE-0303"},
		{"VMID out of the range", func(f *Facts) {}, []protocol.Change{createChange("100", "lab", "1024", "10")}, "PVE-0304"},
		{"another pool", func(f *Facts) {}, []protocol.Change{createChange("7001", "prod", "1024", "10")}, "PVE-0304"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := healthy()
			c.mutate(f)
			fs := Check(settings, m, f, c.changes)
			if !slices.Contains(codes(fs, protocol.SeverityBlock), c.want) {
				t.Errorf("want %s to block; findings %+v", c.want, fs)
			}
		})
	}
}

func TestOutsideOtherBridgeAndTag(t *testing.T) {
	c := createChange("7001", "lab", "1024", "10")
	c.Requests[0].Params["net0"] = []string{"virtio,bridge=vmbr9"}
	c.Requests[2].Params["tags"] = []string{"prod"}
	c.IDs["node"] = "pve2"
	if got := Outside(settings, c); len(got) != 3 {
		t.Errorf("want node, bridge and tag outside, got %v", got)
	}
}

func TestCompare(t *testing.T) {
	before := NewBaseline(settings, healthy())
	if len(before.Guests) != 2 {
		t.Fatalf("baseline holds the guests outside the boundary only: %+v", before.Guests)
	}
	if fs := Compare(settings, before, healthy()); len(codes(fs, protocol.SeverityBlock)) > 0 || !slices.Contains(codes(fs, protocol.SeverityOK), "PVE-0400") {
		t.Errorf("unchanged: %+v", fs)
	}
	for _, c := range []struct {
		name   string
		mutate func(f *Facts)
		want   string
	}{
		{"disappeared", func(f *Facts) { f.Guests = f.Guests[1:] }, "PVE-0401"},
		{"stopped", func(f *Facts) { f.Guests[0].Status = "stopped" }, "PVE-0402"},
		{"reconfigured", func(f *Facts) { f.Guests[1].ConfigDigest = "c" }, "PVE-0403"},
		{"restarted", func(f *Facts) { f.Guests[0].Uptime = 10 }, "PVE-0404"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := healthy()
			c.mutate(f)
			if fs := Compare(settings, before, f); !slices.Contains(codes(fs, protocol.SeverityBlock), c.want) {
				t.Errorf("want %s: %+v", c.want, fs)
			}
		})
	}
	f := healthy()
	f.Guests = append(f.Guests, Guest{VMID: 300, Type: "qemu", Node: "pve1", Name: "new", Status: "running"})
	if fs := Compare(settings, before, f); !slices.Contains(codes(fs, protocol.SeverityWarn), "PVE-0405") {
		t.Errorf("a new guest outside is a warning: %+v", fs)
	}
}

func TestParseGuardSettingsRejects(t *testing.T) {
	for _, in := range []string{
		``,
		`{"node":"pve1"}`,
		`{"node":"pve1","pool":"lab","vmid_min":7000,"vmid_max":7009,"tag":"t","disk_storage":"d","image_storage":"i","bridge":"b","operating_token":"mamela@pve","max_vms":1,"max_storage_used_percent":80}`,
		`{"node":"pve1","pool":"lab","vmid_min":7000,"vmid_max":7009,"tag":"t","disk_storage":"d","image_storage":"i","bridge":"b","operating_token":"a@pve!b","max_vms":1,"max_storage_used_percent":80,"extra":1}`,
		`{"node":"pve1","pool":"lab","vmid_min":7000,"vmid_max":10,"tag":"t","disk_storage":"d","image_storage":"i","bridge":"b","operating_token":"a@pve!b","max_vms":1,"max_storage_used_percent":80}`,
	} {
		if _, err := ParseGuardSettings([]byte(in)); err == nil {
			t.Errorf("accepted %s", in)
		}
	}
}

func observed(t *testing.T, st State, adoptable bool) protocol.Observed {
	b, _ := json.Marshal(st)
	return protocol.Observed{ID: "ProxmoxVM web", Kind: Kind, Name: "web", Exists: true, Owned: !adoptable, Adoptable: adoptable,
		IDs: map[string]string{"node": "pve1", "vmid": "7001"}, Version: "d1", State: b}
}

func TestPrepare(t *testing.T) {
	p := Provider{}
	ctx := context.Background()
	got, err := p.Prepare(ctx, protocol.PrepareParams{Op: OpDelete, Observed: observed(t, State{Running: true}, false)})
	if err != nil || len(got.Change.Requests) != 2 || got.Change.Requests[0].Path != stopVM.Path || got.Change.Requests[1].Method != "DELETE" {
		t.Errorf("delete of a running VM stops it first: %+v %v", got.Change.Requests, err)
	}
	if _, err := p.Prepare(ctx, protocol.PrepareParams{Op: OpReboot, Observed: observed(t, State{}, false)}); err == nil {
		t.Error("rebooting a stopped VM is refused")
	}
	if _, err := p.Prepare(ctx, protocol.PrepareParams{Op: OpAdopt, Observed: observed(t, State{Running: true}, false)}); err == nil {
		t.Error("adopting a VM that is not adoptable is refused")
	}
	if _, err := p.Prepare(ctx, protocol.PrepareParams{Op: OpCreate, Observed: observed(t, State{}, false)}); err == nil {
		t.Error("prepare refuses an operation that is not explicit")
	}
	o := observed(t, State{Image: "ubuntu-24.04", Network: "dhcp"}, true)
	o.Exists = false
	if _, err := p.Prepare(ctx, protocol.PrepareParams{Op: OpDelete, Observed: o}); err == nil {
		t.Error("prepare refuses a VM that does not exist")
	}
}

func TestOldMarker(t *testing.T) {
	ok := OldMarker + "\nmamela-name: web\nmamela-image: ubuntu-24.04\nmamela-state: ready\n"
	if m, found := parseOldMarker(ok); !found || m.Name != "web" || m.Network != "dhcp" {
		t.Errorf("%+v %v", m, found)
	}
	for _, bad := range []string{strings.Replace(ok, "ready", "creating", 1), Marker + "\nmamela-name: web\n", "hello"} {
		if _, found := parseOldMarker(bad); found {
			t.Errorf("adoptable: %q", bad)
		}
	}
}

func startChange(vmid string) protocol.Change {
	return protocol.Change{Resource: "ProxmoxVM x", Kind: Kind, Op: OpStart, IDs: map[string]string{"node": "pve1", "vmid": vmid},
		Requests: []protocol.Approved{{Method: "POST", Path: startVM.Path}}, Late: true}
}

// Starting a stopped VM needs its memory: with 5 GiB available and 4 GiB to keep, an 8 GiB VM may
// not start (the guard used to count 0 MiB for a start).
func TestStartingAStoppedVMCountsItsMemory(t *testing.T) {
	f := healthy()
	f.MemAvailable = 5 << 30
	f.Guests = append(f.Guests, Guest{VMID: 7001, Type: "qemu", Node: "pve1", Name: "big", Pool: "lab", Status: "stopped", MemoryMiB: 8192, DiskGiB: 20})
	fs := Check(settings, Provider{}.Manifest(), f, []protocol.Change{startChange("7001")})
	if !slices.Contains(codes(fs, protocol.SeverityBlock), "PVE-0201") {
		t.Fatalf("an 8 GiB start with 5 GiB available: %+v", fs)
	}
	if n := NeedOf([]protocol.Change{startChange("7001")}, f.Guests); n.MemoryMiB != 8192 {
		t.Fatalf("need %+v", n)
	}
}

func TestNeedCountsEachVMOnce(t *testing.T) {
	guests := []Guest{
		{VMID: 7001, Type: "qemu", Status: "running", MemoryMiB: 2048, DiskGiB: 10},
		{VMID: 7002, Type: "qemu", Status: "stopped", MemoryMiB: 1024, DiskGiB: 10},
	}
	resize := func(vmid, memory string) protocol.Change {
		return protocol.Change{Op: OpResize, IDs: map[string]string{"vmid": vmid},
			Requests: []protocol.Approved{{Method: "PUT", Path: setConfig.Path, Params: map[string][]string{"memory": {memory}}}}}
	}
	grow := func(vmid, size string) protocol.Change {
		return protocol.Change{Op: OpGrow, IDs: map[string]string{"vmid": vmid},
			Requests: []protocol.Approved{{Method: "PUT", Path: resizeDisk.Path, Params: map[string][]string{"size": {size}}}}}
	}
	create := createChange("7003", "lab", "2048", "20")
	create.IDs["vmid"] = "7003"
	for _, c := range []struct {
		name    string
		changes []protocol.Change
		memory  int64
		disk    int64
		creates int
	}{
		{"create and its start, once", []protocol.Change{create, startChange("7003")}, 2048, 20, 1},
		{"a running VM grows its memory by 1 GiB", []protocol.Change{resize("7001", "3072")}, 1024, 0, 0},
		{"a running VM shrinks its memory", []protocol.Change{resize("7001", "1024")}, 0, 0, 0},
		{"a stopped VM resized and started: its new memory", []protocol.Change{resize("7002", "4096"), startChange("7002")}, 4096, 0, 0},
		{"a stopped VM resized, not started", []protocol.Change{resize("7002", "4096")}, 0, 0, 0},
		{"a disk grown by 10 GiB", []protocol.Change{grow("7001", "20G")}, 0, 10, 0},
		{"an unknown VM: at the asked size", []protocol.Change{resize("7999", "512")}, 512, 0, 0},
	} {
		n := NeedOf(c.changes, guests)
		if n.MemoryMiB != c.memory || n.DiskGiB != c.disk || n.Creates != c.creates {
			t.Errorf("%s: %+v, want %d MiB, %d GiB, %d creates", c.name, n, c.memory, c.disk, c.creates)
		}
	}
}

