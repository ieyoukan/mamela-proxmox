package proxmox_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela-proxmox/catalogs"
	"github.com/ieyoukan/mamela-proxmox/fakepve"
	"github.com/ieyoukan/mamela-proxmox/proxmox"
	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/providertest"
)

const site = `"node":"pve1","pool":"lab","diskStorage":"local-lvm","imageStorage":"local","bridge":"vmbr0","tag":"mamela-lab","vmidMin":7000,"vmidMax":7009`

func vm(name, rest string) protocol.Resource {
	return protocol.Resource{ID: "ProxmoxVM " + name, Kind: "ProxmoxVM", Name: name, Spec: []byte(`{` + site + `,` + rest + `}`)}
}

// template is the template of the image on the cases' site, as the Pome declares it.
func template(name string) protocol.Resource {
	return protocol.Resource{ID: "ProxmoxTemplate " + name, Kind: "ProxmoxTemplate", Name: name,
		Spec: []byte(`{` + site + `,"image":"ubuntu-24.04"}`)}
}

// clone is a VM cloned from the template.
func clone(name, rest string) protocol.Resource {
	r := vm(name, rest+`,"template":"ubuntu-24.04-pve1"`)
	r.After = []string{"ProxmoxTemplate ubuntu-24.04-pve1"}
	return r
}

// ct declares a container of the cases' site by its VMID (ProxmoxLXC), with the values it manages.
func ct(name string, vmid int, rest string) protocol.Resource {
	spec := fmt.Sprintf(`{"node":"pve1","pool":"lab","tag":"mamela-lab","vmidMin":7000,"vmidMax":7009,"vmid":%d`, vmid)
	if rest != "" {
		spec += "," + rest
	}
	return protocol.Resource{ID: "ProxmoxLXC " + name, Kind: "ProxmoxLXC", Name: name, Spec: []byte(spec + `}`)}
}

// handmadeCT is a container a person made: in the pool, without a mark of Mamela's, with a
// description and tags of its own.
func handmadeCT(vmid int64, name string, running bool) fakepve.VM {
	return fakepve.VM{VMID: vmid, Name: name, Node: "pve1", Pool: "lab", Type: "lxc", Cores: 1, Memory: 512, Swap: 512, DiskGiB: 4,
		Running: running, Tags: "handmade", Description: "hand-made container: " + name + "\n"}
}

// guardSettings are the guard's settings for the fake cluster, with the boundary of the cases.
var guardSettings = []byte(`{"node":"pve1","pool":"lab","vmid_min":7000,"vmid_max":7009,"tag":"mamela-lab","disk_storage":"local-lvm",` +
	`"image_storage":"local","bridge":"vmbr0","operating_token":"mamela@pve!lab","max_vms":4,` +
	`"min_host_mem_available_mib":4096,"max_storage_used_percent":80}`)

// newFake is a fake Proxmox whose operating token holds exactly the grants the provider needs.
func newFake() *fakepve.Server {
	s, err := proxmox.ParseGuardSettings(guardSettings)
	if err != nil {
		panic(err)
	}
	f := fakepve.New()
	f.Permissions = map[string]map[string]int{}
	for _, g := range proxmox.Grants(proxmox.New().Manifest(), s) {
		f.Permissions[g.Path] = map[string]int{}
		for _, p := range g.Privs {
			f.Permissions[g.Path][p] = 1
		}
	}
	return f
}

// oldVM is a VM Mamela's built-in provider made (before ADR 0006): its marker, in the pool.
func oldVM(vmid int64, name string) fakepve.VM {
	return fakepve.VM{VMID: vmid, Name: name, Node: "pve1", Pool: "lab", Cores: 1, Memory: 1024, DiskGiB: 10, Tags: "mamela-lab", Running: true,
		Description: proxmox.OldMarker + "\nmamela-name: " + name + "\nmamela-image: ubuntu-24.04\nmamela-network: dhcp\nmamela-state: ready\n"}
}

// The provider passes Mamela's conformance suite against a fake Proxmox, with the catalog Mamela
// ships for Proxmox: every request within its permit, convergence, undeclared VMs kept, and a
// lost or unsent request at any point recovered.
func TestConformance(t *testing.T) {
	providertest.Run(t, providertest.Suite{
		Provider:      providertest.BuildWASI(t, "../cmd/proxmox-provider"),
		Catalog:       providertest.Catalog(t, "../pome/catalogs/proxmox.json"), // as the Pome ships it
		NewAPI:        func() http.Handler { return newFake() },
		GuardSettings: guardSettings,
		ScriptCredentials: protocol.ScriptCredentials{TokenID: "mamela@pve!lab", TokenFile: "/token",
			GuardTokenID: "mamela-audit@pve!audit", GuardTokenFile: "/audit-token"},
		Cases: []providertest.Case{
			{Name: "one-vm", Desired: []protocol.Resource{
				vm("web", `"cpu":1,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04","network":{"mode":"dhcp"}`)},
				Explicit: []providertest.Explicit{{Op: proxmox.OpReboot, Resource: "ProxmoxVM web"}, {Op: proxmox.OpReset, Resource: "ProxmoxVM web"},
					{Op: proxmox.OpStop, Resource: "ProxmoxVM web"}, {Op: proxmox.OpDelete, Resource: "ProxmoxVM web"}}},
			// A VM declared stopped is made and never started; a running one is stopped by hand.
			{Name: "stopped", Desired: []protocol.Resource{template("ubuntu-24.04-pve1"),
				clone("batch", `"cpu":1,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04","network":{"mode":"dhcp"},"stopped":true`),
				clone("web", `"cpu":1,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04","network":{"mode":"dhcp"}`)},
				Explicit: []providertest.Explicit{{Op: proxmox.OpStop, Resource: "ProxmoxVM web"}}},
			// A VM of the old built-in provider, taken over, then managed as any other; a container
			// of someone else's, outside the boundary, is left alone.
			{Name: "adopt", Desired: []protocol.Resource{
				vm("old", `"cpu":1,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04","network":{"mode":"dhcp"}`)},
				World: func(api http.Handler) {
					f := api.(*fakepve.Server)
					f.Put(oldVM(7003, "old"))
					f.Put(fakepve.VM{VMID: 105, Name: "someone-else", Node: "pve1", Type: "lxc", Running: true})
				},
				Explicit: []providertest.Explicit{{Op: proxmox.OpAdopt, Resource: "ProxmoxVM old"}}},
			// Containers a person made, adopted by their declared VMIDs (no import), then managed as
			// declared: grown, resized at once, kept running or shut down, restarted and stopped by
			// hand, and deleted by hand (a stopped one: the delete alone); a VM beside them is made
			// with a VMID no container has, and someone else's container is left alone.
			{Name: "containers", Desired: []protocol.Resource{
				ct("dns", 7000, `"cores":2,"memoryMib":1024,"swapMib":256,"rootfsGib":8,"onboot":true,"power":"running"`),
				ct("batch", 7001, `"power":"stopped"`),
				vm("web", `"cpu":1,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04","network":{"mode":"dhcp"}`)},
				World: func(api http.Handler) {
					f := api.(*fakepve.Server)
					f.Put(handmadeCT(7000, "dns", false))
					f.Put(handmadeCT(7001, "batch", true))
					f.Put(fakepve.VM{VMID: 105, Name: "someone-else", Node: "pve1", Type: "lxc", Running: true, Memory: 2048, DiskGiB: 8})
				},
				Explicit: []providertest.Explicit{{Op: proxmox.OpAdoptLXC, Resource: "ProxmoxLXC dns"}, {Op: proxmox.OpAdoptLXC, Resource: "ProxmoxLXC batch"},
					{Op: proxmox.OpRebootLXC, Resource: "ProxmoxLXC dns"}, {Op: proxmox.OpStopLXC, Resource: "ProxmoxLXC dns"},
					{Op: proxmox.OpDeleteLXC, Resource: "ProxmoxLXC batch"}}},
			{Name: "two-vms-static", Desired: []protocol.Resource{
				vm("a", `"cpu":2,"memoryMiB":2048,"diskGiB":20,"image":"ubuntu-24.04","network":{"mode":"static","address":"192.0.2.40/24","gateway":"192.0.2.1","dns":["192.0.2.1"]}`),
				vm("b", `"cpu":1,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04","network":{"mode":"dhcp"}`)}},
			// VMs as linked clones of a template made once: the template first, then the clones;
			// deleting them, then the template.
			{Name: "cloned", Desired: []protocol.Resource{template("ubuntu-24.04-pve1"),
				clone("a", `"cpu":2,"memoryMiB":2048,"diskGiB":20,"image":"ubuntu-24.04","network":{"mode":"static","address":"192.0.2.40/24","gateway":"192.0.2.1","dns":["192.0.2.1"]}`),
				clone("b", `"cpu":1,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04","network":{"mode":"dhcp"}`)},
				Explicit: []providertest.Explicit{{Op: proxmox.OpDelete, Resource: "ProxmoxVM a"}, {Op: proxmox.OpDelete, Resource: "ProxmoxVM b"},
					{Op: proxmox.OpDeleteTemplate, Resource: "ProxmoxTemplate ubuntu-24.04-pve1"}}},
		},
	})
}

// namedGuardSettings are the cases' settings with container 150 on node pve2 named into the
// boundary (outside the pool and the range), to adopt and change.
var namedGuardSettings = []byte(strings.TrimSuffix(string(guardSettings), "}") +
	`,"named":[{"node":"pve2","vmid":150,"type":"lxc","access":"change"}]}`)

// The provider passes the conformance suite with a named container too (on another node, in no
// pool, out of the range): declared by its VMID, adopted where it is, managed, restarted and
// stopped through the host and the catalog the Pome ships, while someone else's container beside
// it is left alone. A running container in the pool is deleted by hand (shut down, then deleted). The guard around the converge finds nothing to block: the token holds exactly
// the grants (the named one's on /vms/150 only), and the named container is where it is named.
// (The suite's guard sees no changes; named_test.go checks the changes against the boundary.)
func TestConformanceNamed(t *testing.T) {
	s, err := proxmox.ParseGuardSettings(namedGuardSettings)
	if err != nil {
		t.Fatal(err)
	}
	newNamedFake := func() http.Handler {
		f := fakepve.New()
		f.Permissions = map[string]map[string]int{}
		for _, g := range proxmox.Grants(proxmox.New().Manifest(), s) {
			f.Permissions[g.Path] = map[string]int{}
			for _, p := range g.Privs {
				f.Permissions[g.Path][p] = 1
			}
		}
		if len(f.Permissions["/vms/150"]) == 0 {
			panic("no grant on the named container")
		}
		return f
	}
	far := ct("far", 150, `"cores":2,"memoryMib":1024,"rootfsGib":8,"onboot":true,"power":"running"`)
	far.Spec = []byte(strings.Replace(strings.TrimSuffix(string(far.Spec), "}")+`,"named":true}`, `"node":"pve1"`, `"node":"pve2"`, 1))
	providertest.Run(t, providertest.Suite{
		Provider:      providertest.BuildWASI(t, "../cmd/proxmox-provider"),
		Catalog:       providertest.Catalog(t, "../pome/catalogs/proxmox.json"),
		NewAPI:        newNamedFake,
		GuardSettings: namedGuardSettings,
		ScriptCredentials: protocol.ScriptCredentials{TokenID: "mamela@pve!lab", TokenFile: "/token",
			GuardTokenID: "mamela-audit@pve!audit", GuardTokenFile: "/audit-token"},
		Cases: []providertest.Case{
			{Name: "named-container", Desired: []protocol.Resource{far, ct("dns", 7000, `"cores":2`)},
				World: func(api http.Handler) {
					f := api.(*fakepve.Server)
					named := handmadeCT(150, "far", false)
					named.Node, named.Pool = "pve2", ""
					f.Put(named)
					neighbour := handmadeCT(151, "neighbour", true)
					neighbour.Node, neighbour.Pool = "pve2", ""
					f.Put(neighbour)
					f.Put(handmadeCT(7000, "dns", true))
				},
				Explicit: []providertest.Explicit{{Op: proxmox.OpAdoptLXC, Resource: "ProxmoxLXC far"}, {Op: proxmox.OpAdoptLXC, Resource: "ProxmoxLXC dns"},
					{Op: proxmox.OpRebootLXC, Resource: "ProxmoxLXC far"}, {Op: proxmox.OpStopLXC, Resource: "ProxmoxLXC far"},
					{Op: proxmox.OpDeleteLXC, Resource: "ProxmoxLXC dns"}}},
		},
	})
}

// The program in the Pome is exactly a build of this source (build.sh), and its manifest file
// names it.
func TestPomeProgramIsTheBuild(t *testing.T) {
	out := filepath.Join(t.TempDir(), "provider.wasm")
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", out, "../cmd/proxmox-provider")
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	built, _ := os.ReadFile(out)
	committed, err := os.ReadFile("../pome/provider.wasm")
	if err != nil || !bytes.Equal(built, committed) {
		t.Fatal("pome/provider.wasm is not a build of this source: run ./build.sh")
	}
	sum := sha256.Sum256(committed)
	toml, _ := os.ReadFile("../pome/mamela-provider.toml")
	if !strings.Contains(string(toml), `sha256 = "`+hex.EncodeToString(sum[:])+`"`) {
		t.Error("pome/mamela-provider.toml does not name the program: run ./build.sh")
	}
	// The typed declarations (src/kinds.emel) are what the manifest generates.
	check := exec.Command("go", "tool", "mamela", "provider", "pome", "--check", "--out", "src/kinds.emel", "../pome")
	check.Env = append(os.Environ(), "GOWORK=off")
	if b, err := check.CombinedOutput(); err != nil {
		t.Errorf("%v\n%s", err, b)
	}
}

// The Pome (its hand-written Proxmox and its generated Proxmox.Kinds) and the examples compile.
func TestPomeChecks(t *testing.T) {
	dir := os.Getenv("MAMELA_TOOLCHAIN_DIR")
	if dir == "" {
		t.Skip("MAMELA_TOOLCHAIN_DIR: the toolchain of Mamela")
	}
	for _, pome := range []string{"../pome", "../examples/homelab", "../examples/k3s"} {
		cmd := exec.Command(filepath.Join(dir, "emela"), "check")
		cmd.Dir = pome
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: emela check: %v\n%s", pome, err, b)
		}
	}
}

// The catalog the Pome ships is exactly what the catalogs package writes (build.sh), and
// mamela-provider.toml lists it.
func TestShippedCatalogIsTheSource(t *testing.T) {
	toml, _ := os.ReadFile("../pome/mamela-provider.toml")
	all, err := catalogs.All()
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range all {
		want, err := catalogs.Encode(c)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile("../pome/catalogs/" + name + ".json")
		if !bytes.Equal(got, want) {
			t.Errorf("pome/catalogs/%s.json is not what catalogs writes: run ./build.sh", name)
		}
		if !strings.Contains(string(toml), name+` = "catalogs/`+name+`.json"`) {
			t.Errorf("pome/mamela-provider.toml does not list catalog %s: run ./build.sh", name)
		}
	}
}
