package proxmox

import (
	"context"
	"encoding/json/v2"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/provider/protocol"
)

const testSite = `"node":"pve1","pool":"lab","diskStorage":"local-lvm","imageStorage":"local","bridge":"vmbr0","tag":"mamela-lab","vmidMin":7000,"vmidMax":7009`

func ops(changes []protocol.Change) []protocol.Op {
	var out []protocol.Op
	for _, c := range changes {
		out = append(out, c.Op)
	}
	return out
}

// What the provider never changes (a smaller disk, another image, another network) is reported as
// a change of an unsupported operation with nothing to send, not as "no difference".
func TestUnsupportedDifferencesAreReported(t *testing.T) {
	observed := func(st State) protocol.Observed {
		b, _ := json.Marshal(st)
		return protocol.Observed{ID: "ProxmoxVM web", Kind: Kind, Name: "web", Exists: true, Owned: true, Version: "d1",
			IDs: map[string]string{"node": "pve1", "vmid": "7001"}, State: b}
	}
	had := State{CPU: 1, MemoryMiB: 1024, DiskGiB: 20, Image: "ubuntu-24.04", Network: "dhcp", Tagged: true, Running: true, Pool: "lab"}
	for _, c := range []struct {
		name, spec string
		want       []protocol.Op
	}{
		{"same", `"cpu":1,"memoryMiB":1024,"diskGiB":20,"image":"ubuntu-24.04","network":{"mode":"dhcp"}`, nil},
		{"smaller disk", `"cpu":1,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04","network":{"mode":"dhcp"}`, []protocol.Op{OpShrink}},
		{"static network", `"cpu":1,"memoryMiB":1024,"diskGiB":20,"image":"ubuntu-24.04","network":{"mode":"static","address":"192.0.2.9/24","gateway":"192.0.2.1","dns":["192.0.2.1"]}`, []protocol.Op{OpChangeNetwork}},
		{"bigger and smaller", `"cpu":2,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04","network":{"mode":"dhcp"}`, []protocol.Op{OpResize, OpShrink}},
	} {
		r := protocol.Resource{ID: "ProxmoxVM web", Kind: Kind, Name: "web", Spec: []byte(`{` + testSite + `,` + c.spec + `}`)}
		out, err := New().Diff(context.Background(), protocol.DiffParams{Desired: []protocol.Resource{r}, Observed: []protocol.Observed{observed(had)}})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !slices.Equal(ops(out.Changes), c.want) {
			t.Errorf("%s: %v, want %v", c.name, ops(out.Changes), c.want)
		}
		for _, ch := range out.Changes {
			if o, _ := New().Manifest().Operation(Kind, ch.Op); o.Support == protocol.Unsupported && (len(ch.Requests) > 0 || len(ch.Reasons) == 0) {
				t.Errorf("%s: unsupported %s sends %v (reasons %v)", c.name, ch.Op, ch.Requests, ch.Reasons)
			}
		}
	}
	// Another image is reported too.
	other := had
	other.Image = "debian-12"
	r := protocol.Resource{ID: "ProxmoxVM web", Kind: Kind, Name: "web", Spec: []byte(`{` + testSite + `,"cpu":1,"memoryMiB":1024,"diskGiB":20,"image":"ubuntu-24.04","network":{"mode":"dhcp"}}`)}
	out, _ := New().Diff(context.Background(), protocol.DiffParams{Desired: []protocol.Resource{r}, Observed: []protocol.Observed{observed(other)}})
	if !slices.Equal(ops(out.Changes), []protocol.Op{OpReplaceImage}) {
		t.Errorf("another image: %v", ops(out.Changes))
	}
}

// A VM with the marker but without the tag is only unfinished: Mamela keeps it as its own only if
// it recorded creating it.
func TestOwnershipNeedsTheTag(t *testing.T) {
	if owned, unfinished := Ownership(true); !owned || unfinished {
		t.Fatal("a tagged VM")
	}
	if owned, unfinished := Ownership(false); owned || !unfinished {
		t.Fatal("an untagged VM is claimed as owned")
	}
}

// The start planned with a create is exactly the start of the VM found stopped later (after a
// create whose answer was lost): Mamela goes on with the approved plan only while the changes are
// the same. Both are late, so that every create of the apply comes first.
func TestStartIsTheSameChangeEitherWay(t *testing.T) {
	spec := []byte(`{` + testSite + `,"cpu":1,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04","network":{"mode":"dhcp"}}`)
	r := protocol.Resource{ID: "ProxmoxVM web", Kind: Kind, Name: "web", Spec: spec}
	free, _ := json.Marshal(State{FreeVMIDs: []int64{7001}})
	created, err := New().Diff(context.Background(), protocol.DiffParams{Desired: []protocol.Resource{r},
		Observed: []protocol.Observed{{ID: r.ID, Kind: Kind, Name: "web", State: free}}})
	if err != nil || len(created.Changes) != 2 {
		t.Fatalf("%v %v", created.Changes, err)
	}
	stopped, _ := json.Marshal(State{CPU: 1, MemoryMiB: 1024, DiskGiB: 10, Image: "ubuntu-24.04", Network: "dhcp", Tagged: true, Pool: "lab"})
	later, err := New().Diff(context.Background(), protocol.DiffParams{Desired: []protocol.Resource{r},
		Observed: []protocol.Observed{{ID: r.ID, Kind: Kind, Name: "web", Exists: true, Owned: true, Version: "d1",
			IDs: map[string]string{"node": "pve1", "vmid": "7001"}, State: stopped}}})
	if err != nil || len(later.Changes) != 1 {
		t.Fatalf("%v %v", later.Changes, err)
	}
	a, _ := json.Marshal(created.Changes[1], json.Deterministic(true))
	b, _ := json.Marshal(later.Changes[0], json.Deterministic(true))
	if string(a) != string(b) || !later.Changes[0].Late {
		t.Errorf("the starts differ:\n%s\n%s", a, b)
	}
}

// A template is never deleted while a VM is a linked clone of it: delete-template refuses, and
// says which VMs to delete first.
func TestDeleteTemplateWithClonesIsRefused(t *testing.T) {
	o := protocol.Observed{ID: "ProxmoxTemplate ubuntu-24.04-pve1", Kind: KindTemplate, Name: "ubuntu-24.04-pve1", Exists: true, Owned: true,
		IDs: map[string]string{"node": "pve1", "vmid": "7009"}, State: []byte(`{"image":"ubuntu-24.04","template":true,"tagged":true,"clones":[7001]}`)}
	if _, err := New().Prepare(t.Context(), protocol.PrepareParams{Op: OpDeleteTemplate, Observed: o}); err == nil ||
		!strings.Contains(err.Error(), "7001") {
		t.Fatalf("a template with a clone: %v", err)
	}
	o.State = []byte(`{"image":"ubuntu-24.04","template":true,"tagged":true}`)
	r, err := New().Prepare(t.Context(), protocol.PrepareParams{Op: OpDeleteTemplate, Observed: o})
	if err != nil || len(r.Change.Requests) != 1 || r.Change.Requests[0].Method != "DELETE" {
		t.Fatalf("a template without clones: %+v %v", r.Change, err)
	}
	if _, err := New().Prepare(t.Context(), protocol.PrepareParams{Op: OpReboot, Observed: o}); err == nil {
		t.Error("a template is rebooted")
	}
}

// The VMIDs other plans are to create (Mamela's claims) are taken: plans made one after another
// and applied together do not choose the same VMID.
func TestClaimedVMIDsAreTaken(t *testing.T) {
	vms := map[int64]resource{7000: {VMID: 7000}}
	claimed := []map[string]string{{"node": "pve1", "vmid": "7001"}, {"node": "pve2", "vmid": "7002"}, {"node": "pve1", "vmid": "7009", "newid": "7009"}}
	if got := freeVMIDs(vms, claimed, "pve1", 7000, 7009, false); !slices.Equal(got[:3], []int64{7002, 7003, 7004}) {
		t.Errorf("from the bottom: %v", got)
	}
	if got := freeVMIDs(vms, claimed, "pve1", 7000, 7009, true); got[0] != 7008 {
		t.Errorf("from the top: %v", got)
	}
	if got := freeVMIDs(vms, nil, "pve1", 7000, 7001, false); !slices.Equal(got, []int64{7001}) {
		t.Errorf("no claims: %v", got)
	}
}

func diffOne(t *testing.T, spec string, o protocol.Observed) []protocol.Change {
	t.Helper()
	r := protocol.Resource{ID: "ProxmoxVM web", Kind: Kind, Name: "web", Spec: []byte(`{` + testSite + `,` + spec + `}`)}
	o.ID, o.Kind, o.Name = r.ID, Kind, "web"
	out, err := New().Diff(context.Background(), protocol.DiffParams{Desired: []protocol.Resource{r}, Observed: []protocol.Observed{o}})
	if err != nil {
		t.Fatal(err)
	}
	return out.Changes
}

func owned(st State) protocol.Observed {
	b, _ := json.Marshal(st)
	return protocol.Observed{Exists: true, Owned: true, Version: "d1", IDs: map[string]string{"node": "pve1", "vmid": "7001", "storage": "local"}, State: b}
}

// A VM declared stopped is shut down when it runs (late, after the configuration's changes, which
// then need no restart), and never started; one declared running is started.
func TestPowerAsDeclared(t *testing.T) {
	const base = `"cpu":1,"memoryMiB":1024,"diskGiB":20,"image":"ubuntu-24.04","network":{"mode":"dhcp"}`
	running := State{CPU: 1, MemoryMiB: 1024, DiskGiB: 20, Image: "ubuntu-24.04", Network: "dhcp", Tagged: true, Running: true, Pool: "lab"}
	cs := diffOne(t, base+`,"stopped":true`, owned(running))
	if !slices.Equal(ops(cs), []protocol.Op{OpShutdown}) || !cs[0].Late || cs[0].Requests[0].Path != shutdownVM.Path {
		t.Errorf("declared stopped, running: %+v", cs)
	}
	stopped := running
	stopped.Running = false
	if cs := diffOne(t, base+`,"stopped":true`, owned(stopped)); len(cs) != 0 {
		t.Errorf("declared stopped, stopped: %v", ops(cs))
	}
	if cs := diffOne(t, base+`,"stopped":false`, owned(stopped)); !slices.Equal(ops(cs), []protocol.Op{OpStart}) {
		t.Errorf("declared running, stopped: %v", ops(cs))
	}
	// Resized while declared stopped: no restart, then the shutdown.
	cs = diffOne(t, strings.Replace(base, `"cpu":1`, `"cpu":2`, 1)+`,"stopped":true`, owned(running))
	if !slices.Equal(ops(cs), []protocol.Op{OpResize, OpShutdown}) || len(cs[0].Requests) != 1 {
		t.Errorf("resized while declared stopped: %+v", cs)
	}
	// Created stopped: no start.
	free, _ := json.Marshal(State{FreeVMIDs: []int64{7001}})
	if cs := diffOne(t, base+`,"stopped":true`, protocol.Observed{State: free}); !slices.Equal(ops(cs), []protocol.Op{OpCreate}) {
		t.Errorf("created stopped: %v", ops(cs))
	}
}

// A VM that is not where its declaration puts it (another pool or node, a VMID outside the range)
// is never moved: the difference is shown and denied, and nothing else is planned for it.
func TestMovedIsUnsupported(t *testing.T) {
	const base = `"cpu":2,"memoryMiB":1024,"diskGiB":20,"image":"ubuntu-24.04","network":{"mode":"dhcp"}`
	st := State{CPU: 1, MemoryMiB: 1024, DiskGiB: 20, Image: "ubuntu-24.04", Network: "dhcp", Tagged: true, Running: true, Pool: "prod"}
	cs := diffOne(t, base, owned(st))
	if !slices.Equal(ops(cs), []protocol.Op{OpMove}) || len(cs[0].Requests) > 0 || !strings.Contains(cs[0].Reasons[0], `pool "prod"`) {
		t.Errorf("in another pool: %+v", cs)
	}
	st.Pool = "lab"
	o := owned(st)
	o.IDs["vmid"] = "150"
	if cs := diffOne(t, base, o); !slices.Equal(ops(cs), []protocol.Op{OpMove}) {
		t.Errorf("outside the range: %v", ops(cs))
	}
	if op, _ := New().Manifest().Operation(Kind, OpMove); op.Support != protocol.Unsupported {
		t.Error("move-vm is carried out")
	}
}

// A VM imported to adopt (no image) that is not there is never created: Mamela makes VMs only from
// its pinned images.
func TestNoImageIsNeverCreated(t *testing.T) {
	free, _ := json.Marshal(State{FreeVMIDs: []int64{7001}})
	cs := diffOne(t, `"cpu":1,"memoryMiB":1024,"diskGiB":20,"network":{"mode":"dhcp"}`, protocol.Observed{State: free})
	if !slices.Equal(ops(cs), []protocol.Op{OpCreateNoImage}) || len(cs[0].Requests) > 0 {
		t.Errorf("%+v", cs)
	}
	if op, _ := New().Manifest().Operation(Kind, OpCreateNoImage); op.Support != protocol.Unsupported {
		t.Error("creating a VM without an image is carried out")
	}
	if _, err := spec([]byte(`{` + testSite + `,"cpu":1,"memoryMiB":1024,"diskGiB":20,"network":{"mode":"dhcp"},"template":"t"}`)); err == nil {
		t.Error("a VM without an image is a clone of a template")
	}
}

// Adopting a VM imported from Terraform writes the marker (the declaration's network) and the tag,
// on the configuration observed, and nothing else; one Mamela cannot manage, or outside the
// boundary, is refused with why.
func TestAdoptImported(t *testing.T) {
	decl := protocol.Resource{ID: "ProxmoxVM web", Kind: Kind, Name: "web",
		Spec: []byte(`{` + testSite + `,"cpu":2,"memoryMiB":2048,"diskGiB":20,"network":{"mode":"static","address":"192.0.2.50/24","gateway":"192.0.2.1","dns":["192.0.2.1"]}}`)}
	found := State{CPU: 2, MemoryMiB: 2048, DiskGiB: 20, Running: true, Pool: "lab", Tags: []string{"terraform"}, Origin: originUnmarked,
		Sockets: 1, DiskStorage: "local-lvm", Bridge: "vmbr0"}
	observed := func(st State) protocol.Observed {
		b, _ := json.Marshal(st)
		return protocol.Observed{ID: decl.ID, Kind: Kind, Name: "web", Exists: true, Adoptable: true, Version: "d7",
			IDs: map[string]string{"node": "pve1", "vmid": "7005"}, State: b}
	}
	r, err := New().Prepare(t.Context(), protocol.PrepareParams{Op: OpAdopt, Observed: observed(found), Desired: &decl})
	if err != nil {
		t.Fatal(err)
	}
	c := r.Change
	if c.Precondition != "d7" || len(c.Requests) != 1 || c.Requests[0].Params["tags"][0] != "mamela-lab" || c.Requests[0].Params["digest"][0] != "d7" ||
		!strings.Contains(c.Requests[0].Params["description"][0], "mamela-network: static 192.0.2.50/24 via 192.0.2.1 dns 192.0.2.1") ||
		!maps.Equal(c.IDs, map[string]string{"node": "pve1", "vmid": "7005"}) {
		t.Errorf("%+v", c)
	}
	for name, edit := range map[string]func(*State){
		"two sockets":              func(s *State) { s.Sockets = 2 },
		"another storage":          func(s *State) { s.DiskStorage = "ceph" },
		"another bridge":           func(s *State) { s.Bridge = "vmbr1" },
		"another pool":             func(s *State) { s.Pool = "prod" },
		"Mamela's own (no origin)": func(s *State) { s.Origin = "" },
	} {
		st := found
		edit(&st)
		if _, err := New().Prepare(t.Context(), protocol.PrepareParams{Op: OpAdopt, Observed: observed(st), Desired: &decl}); err == nil {
			t.Errorf("%s: adopted", name)
		}
	}
	// A running VM is stopped by hand; a stopped one is not.
	o := observed(found)
	o.Adoptable, o.Owned = false, true
	if r, err := New().Prepare(t.Context(), protocol.PrepareParams{Op: OpStop, Observed: o}); err != nil || r.Change.Requests[0].Path != stopVM.Path {
		t.Errorf("stop-vm: %+v %v", r.Change, err)
	}
	found.Running = false
	o = observed(found)
	o.Adoptable, o.Owned = false, true
	if _, err := New().Prepare(t.Context(), protocol.PrepareParams{Op: OpStop, Observed: o}); err == nil {
		t.Error("a stopped VM is stopped")
	}
}
