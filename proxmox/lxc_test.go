package proxmox

import (
	"context"
	"encoding/json/v2"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela-proxmox/fakepve"
	"github.com/ieyoukan/mamela/provider/protocol"
)

const lxcSite = `"node":"pve1","pool":"lab","tag":"mamela-lab","vmidMin":7000,"vmidMax":7009`

// lxcDecl declares a container of the cases' site by its VMID, with the values it manages.
func lxcDecl(name string, vmid int64, rest string) protocol.Resource {
	spec := `{` + lxcSite + `,"vmid":` + strconv.FormatInt(vmid, 10)
	if rest != "" {
		spec += "," + rest
	}
	return declare(KindLXC, name, spec+`}`)
}

// handmade is a container a person made: in the pool, running, without a mark of Mamela's, with a
// description and tags people rely on. The values are fictional.
func handmade(vmid int64, name string) fakepve.VM {
	return fakepve.VM{VMID: vmid, Name: name, Node: "pve1", Pool: "lab", Type: "lxc", Cores: 1, Memory: 512, Swap: 512, DiskGiB: 4,
		Running: true, Uptime: 600, Tags: "handmade", Description: "hand-made container: " + name + "\nask the team before changing it\n"}
}

// lxcWorld is a cluster with containers in and around the boundary:
//
//   - 7003 vmmarked: a container whose description carries a VM's marker;
//   - 7004 app: Mamela's (adopted: its marker, the tag);
//   - 7005 dns: made by hand, to adopt;
//   - 7006 elsewhere: made by hand, in pool prod;
//   - 7007 other: Mamela's, for another declaration;
//   - 7008 vm: a VM (qemu), made by hand;
//   - 105: someone else's container, in no pool.
func lxcWorld() *fakepve.Server {
	f := fakepve.New()
	vmMarked := handmade(7003, "vmmarked")
	vmMarked.Description = marker{Name: "vmmarked", Image: "ubuntu-24.04", Network: "dhcp"}.String()
	f.Put(vmMarked)
	app := handmade(7004, "app")
	app.Description += LXCMarker("app") + "\n"
	app.Tags = "handmade;mamela-lab"
	f.Put(app)
	f.Put(handmade(7005, "dns"))
	elsewhere := handmade(7006, "elsewhere")
	elsewhere.Pool = "prod"
	f.Put(elsewhere)
	other := handmade(7007, "other")
	other.Description, other.Tags = LXCMarker("other")+"\n", "mamela-lab"
	f.Put(other)
	f.Put(fakepve.VM{VMID: 7008, Name: "vm", Node: "pve1", Pool: "lab", Cores: 1, Memory: 1024, DiskGiB: 10, Running: true, Net0: "virtio,bridge=vmbr0"})
	f.Put(fakepve.VM{VMID: 105, Name: "someone-else", Node: "pve1", Type: "lxc", Running: true, Memory: 2048, DiskGiB: 8})
	return f
}

func sameParams(a, b map[string][]string) bool {
	return maps.EqualFunc(a, b, func(x, y []string) bool { return slices.Equal(x, y) })
}

func lxcState(t *testing.T, o protocol.Observed) LXCState {
	t.Helper()
	var st LXCState
	if len(o.State) > 0 {
		if err := json.Unmarshal(o.State, &st); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// A container's marker is one line of its own, appended to what people wrote; a VM's marker is
// never a container's, nor a container's a VM's.
func TestLXCMarker(t *testing.T) {
	notes := "hand-made container: dns\nask the team before changing it\n"
	for _, d := range []string{
		withMarker(notes, "dns"),
		strings.TrimSuffix(withMarker(notes, "dns"), "\n"), // the last line, without a newline
		LXCMarker("dns"),
		notes + "  " + LXCMarker("dns") + "  \nmore notes below it\n",
	} {
		if name, ok := parseLXCMarker(d); !ok || name != "dns" {
			t.Errorf("%q: %q %v", d, name, ok)
		}
		if !anyMark(d) {
			t.Errorf("%q: not marked", d)
		}
		if _, ok := parseMarker(d); ok {
			t.Errorf("%q: read as a VM's marker", d)
		}
		if _, ok := parseTemplateMarker(d); ok {
			t.Errorf("%q: read as a template's marker", d)
		}
	}
	// What people wrote stays, byte for byte.
	if got := withMarker(notes, "dns"); !strings.HasPrefix(got, notes) || got != notes+LXCMarker("dns")+"\n" {
		t.Errorf("%q", got)
	}
	if got := withMarker("no newline", "dns"); got != "no newline\n"+LXCMarker("dns")+"\n" {
		t.Errorf("%q", got)
	}
	if got := withMarker("", "dns"); got != LXCMarker("dns")+"\n" {
		t.Errorf("%q", got)
	}
	for _, d := range []string{
		marker{Name: "web", Image: "ubuntu-24.04", Network: "dhcp"}.String(),
		templateMarker("ubuntu-24.04-pve1", "ubuntu-24.04"),
		OldMarker + "\nmamela-name: web\nmamela-image: ubuntu-24.04\nmamela-state: ready\n",
		LXCMarker("a") + "\n" + LXCMarker("b") + "\n", // two: whose is it?
		lxcMarkerPrefix + "two words" + lxcMarkerSuffix,
		lxcMarkerPrefix + "dns. Do not edit.",
		"notes only\n",
	} {
		if name, ok := parseLXCMarker(d); ok {
			t.Errorf("%q: read as container %q's marker", d, name)
		}
	}
	if anyMark("notes only\n") || !anyMark("notes\n"+Marker+"\n") || !anyMark(templateMarker("t", "i")) {
		t.Error("anyMark")
	}
}

// The live Control Plane's records vouch for a container by its observed IDs: node and vmid, the
// same before and after it is adopted. Mamela's container is a node; one to adopt is not yet.
// VMs and containers never mix: the VM kind lists no container, and the container kind no VM.
func TestLXCObservedIDsKeepTheirShape(t *testing.T) {
	got := observe(t, lxcWorld(), protocol.ObserveParams{Resources: []protocol.Resource{lxcDecl("app", 7004, ""), lxcDecl("dns", 7005, ""),
		declare(Kind, "b", vmSpec)}})
	for id, want := range map[string]map[string]string{
		"ProxmoxLXC app":   {"node": "pve1", "vmid": "7004"},
		"ProxmoxLXC dns":   {"node": "pve1", "vmid": "7005"},
		"ProxmoxLXC other": {"node": "pve1", "vmid": "7007"},
	} {
		if !maps.Equal(got[id].IDs, want) {
			t.Errorf("%s: IDs %v, want %v", id, got[id].IDs, want)
		}
	}
	record := []map[string]string{{"node": "pve1", "vmid": "7004"}}
	if o := protocol.Vouch(got["ProxmoxLXC app"], record); !o.Owned || o.Unfinished {
		t.Errorf("the record of adopting app does not vouch for it: %+v", got["ProxmoxLXC app"])
	}
	if o := got["ProxmoxLXC app"]; o.Node == nil || o.Node.ID != 7004 || !o.Node.Running || o.Node.UptimeSeconds != 600 || o.Adoptable {
		t.Errorf("app is Mamela's running node: %+v %+v", o, o.Node)
	}
	if st := lxcState(t, got["ProxmoxLXC app"]); !st.Tagged || st.Cores != 1 || st.MemoryMiB != 512 || st.SwapMiB != 512 || st.RootfsGiB != 4 ||
		st.RootfsStorage != "local-lvm" || st.Description != "" || st.Uptime != 0 {
		t.Errorf("app's state: %+v", st)
	}
	if o := got["ProxmoxLXC dns"]; !o.Exists || !o.Adoptable || o.Owned || o.Node != nil || o.Version == "" {
		t.Errorf("dns is to adopt: %+v", o)
	}
	if st := lxcState(t, got["ProxmoxLXC dns"]); !strings.HasPrefix(st.Description, "hand-made container: dns") || !slices.Equal(st.Tags, []string{"handmade"}) {
		t.Errorf("dns's state has what adopting it keeps: %+v", st)
	}
	if o := got["ProxmoxLXC other"]; !o.Owned || o.Node == nil {
		t.Errorf("an undeclared container with the marker is reported as Mamela's (for Mamela to keep): %+v", o)
	}
	for id := range got {
		switch id {
		case "ProxmoxLXC app", "ProxmoxLXC dns", "ProxmoxLXC other", "ProxmoxVM b":
		default:
			t.Errorf("%s observed: containers and VMs mix", id)
		}
	}
	if o := got["ProxmoxVM b"]; o.Exists {
		t.Errorf("a VM declaration finds a container: %+v", o)
	}
}

// A declaration that names, by its VMID, the container another declaration has (it carries that
// one's marker) is an error of the declaration: observe fails and says so (kit), rather than
// planning a container to create there.
func TestLXCNamingMamelasContainerFails(t *testing.T) {
	var res protocol.ObserveResult
	err := call(t, lxcWorld(), protocol.MethodObserve, protocol.ObserveParams{Resources: []protocol.Resource{lxcDecl("taken", 7007, "")}}, &res)
	if err == nil || !strings.Contains(err.Error(), "Mamela's already (ProxmoxLXC other)") {
		t.Fatalf("observe of a declaration naming another's container: %v", err)
	}
}

// A container is adoptable only inside the declared boundary, without a mark of Mamela's, and only
// a container: anything else at the declared VMID is reported, and Mamela never creates one.
func TestLXCAdoptableOnlyInsideTheBoundary(t *testing.T) {
	decls := []protocol.Resource{lxcDecl("dns", 7005, `"cores":2`), lxcDecl("elsewhere", 7006, ""),
		lxcDecl("qemu", 7008, ""), lxcDecl("vmmarked", 7003, ""), lxcDecl("none", 7009, "")}
	got := observe(t, lxcWorld(), protocol.ObserveParams{Resources: decls})
	if o := got["ProxmoxLXC dns"]; !o.Adoptable || o.Owned {
		t.Errorf("dns (inside): %+v", o)
	}
	if o := got["ProxmoxLXC elsewhere"]; !o.Exists || o.Adoptable || o.Owned {
		t.Errorf("elsewhere (pool prod) is adoptable or Mamela's: %+v", o)
	}
	if st := lxcState(t, got["ProxmoxLXC elsewhere"]); st.Description != "" {
		t.Errorf("the description of a container outside the boundary is kept: %+v", st)
	}
	for id, why := range map[string]string{
		"ProxmoxLXC qemu":     "is a VM (qemu), not a container",
		"ProxmoxLXC vmmarked": "a mark of Mamela's that is not a container's marker",
		"ProxmoxLXC none":     "no container has VMID 7009",
	} {
		o := got[id]
		if o.Exists || o.Adoptable || !strings.Contains(lxcState(t, o).Missing, why) {
			t.Errorf("%s: %+v, want missing: %s", id, o, why)
		}
	}
	var observed []protocol.Observed
	for _, o := range got {
		observed = append(observed, o)
	}
	var res protocol.DiffResult
	rpc(t, lxcWorld(), protocol.MethodDiff, protocol.DiffParams{Desired: decls, Observed: observed}, &res)
	creates := map[string]protocol.Change{}
	for _, c := range res.Changes {
		if c.Op != OpCreateLXC || len(c.Requests) > 0 {
			t.Errorf("%s: %s %v", c.Resource, c.Op, c.Requests)
		}
		creates[c.Resource] = c
	}
	if len(creates) != 3 || !strings.Contains(creates["ProxmoxLXC qemu"].Reasons[0], "declare it as a ProxmoxVM") ||
		!strings.Contains(creates["ProxmoxLXC none"].Reasons[0], "Mamela does not create containers") {
		t.Errorf("creates: %+v", res.Changes)
	}
	if len(res.Previews) != 1 || res.Previews[0].Resource != "ProxmoxLXC dns" {
		t.Errorf("previews: %+v", res.Previews)
	}
	if op, _ := New().Manifest().Operation(KindLXC, OpCreateLXC); op.Support != protocol.Unsupported {
		t.Error("create-lxc is carried out")
	}

	// On another node than declared: not the declared container.
	other := declare(KindLXC, "dns", `{"node":"pve2","pool":"lab","tag":"mamela-lab","vmidMin":7000,"vmidMax":7009,"vmid":7005}`)
	got = observe(t, lxcWorld(), protocol.ObserveParams{Resources: []protocol.Resource{other}})
	if o := got["ProxmoxLXC dns"]; o.Exists || !strings.Contains(lxcState(t, o).Missing, "on node pve1") {
		t.Errorf("on another node: %+v", o)
	}
	// A VM declaration never finds a container to adopt, even by its IDs.
	imported := strings.Replace(vmSpec, `"image":"ubuntu-24.04",`, ``, 1)
	got = observe(t, lxcWorld(), protocol.ObserveParams{Resources: []protocol.Resource{declare(Kind, "dns", imported)},
		Adopting: []protocol.Adopting{{Resource: "ProxmoxVM dns", IDs: ids("pve1", 7005)}}})
	if o := got["ProxmoxVM dns"]; o.Exists {
		t.Errorf("a VM declaration adopts a container: %+v", o)
	}
	// A VMID outside the declared range is not a declaration Mamela takes.
	var out protocol.ObserveResult
	err := call(t, lxcWorld(), protocol.MethodObserve, protocol.ObserveParams{Resources: []protocol.Resource{lxcDecl("far", 150, "")}}, &out)
	if err == nil || !strings.Contains(err.Error(), "outside the declared VMIDs") {
		t.Errorf("a VMID outside the range: %v", err)
	}
}

// Until a person adopts it, the plan shows what would change of a container (previews), and
// nothing is planned; its tag and marker are not previewed (adopting writes them).
func TestLXCPreviewsBeforeAdopting(t *testing.T) {
	decl := lxcDecl("dns", 7005, `"cores":2,"memoryMib":1024,"swapMib":256,"rootfsGib":8,"onboot":true,"power":"stopped"`)
	got := observe(t, lxcWorld(), protocol.ObserveParams{Resources: []protocol.Resource{decl}})
	var res protocol.DiffResult
	rpc(t, lxcWorld(), protocol.MethodDiff, protocol.DiffParams{Desired: []protocol.Resource{decl}, Observed: []protocol.Observed{got["ProxmoxLXC dns"]}}, &res)
	if len(res.Changes) > 0 {
		t.Fatalf("changes planned for a container to adopt: %+v", res.Changes)
	}
	if !slices.Equal(ops(res.Previews), []protocol.Op{OpGrowLXC, OpConfigureLXC, OpShutdownLXC}) {
		t.Fatalf("previews %v", ops(res.Previews))
	}
	conf := res.Previews[1].Requests[0].Params
	if !sameParams(conf, map[string][]string{"cores": {"2"}, "memory": {"1024"}, "swap": {"256"}, "onboot": {"1"}, "digest": {got["ProxmoxLXC dns"].Version}}) {
		t.Errorf("configure: %v", conf)
	}
	if !strings.Contains(strings.Join(res.Previews[1].Reasons, "; "), "it is not restarted") {
		t.Errorf("configure's reasons: %v", res.Previews[1].Reasons)
	}
	// Omitted values are not managed: nothing to preview.
	bare := lxcDecl("dns", 7005, "")
	var none protocol.DiffResult
	rpc(t, lxcWorld(), protocol.MethodDiff, protocol.DiffParams{Desired: []protocol.Resource{bare}, Observed: []protocol.Observed{got["ProxmoxLXC dns"]}}, &none)
	if len(none.Changes)+len(none.Previews) > 0 {
		t.Errorf("a declaration that manages nothing: %+v", none)
	}
}

func ownedLXC(st LXCState) protocol.Observed {
	b, _ := json.Marshal(st)
	return protocol.Observed{ID: "ProxmoxLXC app", Kind: KindLXC, Name: "app", Exists: true, Owned: true, Version: "d1",
		IDs: map[string]string{"node": "pve1", "vmid": "7004"}, State: b}
}

func diffLXC(t *testing.T, rest string, o protocol.Observed) []protocol.Change {
	t.Helper()
	r := lxcDecl("app", 7004, rest)
	out, err := New().Diff(context.Background(), protocol.DiffParams{Desired: []protocol.Resource{r}, Observed: []protocol.Observed{o}})
	if err != nil {
		t.Fatal(err)
	}
	return out.Changes
}

// A container Mamela has changes as declared, one change of its configuration at a time; what is
// omitted never changes; a smaller root filesystem and a move are shown and denied.
func TestLXCChanges(t *testing.T) {
	had := LXCState{Cores: 1, MemoryMiB: 512, SwapMiB: 512, RootfsGiB: 4, RootfsStorage: "local-lvm", Running: true, Pool: "lab",
		Tags: []string{"handmade", "mamela-lab"}, Tagged: true}
	for _, c := range []struct {
		name, rest string
		want       []protocol.Op
	}{
		{"nothing managed", ``, nil},
		{"as it is", `"cores":1,"memoryMib":512,"swapMib":512,"rootfsGib":4,"onboot":false,"power":"running"`, nil},
		{"grown first", `"cores":2,"rootfsGib":8`, []protocol.Op{OpGrowLXC}},
		{"configured", `"cores":2,"memoryMib":1024,"onboot":true`, []protocol.Op{OpConfigureLXC}},
		{"stopped", `"power":"stopped"`, []protocol.Op{OpShutdownLXC}},
		{"shrunk", `"rootfsGib":2`, []protocol.Op{OpShrinkLXC}},
		{"shrunk and configured", `"rootfsGib":2,"swapMib":0`, []protocol.Op{OpConfigureLXC, OpShrinkLXC}},
	} {
		cs := diffLXC(t, c.rest, ownedLXC(had))
		if !slices.Equal(ops(cs), c.want) {
			t.Errorf("%s: %v, want %v", c.name, ops(cs), c.want)
		}
		for _, ch := range cs {
			if o, _ := New().Manifest().Operation(KindLXC, ch.Op); o.Support == protocol.Unsupported && (len(ch.Requests) > 0 || len(ch.Reasons) == 0) {
				t.Errorf("%s: unsupported %s sends %v (reasons %v)", c.name, ch.Op, ch.Requests, ch.Reasons)
			}
		}
	}
	cs := diffLXC(t, `"cores":2,"rootfsGib":8`, ownedLXC(had))
	if r := cs[0].Requests[0]; r.Path != resizeLXC.Path || !sameParams(r.Params, map[string][]string{"disk": {"rootfs"}, "size": {"8G"}, "digest": {"d1"}}) ||
		cs[0].Precondition != "d1" || !strings.Contains(strings.Join(cs[0].Reasons, "; "), "then, in the next plan") ||
		!strings.Contains(strings.Join(cs[0].Reasons, "; "), "cores 1 -> 2") {
		t.Errorf("grow: %+v", cs[0])
	}
	cs = diffLXC(t, `"cores":2,"memoryMib":256,"onboot":true`, ownedLXC(had))
	why := strings.Join(cs[0].Reasons, "; ")
	if r := cs[0].Requests[0]; r.Path != setLXCConfig.Path || !sameParams(r.Params, map[string][]string{"cores": {"2"}, "memory": {"256"}, "onboot": {"1"}, "digest": {"d1"}}) ||
		!strings.Contains(why, "it is not restarted") || !strings.Contains(why, "reclaims memory") || !strings.Contains(why, "start at boot off -> on") {
		t.Errorf("configure: %+v", cs[0])
	}
	// A stopped container is started when declared running; its configuration changes need no restart either way.
	stopped := had
	stopped.Running = false
	cs = diffLXC(t, `"cores":2,"power":"running"`, ownedLXC(stopped))
	if !slices.Equal(ops(cs), []protocol.Op{OpConfigureLXC, OpStartLXC}) || !cs[1].Late || cs[1].Requests[0].Path != startLXC.Path ||
		strings.Contains(strings.Join(cs[0].Reasons, "; "), "running container") {
		t.Errorf("started: %+v", cs)
	}
	// Its tag, taken off by someone, is put back; its other tags stay.
	untagged := had
	untagged.Tags, untagged.Tagged = []string{"handmade"}, false
	cs = diffLXC(t, ``, ownedLXC(untagged))
	if !slices.Equal(ops(cs), []protocol.Op{OpConfigureLXC}) || cs[0].Requests[0].Params["tags"][0] != "handmade;mamela-lab" {
		t.Errorf("untagged: %+v", cs)
	}
	// Not where its declaration puts it: never moved, nothing else planned.
	elsewhere := had
	elsewhere.Pool = "prod"
	if cs := diffLXC(t, `"cores":2`, ownedLXC(elsewhere)); !slices.Equal(ops(cs), []protocol.Op{OpMoveLXC}) || !strings.Contains(cs[0].Reasons[0], `pool "prod"`) {
		t.Errorf("in another pool: %+v", cs)
	}
	o := ownedLXC(had)
	o.IDs["vmid"] = "7003"
	if cs := diffLXC(t, `"cores":2`, o); !slices.Equal(ops(cs), []protocol.Op{OpMoveLXC}) || !strings.Contains(cs[0].Reasons[0], "not the declared 7004") {
		t.Errorf("another VMID: %+v", cs)
	}
	for _, op := range []protocol.Op{OpMoveLXC, OpShrinkLXC, OpCreateLXC} {
		if o, _ := New().Manifest().Operation(KindLXC, op); o.Support != protocol.Unsupported {
			t.Errorf("%s is carried out", op)
		}
	}
	// Someone else's container (not Mamela's, not to adopt) gets nothing.
	o = ownedLXC(had)
	o.Owned = false
	if cs := diffLXC(t, `"cores":2`, o); len(cs) > 0 {
		t.Errorf("someone else's: %+v", cs)
	}
}

// Adopting a container appends the marker line to its description and the tag to its tags, on the
// configuration observed, and nothing else; it is refused, with nothing to send, outside the
// boundary or when it carries a mark of Mamela's.
func TestAdoptLXC(t *testing.T) {
	decl := lxcDecl("dns", 7005, `"cores":2`)
	got := observe(t, lxcWorld(), protocol.ObserveParams{Resources: []protocol.Resource{decl}})
	o := got["ProxmoxLXC dns"]
	r, err := New().Prepare(t.Context(), protocol.PrepareParams{Op: OpAdoptLXC, Observed: o, Desired: &decl})
	if err != nil {
		t.Fatal(err)
	}
	c := r.Change
	notes := handmade(7005, "dns").Description
	if c.Precondition != o.Version || len(c.Requests) != 1 || c.Requests[0].Path != setLXCMarker.Path ||
		!sameParams(c.Requests[0].Params, map[string][]string{"description": {notes + LXCMarker("dns") + "\n"}, "tags": {"handmade;mamela-lab"}, "digest": {o.Version}}) ||
		!maps.Equal(c.IDs, ids("pve1", 7005)) || !strings.Contains(c.Reasons[0], "not restarted") {
		t.Errorf("%+v", c)
	}
	for name, edit := range map[string]func(*protocol.Observed, *LXCState){
		"not adoptable":  func(o *protocol.Observed, _ *LXCState) { o.Adoptable = false },
		"another pool":   func(_ *protocol.Observed, s *LXCState) { s.Pool = "prod" },
		"another node":   func(o *protocol.Observed, _ *LXCState) { o.IDs = ids("pve2", 7005) },
		"another VMID":   func(o *protocol.Observed, _ *LXCState) { o.IDs = ids("pve1", 7006) },
		"marked":         func(_ *protocol.Observed, s *LXCState) { s.Description += LXCMarker("other") + "\n" },
		"a VM's marker":  func(_ *protocol.Observed, s *LXCState) { s.Description = Marker + "\nmamela-name: dns\n" },
		"too long notes": func(_ *protocol.Observed, s *LXCState) { s.Description = strings.Repeat("x", 8150) },
	} {
		x := o
		x.IDs = maps.Clone(o.IDs)
		st := lxcState(t, o)
		edit(&x, &st)
		x.State, _ = json.Marshal(st)
		if r, err := New().Prepare(t.Context(), protocol.PrepareParams{Op: OpAdoptLXC, Observed: x, Desired: &decl}); err == nil ||
			!strings.Contains(err.Error(), "nothing was done") {
			t.Errorf("%s: adopted: %+v %v", name, r.Change, err)
		}
	}
	if _, err := New().Prepare(t.Context(), protocol.PrepareParams{Op: OpAdoptLXC, Observed: o}); err == nil {
		t.Error("adopted without its declaration")
	}
	// Restarting and stopping are for a container Mamela has, running.
	for _, op := range []protocol.Op{OpRebootLXC, OpStopLXC} {
		if _, err := New().Prepare(t.Context(), protocol.PrepareParams{Op: op, Observed: o}); err == nil {
			t.Errorf("%s of a container to adopt", op)
		}
		app := ownedLXC(LXCState{Running: true, Pool: "lab"})
		r, err := New().Prepare(t.Context(), protocol.PrepareParams{Op: op, Observed: app})
		if err != nil || len(r.Change.Requests) != 1 || !strings.HasSuffix(r.Change.Requests[0].Path, strings.TrimSuffix(string(op), "-lxc")) {
			t.Errorf("%s: %+v %v", op, r.Change, err)
		}
		if _, err := New().Prepare(t.Context(), protocol.PrepareParams{Op: op, Observed: ownedLXC(LXCState{Pool: "lab"})}); err == nil {
			t.Errorf("%s of a stopped container", op)
		}
	}
}

// faulty sends requests to the fake, and makes the next change (any request but a GET) fail: not
// sent (it never reaches the fake), or lost (it takes effect, and its answer is lost).
type faulty struct {
	api   http.Handler
	fault string
}

func (f *faulty) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "GET" || f.fault == "":
	case f.fault == "not-sent":
		f.fault = ""
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	case f.fault == "lost":
		f.fault = ""
		f.api.ServeHTTP(httptest.NewRecorder(), r)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	f.api.ServeHTTP(w, r)
}

func applyLXC(t *testing.T, api http.Handler, c protocol.Change) error {
	t.Helper()
	var res protocol.ApplyResult
	return call(t, api, protocol.MethodApply, protocol.ApplyParams{Change: c}, &res)
}

func lookupLXC(t *testing.T, api http.Handler, c protocol.Change) protocol.LookupStatus {
	t.Helper()
	var res protocol.LookupResult
	rpc(t, api, protocol.MethodLookup, protocol.LookupParams{Change: c}, &res)
	return res.Status
}

// An adoption and the changes after it, each interrupted (not sent, or its answer lost), are looked
// up truly and recovered by applying again: what took effect is never sent twice.
func TestLXCInterruptedAndRetried(t *testing.T) {
	world := lxcWorld()
	api := &faulty{api: world}
	decl := lxcDecl("dns", 7005, `"cores":2,"memoryMib":1024,"rootfsGib":8,"power":"stopped"`)
	in := protocol.ObserveParams{Resources: []protocol.Resource{decl}}
	o := observe(t, api, in)["ProxmoxLXC dns"]
	var prep protocol.PrepareResult
	rpc(t, api, protocol.MethodPrepare, protocol.PrepareParams{Op: OpAdoptLXC, Observed: o, Desired: &decl}, &prep)
	adopt := prep.Change
	if s := lookupLXC(t, api, adopt); s != protocol.LookupNotDone {
		t.Fatalf("adopt before: %s", s)
	}
	api.fault = "not-sent"
	if err := applyLXC(t, api, adopt); err == nil {
		t.Fatal("an adopt not sent is done")
	}
	if s := lookupLXC(t, api, adopt); s != protocol.LookupNotDone {
		t.Fatalf("adopt not sent: %s", s)
	}
	api.fault = "lost"
	if err := applyLXC(t, api, adopt); err == nil {
		t.Fatal("an adopt whose answer was lost is done")
	}
	if s := lookupLXC(t, api, adopt); s != protocol.LookupDone {
		t.Fatalf("adopt lost: %s", s)
	}
	serial := world.VMs()[7005].Serial
	if err := applyLXC(t, api, adopt); err != nil || world.VMs()[7005].Serial != serial {
		t.Fatalf("applying the adopt again sends it again: %v", err)
	}
	if v := world.VMs()[7005]; v.Description != handmade(7005, "dns").Description+LXCMarker("dns")+"\n" || v.Tags != "handmade;mamela-lab" ||
		v.Cores != 1 || !v.Running {
		t.Fatalf("adopted: %+v", v)
	}

	// Then each change (the root filesystem, the shutdown, the rest of the configuration), not sent
	// or with its answer lost, then applied again.
	n := 0
	for round := 0; round < 4; round++ {
		o := observe(t, api, in)["ProxmoxLXC dns"]
		if !o.Owned {
			t.Fatalf("round %d: dns is not Mamela's: %+v", round, o)
		}
		var res protocol.DiffResult
		rpc(t, api, protocol.MethodDiff, protocol.DiffParams{Desired: in.Resources, Observed: []protocol.Observed{o}}, &res)
		for _, c := range res.Changes {
			fault, want := "not-sent", protocol.LookupNotDone
			if n%2 == 1 {
				fault, want = "lost", protocol.LookupDone
			}
			n++
			api.fault = fault
			if err := applyLXC(t, api, c); err == nil {
				t.Fatalf("%s with its request %s is done", c.Op, fault)
			}
			if s := lookupLXC(t, api, c); s != want {
				t.Fatalf("%s %s: lookup %s, want %s", c.Op, fault, s, want)
			}
			if err := applyLXC(t, api, c); err != nil {
				t.Fatalf("%s again: %v", c.Op, err)
			}
			if s := lookupLXC(t, api, c); s != protocol.LookupDone {
				t.Fatalf("%s applied: %s", c.Op, s)
			}
		}
	}
	if n != 3 {
		t.Errorf("%d changes, want 3 (the root filesystem, the shutdown, the rest of the configuration)", n)
	}
	o = observe(t, api, in)["ProxmoxLXC dns"]
	var res protocol.DiffResult
	rpc(t, api, protocol.MethodDiff, protocol.DiffParams{Desired: in.Resources, Observed: []protocol.Observed{o}}, &res)
	if len(res.Changes) > 0 {
		t.Fatalf("not converged: %+v", res.Changes)
	}
	if v := world.VMs()[7005]; v.Cores != 2 || v.Memory != 1024 || v.Swap != 512 || v.DiskGiB != 8 || v.Running || v.Tags != "handmade;mamela-lab" {
		t.Errorf("dns: %+v", v)
	}
}

// A change based on a configuration that changed since (someone edited the container between plan
// and apply) is never sent: adopting refuses it, and Proxmox refuses a change with the old digest.
func TestLXCDigestChangedBetweenPlanAndApply(t *testing.T) {
	world := lxcWorld()
	decl := lxcDecl("dns", 7005, "")
	o := observe(t, world, protocol.ObserveParams{Resources: []protocol.Resource{decl}})["ProxmoxLXC dns"]
	var prep protocol.PrepareResult
	rpc(t, world, protocol.MethodPrepare, protocol.PrepareParams{Op: OpAdoptLXC, Observed: o, Desired: &decl}, &prep)
	edited := world.VMs()[7005]
	edited.Description, edited.Serial = "edited by a person\n", edited.Serial+1
	world.Put(edited)
	if err := applyLXC(t, world, prep.Change); err == nil || !strings.Contains(err.Error(), "changed since it was prepared") {
		t.Errorf("adopting an edited container: %v", err)
	}
	if v := world.VMs()[7005]; v.Description != "edited by a person\n" || v.Tags != "handmade" {
		t.Errorf("the edited container was changed: %+v", v)
	}

	app := lxcDecl("app", 7004, `"cores":2`)
	o = observe(t, world, protocol.ObserveParams{Resources: []protocol.Resource{app}})["ProxmoxLXC app"]
	var res protocol.DiffResult
	rpc(t, world, protocol.MethodDiff, protocol.DiffParams{Desired: []protocol.Resource{app}, Observed: []protocol.Observed{o}}, &res)
	if len(res.Changes) != 1 || res.Changes[0].Op != OpConfigureLXC {
		t.Fatalf("%+v", res.Changes)
	}
	v := world.VMs()[7004]
	v.Memory, v.Serial = 768, v.Serial+1
	world.Put(v)
	if err := applyLXC(t, world, res.Changes[0]); err == nil {
		t.Error("a change with an old digest is applied")
	}
	if v := world.VMs()[7004]; v.Cores != 1 || v.Memory != 768 {
		t.Errorf("the changed container was changed: %+v", v)
	}
	// And a container whose marker someone took off is not Mamela's to change any more.
	v = world.VMs()[7004]
	v.Description = "notes\n"
	world.Put(v)
	if err := applyLXC(t, world, res.Changes[0]); err == nil || !strings.Contains(err.Error(), "not app's any more") {
		t.Errorf("a container without its marker: %v", err)
	}
}

// A VM's VMIDs skip the containers': a VMID is one guest's in the whole cluster.
func TestVMsSkipContainersVMIDs(t *testing.T) {
	f := fakepve.New()
	f.Put(handmade(7000, "dns"))
	got := observe(t, f, protocol.ObserveParams{Resources: []protocol.Resource{declare(Kind, "web", vmSpec)}})
	var st State
	json.Unmarshal(got["ProxmoxVM web"].State, &st)
	if len(st.FreeVMIDs) == 0 || st.FreeVMIDs[0] != 7001 {
		t.Errorf("free VMIDs %v", st.FreeVMIDs)
	}
}

func lxcChange(op protocol.Op, vmid string, reqs ...protocol.Approved) protocol.Change {
	return protocol.Change{Resource: "ProxmoxLXC dns", Kind: KindLXC, Op: op, IDs: map[string]string{"node": "pve1", "vmid": vmid}, Requests: reqs}
}

// The guard counts what a container's changes need (memory it grows by or starts with, its root
// filesystem's growth on the disk storage), and blocks a change of a container outside the
// boundary: another pool, a VM's VMID, a root filesystem on another storage, tags without the tag.
func TestGuardCountsContainers(t *testing.T) {
	guests := []Guest{
		{VMID: 7005, Type: "lxc", Node: "pve1", Name: "dns", Pool: "lab", Status: "running", MemoryMiB: 512, DiskGiB: 4, DiskStorage: "local-lvm"},
		{VMID: 7006, Type: "lxc", Node: "pve1", Name: "batch", Pool: "lab", Status: "stopped", MemoryMiB: 2048, DiskGiB: 4, DiskStorage: "local-lvm"},
		{VMID: 7007, Type: "lxc", Node: "pve1", Name: "zfs", Pool: "lab", Status: "running", MemoryMiB: 512, DiskGiB: 4, DiskStorage: "local-zfs"},
		{VMID: 7008, Type: "qemu", Node: "pve1", Name: "vm", Pool: "lab", Status: "running", MemoryMiB: 1024, DiskGiB: 10},
		{VMID: 7002, Type: "lxc", Node: "pve1", Name: "prod", Pool: "prod", Status: "running", MemoryMiB: 512, DiskGiB: 4, DiskStorage: "local-lvm"},
	}
	configure := func(vmid, memory string) protocol.Change {
		return lxcChange(OpConfigureLXC, vmid, req(setLXCConfig, map[string][]string{"memory": {memory}, "digest": {"d"}}))
	}
	grow := func(vmid, size string) protocol.Change {
		return lxcChange(OpGrowLXC, vmid, req(resizeLXC, map[string][]string{"disk": {"rootfs"}, "size": {size}, "digest": {"d"}}))
	}
	start := func(vmid string) protocol.Change { return lxcChange(OpStartLXC, vmid, req(startLXC, nil)) }
	for _, c := range []struct {
		name         string
		changes      []protocol.Change
		memory, disk int64
	}{
		{"a running container grows its memory", []protocol.Change{configure("7005", "1536")}, 1024, 0},
		{"a running container shrinks its memory", []protocol.Change{configure("7005", "256")}, 0, 0},
		{"a stopped container started", []protocol.Change{start("7006")}, 2048, 0},
		{"a stopped container resized and started", []protocol.Change{configure("7006", "4096"), start("7006")}, 4096, 0},
		{"its root filesystem grown on the disk storage", []protocol.Change{grow("7005", "8G")}, 0, 4},
		{"its root filesystem on another storage", []protocol.Change{grow("7007", "8G")}, 0, 0},
	} {
		if n := NeedOf(c.changes, guests, "local-lvm"); n.MemoryMiB != c.memory || n.DiskGiB != c.disk || n.Creates != 0 {
			t.Errorf("%s: %+v, want %d MiB, %d GiB", c.name, n, c.memory, c.disk)
		}
	}
	adopt := func(vmid, tags string) protocol.Change {
		return lxcChange(OpAdoptLXC, vmid, req(setLXCMarker, map[string][]string{"description": {"x"}, "tags": {tags}, "digest": {"d"}}))
	}
	for _, c := range []struct {
		name   string
		change protocol.Change
		want   string // "" for inside
	}{
		{"adopt inside", adopt("7005", "handmade;mamela-lab"), ""},
		{"configure inside", configure("7005", "1024"), ""},
		{"grow inside", grow("7005", "8G"), ""},
		{"another pool", configure("7002", "1024"), `in pool "prod"`},
		{"a VM's VMID", configure("7008", "1024"), "is a VM (qemu)"},
		{"no guest", configure("7009", "1024"), "no guest has VMID"},
		{"another storage", grow("7007", "8G"), `on storage "local-zfs"`},
		{"without the tag", adopt("7005", "handmade"), "do not keep mamela-lab"},
		{"out of the range", configure("150", "1024"), "is not in 7000..7009"},
	} {
		got := strings.Join(Outside(settings, c.change, guests), "; ")
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	f := healthy()
	f.Guests = append(f.Guests, guests[:2]...)
	if fs := Check(settings, New().Manifest(), f, []protocol.Change{configure("7005", "1024"), start("7006")}); len(codes(fs, protocol.SeverityBlock)) > 0 {
		t.Errorf("containers' changes inside the boundary block: %+v", fs)
	}
	f.MemAvailable = 5 << 30
	if fs := Check(settings, New().Manifest(), f, []protocol.Change{start("7006")}); !slices.Contains(codes(fs, protocol.SeverityBlock), "PVE-0201") {
		t.Errorf("a 2 GiB start with 5 GiB available and 4 GiB to keep: %+v", fs)
	}
}

// The guard reads containers through the audit token (their configurations, root filesystems), and
// blocks a change of one whose root filesystem is on another storage.
func TestGuardReadsContainers(t *testing.T) {
	world := lxcWorld()
	zfs := handmade(7002, "zfs")
	zfs.DiskStorage = "local-zfs"
	world.Put(zfs)
	b, _ := json.Marshal(settings)
	grow := lxcChange(OpGrowLXC, "7002", req(resizeLXC, map[string][]string{"disk": {"rootfs"}, "size": {"8G"}, "digest": {"d"}}))
	var res protocol.GuardResult
	rpc(t, world, protocol.MethodGuard, protocol.GuardParams{Stage: protocol.GuardBefore, Settings: b, Changes: []protocol.Change{grow}}, &res)
	found := false
	for _, f := range res.Findings {
		if f.Code == "PVE-0304" && f.Severity == protocol.SeverityBlock && strings.Contains(f.Message, `on storage "local-zfs"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("findings %+v", res.Findings)
	}
}
