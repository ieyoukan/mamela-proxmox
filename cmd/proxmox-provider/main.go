// Command proxmox-provider is the Proxmox VE provider as a program. build.sh builds it for WASI
// into the Pome (pome/provider.wasm).
package main

import (
	"github.com/ieyoukan/mamela-proxmox/proxmox"
	"github.com/ieyoukan/mamela/provider/sdk"
)

func main() { sdk.Serve(proxmox.New()) }
