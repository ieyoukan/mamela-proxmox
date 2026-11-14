// Package catalogs is the catalog of the Proxmox VE API the provider calls (ADR 0008 of Mamela):
// what each call does (its effect, where the API reads each parameter, its precondition, what it
// must name, how big what it makes is).
//
// It is data. build.sh writes it into the Pome (pome/catalogs/proxmox.json, by
// cmd/proxmox-catalogs), and Mamela uses it only when the operator names it in
// mamela.providers.toml (catalog = "pome:proxmox"). The provider program never sees it: Mamela
// classifies every request the program sends with it. The JSON is what is shipped and reviewed
// (mamela provider catalog pome).
//
// It is generated, so that a new Proxmox VE is easy to follow (generate.go):
//
//   - the calls and their parameters are the ones the provider's manifest declares, placed where
//     Proxmox reads them (the query of a GET or DELETE, the form of a POST or PUT);
//   - each call's effect comes from rules on its method and path, or from the reviewed table
//     (review.go), which also holds what a person decided about a call: its precondition, the
//     values it may have, what it must name, what it sizes; a call that neither classifies is an
//     error, never a read;
//   - checked against Proxmox VE's own API schema (schema.go: the apidoc.js of pve-docs, pinned
//     by pve-api.lock): every call and parameter exists, takes API tokens, the reviewed handler is
//     the same, and what the schema requires of a call is what the permission catalog
//     (proxmox/permissions.go) grants.
package catalogs

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/ieyoukan/mamela-proxmox/proxmox"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// All are the catalogs, by the name the Pome ships each under.
func All() (map[string]protocol.Catalog, error) {
	c, err := Proxmox()
	if err != nil {
		return nil, err
	}
	return map[string]protocol.Catalog{"proxmox": c}, nil
}

// Proxmox is the catalog of the part of the Proxmox VE API that the provider uses: the calls its
// manifest declares, with the reviewed table.
func Proxmox() (protocol.Catalog, error) {
	return Generate(proxmox.API, proxmox.New().Manifest(), Reviewed)
}

// Encode writes a catalog file: the schema, then the catalog, indented and deterministic.
func Encode(c protocol.Catalog) ([]byte, error) {
	c.Schema = protocol.CatalogSchema
	if err := c.Check(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(c, json.Deterministic(true), jsontext.Multiline(true), jsontext.WithIndent("  "))
	return append(data, '\n'), err
}
