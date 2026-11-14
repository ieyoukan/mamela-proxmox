package proxmox

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela-proxmox/fakepve"
	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/sdk"
)

// rpc calls one method of the provider in this process, as a host would, and answers its
// host/request with the fake API (without the host's checks: the conformance suite has them).
func rpc(t *testing.T, api http.Handler, method string, params, out any) {
	t.Helper()
	toProvider, fromHost := io.Pipe()
	fromProvider, toHost := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- sdk.ServeIO(New(), toProvider, toHost); toHost.Close() }()
	defer func() {
		fromHost.Close()
		<-done
	}()
	write := func(m protocol.Message) {
		m.JSONRPC = "2.0"
		b, _ := json.Marshal(m)
		fromHost.Write(append(b, '\n'))
	}
	p, _ := json.Marshal(params)
	write(protocol.Message{ID: 1, Method: method, Params: p})
	lines := bufio.NewScanner(fromProvider)
	lines.Buffer(nil, 16<<20)
	for lines.Scan() {
		var m protocol.Message
		if err := json.Unmarshal(lines.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		switch {
		case m.Method == protocol.MethodHostRequest:
			var r protocol.HostRequest
			json.Unmarshal(m.Params, &r)
			u := url.URL{Path: r.Path, RawQuery: url.Values(r.Query).Encode()}
			hr := httptest.NewRequest(r.Method, u.String(), strings.NewReader(url.Values(r.Form).Encode()))
			if r.Form != nil {
				hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			rec := httptest.NewRecorder()
			api.ServeHTTP(rec, hr)
			res, _ := json.Marshal(protocol.HostResponse{Status: rec.Code, Body: rec.Body.String()})
			write(protocol.Message{ID: m.ID, Result: res})
		case m.Method != "":
			write(protocol.Message{ID: m.ID, Error: &protocol.Error{Code: protocol.CodeRefused, Message: "not in this test"}})
		case m.Error != nil:
			t.Fatalf("%s: %v", method, m.Error)
		default:
			if err := json.Unmarshal(m.Result, out); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("%s: no answer", method)
}

func observe(t *testing.T, api http.Handler, in protocol.ObserveParams) map[string]protocol.Observed {
	t.Helper()
	var res protocol.ObserveResult
	rpc(t, api, protocol.MethodObserve, in, &res)
	out := map[string]protocol.Observed{}
	for _, o := range res.Resources {
		if _, dup := out[o.ID]; dup {
			t.Fatalf("%s observed twice", o.ID)
		}
		out[o.ID] = o
	}
	return out
}

func declare(kind protocol.ResourceKind, name, spec string) protocol.Resource {
	return protocol.Resource{ID: string(kind) + " " + name, Kind: kind, Name: name, Spec: jsontext.Value(spec)}
}

const vmSpec = `{` + testSite + `,"cpu":1,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04","network":{"mode":"dhcp"}}`

// A cluster where Mamela made a template, a clone of it, a VM from the image and a VM that is no
// longer declared, and the built-in provider made a VM to adopt.
func madeWorld() *fakepve.Server {
	f := fakepve.New()
	mark := func(name string) string {
		return marker{Name: name, Image: "ubuntu-24.04", Network: "dhcp"}.String()
	}
	f.Put(fakepve.VM{VMID: 7009, Name: "ubuntu-24.04-pve1", Node: "pve1", Pool: "lab", Tags: "mamela-lab", Template: true,
		Description: templateMarker("ubuntu-24.04-pve1", "ubuntu-24.04"), DiskGiB: 3})
	f.Put(fakepve.VM{VMID: 7000, Name: "a", Node: "pve1", Pool: "lab", Cores: 1, Memory: 1024, DiskGiB: 10, Tags: "mamela-lab", Running: true,
		Base: 7009, IDE2: "local:iso/mamela-seed-a-7000.iso,media=cdrom", Description: mark("a"), Uptime: 300})
	f.Put(fakepve.VM{VMID: 7001, Name: "b", Node: "pve1", Pool: "lab", Cores: 1, Memory: 1024, DiskGiB: 10, Tags: "mamela-lab",
		IDE2: "local:iso/mamela-seed-b-7001.iso,media=cdrom", Description: mark("b")})
	f.Put(fakepve.VM{VMID: 7002, Name: "gone", Node: "pve1", Pool: "lab", Cores: 1, Memory: 1024, DiskGiB: 10, Tags: "mamela-lab",
		Running: true, Description: mark("gone")})
	f.Put(fakepve.VM{VMID: 7003, Name: "old", Node: "pve1", Pool: "lab", Cores: 1, Memory: 1024, DiskGiB: 10, Tags: "mamela-lab", Running: true,
		Description: OldMarker + "\nmamela-name: old\nmamela-image: ubuntu-24.04\nmamela-network: dhcp\nmamela-state: ready\n"})
	return f
}

// The live Control Plane's records vouch for existing VMs by their observed IDs: observing must
// give every existing object exactly the IDs it had before the provider moved onto kit (a declared
// VM: node, vmid and its seeds' storage; anything else: node and vmid), and the records of every
// way Mamela made one (a create from the image, a linked clone, a template, adopt-vm) still vouch
// for it.
func TestObservedIDsKeepTheirShape(t *testing.T) {
	tmpl := declare(KindTemplate, "ubuntu-24.04-pve1", `{`+testSite+`,"image":"ubuntu-24.04"}`)
	a := declare(Kind, "a", strings.Replace(vmSpec, `"network"`, `"template":"ubuntu-24.04-pve1","network"`, 1))
	got := observe(t, madeWorld(), protocol.ObserveParams{Resources: []protocol.Resource{tmpl, a,
		declare(Kind, "b", vmSpec), declare(Kind, "old", vmSpec)}})
	for id, want := range map[string]map[string]string{
		"ProxmoxTemplate ubuntu-24.04-pve1": {"node": "pve1", "vmid": "7009"},
		"ProxmoxVM a":                       {"node": "pve1", "vmid": "7000", "storage": "local"},
		"ProxmoxVM b":                       {"node": "pve1", "vmid": "7001", "storage": "local"},
		"ProxmoxVM gone":                    {"node": "pve1", "vmid": "7002"},
		"ProxmoxVM old":                     {"node": "pve1", "vmid": "7003"},
	} {
		if !maps.Equal(got[id].IDs, want) {
			t.Errorf("%s: IDs %v, want %v", id, got[id].IDs, want)
		}
	}
	for id, records := range map[string][]map[string]string{
		"ProxmoxTemplate ubuntu-24.04-pve1": {{"node": "pve1", "vmid": "7009", "storage": "local"}},
		"ProxmoxVM a":                       {{"node": "pve1", "vmid": "7000", "storage": "local", "template": "7009", "newid": "7000"}},
		"ProxmoxVM b":                       {{"node": "pve1", "vmid": "7001", "storage": "local"}},
		"ProxmoxVM gone":                    {{"node": "pve1", "vmid": "7002", "storage": "local", "template": "7009", "newid": "7002"}},
	} {
		if o := protocol.Vouch(got[id], records); !o.Owned || o.Unfinished {
			t.Errorf("%s: Mamela's record %v no longer vouches for it (observed %v)", id, records, got[id].IDs)
		}
	}
	if o := got["ProxmoxVM a"]; o.Node == nil || o.Node.ID != 7000 || !o.Node.Running || o.Node.UptimeSeconds != 300 {
		t.Errorf("a is a running node: %+v", o.Node)
	}
	if o := got["ProxmoxVM b"]; o.Node == nil || o.Node.Running {
		t.Errorf("b is a stopped node: %+v", o.Node)
	}
	if o := got["ProxmoxVM old"]; !o.Adoptable || o.Owned || o.Node != nil {
		t.Errorf("old is adoptable, not a node: %+v", o)
	}
	// A VM without the declared tag is only unfinished; one not declared is found by its marker.
	other := strings.Replace(vmSpec, `"tag":"mamela-lab"`, `"tag":"other"`, 1)
	got = observe(t, madeWorld(), protocol.ObserveParams{Resources: []protocol.Resource{declare(Kind, "b", other)}})
	if o := got["ProxmoxVM b"]; o.Owned || !o.Unfinished {
		t.Errorf("b without the declared tag: owned %v, unfinished %v", o.Owned, o.Unfinished)
	}
	if o := got["ProxmoxVM gone"]; !o.Owned {
		t.Error("an undeclared VM with the marker is reported as Mamela's (for Mamela to keep)")
	}
}

// A VM imported from Terraform is adoptable only inside the declared boundary (node, pool, VMID
// range), and only without a marker of Mamela's.
func TestAdoptableOnlyInsideTheBoundary(t *testing.T) {
	f := fakepve.New()
	f.Put(fakepve.VM{VMID: 7004, Name: "tf", Node: "pve1", Pool: "lab", Cores: 1, Memory: 1024, DiskGiB: 10, Running: true, Net0: "virtio,bridge=vmbr0"})
	f.Put(fakepve.VM{VMID: 7005, Name: "elsewhere", Node: "pve1", Pool: "prod", Cores: 1, Memory: 1024, DiskGiB: 10})
	f.Put(fakepve.VM{VMID: 150, Name: "far", Node: "pve1", Pool: "lab", Cores: 1, Memory: 1024, DiskGiB: 10})
	imported := strings.Replace(vmSpec, `"image":"ubuntu-24.04",`, ``, 1)
	in := protocol.ObserveParams{Resources: []protocol.Resource{declare(Kind, "tf", imported), declare(Kind, "elsewhere", imported),
		declare(Kind, "far", imported), declare(Kind, "nothing", imported)},
		Adopting: []protocol.Adopting{{Resource: "ProxmoxVM tf", IDs: ids("pve1", 7004)}, {Resource: "ProxmoxVM elsewhere", IDs: ids("pve1", 7005)},
			{Resource: "ProxmoxVM far", IDs: ids("pve1", 150)}, {Resource: "ProxmoxVM nothing", IDs: ids("pve1", 7007)}}}
	got := observe(t, f, in)
	if o := got["ProxmoxVM tf"]; !o.Adoptable || o.Owned || !maps.Equal(o.IDs, ids("pve1", 7004)) || o.Node != nil {
		t.Errorf("tf (inside): %+v", o)
	}
	var st State
	json.Unmarshal(got["ProxmoxVM tf"].State, &st)
	if st.Origin != originUnmarked || st.Sockets != 1 || st.DiskStorage != "local-lvm" || st.Bridge != "vmbr0" || st.Uptime != 0 {
		t.Errorf("tf's state: %+v", st)
	}
	for _, id := range []string{"ProxmoxVM elsewhere", "ProxmoxVM far"} {
		if o := got[id]; !o.Exists || o.Adoptable || o.Owned {
			t.Errorf("%s (outside the boundary) is adoptable or Mamela's: %+v", id, o)
		}
	}
	if o := got["ProxmoxVM nothing"]; o.Exists {
		t.Errorf("a VM that is not there: %+v", o)
	}
	// What Mamela made is never found as one to adopt, even by its IDs.
	got = observe(t, madeWorld(), protocol.ObserveParams{Resources: []protocol.Resource{declare(Kind, "x", imported)},
		Adopting: []protocol.Adopting{{Resource: "ProxmoxVM x", IDs: ids("pve1", 7009)}}})
	if o := got["ProxmoxVM x"]; o.Exists {
		t.Errorf("the template was found to adopt: %+v", o)
	}
	got = observe(t, madeWorld(), protocol.ObserveParams{Resources: []protocol.Resource{declare(Kind, "x", imported)},
		Adopting: []protocol.Adopting{{Resource: "ProxmoxVM x", IDs: ids("pve1", 7001)}}})
	if o := got["ProxmoxVM x"]; o.Adoptable || o.Owned {
		t.Errorf("Mamela's VM b is to be adopted as x: %+v", o)
	}
}

// Terraform's clone of Mamela's template copies the template's marker into the clone's description
// (as found on a real cluster). The clone is not the template: the template is observed as itself,
// and the clone, imported, is found by its IDs to adopt.
func TestCloneWithACopiedTemplateMarker(t *testing.T) {
	world := madeWorld()
	// As bpg/proxmox made it: its disk given (so not linked to the template), its own name.
	world.Put(fakepve.VM{VMID: 7004, Name: "tf-legacy-1", Node: "pve1", Pool: "lab", Cores: 1, Memory: 1024, DiskGiB: 10, Tags: "mamela-lab",
		Description: templateMarker("ubuntu-24.04-pve1", "ubuntu-24.04"), Net0: "virtio=BC:24:11:00:00:04,bridge=vmbr0"})
	tmpl := declare(KindTemplate, "ubuntu-24.04-pve1", `{`+testSite+`,"image":"ubuntu-24.04"}`)
	legacy := declare(Kind, "legacy", `{`+testSite+`,"cpu":1,"memoryMiB":1024,"diskGiB":10,"network":{"mode":"dhcp"},"stopped":true}`)
	got := observe(t, world, protocol.ObserveParams{Resources: []protocol.Resource{tmpl, legacy},
		Adopting: []protocol.Adopting{{Resource: "ProxmoxVM legacy", IDs: map[string]string{"node": "pve1", "vmid": "7004"}}}})
	if o := got["ProxmoxTemplate ubuntu-24.04-pve1"]; o.IDs["vmid"] != "7009" || o.Unfinished {
		t.Errorf("the template is taken for its clone: %+v", o)
	}
	if o := got["ProxmoxVM legacy"]; !o.Exists || !o.Adoptable || o.Owned || o.IDs["vmid"] != "7004" {
		t.Errorf("the imported clone is not found to adopt: %+v", o)
	}
}
