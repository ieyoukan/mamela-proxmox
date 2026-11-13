package catalogs

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// Rules classify a call from its method and path. A call no rule classifies needs a reviewed
// effect (Review.Effect): there is no default, so a call that changes something is never taken for
// a read.
//
//   - GET: a read (Proxmox VE's GET handlers change nothing);
//   - DELETE: lose-data (deleting something may destroy what it holds);
//   - POST .../status/<start|stop|shutdown|reboot|reset|suspend|resume> of a guest: restart.
var powerAction = regexp.MustCompile(`^/nodes/\{[^}]+\}/(qemu|lxc)/\{[^}]+\}/status/(start|stop|shutdown|reboot|reset|suspend|resume)$`)

// Classify is a call's effect, and why.
func Classify(method, path string, r Review) (protocol.Effect, string, error) {
	rule, why := protocol.Effect(""), ""
	switch {
	case method == "GET":
		rule, why = protocol.EffectRead, "a GET reads"
	case method == "DELETE":
		rule, why = protocol.EffectLoseData, "a DELETE may destroy data"
	case method == "POST" && powerAction.MatchString(path):
		rule, why = protocol.EffectRestart, "a guest's power action interrupts it"
	}
	switch {
	case r.Effect == "" && rule == "":
		return "", "", fmt.Errorf("%s %s: no rule classifies it: review it (catalogs/review.go: its effect and why)", method, path)
	case r.Effect == "":
		return rule, why, nil
	case r.Effect == protocol.EffectRead && method != "GET":
		return "", "", fmt.Errorf("%s %s: a call that is not a GET is never classified as a read", method, path)
	case r.Why == "":
		return "", "", fmt.Errorf("%s %s: a reviewed effect says why", method, path)
	}
	return r.Effect, r.Why, nil
}

// placement is where Proxmox reads a call's parameters: the query of a GET or DELETE, the form
// of a POST or PUT.
func placement(method string) (string, error) {
	switch method {
	case "GET", "DELETE":
		return "query", nil
	case "POST", "PUT":
		return "form", nil
	}
	return "", fmt.Errorf("method %s is not one Proxmox VE's API has", method)
}

// Calls are every call a manifest declares: observing's, the guard's, each operation's.
func Calls(m protocol.Manifest) []protocol.Call {
	calls := slices.Clone(m.ObserveCalls)
	if m.Guard != nil {
		calls = append(calls, m.Guard.Calls...)
	}
	for _, k := range m.Kinds {
		for _, o := range k.Operations {
			calls = append(calls, o.Calls...)
		}
	}
	return calls
}

// Generate makes the catalog of an API from the calls a manifest declares: one entry for each
// method and path, with every parameter any call declares for it, where Proxmox reads it; the
// file of an upload; its effect (Classify); and the reviewed parts of its review. A call that is
// not a GET must be reviewed, and a review must be of a declared call.
func Generate(api string, m protocol.Manifest, reviewed map[string]Review) (protocol.Catalog, error) {
	type group struct {
		method, path string
		params       []string
		file         string
	}
	groups := map[string]*group{}
	for _, c := range Calls(m) {
		key := c.Method + " " + c.Path
		where, err := placement(c.Method)
		if err != nil {
			return protocol.Catalog{}, err
		}
		switch {
		case len(c.Fixed) > 0 || len(c.Header) > 0 || len(c.JSON) > 0:
			return protocol.Catalog{}, fmt.Errorf("%s: Proxmox VE's API reads no fixed parameter, header or JSON body", key)
		case where == "query" && len(c.Form) > 0, where == "form" && len(c.Query) > 0:
			return protocol.Catalog{}, fmt.Errorf("%s: Proxmox reads its parameters from the %s, and the provider declares %v in the query and %v in the form",
				key, where, c.Query, c.Form)
		}
		g := groups[key]
		if g == nil {
			g = &group{method: c.Method, path: c.Path}
			groups[key] = g
		}
		for _, p := range c.Params() {
			if !slices.Contains(g.params, p) {
				g.params = append(g.params, p)
			}
		}
		if c.File != "" {
			if g.file != "" && g.file != c.File {
				return protocol.Catalog{}, fmt.Errorf("%s: uploads in %s and in %s", key, g.file, c.File)
			}
			g.file = c.File
		}
	}
	for key := range reviewed {
		if groups[key] == nil {
			return protocol.Catalog{}, fmt.Errorf("the review of %s is of no call the provider declares: remove it", key)
		}
	}
	c := protocol.Catalog{API: api}
	for _, g := range slices.SortedFunc(maps.Values(groups), func(a, b *group) int {
		return cmp.Or(strings.Compare(a.path, b.path), strings.Compare(a.method, b.method))
	}) {
		key := g.method + " " + g.path
		r, ok := reviewed[key]
		if !ok && g.method != "GET" {
			return protocol.Catalog{}, fmt.Errorf("%s changes something and is not reviewed: add it to catalogs/review.go (the handler it was reviewed for, "+
				"and its effect if no rule gives one)", key)
		}
		effect, _, err := Classify(g.method, g.path, r)
		if err != nil {
			return protocol.Catalog{}, err
		}
		slices.Sort(g.params)
		e := protocol.Entry{Method: g.method, Path: g.path, Effect: effect, File: g.file,
			Precondition: r.Precondition, Measures: r.Measures, Values: r.Values, Bind: r.Bind}
		if where, _ := placement(g.method); where == "query" {
			e.Query = g.params
		} else {
			e.Form = g.params
		}
		c.Entries = append(c.Entries, e)
	}
	return c, c.Check()
}
