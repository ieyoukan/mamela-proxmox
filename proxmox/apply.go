package proxmox

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// Applying and looking up are the same for both kinds: kit sends a change's approved requests in
// order, each at most once, and skips those in effect already (an earlier apply that was
// interrupted), which done tells; lookup tells how much of a change is in effect.

func req(call protocol.Call, params map[string][]string) protocol.Approved {
	return protocol.Approved{Method: call.Method, Path: call.Path, Params: params}
}

// target is the one VM (or template) on one node a change names.
func target(ch protocol.Change) (string, int64, error) {
	vmid, err := strconv.ParseInt(ch.IDs["vmid"], 10, 64)
	if err != nil || ch.IDs["node"] == "" {
		return "", 0, errors.New("a change names one VM on one node")
	}
	return ch.IDs["node"], vmid, nil
}

// done implements kit.Kind.Done: whether an approved request's effect is there already. An explicit
// operation first makes sure the VM is still the one it was prepared for.
func done(c *kit.Client, ch protocol.Change, a protocol.Approved) (bool, error) {
	node, vmid, err := target(ch)
	if err != nil {
		return false, err
	}
	if explicit(ch.Op) {
		if err := ensureOwn(c, ch, node, vmid); err != nil {
			return false, err
		}
	}
	ready := map[string]protocol.ArtifactInfo{}
	for _, art := range ch.Artifacts {
		info, err := c.Host().Artifact(c.Context(), art.ID) // made once by kit's apply: this reads it back
		if err != nil {
			return false, err
		}
		ready[art.ID] = info
	}
	params, err := protocol.FillArtifacts(a.Params, ready)
	if err != nil {
		return false, err
	}
	storage := ch.IDs["storage"]
	switch {
	case a.Path == upload.Path:
		return hasVolume(c, node, storage, contentOf(params), ready[a.Upload].Name)
	case a.Path == createVM.Path || a.Path == cloneVM.Path:
		vms, err := list(c)
		if err != nil {
			return false, err
		}
		vm, exists := vms[vmid]
		if !exists {
			return false, nil
		}
		cfg, err := config(c, vm.Node, vmid)
		if err != nil {
			return false, err
		}
		if m, ok := parseMarker(cfg.Description); ok && m.Name == params["name"][0] {
			return true, nil // an earlier apply created (or cloned) it
		}
		if m, ok := parseTemplateMarker(cfg.Description); ok && m.Name == params["name"][0] && a.Path == createVM.Path {
			return true, nil // an earlier apply created the template
		}
		return false, fmt.Errorf("VMID %d is taken by a VM this provider did not create; plan again", vmid)
	case a.Path == convertTemplate.Path:
		cfg, err := config(c, node, vmid)
		if err != nil {
			return false, err
		}
		return cfg.Template, nil
	case a.Path == setConfig.Path && params["ide2"] != nil:
		cfg, err := config(c, node, vmid)
		if err != nil {
			return false, err
		}
		return cfg.IDE2 != "" && strconv.FormatInt(cfg.Memory, 10) == params["memory"][0] && strconv.FormatInt(cfg.Cores, 10) == params["cores"][0], nil
	case a.Path == resizeDisk.Path && params["digest"] == nil:
		cfg, err := config(c, node, vmid)
		if err != nil {
			return false, err
		}
		want, _ := strconv.ParseInt(strings.TrimSuffix(params["size"][0], "G"), 10, 64)
		return cfg.DiskGiB >= want, nil
	case a.Path == setConfig.Path && params["digest"] == nil:
		cfg, err := config(c, node, vmid)
		if err != nil {
			return false, err
		}
		return slices.Contains(cfg.Tags, params["tags"][0]), nil
	case a.Path == startVM.Path, a.Path == stopVM.Path, a.Path == shutdownVM.Path:
		vms, err := list(c)
		if err != nil {
			return false, err
		}
		return (vms[vmid].Status == "running") == (a.Path == startVM.Path), nil
	}
	return false, nil
}

func contentOf(params map[string][]string) string {
	if c := params["content"]; len(c) == 1 {
		return c[0]
	}
	return ""
}

// lookup implements kit.Looker: how much of a change is in effect.
func lookup(c *kit.Client, ch protocol.Change) (protocol.LookupStatus, error) {
	node := ch.IDs["node"]
	vmid, _ := strconv.ParseInt(ch.IDs["vmid"], 10, 64)
	vms, err := list(c)
	if err != nil {
		return "", err
	}
	if explicit(ch.Op) {
		return lookupExplicit(c, ch, node, vmid, vms)
	}
	vm, exists := vms[vmid]
	if !exists {
		// Not created; the uploads before the create may have landed (an image, the seed: Mamela
		// names seeds mamela-seed-<node>-<vmid>-...).
		if ch.Op == OpCreate || ch.Op == OpCreateTemplate {
			for _, a := range ch.Artifacts {
				content, prefix := "iso", fmt.Sprintf("mamela-seed-%s-%d", a.Node, a.NodeID)
				if a.Kind == protocol.ArtifactDownload {
					content, prefix = "import", a.Name
				}
				found, err := hasVolumePrefix(c, node, ch.IDs["storage"], content, prefix)
				if err != nil {
					return "", err
				}
				if found && slices.ContainsFunc(ch.Requests, func(r protocol.Approved) bool { return r.Upload == a.ID }) {
					return protocol.LookupPartly, nil
				}
			}
		}
		return protocol.LookupNotDone, nil
	}
	cfg, err := config(c, node, vmid)
	if err != nil {
		return "", err
	}
	_, vmMarked := parseMarker(cfg.Description)
	_, templateMarked := parseTemplateMarker(cfg.Description)
	if !vmMarked && !templateMarked {
		return protocol.LookupNotDone, nil
	}
	want, got := 0, 0
	check := func(ok bool) {
		want++
		if ok {
			got++
		}
	}
	for _, a := range ch.Requests {
		switch a.Path {
		case createVM.Path, cloneVM.Path:
			check(true)
		case convertTemplate.Path:
			check(cfg.Template)
		case resizeDisk.Path:
			n, _ := strconv.ParseInt(strings.TrimSuffix(a.Params["size"][0], "G"), 10, 64)
			check(cfg.DiskGiB >= n)
		case setConfig.Path:
			if t := a.Params["tags"]; t != nil {
				check(slices.Contains(cfg.Tags, t[0]))
			}
			if v := a.Params["cores"]; v != nil {
				check(strconv.FormatInt(cfg.Cores, 10) == v[0])
			}
			if v := a.Params["memory"]; v != nil {
				check(strconv.FormatInt(cfg.Memory, 10) == v[0])
			}
			if v := a.Params["ide2"]; v != nil {
				check(cfg.IDE2 != "")
			}
		case startVM.Path:
			check(vm.Status == "running")
		case shutdownVM.Path:
			check(vm.Status != "running")
		}
	}
	switch {
	case got == want:
		return protocol.LookupDone, nil
	case got == 0 && ch.Op != OpCreate && ch.Op != OpCreateTemplate:
		return protocol.LookupNotDone, nil
	}
	return protocol.LookupPartly, nil
}
