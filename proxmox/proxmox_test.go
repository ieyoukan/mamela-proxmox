package proxmox_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela-proxmox/fakepve"
	"github.com/ieyoukan/mamela-proxmox/proxmox"
	"github.com/ieyoukan/mamela/provider/catalogs"
	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/providertest"
)

const site = `"node":"pve1","pool":"lab","diskStorage":"local-lvm","imageStorage":"local","bridge":"vmbr0","tag":"mamela-lab","vmidMin":7000,"vmidMax":7009`

func vm(name, rest string) protocol.Resource {
	return protocol.Resource{ID: "ProxmoxVM " + name, Kind: "ProxmoxVM", Name: name, Spec: []byte(`{` + site + `,` + rest + `}`)}
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
	for _, g := range proxmox.Grants(proxmox.Provider{}.Manifest(), s) {
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
		Catalog:       catalogs.Proxmox,
		NewAPI:        func() http.Handler { return newFake() },
		GuardSettings: guardSettings,
		Cases: []providertest.Case{
			{Name: "one-vm", Desired: []protocol.Resource{
				vm("web", `"cpu":1,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04","network":{"mode":"dhcp"}`)},
				Explicit: []providertest.Explicit{{Op: proxmox.OpReboot, Resource: "ProxmoxVM web"}, {Op: proxmox.OpReset, Resource: "ProxmoxVM web"},
					{Op: proxmox.OpDelete, Resource: "ProxmoxVM web"}}},
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
			{Name: "two-vms-static", Desired: []protocol.Resource{
				vm("a", `"cpu":2,"memoryMiB":2048,"diskGiB":20,"image":"ubuntu-24.04","network":{"mode":"static","address":"192.0.2.40/24","gateway":"192.0.2.1","dns":["192.0.2.1"]}`),
				vm("b", `"cpu":1,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04","network":{"mode":"dhcp"}`)}},
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
}
