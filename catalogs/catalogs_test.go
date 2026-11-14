package catalogs

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela-proxmox/proxmox"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// A call is a read only when it is a GET; anything else is classified by a rule or by a person,
// and fails without either.
func TestClassifyFailsClosed(t *testing.T) {
	for _, c := range []struct {
		method, path string
		review       Review
		want         protocol.Effect // "" for an error
	}{
		{"GET", "/cluster/resources", Review{}, protocol.EffectRead},
		{"DELETE", "/nodes/{node}/qemu/{vmid}", Review{}, protocol.EffectLoseData},
		{"DELETE", "/nodes/{node}/qemu/{vmid}", Review{Effect: protocol.EffectRead, Why: "it reads"}, ""},
		{"POST", "/nodes/{node}/qemu/{vmid}/status/start", Review{}, protocol.EffectRestart},
		{"POST", "/nodes/{node}/lxc/{vmid}/status/stop", Review{}, protocol.EffectRestart},
		{"POST", "/nodes/{node}/qemu/{vmid}/status/start/now", Review{}, ""},
		{"POST", "/nodes/{node}/qemu", Review{}, ""},
		{"PUT", "/nodes/{node}/qemu/{vmid}/config", Review{}, ""},
		{"POST", "/nodes/{node}/qemu", Review{Effect: protocol.EffectRead, Why: "no"}, ""},
		{"POST", "/nodes/{node}/qemu", Review{Effect: protocol.EffectCreate}, ""},
		{"POST", "/nodes/{node}/qemu", Review{Effect: protocol.EffectCreate, Why: "allocates a VM"}, protocol.EffectCreate},
		{"GET", "/nodes/{node}/vncwebsocket", Review{Effect: protocol.EffectChange, Why: "a console can change anything"}, protocol.EffectChange},
	} {
		got, _, err := Classify(c.method, c.path, c.review)
		if got != c.want || (err == nil) != (c.want != "") {
			t.Errorf("%s %s %+v: %q %v, want %q", c.method, c.path, c.review, got, err, c.want)
		}
	}
}

func manifestOf(calls ...protocol.Call) protocol.Manifest {
	return protocol.Manifest{Kinds: []protocol.Kind{{Kind: "K", Operations: []protocol.Operation{{Op: "op", Support: protocol.Supported, Calls: calls}}}}}
}

func TestGenerateRefuses(t *testing.T) {
	get := protocol.Call{Method: "GET", Path: "/cluster/resources", Query: []string{"type"}}
	create := protocol.Call{Method: "POST", Path: "/nodes/{node}/qemu", Form: []string{"vmid"}}
	reviewed := map[string]Review{"POST /nodes/{node}/qemu": {Handler: "create_vm", Effect: protocol.EffectCreate, Why: "allocates a VM"}}
	if c, err := Generate("proxmox", manifestOf(get, create), reviewed); err != nil || len(c.Entries) != 2 {
		t.Fatalf("%+v %v", c, err)
	}
	for name, c := range map[string]struct {
		calls    []protocol.Call
		reviewed map[string]Review
	}{
		"a change not reviewed":   {[]protocol.Call{get, create}, nil},
		"a review of no call":     {[]protocol.Call{get}, reviewed},
		"a GET's form":            {[]protocol.Call{{Method: "GET", Path: "/x", Form: []string{"a"}}}, nil},
		"a POST's query":          {[]protocol.Call{{Method: "POST", Path: "/nodes/{node}/qemu", Query: []string{"vmid"}}}, reviewed},
		"a fixed parameter":       {[]protocol.Call{{Method: "GET", Path: "/x", Query: []string{"a"}, Fixed: map[string]string{"a": "b"}}}, nil},
		"a JSON body":             {[]protocol.Call{{Method: "POST", Path: "/nodes/{node}/qemu", JSON: []string{"a"}}}, reviewed},
		"a precondition not sent": {[]protocol.Call{create}, map[string]Review{"POST /nodes/{node}/qemu": {Effect: protocol.EffectCreate, Why: "x", Precondition: "digest"}}},
	} {
		if _, err := Generate("proxmox", manifestOf(c.calls...), c.reviewed); err == nil {
			t.Errorf("%s: generated", name)
		}
	}
	// The calls of several operations on one path are one entry, with all their parameters.
	c, err := Generate("proxmox", manifestOf(create, protocol.Call{Method: "POST", Path: "/nodes/{node}/qemu", Form: []string{"name", "vmid"}}), reviewed)
	if err != nil || strings.Join(c.Entries[0].Form, ",") != "name,vmid" {
		t.Errorf("%+v %v", c.Entries, err)
	}
}

// miniSchema is a small API schema in the form of pve-docs' apidoc.js (made up for the test).
const miniSchema = `const apiSchema = [
 {"path": "/nodes", "text": "nodes", "children": [
  {"path": "/nodes/{node}", "text": "{node}", "children": [
   {"path": "/nodes/{node}/qemu", "text": "qemu", "info": {
     "POST": {"name": "create_vm", "allowtoken": 1, "parameters": {"properties": {"node": {}, "vmid": {}, "name": {}, "scsi[n]": {}}},
       "permissions": {"description": "the handler checks", "user": "all"}}},
    "children": [
    {"path": "/nodes/{node}/qemu/{vmid}", "text": "{vmid}", "children": [
     {"path": "/nodes/{node}/qemu/{vmid}/config", "text": "config", "info": {
       "PUT": {"name": "update_vm", "allowtoken": 1, "parameters": {"properties": {"node": {}, "vmid": {}, "cores": {}, "digest": {}}},
         "permissions": {"check": ["perm", "/vms/{vmid}", ["VM.Config.CPU", "VM.Config.Disk"], "any", 1]}}}},
     {"path": "/nodes/{node}/qemu/{vmid}/clone", "text": "clone", "info": {
       "POST": {"name": "clone_vm", "allowtoken": 1, "parameters": {"properties": {"node": {}, "vmid": {}, "newid": {}, "pool": {}}},
         "permissions": {"check": ["and", ["perm", "/vms/{vmid}", ["VM.Clone"]], ["or", ["perm", "/vms/{newid}", ["VM.Allocate"]],
           ["perm", "/pool/{pool}", ["VM.Allocate"], "require_param", "pool"]]]}}}},
     {"path": "/nodes/{node}/qemu/{vmid}/status", "text": "status", "children": [
      {"path": "/nodes/{node}/qemu/{vmid}/status/start", "text": "start", "info": {
        "POST": {"name": "vm_start", "allowtoken": 1, "parameters": {"properties": {"node": {}, "vmid": {}}},
          "permissions": {"check": ["perm", "/vms/{vmid}", ["VM.PowerMgmt"]]}}}}]}]}]}]}]},
 {"path": "/cluster", "text": "cluster", "children": [
  {"path": "/cluster/status", "text": "status", "info": {
    "GET": {"name": "get_status", "allowtoken": 1, "parameters": {"properties": {}}, "permissions": {"check": ["perm", "/", ["Sys.Audit"]]}}}}]}
]
;
`

// The checks against Proxmox VE's API schema find what changed: a path or parameter that is not
// there, a call tokens may not make, a handler that is not the one reviewed, a privilege the
// permission catalog does not grant, and a guard read the audit token cannot make.
func TestValidate(t *testing.T) {
	create := protocol.Call{Method: "POST", Path: "/nodes/{node}/qemu", Form: []string{"vmid", "name", "scsi0"}}
	setCPU := protocol.Call{Method: "PUT", Path: "/nodes/{node}/qemu/{vmid}/config", Form: []string{"cores", "digest"}}
	clone := protocol.Call{Method: "POST", Path: "/nodes/{node}/qemu/{template}/clone", Form: []string{"newid", "pool"}}
	start := protocol.Call{Method: "POST", Path: "/nodes/{node}/qemu/{vmid}/status/start"}
	status := protocol.Call{Method: "GET", Path: "/cluster/status"}
	reviewed := map[string]Review{
		"POST /nodes/{node}/qemu":                     {Handler: "create_vm", Effect: protocol.EffectCreate, Why: "x"},
		"PUT /nodes/{node}/qemu/{vmid}/config":        {Handler: "update_vm", Effect: protocol.EffectChange, Why: "x"},
		"POST /nodes/{node}/qemu/{template}/clone":    {Handler: "clone_vm", Effect: protocol.EffectCreate, Why: "x"},
		"POST /nodes/{node}/qemu/{vmid}/status/start": {Handler: "vm_start"},
	}
	validate := func(schema string, reviewed map[string]Review, calls ...protocol.Call) []error {
		t.Helper()
		s, err := ReadSchema([]byte(schema))
		if err != nil {
			t.Fatal(err)
		}
		m := manifestOf(calls...)
		m.Guard = &protocol.Guard{Calls: []protocol.Call{status}}
		mine := map[string]Review{} // the reviews of the calls declared
		for _, call := range calls {
			if r, ok := reviewed[call.Method+" "+call.Path]; ok {
				mine[call.Method+" "+call.Path] = r
			}
		}
		reviewed = mine
		c, err := Generate("proxmox", m, reviewed)
		if err != nil {
			t.Fatal(err)
		}
		return Validate(s, m, c, reviewed)
	}
	if errs := validate(miniSchema, reviewed, create, setCPU, clone, start); len(errs) > 0 {
		t.Fatalf("a catalog that holds: %v", errs)
	}
	for name, c := range map[string]struct {
		schema   string
		reviewed map[string]Review
		calls    []protocol.Call
		want     string
	}{
		"an unknown parameter": {miniSchema, reviewed, []protocol.Call{{Method: "PUT", Path: setCPU.Path, Form: []string{"cores", "sockets"}}}, "no parameter sockets"},
		"a family's member":    {miniSchema, reviewed, []protocol.Call{{Method: "POST", Path: create.Path, Form: []string{"ide2"}}}, "no parameter ide2"},
		"a path's ID":          {miniSchema, reviewed, []protocol.Call{{Method: "POST", Path: clone.Path, Form: []string{"vmid"}}}, "is an ID of the path"},
		"a path gone":          {strings.Replace(miniSchema, "/status/start", "/status/begin", 2), reviewed, []protocol.Call{start}, "no such path"},
		"a method gone":        {strings.Replace(miniSchema, `"PUT": {"name": "update_vm"`, `"POST": {"name": "update_vm"`, 1), reviewed, []protocol.Call{setCPU}, "has no PUT"},
		"no tokens":            {strings.Replace(miniSchema, `"name": "vm_start", "allowtoken": 1`, `"name": "vm_start", "allowtoken": 0`, 1), reviewed, []protocol.Call{start}, "does not allow API tokens"},
		"another handler":      {strings.Replace(miniSchema, `"vm_start"`, `"vm_begin"`, 1), reviewed, []protocol.Call{start}, "reviewed for Proxmox VE's handler"},
		"a privilege more":     {strings.Replace(miniSchema, `["VM.PowerMgmt"]`, `["VM.PowerMgmt", "VM.Monitor"]`, 1), reviewed, []protocol.Call{start}, "does not grant"},
		"a privilege less":     {strings.Replace(miniSchema, `["VM.Config.CPU", "VM.Config.Disk"]`, `["VM.Config.Disk"]`, 1), reviewed, []protocol.Call{setCPU}, "does not grant"},
		"a guard read":         {strings.Replace(miniSchema, `["Sys.Audit"]`, `["Sys.Modify"]`, 1), reviewed, []protocol.Call{start}, "audit token"},
		"an unknown check":     {strings.Replace(miniSchema, `["perm", "/vms/{vmid}", ["VM.PowerMgmt"]]`, `["userid-group", ["Sys.Audit"]]`, 1), reviewed, []protocol.Call{start}, "permissions"},
	} {
		errs := validate(c.schema, c.reviewed, c.calls...)
		if len(errs) == 0 || !strings.Contains(errorsText(errs), c.want) {
			t.Errorf("%s: %v, want %q", name, errs, c.want)
		}
	}
	if _, err := ReadSchema([]byte("const apiSchema = {")); err == nil {
		t.Error("a broken schema is read")
	}
}

func errorsText(errs []error) string {
	var b strings.Builder
	for _, e := range errs {
		b.WriteString(e.Error() + "\n")
	}
	return b.String()
}

func TestLock(t *testing.T) {
	l, err := Locked()
	if err != nil {
		t.Fatal(err)
	}
	if again, err := ParseLock(l.Encode()); err != nil || again != l {
		t.Errorf("%+v %v", again, err)
	}
	for _, bad := range []string{"", "pve-manager 9.2.26\n", "pve-manager 9.2.26\nsha256 abc\n", "pve-manager 9\nsha256 " + strings.Repeat("a", 64) + "\nx y\n"} {
		if _, err := ParseLock([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// With Proxmox VE's API schema (PVE_APIDOC: the apidoc.js pve-api.lock names), the catalog and the
// permission catalog hold against it, and generating the catalog gives the one the Pome ships.
// Without it, the test is skipped: the schema is not in this repository (README).
func TestAgainstPVESchema(t *testing.T) {
	path := os.Getenv("PVE_APIDOC")
	if path == "" {
		t.Skip("PVE_APIDOC: the apidoc.js of the Proxmox VE release pve-api.lock names")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ReadSchema(data)
	if err != nil {
		t.Fatal(err)
	}
	lock, _ := Locked()
	if s.SHA256 != lock.SHA256 {
		t.Fatalf("%s is not the schema pve-api.lock names (pve-manager %s): check a new one with proxmox-catalogs -update-lock", path, lock.PVEManager)
	}
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range Validate(s, proxmox.New().Manifest(), all["proxmox"], Reviewed) {
		t.Error(e)
	}
	want, err := Encode(all["proxmox"])
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile("../pome/catalogs/proxmox.json"); !bytes.Equal(got, want) {
		t.Error("pome/catalogs/proxmox.json is not what is generated: run ./build.sh")
	}
}
