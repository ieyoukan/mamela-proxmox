package proxmox_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela-proxmox/fakepve"
	"github.com/ieyoukan/mamela-proxmox/proxmox"
	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/providertest"
)

// importObjects is what `mamela import terraform` gives the provider: the managed resources of a
// state (in its modules too), named as Mamela names them (modules, name and index, joined by -).
func importObjects(t *testing.T, state []byte) []protocol.ImportObject {
	t.Helper()
	type resource struct {
		Address string         `json:"address"`
		Mode    string         `json:"mode"`
		Type    string         `json:"type"`
		Name    string         `json:"name"`
		Index   jsontext.Value `json:"index"`
		Values  jsontext.Value `json:"values"`
	}
	type module struct {
		Resources    []resource `json:"resources"`
		ChildModules []module   `json:"child_modules"`
	}
	var s struct {
		Values struct {
			Root module `json:"root_module"`
		} `json:"values"`
	}
	if err := json.Unmarshal(state, &s); err != nil {
		t.Fatal(err)
	}
	var out []protocol.ImportObject
	modules := regexp.MustCompile(`module\.([^.\[]+)(?:\["?([^\]"]*)"?\])?`)
	var walk func(m module)
	walk = func(m module) {
		for _, r := range m.Resources {
			if r.Mode != "managed" {
				continue
			}
			var parts []string
			for _, x := range modules.FindAllStringSubmatch(strings.TrimSuffix(r.Address, "."+r.Type+"."+r.Name), -1) {
				parts = append(parts, x[1])
				if x[2] != "" {
					parts = append(parts, x[2])
				}
			}
			parts = append(parts, r.Name)
			if len(r.Index) > 0 {
				parts = append(parts, strings.Trim(string(r.Index), `"`))
			}
			out = append(out, protocol.ImportObject{Address: r.Address, Type: r.Type, Name: strings.Join(parts, "-"), Values: r.Values})
		}
		for _, c := range m.ChildModules {
			walk(c)
		}
	}
	walk(s.Values.Root)
	return out
}

func canonical(b []byte) string {
	var v any
	json.Unmarshal(b, &v)
	out, _ := json.Marshal(v, json.Deterministic(true))
	return string(out)
}

func importAll(t *testing.T, state []byte, settings []byte) (map[string]protocol.Imported, map[string]string) {
	t.Helper()
	res, err := proxmox.New().Import(context.Background(), protocol.ImportParams{Objects: importObjects(t, state), Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	got, skipped := map[string]protocol.Imported{}, map[string]string{}
	for _, r := range res.Resources {
		got[r.Address] = r
	}
	for _, s := range res.Skipped {
		skipped[s.Address] = s.Why
	}
	return got, skipped
}

// VMs Terraform made move to Mamela: each one inside the guard's boundary is declared as it is
// (without an image: Mamela did not make it), with the IDs observing finds it by; a template, a
// VM outside the boundary and a container are not imported, with why. After adopt-vm, each is
// Mamela's: a VM whose Terraform apply was not made is grown and resized to what the state
// says, one declared stopped (started = false) is shut down, and the others are in sync. A lost or
// unsent adopt is recovered.
func TestImportFromTerraform(t *testing.T) {
	tf := fakepve.New().AddTerraformVMs()
	got, skipped := importAll(t, tf.State, guardSettings)
	for addr, want := range map[string]string{
		"proxmox_virtual_environment_vm.web": `{` + site + `,"cpu":2,"memoryMiB":2048,"diskGiB":20,` +
			`"network":{"mode":"static","address":"192.0.2.50/24","gateway":"192.0.2.1","dns":["192.0.2.1"]}}`,
		"proxmox_vm_qemu.db": `{` + site + `,"cpu":1,"memoryMiB":1024,"diskGiB":10,"network":{"mode":"dhcp"},"stopped":true}`,
		"proxmox_vm_qemu.drifted": `{` + site + `,"cpu":2,"memoryMiB":2048,"diskGiB":16,` +
			`"network":{"mode":"static","address":"192.0.2.70/24","gateway":"192.0.2.1","dns":["192.0.2.1"]}}`,
		"proxmox_virtual_environment_vm.batch": `{` + site + `,"cpu":1,"memoryMiB":1024,"diskGiB":8,"network":{"mode":"dhcp"},"stopped":true}`,
	} {
		r, ok := got[addr]
		if !ok {
			t.Errorf("%s is not imported: %s", addr, skipped[addr])
			continue
		}
		if canonical(r.Spec) != canonical([]byte(want)) || r.Kind != proxmox.Kind {
			t.Errorf("%s: %s %s, want %s", addr, r.Kind, r.Spec, want)
		}
	}
	if ids := got["proxmox_vm_qemu.db"].IDs; !maps.Equal(ids, map[string]string{"node": "pve1", "vmid": "7006"}) {
		t.Errorf("db's IDs: %v", ids)
	}
	for addr, why := range map[string]string{
		"proxmox_virtual_environment_vm.legacy":       "VMID 120 is outside Mamela's VMIDs 7000..7009",
		"proxmox_virtual_environment_vm.template":     "it is a template",
		"proxmox_virtual_environment_container.dns":   "no kind of provider proxmox imports",
		"data.proxmox_virtual_environment_nodes.all":  "",
		"proxmox_virtual_environment_vm.not-in-state": "",
	} {
		if !strings.Contains(skipped[addr], why) || (why == "") != (skipped[addr] == "") {
			t.Errorf("%s: skipped %q, want %q", addr, skipped[addr], why)
		}
	}
	if len(got) != 4 || len(skipped) != 3 {
		t.Errorf("imported %d, skipped %v", len(got), skipped)
	}
	// Without the guard's boundary there is nothing to import into.
	_, none := importAll(t, tf.State, nil)
	if !strings.Contains(none["proxmox_vm_qemu.db"], "[provider.proxmox.guard]") {
		t.Errorf("without settings: %v", none)
	}

	var desired []protocol.Resource
	var adopting []protocol.Adopting
	var adopt []providertest.Explicit
	for _, addr := range []string{"proxmox_virtual_environment_vm.web", "proxmox_vm_qemu.db", "proxmox_vm_qemu.drifted", "proxmox_virtual_environment_vm.batch"} {
		r := got[addr]
		id := string(r.Kind) + " " + r.Name
		desired = append(desired, protocol.Resource{ID: id, Kind: r.Kind, Name: r.Name, Spec: r.Spec})
		adopting = append(adopting, protocol.Adopting{Resource: id, IDs: r.IDs})
		adopt = append(adopt, providertest.Explicit{Op: proxmox.OpAdopt, Resource: id})
	}
	providertest.Run(t, providertest.Suite{
		Provider:      providertest.BuildWASI(t, "../cmd/proxmox-provider"),
		Catalog:       providertest.Catalog(t, "../pome/catalogs/proxmox.json"),
		NewAPI:        func() http.Handler { return newFake() },
		GuardSettings: guardSettings,
		ScriptCredentials: protocol.ScriptCredentials{TokenID: "mamela@pve!lab", TokenFile: "/token",
			GuardTokenID: "mamela-audit@pve!audit", GuardTokenFile: "/audit-token"},
		Cases: []providertest.Case{{Name: "terraform-vms", Desired: desired, Adopting: adopting, Explicit: adopt,
			World: func(api http.Handler) { api.(*fakepve.Server).AddTerraformVMs() }}},
	})
}

// Realistic states of both Terraform providers: each VM is imported or skipped with why, and
// nothing outside the boundary is ever declared.
func TestImportRealisticStates(t *testing.T) {
	for _, file := range []string{"testdata/terraform-bpg.json", "testdata/terraform-telmate.json"} {
		state, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		got, skipped := importAll(t, state, guardSettings)
		var want struct {
			Imported map[string]string `json:"imported"` // address -> spec
			Skipped  map[string]string `json:"skipped"`  // address -> a part of why
		}
		expect, err := os.ReadFile(strings.TrimSuffix(file, ".json") + ".want.json")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(expect, &want); err != nil {
			t.Fatal(err)
		}
		for addr, spec := range want.Imported {
			if r, ok := got[addr]; !ok || canonical(r.Spec) != canonical([]byte(spec)) {
				t.Errorf("%s: %s: %s (skipped: %s), want %s", file, addr, r.Spec, skipped[addr], spec)
			}
		}
		for addr, why := range want.Skipped {
			if !strings.Contains(skipped[addr], why) {
				t.Errorf("%s: %s: skipped %q, want %q", file, addr, skipped[addr], why)
			}
		}
		if len(got) != len(want.Imported) || len(skipped) != len(want.Skipped) {
			t.Errorf("%s: imported %v, skipped %v", file, slices.Sorted(maps.Keys(got)), skipped)
		}
	}
}

// The whole move, as a person makes it: `mamela import terraform` with the state's JSON writes the
// declarations (with the Pome's generated Proxmox.Kinds), says what it does not import and why,
// and the declaration that uses them evaluates into a graph of the VMs.
func TestImportTerraformCommand(t *testing.T) {
	if os.Getenv("MAMELA_TOOLCHAIN_DIR") == "" {
		t.Skip("MAMELA_TOOLCHAIN_DIR: the toolchain of Mamela")
	}
	tf := fakepve.New().AddTerraformVMs()
	dir := t.TempDir()
	decl := filepath.Join(dir, "decl")
	repo, _ := filepath.Abs("..")
	lock, _ := os.ReadFile("../examples/homelab/Pome.lock")
	files := map[string]string{
		"decl/Pome.toml": `[pome]
name = "example.com/decl"
version = "0.1.0"
emela = "0.27.0"

[build]
target = "wasm"

[dependencies]
"github.com/ieyoukan/mamela/pome/mamela" = "v0.3.0"
"github.com/ieyoukan/mamela-proxmox/pome" = "v0.1.0"

[replace]
"github.com/ieyoukan/mamela/pome/mamela" = { path = "` + filepath.Join(repo, "../mamela/pome/mamela") + `" }
"github.com/ieyoukan/mamela-proxmox/pome" = { path = "` + filepath.Join(repo, "pome") + `" }
`,
		"decl/Pome.lock": string(lock),
		"decl/src/main.emel": `import Std.Console.{SystemConsole}
import Mamela

fn main() {
  with SystemConsole {
    Mamela.empty() |> Mamela.print
  }
}
`,
		"state.json": string(tf.State),
		"mamela.providers.toml": `[provider.proxmox]
api_url = "https://pve.example:8006/api2/json"
auth = "proxmox"
token_id = "mamela@pve!lab"
token_file = "token"
catalog = "pome:proxmox"

[provider.proxmox.guard]
node = "pve1"
pool = "lab"
vmid_min = 7000
vmid_max = 7009
tag = "mamela-lab"
disk_storage = "local-lvm"
image_storage = "local"
bridge = "vmbr0"
operating_token = "mamela@pve!lab"
max_vms = 4
min_host_mem_available_mib = 4096
max_storage_used_percent = 80
`,
		"token": "secret",
	}
	for name, data := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mamela := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("go", append([]string{"tool", "mamela"}, args...)...)
		cmd.Env = append(os.Environ(), "GOWORK=off")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("mamela %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	out := mamela("import", "terraform", "--state", filepath.Join(dir, "state.json"), "--db", filepath.Join(dir, "state.db"), "--by", "tester",
		"--providers", filepath.Join(dir, "mamela.providers.toml"), decl)
	for _, want := range []string{
		"4 of 7 resources import, as 4 declarations",
		"proxmox_virtual_environment_vm.legacy", "VMID 120 is outside Mamela's VMIDs 7000..7009",
		"proxmox_virtual_environment_vm.template", "it is a template",
		`mamela act --confirm web adopt-vm "ProxmoxVM web"`,
		"terraform state rm 'proxmox_virtual_environment_vm.batch'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("import: no %q in\n%s", want, out)
		}
	}
	src, _ := os.ReadFile(filepath.Join(decl, "src/imported.emel"))
	for _, want := range []string{"import Proxmox.Kinds.{", "proxmox_vm_db = ProxmoxVm(", "stopped: Some(True)", "|> Kinds.proxmox_vm("} {
		if !strings.Contains(string(src), want) {
			t.Errorf("imported.emel: no %q in\n%s", want, src)
		}
	}
	os.WriteFile(filepath.Join(decl, "src/main.emel"), []byte(`import Std.Console.{SystemConsole}
import Mamela
import Imported

fn main() {
  with SystemConsole {
    Mamela.empty() |> Imported.imported |> Mamela.print
  }
}
`), 0o644)
	graph := mamela("eval", decl)
	for _, want := range []string{"4 resources", `"diskGiB":16`, `"stopped":true`, `"address":"192.0.2.50/24"`} {
		if !strings.Contains(graph, want) {
			t.Errorf("the graph has no %s:\n%s", want, graph)
		}
	}
}
