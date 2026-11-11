#!/usr/bin/env bash
# Builds the provider into its Pome (pome/provider.wasm) and writes its sha256 into
# pome/mamela-provider.toml. The build is reproducible: TestPomeProgramIsTheBuild rebuilds it and
# checks that the committed program is exactly this build of the source.
set -euo pipefail
cd "$(dirname "$0")"
out=pome/provider.wasm
GOOS=wasip1 GOARCH=wasm go build -trimpath -buildvcs=false -o "$out" ./cmd/proxmox-provider
sum="$(shasum -a 256 "$out" | cut -d' ' -f1)"
version="$(sed -n 's/^\tVersion = "\(.*\)"$/\1/p' proxmox/proxmox.go)"
cat > pome/mamela-provider.toml <<TOML
# The provider program of this Pome. Mamela runs it isolated (WASM) and checks this sha256;
# Pome.lock and the sumdb pin the whole Pome, this file and the program included.
provider = "proxmox"
version = "$version"
program = "provider.wasm"
sha256 = "$sum"
TOML
echo "$out $sum (version $version)"
