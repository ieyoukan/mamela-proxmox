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
	"github.com/ieyoukan/mamela/provider/catalogs"
	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/providertest"
)

const site = `"node":"pve1","pool":"lab","diskStorage":"local-lvm","imageStorage":"local","bridge":"vmbr0","tag":"mamela-lab","vmidMin":7000,"vmidMax":7009`

func vm(name, rest string) protocol.Resource {
	return protocol.Resource{ID: "ProxmoxVM " + name, Kind: "ProxmoxVM", Name: name, Spec: []byte(`{` + site + `,` + rest + `}`)}
}

// The provider passes Mamela's conformance suite against a fake Proxmox, with the catalog Mamela
// ships for Proxmox: every request within its permit, convergence, undeclared VMs kept, and a
// lost or unsent request at any point recovered.
func TestConformance(t *testing.T) {
	providertest.Run(t, providertest.Suite{
		Provider: providertest.BuildWASI(t, "../cmd/proxmox-provider"),
		Catalog:  catalogs.Proxmox,
		NewAPI:   func() http.Handler { return fakepve.New() },
		Cases: []providertest.Case{
			{Name: "one-vm", Desired: []protocol.Resource{
				vm("web", `"cpu":1,"memoryMiB":1024,"diskGiB":10,"image":"ubuntu-24.04","network":{"mode":"dhcp"}`)}},
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
