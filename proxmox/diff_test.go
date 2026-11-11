package proxmox

import (
	"context"
	"encoding/json/v2"
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
	had := State{CPU: 1, MemoryMiB: 1024, DiskGiB: 20, Image: "ubuntu-24.04", Network: "dhcp", Tagged: true, Running: true}
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
		out, err := Provider{}.Diff(context.Background(), protocol.DiffParams{Desired: []protocol.Resource{r}, Observed: []protocol.Observed{observed(had)}})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !slices.Equal(ops(out.Changes), c.want) {
			t.Errorf("%s: %v, want %v", c.name, ops(out.Changes), c.want)
		}
		for _, ch := range out.Changes {
			if o, _ := (Provider{}).Manifest().Operation(Kind, ch.Op); o.Support == protocol.Unsupported && (len(ch.Requests) > 0 || len(ch.Reasons) == 0) {
				t.Errorf("%s: unsupported %s sends %v (reasons %v)", c.name, ch.Op, ch.Requests, ch.Reasons)
			}
		}
	}
	// Another image is reported too.
	other := had
	other.Image = "debian-12"
	r := protocol.Resource{ID: "ProxmoxVM web", Kind: Kind, Name: "web", Spec: []byte(`{` + testSite + `,"cpu":1,"memoryMiB":1024,"diskGiB":20,"image":"ubuntu-24.04","network":{"mode":"dhcp"}}`)}
	out, _ := Provider{}.Diff(context.Background(), protocol.DiffParams{Desired: []protocol.Resource{r}, Observed: []protocol.Observed{observed(other)}})
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
	created, err := Provider{}.Diff(context.Background(), protocol.DiffParams{Desired: []protocol.Resource{r},
		Observed: []protocol.Observed{{ID: r.ID, Kind: Kind, Name: "web", State: free}}})
	if err != nil || len(created.Changes) != 2 {
		t.Fatalf("%v %v", created.Changes, err)
	}
	stopped, _ := json.Marshal(State{CPU: 1, MemoryMiB: 1024, DiskGiB: 10, Image: "ubuntu-24.04", Network: "dhcp", Tagged: true})
	later, err := Provider{}.Diff(context.Background(), protocol.DiffParams{Desired: []protocol.Resource{r},
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
	if _, err := (Provider{}).Prepare(t.Context(), protocol.PrepareParams{Op: OpDeleteTemplate, Observed: o}); err == nil ||
		!strings.Contains(err.Error(), "7001") {
		t.Fatalf("a template with a clone: %v", err)
	}
	o.State = []byte(`{"image":"ubuntu-24.04","template":true,"tagged":true}`)
	r, err := (Provider{}).Prepare(t.Context(), protocol.PrepareParams{Op: OpDeleteTemplate, Observed: o})
	if err != nil || len(r.Change.Requests) != 1 || r.Change.Requests[0].Method != "DELETE" {
		t.Fatalf("a template without clones: %+v %v", r.Change, err)
	}
	if _, err := (Provider{}).Prepare(t.Context(), protocol.PrepareParams{Op: OpReboot, Observed: o}); err == nil {
		t.Error("a template is rebooted")
	}
}
