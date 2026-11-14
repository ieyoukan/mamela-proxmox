package proxmox

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/sdk"
)

// Provider is the provider: Mamela's provider kit (kit.Provider) with this package's kinds, and
// what kit does not do (yet) for a cluster shared with guests Mamela must never touch:
//
//   - observing: a declared VM's claim depends on its declaration (owned only with the declared
//     tag; its seeds' storage among its IDs; adoptable only inside the declared pool, node and
//     VMID range), and a VM is a node (kit reports no node): each kind's claim does it, after
//     kit's observation;
//   - diffing: a declaration of a VM that exists and is not Mamela's gets no changes (the plan
//     shows it as someone else's, and denies changing it) instead of failing the whole diff; and
//     the creates of one diff choose different VMIDs and upload an image once (a diff's choices
//     across resources).
type Provider struct {
	*kit.Provider
	mu   sync.Mutex
	run  *run
	vm   *vmKind
	tmpl *templateKind
}

// Options tune a provider for tests: Poll is how long to wait between task status reads (default
// 2s); Timeout bounds a task (default 2 hours: importing an image can take long).
type Options struct {
	Poll, Timeout time.Duration
}

// New is the provider.
func New() *Provider { return NewWith(Options{}) }

// NewWith is the provider with options.
func NewWith(o Options) *Provider {
	r := &run{}
	p := &Provider{run: r, vm: &vmKind{run: r}, tmpl: &templateKind{run: r}}
	p.Provider = &kit.Provider{
		Name: Name, Version: Version, API: API,
		ObserveNeeds: append(atPool("VM.Audit", "Pool.Audit"), atStorage("imageStorage", "Datastore.Audit")...),
		Kinds:        []kit.Kind{p.vm, p.tmpl},
		GuardCalls:   GuardCalls,
		GuardFunc: func(c *kit.Client, in protocol.GuardParams) (protocol.GuardResult, error) {
			return guard(c, p.Manifest(), in)
		},
		Scripts: Scripts,
		ScriptFunc: func(in protocol.ScriptParams) (protocol.ScriptResult, error) {
			return writeScript(p.Manifest(), in)
		},
		AfterSend: waiter(o).afterSend,
	}
	return p
}

// run is what one call of the provider shares between its kinds: an observation's reads, a diff's
// choices.
type run struct {
	obs *reader // during an observation
	// During a diff: the VMIDs earlier creates chose, and the images (node/storage/image) an
	// earlier create uploads.
	taken     map[int64]bool
	uploading map[string]bool
}

// reader reads with the observation's reader during one, afresh otherwise.
func (r *run) reader(c *kit.Client) *reader {
	if r.obs != nil {
		r.obs.c = c
		return r.obs
	}
	return &reader{c: c}
}

// claimer is a kind whose observed objects depend on their declarations (declared is nil for an
// object that is not declared).
type claimer interface {
	claim(o *protocol.Observed, declared *protocol.Resource) error
}

// Observe implements sdk.Provider: kit observes, then each kind claims what it observed.
func (p *Provider) Observe(ctx context.Context, h *sdk.Host, in protocol.ObserveParams) (protocol.ObserveResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.run.obs = &reader{cfgs: map[int64]vmConfig{}}
	defer func() { p.run.obs = nil }()
	out, err := p.Provider.Observe(ctx, h, in)
	if err != nil {
		return out, err
	}
	declared := map[string]*protocol.Resource{}
	for i := range in.Resources {
		declared[in.Resources[i].ID] = &in.Resources[i]
	}
	for i := range out.Resources {
		o := &out.Resources[i]
		var k claimer = p.vm
		if o.Kind == KindTemplate {
			k = p.tmpl
		}
		if err := k.claim(o, declared[o.ID]); err != nil {
			return out, fmt.Errorf("%s: %w", o.ID, err)
		}
	}
	return out, nil
}

// Diff implements sdk.Provider: kit diffs each declaration Mamela may change (what exists and is
// not Mamela's gets none), with the diff's choices shared.
func (p *Provider) Diff(ctx context.Context, in protocol.DiffParams) (protocol.DiffResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.run.taken, p.run.uploading = map[int64]bool{}, map[string]bool{}
	defer func() { p.run.taken, p.run.uploading = nil, nil }()
	observed := map[string]protocol.Observed{}
	for _, o := range in.Observed {
		observed[o.ID] = o
	}
	in.Desired = slices.DeleteFunc(slices.Clone(in.Desired), func(r protocol.Resource) bool {
		o := observed[r.ID]
		return o.Exists && !o.Owned && !o.Unfinished && !o.Adoptable
	})
	return p.Provider.Diff(ctx, in)
}

// taken says whether an earlier create of the diff chose a VMID, and takes it if not.
func (r *run) take(vmid int64) bool {
	if r.taken == nil {
		r.taken = map[int64]bool{}
	}
	if r.taken[vmid] {
		return false
	}
	r.taken[vmid] = true
	return true
}

// upload says whether an earlier create of the diff uploads an image to a storage, and that this
// one does if not: an image is uploaded by one change only (another change that finds it on the
// storage could not tell whether it uploaded it: lookup must tell its own effect).
func (r *run) upload(node, storage, image string) (earlier bool) {
	if r.uploading == nil {
		r.uploading = map[string]bool{}
	}
	key := node + "/" + storage + "/" + image
	earlier = r.uploading[key]
	r.uploading[key] = true
	return earlier
}

// Needs.

func atPool(caps ...string) []protocol.Need {
	var out []protocol.Need
	for _, c := range caps {
		out = append(out, protocol.Need{Capability: c, Target: protocol.Target{Type: "proxmox-acl", Ref: AtPool}, Always: true})
	}
	return out
}

func atStorage(ref string, caps ...string) []protocol.Need {
	var out []protocol.Need
	for _, c := range caps {
		out = append(out, protocol.Need{Capability: c, Target: protocol.Target{Type: "proxmox-acl", Ref: "/storage/{" + ref + "}"}, Always: true})
	}
	return out
}

func atBridge(caps ...string) []protocol.Need {
	var out []protocol.Need
	for _, c := range caps {
		out = append(out, protocol.Need{Capability: c, Target: protocol.Target{Type: "proxmox-acl", Ref: AtBridge}, Always: true})
	}
	return out
}

func needs(parts ...[]protocol.Need) []protocol.Need { return slices.Concat(parts...) }

// waiter waits for the tasks Proxmox answers a long request with.
type waiter Options

// afterSend implements kit.Provider.AfterSend: Proxmox answers a long operation with its task ID;
// apply goes on once the task ended OK.
func (w waiter) afterSend(c *kit.Client, ch protocol.Change, a protocol.Approved, body []byte) error {
	var answer jsontext.Value
	if err := data(protocol.Call{Method: a.Method, Path: a.Path}, body, &answer); err != nil {
		return err
	}
	var upid string
	if json.Unmarshal(answer, &upid) != nil || !strings.HasPrefix(upid, "UPID:") {
		return nil // done at once
	}
	if err := w.wait(c, ch.IDs["node"], upid); err != nil {
		return fmt.Errorf("%s %s: %w", a.Method, a.Path, err)
	}
	return nil
}

// wait reads a task's status until it ends, and fails unless it ended OK.
func (w waiter) wait(c *kit.Client, node, upid string) error {
	poll, timeout := w.Poll, w.Timeout
	if poll == 0 {
		poll = 2 * time.Second
	}
	if timeout == 0 {
		timeout = 2 * time.Hour
	}
	deadline := time.Now().Add(timeout)
	for {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if err := get(c, getTask, map[string]string{"node": node, "upid": upid}, nil, &st); err != nil {
			return err
		}
		if st.Status == "stopped" {
			if st.ExitStatus != "OK" && !strings.HasPrefix(st.ExitStatus, "WARNINGS") {
				return fmt.Errorf("task %s ended: %s", upid, st.ExitStatus)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("task %s is still running after %s", upid, timeout)
		}
		time.Sleep(poll)
	}
}

// decodeState decodes an observed state (nothing when there is none).
func decodeState(o protocol.Observed, v any) error {
	if len(o.State) == 0 {
		return nil
	}
	if err := json.Unmarshal(o.State, v); err != nil {
		return fmt.Errorf("%s: its state: %w", o.ID, err)
	}
	return nil
}
