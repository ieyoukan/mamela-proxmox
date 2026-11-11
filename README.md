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

しないこと（差分としては示し、Mamela が拒みます）:

- ディスクの縮小
- イメージの変更
- ネットワークの変更

マーカーのない VM には触れません。宣言を消しても、VM は消しません。

### 明示操作

plan には入らず、人が名前を打ち直したときだけ行う操作です（`mamela act <op> <resource> --confirm <name>`）。対象は、Mamela が作成を記録した VM に限ります。

| 操作 | 内容 |
|---|---|
| `delete-vm` | 止めてから、VM とディスクを消す（データを失う） |
| `reboot-vm` | ゲストに再起動を頼む（ACPI） |
| `reset-vm` | すぐに再起動する（リセットボタンと同じ。応答しないゲスト向け） |
| `adopt-vm` | Mamela の旧組み込みプロバイダーが作った VM を引き継ぐ（このプロバイダーのマーカーとタグを書く） |

送る直前に、VMID の VM がまだその名前のマーカーを持つことを確かめます。違えば何も送りません。

### guard（適用の前後の安全確認）

Proxmox のクラスタには、Mamela と関係のないゲストが動いていることがあります。Mamela は、このプロバイダーの変更を apply する前後に guard を呼びます。guard は読み取りしかできず、lab の外も見える監査用の Token（`guard_token_id`）で読みます。

| コード | 前 / 後 | 確かめること |
|---|---|---|
| `PVE-0001`〜`0003` | 前 | クォーラム、ノードの数、対象のノードが動いていること |
| `PVE-0101`・`0102` | 前 | 操作用の Token の権限が、このプロバイダーに要る権限とちょうど同じこと（多すぎも少なすぎも止める） |
| `PVE-0201`〜`0203` | 前 | 変更の後に残るメモリとストレージの余裕、イメージを置けるストレージ |
| `PVE-0301`〜`0303` | 前 | pool と VMID の範囲に他人のゲストがないこと、VM の数 |
| `PVE-0304` | 前 | 各変更が、運用者の決めた境界（ノード、範囲、pool、ストレージ、ブリッジ、タグ）の中にあること。宣言が何を書いても、ここで止まる |
| `PVE-0400`〜`0405` | 後 | 境界の外のゲストが、消えず、止まらず、設定も変わらず、再起動もしていないこと |

`block` が 1 つでもあれば、Mamela はそのプロバイダーの変更を送りません。

## 使い方

宣言の Pome に足します。

```bash
emela pome add github.com/ieyoukan/mamela-proxmox/pome@v0.2.0
```

Mamela が依存から見つけて、隔離して動かします。プログラムは Pome.lock と sumdb で固定され、`mamela-provider.toml` の sha256 で確かめられます。

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
# guard だけが使う、読み取り専用の監査用 Token（PVEAuditor）
guard_token_id = "mamela-audit@pve!audit"
guard_token_file = "~/.config/mamela/audit-token"

# guard の設定: 運用者が決める境界と安全の閾値（秘密ではない）
[provider.proxmox.guard]
node = "pve1"
pool = "mamela"
vmid_min = 7000
vmid_max = 7099
tag = "mamela"
disk_storage = "local-lvm"
image_storage = "local"
bridge = "vmbr0"
operating_token = "mamela@pve!lab"     # token_id と同じ
max_vms = 6
min_host_mem_available_mib = 4096
max_storage_used_percent = 85
```

### 準備と片付け（root で実行するスクリプト）

Mamela の Token には、pool や Token や ACL を作る権限はありません。そこで、人が読んでから root で実行するスクリプトを、このプロバイダー（Pome で固定された WASM）が書きます。Mamela だけで得られ、Go は要りません。

```bash
mamela provider script <pome-dir> proxmox                  # 書けるスクリプトの一覧
mamela provider script <pome-dir> proxmox permissions      # 操作用の Token の権限と、それが要る操作
mamela provider script <pome-dir> proxmox bootstrap > bootstrap.sh
mamela provider script <pome-dir> proxmox cleanup   > cleanup.sh
```

- **プロバイダーに渡るもの:** `mamela.providers.toml` の `[provider.proxmox.guard]` と、Token の ID と秘密のファイルの場所だけです。秘密そのものは渡りません。
- **Proxmox への要求:** スクリプトを書く間、プロバイダーは Proxmox に何も送れません（host が拒みます）。
- **bootstrap:** pool、イメージを置けるストレージ、パスごとのロール、操作用と監査用のユーザーと Token、ACL を作ります。不要になった ACL は外します。何度実行しても同じ状態になります。権限は、guard が確かめるのと同じマニフェストから導くので、ずれません。
- **cleanup:** どの VM も使っていないアップロード（`mamela-` で始まるイメージと seed）を一覧にします。`--free` を付けたときだけ消します。

## 開発

```bash
# Go が要るのは、プロバイダーを開発するときだけ（WASM にビルドし、Mamela の適合試験を通す）
./build.sh                          # pome/provider.wasm と pome/mamela-provider.toml を作り直す
MAMELA_TOOLCHAIN_DIR=../mamela/.tools/bin go test ./...   # Mamela の適合試験（偽の Proxmox で）
```

- **公開の方法:** このリポジトリのタグ（`pome/vX.Y.Z`）を push します。Pome の公開は取り消せません。プログラムを変えたら `version` を上げ、`./build.sh` で作り直してからタグを付けます。
