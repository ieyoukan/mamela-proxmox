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

Proxmox の安全装置（guard、明示操作、root 用のスクリプト）もこのプロバイダーにあります。Mamela 本体は、それを汎用の仕組みとして強制します（Mamela の ADR 0006）。共有の本番クラスタで使う手順は [docs/lab.md](docs/lab.md) にあります。

## できること

| 操作 | 内容 |
|---|---|
| `create-template` | イメージから VM を一度だけ作り、テンプレートにする（タグとマーカーを付ける。VMID は範囲の上から） |
| `finish-template` | 中断された作成の、タグ付けとテンプレートへの変換の続き |
| `create-vm` | seed をアップロードし、テンプレートの linked clone として VM を作る（数秒。止まったまま。サイズ・seed・ディスク・タグを設定する）。テンプレートを使わない宣言では、イメージを VM のディスクに取り込む |
| `finish-clone` | 中断された複製の、seed とサイズの設定の続き |
| `start-vm` | 起動する（同じ実行のすべての VM を作った後） |
| `shutdown-vm` | 止めておくと宣言した（`stopped`）VM が動いていれば、ゲストに電源を切るよう頼む（ACPI） |
| `grow-vm-disk` | ディスク（scsi0）を大きくする |
| `resize-vm` | vCPU とメモリを変える（動いていて、止めておく宣言でなければ再起動する） |
| `tag-vm` | 中断された作成の、タグ付けの続き |

しないこと（差分としては示し、Mamela が拒みます）:

| 操作 | 理由 |
|---|---|
| `shrink-vm-disk` | ディスクの縮小はデータを壊しうる |
| `replace-vm-image` | イメージの変更は、VM とそのデータを作り直すことになる |
| `change-vm-network` | ネットワークは作るときに一度だけ与える（seed） |
| `move-vm` | 別のノード・pool・範囲外の VMID への移動はしない。手で移す（`qm migrate`）か、今ある場所を宣言する |
| `create-vm-without-image` | イメージのない宣言（Terraform から取り込んだもの）の VM がなくなっても、作り直さない |

マーカーのない VM には触れません。宣言を消しても、VM もテンプレートも消しません。

### テンプレート

イメージを VM ごとに取り込むと、遅いディスクでは 1 台に 15〜20 分かかります。そのため VM は、イメージのテンプレートの linked clone にします。LVM-thin のスナップショットなので、数秒でできます。

- **宣言:** `Proxmox.vm` は、VM の複製元のテンプレート（`ProxmoxTemplate <image>-<node>`）も宣言します。同じイメージとノードの VM がいくつあっても、Mamela は同じ宣言を 1 つのリソースとみなします。VM は、テンプレートができあがるまで待ちます。
- **作る:** Mamela は、テンプレートを最初の plan で一度だけ作ります。pool・VMID の範囲（上から）・タグの中に置き、マーカーを付けます。テンプレートは起動しません。
- **消す:** 明示操作 `delete-template` だけです。そのテンプレートの linked clone の VM が 1 台でもあれば拒みます。
- **イメージの更新:** イメージの版を上げると、新しいテンプレートを宣言することになります。古いテンプレートは、使う VM がなくなってから消します。
- **要る権限:** 操作用 Token に、`VM.Clone`（テンプレートの複製）と `VM.Config.CDROM`（複製した VM に seed を付ける）が足されます。bootstrap を root でもう一度実行するまで、guard の PVE-0102 が止めます。

### 明示操作

plan には入らず、人が名前を打ち直したときだけ行う操作です（`mamela act <op> <resource> --confirm <name>`）。対象は、Mamela が作成を記録した VM に限ります。

| 操作 | 内容 |
|---|---|
| `delete-vm` | 止めてから、VM とディスクを消す（データを失う） |
| `reboot-vm` | ゲストに再起動を頼む（ACPI） |
| `reset-vm` | すぐに再起動する（リセットボタンと同じ。応答しないゲスト向け） |
| `stop-vm` | すぐに止める（電源を抜くのと同じ。`shutdown-vm` に応えないゲスト向け）。動かす宣言の VM は、次の plan がまた起動する |
| `adopt-vm` | Mamela の旧組み込みプロバイダーが作った VM か、Terraform から取り込んだ VM を引き継ぐ（このプロバイダーのマーカーとタグを書く。下の「Terraform からの乗り換え」） |
| `delete-template` | テンプレートとそのディスクを消す（linked clone の VM が残っていれば拒む） |

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

## Terraform からの乗り換え（Mamela の ADR 0009）

Terraform（OpenTofu）で作った VM を、作り直さずに Mamela へ移せます。Mamela は Terraform を実行しません。取り込むだけでは、Proxmox に何も送りません。

| Terraform の型 | プロバイダー | 宣言 |
|---|---|---|
| `proxmox_virtual_environment_vm` | bpg/proxmox | `ProxmoxVM` |
| `proxmox_vm_qemu` | Telmate/proxmox（2.x と 3.x） | `ProxmoxVM` |

境界は厳しく守ります。取り込むのは、guard の設定（`[provider.proxmox.guard]`）の境界の中にある VM だけです。それ以外は、理由と直し方を表示して取り込みません。Terraform が管理し続け、Mamela は触れません。

| 取り込まないもの | 理由と直し方 |
|---|---|
| guard の `node` 以外のノードの VM | 境界の外。先にそのノードへ移すか、Terraform に残す |
| VMID の範囲（`vmid_min`〜`vmid_max`）の外の VM | 境界の外。VMID はその場で変えられない。範囲の空き VMID にバックアップから戻して（pool も Mamela のもの）取り込むか、Terraform に残す |
| guard の `pool` 以外の pool の VM | 境界の外（範囲の中にあれば、guard がすべての apply を止める）。Terraform で pool を移し、apply してから取り込む |
| テンプレート | Mamela のテンプレートは、固定したイメージから自分で作る |
| すでに Mamela のマーカーがある VM | Mamela のもの。Terraform から外す（`terraform state rm`） |
| ソケットが 2 つ以上 | Mamela は vCPU を 1 ソケットのコア数として宣言する。`sockets = 1` にしてから |
| ディスクが scsi0 でない、または guard の `disk_storage` にない | Mamela が管理するディスクは scsi0 の 1 つだけ |
| net0 が guard の `bridge` にない | Mamela の VM のネットワークは、guard のブリッジにある |

取り込んだ宣言:

- **値:** vCPU、メモリ、scsi0 の大きさ、ネットワーク（cloud-init の固定アドレス。なければ DHCP）、止めておくか（bpg の `started`、Telmate の `vm_state`）。場所（node、pool、ストレージ、ブリッジ、タグ、VMID の範囲）は guard の設定から取ります。
- **イメージ:** 書きません（Mamela が作った VM ではないため）。イメージのない宣言の VM は、なくなっても作り直しません（`create-vm-without-image`）。
- **ほかのディスクとネットワーク:** scsi1 などのディスクや net1 などは、そのまま残します。Mamela は変えません。

手順:

```sh
cd terraform-dir && terraform show -json > /tmp/state.json     # OpenTofu なら tofu show -json
mamela import terraform --state /tmp/state.json --providers mamela.providers.toml <pome-dir>
```

1. `<pome-dir>/src/imported.emel` に宣言が書かれます。この Pome の生成モジュール `Proxmox.Kinds`（`pome/src/kinds.emel`）の型と関数を使います。
2. 宣言で `import Imported` し、グラフに `|> Imported.imported` を足します。
3. `mamela plan` では、それぞれが「引き継ぐ対象」と表示されます（MAM-PLAN-0114）。何も変わりません。
4. 表示された `mamela act --confirm <name> adopt-vm "ProxmoxVM <name>" <pome-dir>` で、1 つずつ引き継ぎます。まず `--confirm` なしで、送るものを確かめます。
   - **書くもの:** 説明（description）にこのプロバイダーのマーカーを書き、タグを guard の `tag` にします。Terraform が付けた説明とタグは置き換わります。それ以外は何も変えません（再起動もしない）。
   - **前提条件:** 観測したときの設定の digest です。その後に誰かが VM を変えていれば、何も送りません。
   - **拒むもの:** 送る前に、VM が宣言の境界（node、pool、VMID の範囲）の中にあり、上の表の条件を満たすことを確かめます。満たさなければ、理由を示して何も送りません。guard も、各変更が境界の中にあることを確かめます（PVE-0304）。
5. 表示された `terraform state rm ...` で、Terraform に忘れさせます。
6. `mamela plan` で、宣言との差（Terraform の apply が済んでいない大きさなど）が示されます。

引き継いだ VM には mamelet がいません。Mamela は VM そのもの（大きさ、ディスク、電源、明示操作）を管理します。中のパッケージやサービスを宣言するには、別に mamelet を入れる必要があります。

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
  |> Mamela.package(Proxmox.node(web), "nginx")
  |> Mamela.service(Proxmox.node(web), "nginx", True, True)
```

止めておく VM は `Proxmox.vm_stopped(site, vm)` で宣言します。作るときに起動せず、動いていれば止めます。

すべての種類の型付きの宣言（生成物）は `Proxmox.Kinds` にあります（`Proxmox.Kinds.proxmox_vm(graph, ProxmoxVm(...))`）。`mamela import terraform` が書く宣言は、これを使います。

`mamela.providers.toml` に、API の場所と Token を書きます。カタログは、この Pome が同梱するもの（`pome/catalogs/proxmox.json`）を名前で選びます。Mamela は、プログラムのすべての要求をこのカタログで分類します（Mamela の ADR 0008）。中身は `mamela provider catalog pome` で読めます。

```toml
[provider.proxmox]
api_url = "https://pve.example:8006/api2/json"
auth = "proxmox"
token_id = "mamela@pve!lab"
token_file = "~/.config/mamela/lab-token"
tls_fingerprint = "AA:BB:..."
catalog = "pome:proxmox"
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

## 例

| 例 | 内容 |
|---|---|
| [examples/homelab](examples/homelab/src/main.emel) | VM 1 台と nginx |
| [examples/k3s](examples/k3s/src/main.emel) | 固定アドレスの k3s（server 1 台、agent 2 台） |

値は架空です。実際の値は、公開しない場所の宣言に書きます。

## 開発

```bash
# Go が要るのは、プロバイダーを開発するときだけ（WASM にビルドし、Mamela の適合試験を通す）
# Mamela の toolchain（emela、wasmtime）と Pome の依存: ../mamela で scripts/install-toolchain.sh
export GOWORK=off MAMELA_TOOLCHAIN_DIR=../mamela/.tools/bin EMELA_POME_CACHE=../mamela/.tools/pomes
./build.sh       # pome/provider.wasm、pome/mamela-provider.toml、pome/catalogs/proxmox.json、pome/src/kinds.emel を作り直す（Go 1.27.3）
go test ./...    # 適合試験（偽の Proxmox で）、guard、取り込み、カタログ、ビルドの再現性
```

- **作り:** Mamela の provider kit（`provider/kit`）の上で動きます。
  - 種類ごとの知識（一覧、差分、効いているか、明示操作、取り込み）は `proxmox/vm.go` と `proxmox/template.go` にあります。
  - `proxmox/provider.go` が kit につなぎます。kit がまだしないこと（宣言による所有の判断、ノードの観測、他人の物の宣言を拒まずに残すこと、1 回の差分の中での VMID の選び方）も、ここにあります。
- **生成物:** 次のものは手で直しません。
  - `pome/src/kinds.emel`: `go tool mamela provider pome --out src/kinds.emel pome` が、マニフェストから生成します。`pome/src/lib.emel` は手で書いています。
  - `pome/catalogs/proxmox.json`: 下の「カタログと Proxmox VE の更新」。
- **偽の Proxmox:** 適合試験は、偽の Proxmox（[fakepve](fakepve)）の上で次を確かめます。
  - 収束すること、応答の喪失・未送信からの回復
  - 明示操作（削除、再起動、停止、引き継ぎ）
  - 宣言されなくなった VM を残すこと
  - Terraform で作った VM を取り込み、引き継ぐと差分がなくなること（`fakepve/terraform.go`）
  - `mamela import terraform` の通し（state の JSON から Emela を書き、宣言として評価できること）
- **観測の互換性:** 稼働中の Control Plane の記録は、観測した IDs で VM を保証します。`TestObservedIDsKeepTheirShape` が、IDs の形（宣言した VM は node・vmid・storage、それ以外は node・vmid）を固定します。
- **公開の方法:** このリポジトリのタグ（`pome/vX.Y.Z`）を push します。Pome の公開は取り消せません。プログラムを変えたら `version` を上げ、`./build.sh` で作り直してからタグを付けます。

### カタログと Proxmox VE の更新

カタログ（`pome/catalogs/proxmox.json`）は生成物です。

- **呼び出しとパラメータ:** プロバイダーのマニフェストが宣言するものです。Proxmox が読む場所に置きます（GET と DELETE はクエリ、POST と PUT はフォーム）。
- **効果:** 規則で決めます（GET は read、DELETE は lose-data、ゲストの電源操作は restart）。
- **人が決めること:** 変更する呼び出しは、すべて `catalogs/review.go` で人が確かめます。
  - 規則で決まらない効果と、その理由
  - 前提条件（`digest`）、許す値、対象の束縛、大きさ
  - 確かめたときの、Proxmox のハンドラーの名前
  - 規則にもレビューにもない呼び出しは、ビルドが失敗します（read にはしません）。
- **Proxmox VE の API スキーマとの照合:** pve-docs の `apidoc.js` で確かめます。`catalogs/pve-api.lock` が、その版と sha256 を固定します。apidoc.js はリポジトリに入れません（大きさとライセンスのため）。確かめること:
  - 呼び出しとパラメータがあること（`scsi0` は `scsi[n]`）
  - API Token で呼べること
  - レビューしたハンドラーが変わっていないこと
  - スキーマが要求する権限を、権限カタログ（`proxmox/permissions.go`）が与えていること。VM に余分な権限を与えていないこと
  - guard の読み取りが、監査用 Token（PVEAuditor）で足りること

新しい Proxmox VE に上げる手順:

1. 新しい版のノードから apidoc.js を写します（秘密は含まれません）。

   ```sh
   scp root@<node>:/usr/share/pve-docs/api-viewer/apidoc.js /tmp/apidoc-<版>.js
   ```

2. 新しいスキーマで確かめ、通ればロックを書き換えます。通らなければ、何がどう変わったかを表示し、何も書きません。

   ```sh
   go run ./cmd/proxmox-catalogs -schema /tmp/apidoc-<版>.js -pve-manager <版> -update-lock pome/catalogs
   ```

3. 表示された問題を直します。
   - パラメータやパスが変わった: プロバイダーの呼び出しを直します。
   - ハンドラーが変わった: `catalogs/review.go` を読み直します。
   - 権限が変わった: `proxmox/permissions.go` とマニフェストの needs を直します。bootstrap の実行し直しが要ります。
4. `PVE_APIDOC=/tmp/apidoc-<版>.js ./build.sh` で作り直し、`git diff` でカタログとロックの差を確かめます。
5. `PVE_APIDOC=/tmp/apidoc-<版>.js go test ./...` を通します。`PVE_APIDOC` がなければ、スキーマとの照合（`TestAgainstPVESchema`）だけは飛ばします（CI はこれ）。
