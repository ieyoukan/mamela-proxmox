package proxmox

import (
	"encoding/json/v2"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela-proxmox/fakepve"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// deleteWorld is a cluster whose operating token holds exactly the grants of the cases' settings,
// with, in the boundary (pool lab, VMIDs 7000..7009, node pve1): container 7004 app (Mamela's,
// adopted, running), container 7001 batch (Mamela's, stopped) and VM 7008; outside it, someone
// else's container 105 and VM 100 (no pool), which the guard records and checks after.
func deleteWorld() *fakepve.Server {
	f := fakepve.New()
	f.Permissions = healthy().Permissions
	app := handmade(7004, "app")
	app.Description += LXCMarker("app") + "\n"
	app.Tags = "handmade;mamela-lab"
	f.Put(app)
	batch := handmade(7001, "batch")
	batch.Description, batch.Tags, batch.Running = LXCMarker("batch")+"\n", "mamela-lab", false
	f.Put(batch)
	f.Put(fakepve.VM{VMID: 7008, Name: "vm", Node: "pve1", Pool: "lab", Cores: 1, Memory: 1024, DiskGiB: 10, Running: true, Net0: "virtio,bridge=vmbr0"})
	f.Put(fakepve.VM{VMID: 105, Name: "someone-else", Node: "pve1", Type: "lxc", Running: true, Memory: 2048, DiskGiB: 8, Uptime: 9000})
	f.Put(fakepve.VM{VMID: 100, Name: "prod-db", Node: "pve1", Cores: 4, Memory: 8192, DiskGiB: 100, Running: true, Uptime: 9000})
	return f
}

// prepareDelete observes the world with the declarations and prepares delete-lxc of a resource
// (desired is its declaration, nil when it is not declared any more).
func prepareDelete(t *testing.T, api *fakepve.Server, decls []protocol.Resource, id string, desired *protocol.Resource) (protocol.Observed, protocol.Change, error) {
	t.Helper()
	o, ok := observe(t, api, protocol.ObserveParams{Resources: decls})[id]
	if !ok {
		t.Fatalf("%s is not observed", id)
	}
	var prep protocol.PrepareResult
	err := call(t, api, protocol.MethodPrepare, protocol.PrepareParams{Op: OpDeleteLXC, Observed: o, Desired: desired}, &prep)
	return o, prep.Change, err
}

// delete-lxc is an explicit operation a person asks for (mamela act), never planned: not a removing
// operation (ADR 0013 of Mamela plans those when a declaration is removed), so that an adopted
// container is never deleted because its declaration was removed.
func TestDeleteLXCIsExplicitOnly(t *testing.T) {
	op, ok := New().Manifest().Operation(KindLXC, OpDeleteLXC)
	if !ok || !op.Explicit || op.Removes || op.Adopts || op.Support != protocol.Supported {
		t.Fatalf("delete-lxc: %+v", op)
	}
	var calls []string
	for _, c := range op.Calls {
		if c.Method != "GET" { // kit adds the kind's reads
			calls = append(calls, c.Method+" "+c.Path+" "+strings.Join(append(slices.Clone(c.Query), c.Form...), ","))
		}
	}
	if want := []string{"POST /nodes/{node}/lxc/{vmid}/status/shutdown timeout,forceStop",
		"DELETE /nodes/{node}/lxc/{vmid} purge,destroy-unreferenced-disks"}; !slices.Equal(calls, want) {
		t.Errorf("calls %v, want %v", calls, want)
	}
	// Nothing a plan makes deletes a container: with its declaration removed, Mamela's container is
	// kept, and the plan of a declared one never has delete-lxc.
	world := deleteWorld()
	got := observe(t, world, protocol.ObserveParams{})
	var res protocol.DiffResult
	rpc(t, world, protocol.MethodDiff, protocol.DiffParams{Observed: slices.Collect(maps.Values(got))}, &res)
	if len(res.Changes)+len(res.Previews) > 0 {
		t.Errorf("undeclared containers: %+v", res)
	}
	decl := lxcDecl("app", 7004, `"cores":2,"power":"stopped"`)
	got = observe(t, world, protocol.ObserveParams{Resources: []protocol.Resource{decl}})
	rpc(t, world, protocol.MethodDiff, protocol.DiffParams{Desired: []protocol.Resource{decl}, Observed: []protocol.Observed{got["ProxmoxLXC app"]}}, &res)
	if slices.Contains(ops(res.Changes), OpDeleteLXC) {
		t.Errorf("planned: %v", ops(res.Changes))
	}
}

// A running container Mamela adopted is shut down cleanly (waited for, never forced), then deleted
// with its volumes and out of the cluster's jobs, inside the guard: before, the change is inside the
// boundary; after, every guest outside it is unchanged. Looked up, it is done; applied again (a
// retry), it sends nothing.
func TestDeleteLXCRunning(t *testing.T) {
	world := deleteWorld()
	decl := lxcDecl("app", 7004, `"cores":1`)
	o, c, err := prepareDelete(t, world, []protocol.Resource{decl}, "ProxmoxLXC app", &decl)
	if err != nil {
		t.Fatal(err)
	}
	if !o.Owned {
		t.Fatalf("app is not Mamela's: %+v", o)
	}
	if len(c.Requests) != 2 || c.Requests[0].Path != shutdownLXC.Path || c.Requests[0].Method != "POST" ||
		!sameParams(c.Requests[0].Params, map[string][]string{"timeout": {"120"}, "forceStop": {"0"}}) ||
		c.Requests[1].Method != "DELETE" || c.Requests[1].Path != deleteLXC.Path ||
		!sameParams(c.Requests[1].Params, map[string][]string{"purge": {"1"}, "destroy-unreferenced-disks": {"1"}}) {
		t.Fatalf("requests %+v", c.Requests)
	}
	why := strings.Join(c.Reasons, "\n")
	if c.Precondition != o.Version || !maps.Equal(c.IDs, ids("pve1", 7004)) || c.Op != OpDeleteLXC {
		t.Errorf("%+v", c)
	}
	for _, want := range []string{"shut container 7004 (app) down first", "never forced", "their data is lost", "vzdump 7004",
		"backup jobs (a job that names only this container is deleted)", "HA resources", "carrying the marker of app",
		"configuration digest observed (" + o.Version + ")"} {
		if !strings.Contains(why, want) {
			t.Errorf("the reasons do not say %q:\n%s", want, why)
		}
	}
	if s := lookupLXC(t, world, c); s != protocol.LookupNotDone {
		t.Errorf("before: %s", s)
	}

	before := guardRPC(t, world, settings, protocol.GuardBefore, nil, []protocol.Change{c})
	if b := codes(before.Findings, protocol.SeverityBlock); len(b) > 0 {
		t.Fatalf("deleting a container inside the boundary blocks: %+v", before.Findings)
	}
	if err := applyLXC(t, world, c); err != nil {
		t.Fatal(err)
	}
	vms := world.VMs()
	if _, ok := vms[7004]; ok {
		t.Fatalf("app is still there: %+v", vms[7004])
	}
	if len(vms) != 4 || !vms[105].Running || !vms[100].Running || !strings.Contains(vms[7001].Description, LXCMarker("batch")) {
		t.Errorf("other guests changed: %+v", vms)
	}
	after := guardRPC(t, world, settings, protocol.GuardAfter, before.Baseline, nil)
	if b := codes(after.Findings, protocol.SeverityBlock); len(b) > 0 || !slices.Contains(codes(after.Findings, protocol.SeverityOK), "PVE-0400") {
		t.Errorf("after: %+v", after.Findings)
	}
	if s := lookupLXC(t, world, c); s != protocol.LookupDone {
		t.Errorf("deleted: %s", s)
	}
	// Applied again (an apply whose answer was lost, retried): it is gone, nothing is sent.
	api := &faulty{api: world, fault: "not-sent"} // a request sent would fail
	if err := applyLXC(t, api, c); err != nil || api.fault == "" {
		t.Errorf("a retry of a done delete: %v (sent: %v)", err, api.fault == "")
	}
	if o := observe(t, world, protocol.ObserveParams{Resources: []protocol.Resource{decl}})["ProxmoxLXC app"]; o.Exists {
		t.Errorf("observed after deleting: %+v", o)
	}
}

// A stopped container is deleted at once (nothing to shut down), also when its declaration was
// removed (Mamela kept it, and a person deletes it by name).
func TestDeleteLXCStoppedUndeclared(t *testing.T) {
	world := deleteWorld()
	o, c, err := prepareDelete(t, world, nil, "ProxmoxLXC batch", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !o.Owned || len(c.Requests) != 1 || c.Requests[0].Path != deleteLXC.Path || strings.Contains(strings.Join(c.Reasons, " "), "shut") {
		t.Fatalf("%+v %+v", o, c)
	}
	before := guardRPC(t, world, settings, protocol.GuardBefore, nil, []protocol.Change{c})
	if b := codes(before.Findings, protocol.SeverityBlock); len(b) > 0 {
		t.Fatalf("%+v", before.Findings)
	}
	if err := applyLXC(t, world, c); err != nil {
		t.Fatal(err)
	}
	if _, ok := world.VMs()[7001]; ok {
		t.Error("batch is still there")
	}
	if after := guardRPC(t, world, settings, protocol.GuardAfter, before.Baseline, nil); len(codes(after.Findings, protocol.SeverityBlock)) > 0 {
		t.Errorf("after: %+v", after.Findings)
	}
}

// Interrupted at each request (not sent, or its answer lost), a delete is looked up truly (not
// done, partly: shut down and still there, done: gone) and finished by applying it again; what took
// effect is never sent again.
func TestDeleteLXCInterruptedAndRetried(t *testing.T) {
	decl := lxcDecl("app", 7004, "")
	for _, c := range []struct {
		name   string
		stop   bool   // app is stopped first (the change is the delete only)
		fault  string // on the first request sent
		lookup protocol.LookupStatus
	}{
		{"shutdown not sent", false, "not-sent", protocol.LookupNotDone},
		{"shutdown's answer lost", false, "lost", protocol.LookupPartly},
		{"delete not sent", true, "not-sent", protocol.LookupNotDone},
		{"delete's answer lost", true, "lost", protocol.LookupDone},
	} {
		t.Run(c.name, func(t *testing.T) {
			world := deleteWorld()
			if c.stop {
				v := world.VMs()[7004]
				v.Running = false
				world.Put(v)
			}
			_, ch, err := prepareDelete(t, world, []protocol.Resource{decl}, "ProxmoxLXC app", &decl)
			if err != nil {
				t.Fatal(err)
			}
			api := &faulty{api: world, fault: c.fault}
			if err := applyLXC(t, api, ch); err == nil {
				t.Fatal("an interrupted delete is done")
			}
			if s := lookupLXC(t, world, ch); s != c.lookup {
				t.Fatalf("lookup %s, want %s", s, c.lookup)
			}
			if err := applyLXC(t, world, ch); err != nil {
				t.Fatalf("again: %v", err)
			}
			if _, ok := world.VMs()[7004]; ok || lookupLXC(t, world, ch) != protocol.LookupDone {
				t.Errorf("not deleted: %+v", world.VMs()[7004])
			}
		})
	}
}

// What delete-lxc is prepared for, or refuses with nothing to send: only a container Mamela has,
// inside the boundary proper, never a named one, never one Proxmox would refuse to delete once it
// was shut down (protected, locked).
func TestDeleteLXCRefused(t *testing.T) {
	app := LXCState{Running: true, Pool: "lab", Cores: 1, Tags: []string{"mamela-lab"}, Tagged: true}
	decl := lxcDecl("app", 7004, "")
	prepare := func(o protocol.Observed, desired *protocol.Resource) (protocol.Change, error) {
		r, err := New().Prepare(t.Context(), protocol.PrepareParams{Op: OpDeleteLXC, Observed: o, Desired: desired})
		return r.Change, err
	}
	if c, err := prepare(ownedLXC(app), &decl); err != nil || len(c.Requests) != 2 {
		t.Fatalf("app: %+v %v", c, err)
	}
	named := ownedLXC(LXCState{Running: true, Tags: []string{"mamela-lab"}, Tagged: true})
	named.IDs = ids("pve2", 150)
	namedApp := namedDecl("app", "pve2", 150, "")
	for name, c := range map[string]struct {
		o       protocol.Observed
		desired *protocol.Resource
		why     string
	}{
		"not Mamela's (to adopt)": {func() protocol.Observed { o := ownedLXC(app); o.Owned, o.Adoptable = false, true; return o }(), &decl,
			"Mamela deletes only a container it adopted"},
		"named":                          {named, &namedApp, "Mamela never deletes one. To delete it, a person does it by hand"},
		"named, its declaration removed": {named, nil, "it is in no pool"},
		"in another pool": {func() protocol.Observed {
			st := app
			st.Pool = "prod"
			o := ownedLXC(st)
			return o
		}(), &decl, `in pool "prod"`},
		"on another node":    {func() protocol.Observed { o := ownedLXC(app); o.IDs = ids("pve2", 7004); return o }(), &decl, "node pve2"},
		"another VMID":       {func() protocol.Observed { o := ownedLXC(app); o.IDs = ids("pve1", 7003); return o }(), &decl, "not the declared 7004"},
		"out of the range":   {func() protocol.Observed { o := ownedLXC(app); o.IDs = ids("pve1", 150); return o }(), &decl, "is outside the declared 7000..7009"},
		"protected":          {func() protocol.Observed { st := app; st.Protection = true; return ownedLXC(st) }(), nil, "--protection 0"},
		"locked":             {func() protocol.Observed { st := app; st.Lock = "backup"; return ownedLXC(st) }(), &decl, "locked (backup"},
		"no observed object": {protocol.Observed{ID: "ProxmoxLXC app", Kind: KindLXC, Name: "app"}, &decl, "nothing to delete-lxc"},
	} {
		ch, err := prepare(c.o, c.desired)
		if err == nil || !strings.Contains(err.Error(), c.why) || len(ch.Requests) > 0 {
			t.Errorf("%s: %+v %v, want %q", name, ch, err, c.why)
			continue
		}
		if name != "no observed object" && !strings.Contains(err.Error(), "nothing was done") {
			t.Errorf("%s: %v does not say nothing was done", name, err)
		}
	}
	if _, err := prepare(named, &namedApp); err == nil || !strings.Contains(err.Error(), "pct destroy 150") || !strings.Contains(err.Error(), "on node pve2") {
		t.Errorf("named: %v does not say how to delete it by hand", err)
	}
}

// Right before each request, the container must still be exactly the one observed; if anything
// changed since it was prepared, nothing (more) is sent and it is left as it is: its marker taken
// off or another's, its configuration changed, a VM's VMID, out of the pool, moved, a template,
// protected. One that does not shut down is not forced, and not deleted.
func TestDeleteLXCRefusedBeforeSending(t *testing.T) {
	decl := lxcDecl("app", 7004, "")
	for _, c := range []struct {
		name string
		edit func(w *fakepve.Server, v *fakepve.VM)
		why  string
	}{
		{"its marker taken off", func(_ *fakepve.Server, v *fakepve.VM) { v.Description = "hand-made container: app\n" }, "not app's any more"},
		{"another name's marker", func(_ *fakepve.Server, v *fakepve.VM) { v.Description = LXCMarker("other") + "\n" }, "not app's any more"},
		{"its configuration changed", func(_ *fakepve.Server, v *fakepve.VM) { v.Memory, v.Serial = 4096, v.Serial+1 }, "changed since it was prepared"},
		{"out of the pool", func(_ *fakepve.Server, v *fakepve.VM) { v.Pool = "" }, "in no pool now"},
		{"migrated", func(_ *fakepve.Server, v *fakepve.VM) { v.Node = "pve2" }, "on node pve2 now"},
		{"a template", func(_ *fakepve.Server, v *fakepve.VM) { v.Template, v.Running = true, false }, "a template now"},
		{"protected", func(_ *fakepve.Server, v *fakepve.VM) { v.Protection = true }, "protected now"},
		{"locked", func(_ *fakepve.Server, v *fakepve.VM) { v.Lock = "backup" }, "locked now (backup)"},
		{"a VM's VMID now", func(w *fakepve.Server, v *fakepve.VM) { v.Type, v.Description = "", "" }, "is a VM (qemu) now"},
		{"does not shut down", func(_ *fakepve.Server, v *fakepve.VM) { v.Stubborn = true }, "did not shut down cleanly"},
	} {
		t.Run(c.name, func(t *testing.T) {
			world := deleteWorld()
			_, ch, err := prepareDelete(t, world, []protocol.Resource{decl}, "ProxmoxLXC app", &decl)
			if err != nil {
				t.Fatal(err)
			}
			v := world.VMs()[7004]
			c.edit(world, &v)
			world.Put(v)
			was := world.VMs()[7004]
			err = applyLXC(t, world, ch)
			if err == nil || !strings.Contains(err.Error(), c.why) || !strings.Contains(err.Error(), "not deleted") &&
				!strings.Contains(err.Error(), "nothing was deleted") {
				t.Fatalf("%v, want %q (and that it was not deleted)", err, c.why)
			}
			now, ok := world.VMs()[7004]
			if !ok || now.Running != was.Running || now.Serial != was.Serial {
				t.Errorf("it was changed: %+v, was %+v", now, was)
			}
			if c.name == "does not shut down" {
				for _, want := range []string{"container did not stop", "it was not forced, and nothing was deleted", "stop-lxc"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%v does not say %q", err, want)
					}
				}
				if s := lookupLXC(t, world, ch); s != protocol.LookupNotDone {
					t.Errorf("lookup %s", s)
				}
			}
		})
	}
}

// The guard keeps deleting inside the boundary proper: never a named guest (whatever its access),
// never a container in another pool, out of the range, or a VM's VMID. After a delete, it still
// finds a guest outside the boundary that changed meanwhile.
func TestGuardDeleteLXC(t *testing.T) {
	del := func(node, vmid string) protocol.Change {
		return protocol.Change{Resource: "ProxmoxLXC x", Kind: KindLXC, Op: OpDeleteLXC, IDs: map[string]string{"node": node, "vmid": vmid},
			Requests: []protocol.Approved{req(shutdownLXCWait, map[string][]string{"timeout": {"120"}, "forceStop": {"0"}}),
				req(deleteLXC, map[string][]string{"purge": {"1"}, "destroy-unreferenced-disks": {"1"}})}}
	}
	m := New().Manifest()
	for _, access := range []string{AccessChange, AccessObserve} {
		s := namedSettings(access)
		if fs := Check(s, m, namedFacts(s), []protocol.Change{del("pve2", "150")}); !blocks(fs, "PVE-0304", "Mamela never deletes a named guest") {
			t.Errorf("deleting a named guest (%s): %+v", access, fs)
		}
	}
	guests := []Guest{
		{VMID: 7005, Type: "lxc", Node: "pve1", Name: "dns", Pool: "lab", Status: "running", MemoryMiB: 512, DiskGiB: 4, DiskStorage: "local-lvm"},
		{VMID: 7002, Type: "lxc", Node: "pve1", Name: "prod", Pool: "prod", Status: "running"},
		{VMID: 7008, Type: "qemu", Node: "pve1", Name: "vm", Pool: "lab", Status: "running"},
		{VMID: 150, Type: "lxc", Node: "pve1", Name: "far", Status: "running"},
	}
	for _, c := range []struct {
		name, node, vmid, want string
	}{
		{"inside", "pve1", "7005", ""},
		{"another pool", "pve1", "7002", `in pool "prod"`},
		{"a VM's VMID", "pve1", "7008", "is a VM (qemu)"},
		{"out of the range", "pve1", "150", "is not in 7000..7009"},
		{"another node", "pve2", "7005", `node "pve2" is not pve1`},
	} {
		got := strings.Join(Outside(settings, del(c.node, c.vmid), guests), "; ")
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// A delete needs no room: nothing is counted for it.
	if n := NeedOf([]protocol.Change{del("pve1", "7005")}, guests, "local-lvm"); n != (Need{}) {
		t.Errorf("need %+v", n)
	}

	// Someone else's container changes while Mamela deletes its own: the guard after says so.
	world := deleteWorld()
	decl := lxcDecl("app", 7004, "")
	_, ch, err := prepareDelete(t, world, []protocol.Resource{decl}, "ProxmoxLXC app", &decl)
	if err != nil {
		t.Fatal(err)
	}
	before := guardRPC(t, world, settings, protocol.GuardBefore, nil, []protocol.Change{ch})
	if err := applyLXC(t, world, ch); err != nil {
		t.Fatal(err)
	}
	other := world.VMs()[105]
	other.Running = false
	world.Put(other)
	if after := guardRPC(t, world, settings, protocol.GuardAfter, before.Baseline, nil); !blocks(after.Findings, "PVE-0402", "guest 105 (someone-else)") {
		t.Errorf("after: %+v", after.Findings)
	}
}

// A named guest's grant never holds what deleting needs (VM.Allocate): Proxmox would refuse it too.
func TestNamedGrantCannotDelete(t *testing.T) {
	privs, reasons := NamedPrivileges(New().Manifest(), NamedGuest{Node: "pve2", VMID: 150, Type: "lxc", Access: AccessChange})
	if slices.Contains(privs, "VM.Allocate") {
		t.Errorf("a named guest's grant holds VM.Allocate: %v", privs)
	}
	for p, ops := range reasons {
		if slices.Contains(ops, string(OpDeleteLXC)) {
			t.Errorf("%s is granted for delete-lxc on a named guest", p)
		}
	}
	// The pool's grant holds it (as it did for delete-vm), for delete-lxc too.
	for _, g := range Grants(New().Manifest(), settings) {
		if g.Path == "/pool/lab" && !slices.Contains(g.Reasons["VM.Allocate"], string(OpDeleteLXC)) {
			t.Errorf("the pool's VM.Allocate is not for delete-lxc: %v", g.Reasons["VM.Allocate"])
		}
	}
}

// The state reports what Proxmox refuses deleting with, read from the configuration.
func TestLXCStateProtectionAndLock(t *testing.T) {
	var cfg ctConfig
	if err := json.Unmarshal([]byte(`{"digest":"d","protection":1,"lock":"backup","memory":512}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Protection || cfg.Lock != "backup" {
		t.Errorf("%+v", cfg)
	}
	world := deleteWorld()
	v := world.VMs()[7004]
	v.Protection = true
	world.Put(v)
	if st := lxcState(t, observe(t, world, protocol.ObserveParams{})["ProxmoxLXC app"]); !st.Protection || st.Lock != "" {
		t.Errorf("%+v", st)
	}
}
