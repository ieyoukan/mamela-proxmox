# mamela-proxmox

[Mamela](https://github.com/ieyoukan/mamela) のプロバイダーです。Proxmox VE の VM を作り、その上で Mamela のエージェント（mamelet）を動かします。

- **隔離して動く:** プロバイダーは WASM で動きます。Proxmox には、Mamela の host を通してしかつなげません。
- **host が持つもの:**
  - Token
  - Proxmox の API のカタログ（各呼び出しが何をするか）
- **host がすること:**
  - 承認した要求だけを送る
  - VM を作るためのファイルを用意する（固定したクラウドイメージ、mamelet の seed）

プロバイダーはファイルの中身にも Token にも触れません（Mamela の ADR 0002、0004）。

## できること

| 操作 | 内容 |
|---|---|
| `create-vm` | イメージと seed をアップロードして、VM を作る（止まったまま。マーカーとタグを付ける） |
| `start-vm` | 起動する（同じ実行のすべての VM を作った後） |
| `grow-vm-disk` | ディスクを大きくする |
| `resize-vm` | vCPU とメモリを変える（動いていれば再起動する） |
| `tag-vm` | 中断された作成の、タグ付けの続き |

しないこと:

- ディスクの縮小
- イメージの変更
- ネットワークの変更
- 削除

マーカーのない VM には触れません。

## 使い方

```emela
import Mamela.{Dhcp, Static}
import Proxmox

site = Proxmox.Site(node: "pve1", pool: "mamela", disk_storage: "local-lvm", image_storage: "local",
  bridge: "vmbr0", tag: "mamela", vmid_min: 7000, vmid_max: 7099)
web = Proxmox.Vm(name: "web", cpu: 2, memory_mib: 2048, disk_gib: 20, image: "ubuntu-24.04",
  network: Static(address: "192.0.2.40/24", gateway: "192.0.2.1", dns: ["192.0.2.1"]))

Mamela.empty()
  |> Proxmox.vm(site, web)
  |> Mamela.package_on(Proxmox.node(web), "nginx")
  |> Mamela.service_on(Proxmox.node(web), "nginx", True, True)
```

`mamela.providers.toml` に、API の場所と Token を書きます。カタログは Mamela のものを使います。

```toml
[provider.proxmox]
api_url = "https://pve.example:8006/api2/json"
auth = "proxmox"
token_id = "mamela@pve!lab"
token_file = "~/.config/mamela/lab-token"
tls_fingerprint = "AA:BB:..."
catalog = "builtin:proxmox"
```

## 開発

```bash
./build.sh                          # pome/provider.wasm と pome/mamela-provider.toml を作り直す
MAMELA_TOOLCHAIN_DIR=../mamela/.tools/bin go test ./...   # Mamela の適合試験（偽の Proxmox で）
```

- **公開の前提:** `pome/Pome.toml` の `[replace]` は、Mamela の Pome（`pome/mamela` v0.2.0）を公開するまでの仮の設定です。公開の前に外します。
- **公開の方法:** このリポジトリのタグ（`pome/vX.Y.Z`）を push します。Pome の公開は取り消せません。
