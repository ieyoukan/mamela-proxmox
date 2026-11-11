#!/usr/bin/env bash
# Builds the provider into its Pome (pome/provider.wasm) and writes its sha256 into
# pome/mamela-provider.toml. The build is reproducible: TestPomeProgramIsTheBuild rebuilds it and
# checks that the committed program is exactly this build of the source.
set -euo pipefail
cd "$(dirname "$0")"
# The program must be reproducible: the same source and the same Go give the same bytes (CI checks
# it). Build with exactly this Go release.
want_go=go1.27.3
got_go="$(GOTOOLCHAIN=${GOTOOLCHAIN:-auto} go env GOVERSION)"
if [[ "$got_go" != "$want_go" ]]; then
  echo "build with $want_go (this is $got_go): GOTOOLCHAIN=$want_go ./build.sh" >&2
  exit 1
fi
# The catalog of the API (ADR 0008 of Mamela), as data in the Pome.
mkdir -p pome/catalogs
GOWORK=off go run ./cmd/proxmox-catalogs pome/catalogs
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

# The catalog of the API the provider calls (ADR 0008 of Mamela). Mamela classifies the program's
# requests with it only when the operator names it (mamela.providers.toml: catalog = "pome:proxmox").
[catalogs]
proxmox = "catalogs/proxmox.json"
TOML
echo "$out $sum (version $version)"
