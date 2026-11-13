package proxmox

import (
	"slices"
	"testing"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// Every call of every operation (and of observing) is in the permission catalog, and the
// operation's needs cover what the catalog says it needs: the grants are never short of a call.
func TestNeedsCoverTheCatalog(t *testing.T) {
	m := New().Manifest()
	covers := func(needs []protocol.Need, p Privilege) bool {
		return slices.ContainsFunc(needs, func(n protocol.Need) bool {
			return n.Target.Type == "proxmox-acl" && n.Target.Ref == p.Ref && slices.Contains(p.Privs, n.Capability)
		})
	}
	check := func(what string, needs []protocol.Need, calls []protocol.Call) {
		for _, c := range calls {
			ps, ok := PrivilegesOf(c)
			if !ok {
				t.Errorf("%s: %s %s (or a parameter of it) is not in the permission catalog", what, c.Method, c.Path)
				continue
			}
			for _, p := range ps {
				if !covers(needs, p) {
					t.Errorf("%s: %s %s needs one of %v on %s (%s), and the manifest does not say so", what, c.Method, c.Path, p.Privs, p.Ref, p.Why)
				}
			}
		}
	}
	check("observe", m.Observe, m.ObserveCalls)
	for _, k := range m.Kinds {
		for _, o := range k.Operations {
			if o.Support == protocol.Supported {
				// An operation's calls include observing's reads: its needs, with observing's.
				check(string(o.Op), append(slices.Clone(m.Observe), o.Needs...), o.Calls)
			}
		}
	}
}

// And no more: every privilege an operation declares is one a call of it needs (the narrowest of
// Proxmox's alternatives), so the bootstrap grants nothing the provider never uses.
func TestNeedsAreNoMoreThanTheCatalog(t *testing.T) {
	m := New().Manifest()
	used := func(calls []protocol.Call, n protocol.Need) bool {
		for _, c := range calls {
			ps, _ := PrivilegesOf(c)
			for _, p := range ps {
				if p.Ref == n.Target.Ref && p.Privs[0] == n.Capability {
					return true
				}
			}
		}
		return false
	}
	for _, n := range m.Observe {
		if !used(m.ObserveCalls, n) {
			t.Errorf("observe declares %s on %s, which no observing call needs", n.Capability, n.Target.Ref)
		}
	}
	for _, k := range m.Kinds {
		for _, o := range k.Operations {
			for _, n := range o.Needs {
				if !used(o.Calls, n) {
					t.Errorf("%s declares %s on %s, which none of its calls needs", o.Op, n.Capability, n.Target.Ref)
				}
			}
		}
	}
}
