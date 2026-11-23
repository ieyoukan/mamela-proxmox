# mamela-proxmox

[Mamela](https://github.com/ieyoukan/mamela) のプロバイダーです。Proxmox VE の VM を作り、その上で Mamela のエージェント（mamelet）を動かします。人が手で作ったコンテナ（LXC）も、引き継いで管理できます（「[コンテナ（LXC）](#コンテナlxc)」）。境界の外（別のノード、pool の外）にある既存のコンテナは、運用者が 1 つずつ名指ししたものだけを、その場所のまま引き継げます（「[境界の外のコンテナを名指しで引き継ぐ](#境界の外のコンテナを名指しで引き継ぐ)」）。

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

マーカーのない VM には触れません。宣言を消しても、VM もテンプレートも消しません。コンテナについては「[コンテナ（LXC）](#コンテナlxc)」にあります。

### テンプレート

イメージを VM ごとに取り込むと、遅いディスクでは 1 台に 15〜20 分かかります。そのため VM は、イメージのテンプレートの linked clone にします。LVM-thin のスナップショットなので、数秒でできます。

- **宣言:** `Proxmox.vm` は、VM の複製元のテンプレート（`ProxmoxTemplate <image>-<node>`）も宣言します。同じイメージとノードの VM がいくつあっても、Mamela は同じ宣言を 1 つのリソースとみなします。VM は、テンプレートができあがるまで待ちます。
- **作る:** Mamela は、テンプレートを最初の plan で一度だけ作ります。pool・VMID の範囲（上から）・タグの中に置き、マーカーを付けます。テンプレートは起動しません。
- **消す:** 明示操作 `delete-template` だけです。そのテンプレートの linked clone の VM が 1 台でもあれば拒みます。
- **イメージの更新:** イメージの版を上げると、新しいテンプレートを宣言することになります。古いテンプレートは、使う VM がなくなってから消します。
- **要る権限:** 操作用 Token に、`VM.Clone`（テンプレートの複製）と `VM.Config.CDROM`（複製した VM に seed を付ける）が足されます。bootstrap を root でもう一度実行するまで、guard の PVE-0102 が止めます。

### 明示操作

plan には入らず、人が名前を打ち直したときだけ行う操作です（`mamela act <op> <resource> --confirm <name>`）。対象は、Mamela が作成か引き継ぎを記録した VM とコンテナに限ります（引き継ぐ操作は、引き継ぐ対象に）。

| 操作 | 内容 |
|---|---|
| `delete-vm` | 止めてから、VM とディスクを消す（データを失う） |
| `reboot-vm` | ゲストに再起動を頼む（ACPI） |
| `reset-vm` | すぐに再起動する（リセットボタンと同じ。応答しないゲスト向け） |
| `stop-vm` | すぐに止める（電源を抜くのと同じ。`shutdown-vm` に応えないゲスト向け）。動かす宣言の VM は、次の plan がまた起動する |
| `adopt-vm` | Mamela の旧組み込みプロバイダーが作った VM か、Terraform から取り込んだ VM を引き継ぐ（このプロバイダーのマーカーとタグを書く。下の「Terraform からの乗り換え」） |
| `delete-template` | テンプレートとそのディスクを消す（linked clone の VM が残っていれば拒む） |
| `adopt-lxc` | 人が作ったコンテナを引き継ぐ（説明の最後にこのプロバイダーのマーカーの行を、タグに guard の `tag` を足す。今ある説明とタグは残す） |
| `reboot-lxc` | コンテナに再起動を頼む |
| `stop-lxc` | コンテナをすぐに止める（電源を抜くのと同じ）。動かす宣言のコンテナは、次の plan がまた起動する |
| `delete-lxc` | 引き継いだコンテナを消す。動いていれば先にきれいに止め（強制しない）、止まってから、ボリュームごと消す（データを失う）。名指ししたコンテナは消さない（下の「[引き継いだコンテナを消す](#引き継いだコンテナを消すdelete-lxc)」） |

送る直前に、VMID の VM（コンテナ）がまだその名前のマーカーを持つことを確かめます。引き継ぎとコンテナの削除では、設定の digest が観測したときのままであることも確かめます（引き継ぎでは、印がまだないこと）。違えば何も送りません。

### guard（適用の前後の安全確認）

Proxmox のクラスタには、Mamela と関係のないゲストが動いていることがあります。Mamela は、このプロバイダーの変更を apply する前後に guard を呼びます。guard は読み取りしかできず、lab の外も見える監査用の Token（`guard_token_id`）で読みます。

| コード | 前 / 後 | 確かめること |
|---|---|---|
| `PVE-0001`〜`0003` | 前 | クォーラム、ノードの数、対象のノードが動いていること（名指ししたゲストの変更があれば、そのノードも） |
| `PVE-0101`・`0102` | 前 | 操作用の Token の権限が、このプロバイダーに要る権限とちょうど同じこと（多すぎも少なすぎも止める） |
| `PVE-0201`〜`0203` | 前 | 変更の後に残るメモリとストレージの余裕（コンテナのメモリの増加と起動、`disk_storage` にあるルートファイルシステムの拡大も数える。名指ししたゲストの変更は、そのゲストのノードで数える）、イメージを置けるストレージ |
| `PVE-0301`〜`0303` | 前 | pool と VMID の範囲に他人のゲストがないこと、名指ししたゲストが pool に入っていないこと、ゲストの数（境界の中のコンテナと名指ししたゲストも `max_vms` に数える） |
| `PVE-0304` | 前 | 各変更が、運用者の決めた境界（ノード、範囲、pool、ストレージ、ブリッジ、タグ、名指ししたゲスト）の中にあること。コンテナの変更は、変えるゲストが pool の中のコンテナ（または名指しした場所にある、名指ししたコンテナ）で、ルートファイルシステムが `disk_storage` にあり、タグを残すこと。名指ししたゲストの変更は、`access = "change"` のときだけ（削除はいつも止める）。宣言が何を書いても、ここで止まる |
| `PVE-0305` | 前 | 名指ししたゲストが、名指ししたとおりの場所にあること（同じ VMID、種類、ノード）。違えば境界の外として扱い、`access = "change"` なら apply を止める（`observe` なら警告） |
| `PVE-0400`〜`0405` | 後 | 境界の外のゲストが、消えず、止まらず、設定も変わらず、再起動もしていないこと |

`block` が 1 つでもあれば、Mamela はそのプロバイダーの変更を送りません。

## コンテナ（LXC）

人が手で作ったコンテナ（Mamela でも Terraform でもなく）を、作り直さずに Mamela で管理できます。Mamela はコンテナを作りません（まだ）。宣言から外しても消しません（Mamela が作ったものではないため。Mamela の ADR 0013、MAM-PLAN-0308）。消すのは、人が名前を打ち直したとき（明示操作 `delete-lxc`）だけです。

- **宣言:** `Proxmox.lxc(site, ct)` で、今あるコンテナを VMID で名指しします。管理する値は、宣言に書いたもの（`Some`）だけです。書かない値（`None`）は、Mamela が変えることも、差分として示すこともありません。
- **境界:** 引き継げるのは、宣言の境界（`node`、`pool`、VMID の範囲）の中にあり、Mamela の印のないコンテナだけです。宣言の Site は guard の設定と同じ値にします。境界の外のコンテナは、宣言で VMID を書いても引き継がず、変えません。
  - pool の外のコンテナは、操作用の Token から見えません。先に人が pool に入れます（VMID の範囲の中で pool の外にあるゲストがあれば、guard の PVE-0301 がすべての apply を止めます）。
  - VMID はその場で変えられません。範囲の外のコンテナは、範囲の空き VMID にバックアップから戻して宣言します。
  - 別のノードや pool の外にあり、その場所のまま引き継ぎたいコンテナは、運用者が guard の設定で 1 つずつ名指しします（下の「[境界の外のコンテナを名指しで引き継ぐ](#境界の外のコンテナを名指しで引き継ぐ)」）。境界は、名指ししたコンテナの分だけ広がります。
- **所有の印:** 引き継ぐと、説明（description）の最後に、このプロバイダーのコンテナ用のマーカーの行（`Managed by Mamela (proxmox provider): container <name>. Do not edit this line.`）を足し、タグに guard の `tag` を足します。今ある説明とタグはそのまま残ります。VM のマーカーとコンテナのマーカーは、取り違えません。

| 値（`Proxmox.Lxc`） | 内容 |
|---|---|
| `vmid` | 引き継ぐコンテナの VMID（必須） |
| `cores` | コア数 |
| `memory_mib` | メモリ（MiB） |
| `swap_mib` | スワップ（MiB） |
| `rootfs_gib` | ルートファイルシステムの大きさ（GiB）。広げるだけで、縮めない |
| `onboot` | ノードの起動時に起動するか |
| `power` | `Running`（動かしておく）か `Stopped`（止めておく） |

```emela
import Proxmox.{Lxc, Stopped}

dns = Lxc(memory_mib: Some(1024), rootfs_gib: Some(8), onboot: Some(True), ..Proxmox.lxc_of("dns", 7005))

Mamela.empty()
  |> Proxmox.lxc(site, dns)
  |> Proxmox.lxc(site, Lxc(power: Some(Stopped), ..Proxmox.lxc_of("batch", 7006)))
```

`Proxmox.lxc_of(name, vmid)` は、何も管理しないコンテナです。型付きの宣言（生成物）は `Proxmox.Kinds.proxmox_lxc` です。

### 引き継ぐ手順（Terraform は要らない）

```sh
mamela guard <pome-dir>                                                    # まず境界を確かめる
mamela plan <pome-dir>                                                     # 「引き継ぐ対象」（MAM-PLAN-0114）と、引き継いだら変わること
mamela act adopt-lxc "ProxmoxLXC <name>" <pome-dir>                        # 送るものを確かめる
mamela act --confirm <name> adopt-lxc "ProxmoxLXC <name>" <pome-dir>       # 引き継ぐ
mamela plan <pome-dir>                                                     # 宣言との差が、変更になる
```

1. 宣言に、コンテナの VMID と、管理したい値を書きます。
2. `mamela plan` では「引き継ぐ対象」と表示されます（MAM-PLAN-0114）。引き継いだあとに要る変更は、プレビューとして示されます。承認も送信もされず、何も変わりません。
3. `adopt-lxc` で引き継ぎます。まず `--confirm` なしで、送るものを確かめます。
   - **書くもの:** コンテナの設定を 1 回だけ変えます。説明の最後にマーカーの行、タグに guard の `tag` です。それ以外は変えません（再起動もしない）。
   - **前提条件:** 観測したときの設定の digest です。その後に誰かがコンテナを変えていれば、何も送りません。
   - **拒むもの:** 境界の外のコンテナ、すでに Mamela の印があるもの（別の宣言のコンテナ、VM のマーカーなど）、マーカーを足すと説明が Proxmox の上限（8192 バイト）を超えるもの。理由を示して何も送りません。VM（qemu）の VMID は、コンテナとして見つかりません。
4. 次の `mamela plan` から、宣言との差が変更になります。

宣言した VMID に引き継げるコンテナがなければ、plan は `create-lxc`（Mamela はしない）として、そこに何があるか（VM、別の宣言のコンテナ、別のノード、何もない）を示します。

### コンテナの変更（plan）

| 操作 | 内容 |
|---|---|
| `configure-lxc` | コア数・メモリ・スワップ・起動時の起動を、1 回の設定変更で変える（観測した digest が前提）。Proxmox は、動いているコンテナにもすぐ反映する（再起動しない）。外されたタグも戻す |
| `grow-lxc-rootfs` | ルートファイルシステムを広げる（`disk=rootfs` だけ。動いたままで広がる） |
| `start-lxc` | 動かしておくと宣言したコンテナが止まっていれば、起動する（late） |
| `shutdown-lxc` | 止めておくと宣言したコンテナが動いていれば、止まるよう頼む（late）。応えなければ `stop-lxc` |

- **1 回の plan に、設定の変更は 1 つ:** それぞれ、plan のときの digest を前提にするためです。広げるのが先で、残りは次の plan です（プレビューは全部を示します）。
- **メモリを減らすとき:** 動いているコンテナのメモリを、使っている量より減らすと、コンテナの中でメモリが回収され、プロセスが止められることがあります。plan の理由にそう書きます。

しないこと（差分としては示し、Mamela が拒みます）:

| 操作 | 理由 |
|---|---|
| `shrink-lxc-rootfs` | 縮小はデータを壊しうる（Proxmox も拒む） |
| `move-lxc` | 別のノード・pool・範囲外の VMID への移動はしない。手で移す（`pct migrate`）か、今ある場所を宣言する（名指ししたコンテナを pool に入れることもしない） |
| `create-lxc` | Mamela はコンテナを作らない。人が作ったものを引き継ぐ |

- **権限:** pool の中のコンテナには、新しい権限は要りません（bootstrap の実行し直しも要りません）。使うのは、VM にもう付いている権限です（名指ししたコンテナの権限は、下の「境界の外のコンテナを名指しで引き継ぐ」）。
  - pool: `VM.Audit`（読む）、`VM.Config.CPU`・`VM.Config.Memory`・`VM.Config.Options`（`configure-lxc`、`adopt-lxc`）、`VM.Config.Disk`（`grow-lxc-rootfs`）、`VM.PowerMgmt`（起動、停止、再起動）、`VM.Allocate`（`delete-lxc`。VM の削除にもう付いている）
  - `disk_storage`: `Datastore.AllocateSpace`（ルートファイルシステムを広げる）。ルートファイルシステムが別のストレージにあるコンテナは広げません（guard の PVE-0304 が止めます。Token にもその権限はありません）。
- **mamelet:** 引き継いだコンテナは node です（`Proxmox.lxc_node(ct)`）。ただし、Mamela は mamelet を入れません。中のパッケージやサービスを宣言するには、別に mamelet を入れる必要があります。

### 引き継いだコンテナを消す（delete-lxc）

宣言から外しても、引き継いだコンテナは消えません（plan は MAM-PLAN-0308 として残すと示します）。消すときは、人が名前を打ち直して `delete-lxc` を実行します。plan には一度も現れません（ADR 0013 の削除の操作ではありません）。

```sh
vzdump <vmid> ...                                                          # 要るなら、先にバックアップを取る（Mamela は取らない）
mamela guard <pome-dir>                                                    # まず境界を確かめる
mamela act delete-lxc "ProxmoxLXC <name>" <pome-dir>                       # 送るものと、その効果を確かめる（何も変えない）
mamela act --confirm <name> delete-lxc "ProxmoxLXC <name>" <pome-dir>      # 消す
```

宣言から外したあとでも、宣言に残したままでも実行できます（宣言に残したままなら、消したあとの plan は `create-lxc`（Mamela はしない）を示すので、宣言も消します）。

- **送るもの（`--confirm` なしで表示されます）:**
  1. 動いていれば `POST /nodes/{node}/lxc/{vmid}/status/shutdown`（`timeout=120`、`forceStop=0`）。中のプロセスに止まるよう頼み、120 秒まで待ちます。**強制はしません。** 止まらなければ、そこで失敗し、何も消しません（コンテナはそのまま残ります。理由を確かめ、消してよければ `stop-lxc` で止めてから、もう一度 `delete-lxc`）。
  2. `DELETE /nodes/{node}/lxc/{vmid}`（`purge=1`、`destroy-unreferenced-disks=1`）。コンテナと、そのボリューム（ルートファイルシステム、マウントポイント、設定にないがその VMID を持つディスク）を消します。データは失われます。
  - **効果（host のカタログ）:** `restart`（止める）と `lose-data`（消す）。カタログは `forceStop=1` と、動いているコンテナを消す `force` を許しません。
- **`purge` がすること:** Proxmox は、その VMID を、クラスタのバックアップのジョブ、レプリケーションのジョブ、HA のリソースからも外します。**その VMID だけを挙げていたバックアップのジョブは、ジョブごと消えます**（ジョブが「すべて」なら、除外の一覧から外れるだけです）。その VMID の ACL とファイアウォールの設定は、`purge` がなくても消えます。表示にもそう書きます。VM の削除（`delete-vm`）と違って `purge` を使うのは、人が作ったコンテナは人がジョブや HA に入れていることが多く、`purge` がないと、Proxmox が削除を拒む（HA、レプリケーション。止めたあとで）か、消えた VMID がバックアップのジョブに残るためです。
- **前提条件:** 送る直前（止める前と、消す前）に毎回、その VMID が、観測したときのコンテナのままであることを確かめます。node の上のコンテナで、テンプレートでなく、pool に入っていて、この名前のマーカーを持ち、設定の digest が観測したときのものであること。違えば何も（それ以上）送りません。
- **中断と再試行:** Mamela は送る前に操作を記録します。応答が失われたら、表示される `--recover` で、送り直さずに確かめます。止めたところで中断した（止まったまま残っている）なら、もう一度 `--confirm` で実行すると、消すことだけを送ります。コンテナがもうなければ、消し終わったとみなし、何も送りません。
- **guard:** 前に、変更が境界（guard の `node`・`pool`・VMID の範囲）の中のコンテナであることを確かめます（PVE-0304）。後に、境界の外のゲストが、消えず、止まらず、変わっていないことを確かめます（PVE-0400〜0405）。
- **権限:** 新しい権限は要りません（pool の `VM.Allocate` と `VM.PowerMgmt`。VM の削除にもう付いています）。

拒むもの（理由と直し方を示し、何も送りません）:

| 場合 | 直し方 |
|---|---|
| 名指ししたコンテナ（`[[provider.proxmox.guard.named]]`、`Proxmox.named_lxc`） | Mamela は消しません（本番のゲストを、運用者が境界に名指しで足したもの）。guard も PVE-0304 で止め、Token にも `VM.Allocate` を付けません。消すなら人が手で: バックアップを取り、宣言と名指しを外して bootstrap を実行し直し、そのノードで `pct shutdown <vmid>`、`pct destroy <vmid>` |
| Mamela が引き継いでいないもの（マーカーがない、別の名前のマーカー、VM のマーカー、Mamela の記録がない） | 消すなら手で。Mamela のものなら、名前を確かめる |
| 境界の外（別のノード、pool の外、VMID の範囲の外）。宣言があれば、宣言の境界の外も | 消すなら手で（Mamela はゲストを動かさない） |
| VM（qemu）の VMID | `delete-lxc` はコンテナだけ。VM は `delete-vm` |
| 保護されている（`protection`） | 人が保護をかけたもの。消すと決めたら、手で外す（`pct set <vmid> --protection 0`） |
| ロックされている（バックアップ、スナップショット、移行の最中、またはその中断） | 終わってから、もう一度 |
| 準備のあとで変わった（digest、マーカー、ノード、pool、テンプレート化） | もう一度観測して、確かめてから実行し直す |
| observe-only の名前 | Mamela が拒みます（MAM-POL-0312）。消すなら、observe-only から外してから |

### 境界の外のコンテナを名指しで引き継ぐ

本番のクラスタには、境界（guard の `node`・`pool`・VMID の範囲）の外に、人が手で作ったコンテナがあります（別のノードにある、pool に入っていない、VMID が範囲の外）。範囲や pool を広げると、隣の無関係なゲストまで境界に入ってしまいます。そこで、引き継ぎたいコンテナだけを、運用者が guard の設定で 1 つずつ名指しします。

- **名指ししたものだけが境界に入る:** 名指ししたコンテナは、名指しした場所（ノード、VMID、種類）にある間だけ、境界の中として扱います。pool にも範囲にも入れる必要はなく、Mamela も pool に入れません（タグとマーカーは、今までどおり `adopt-lxc` が足します）。
- **名指ししないものは今までどおり:** 境界の外のゲストは、同じノードの隣のコンテナも含めて、読むだけです（`Foreign`）。apply の前後に guard が変わっていないことを確かめます（PVE-0400〜0405）。
- **権限はそのゲストの `/vms/<vmid>` にだけ、2 段階で:** bootstrap が、名指ししたゲストの `/vms/<vmid>` にだけ ACL を付けます。`/`、`/vms`、ノード、他の pool には付けません。どの段にするかは、人が設定（`access`）で決めます。
  - **段 1（`access = "observe"`）:** `VM.Audit` だけ。Mamela はコンテナを見て、plan で引き継ぐ対象と変わること（プレビュー）を示します。何も変えません。引き継ぎも guard が止めます（PVE-0304）。
  - **段 2（`access = "change"`）:** さらに、引き継ぎと変更に要る権限（`VM.Config.CPU`・`VM.Config.Disk`・`VM.Config.Memory`・`VM.Config.Options`・`VM.PowerMgmt`）。どれも、コンテナの操作（`adopt-lxc`、`configure-lxc`、`grow-lxc-rootfs`、`start-lxc`、`shutdown-lxc`、`reboot-lxc`、`stop-lxc`）がそのゲストに要るものです（マニフェストから導く）。
- **消さない:** 名指ししたコンテナは、Mamela は消しません。`delete-lxc` は拒み、guard も止めます（PVE-0304）。段 2 でも、Token に削除の権限（`VM.Allocate`）は付けません。消すときは、人が手で消します（上の「[引き継いだコンテナを消す](#引き継いだコンテナを消すdelete-lxc)」）。
- **まだしないこと:** VM（qemu）は名指しできません。VM を宣言の VMID で引き継ぐ仕組みがまだないためです（`type = "qemu"` の設定は、理由を示して拒みます）。

設定（`mamela.providers.toml`。値は例です）:

```toml
[provider.proxmox.guard]
node = "pve1"
pool = "mamela"
vmid_min = 7000
vmid_max = 7099
# ...（ほかの値は「使い方」の例のとおり）

# 境界に名指しで足す既存のゲスト（pool と VMID の範囲の外）。1 つにつき 1 つ書く
[[provider.proxmox.guard.named]]
node = "pve2"          # いまゲストがあるノード
vmid = 150             # VMID（範囲の外であること）
type = "lxc"           # コンテナ。qemu はまだ使えない
access = "observe"     # 段 1: observe（見るだけ）。引き継ぐと決めたら change にして bootstrap をもう一度
```

宣言（`Proxmox.named_lxc(site, node, ct)`）。Site はいつもの値のままで、ノードだけをコンテナのあるノードにします。値は `Proxmox.lxc` と同じです。

```emela
import Proxmox.{Lxc}

far = Lxc(memory_mib: Some(1024), ..Proxmox.lxc_of("far", 150))

Mamela.empty()
  |> Proxmox.named_lxc(site, "pve2", far)
```

型付きの宣言（`Proxmox.Kinds.proxmox_lxc`）では、`node` をそのノードに、`named: Some(True)` にします。

手順:

```sh
# 段 1: 見るだけ（access = "observe"）
mamela provider script <pome-dir> proxmox bootstrap > bootstrap.sh   # 名指ししたゲストに VM.Audit を足す
less bootstrap.sh                                                   # 確かめてから、ノードで root として実行
mamela guard <pome-dir>                                             # PVE-0305: 名指ししたとおりの場所にあるか
mamela plan <pome-dir>                                              # 引き継ぐ対象（MAM-PLAN-0114）と、変わること（プレビュー）

# 段 2: 引き継ぐと決めたら（access = "change" に書き換える）
mamela provider script <pome-dir> proxmox bootstrap > bootstrap.sh   # 変更の権限を足す
less bootstrap.sh                                                   # 確かめてから、ノードで root として実行
mamela guard <pome-dir>
mamela act adopt-lxc "ProxmoxLXC far" <pome-dir>                    # 送るものを確かめる
mamela act --confirm far adopt-lxc "ProxmoxLXC far" <pome-dir>      # 引き継ぐ（説明とタグに書き足すだけ）
mamela plan <pome-dir>
```

bootstrap が名指ししたゲストに実行するのは、次の行だけです（VMID 150、操作用のユーザー `mamela@pve` と Token `mamela@pve!lab` の例）。手順 3 でロールを作り、手順 5b で ACL を付けます。手順 6 は、設定にない ACL（段を戻した、名指しを外した）を外します。

```sh
# 段 1（access = "observe"）
ensure_role MamelaLabNamedObserve "VM.Audit"
pveum acl modify /vms/150 --users mamela@pve --roles MamelaLabNamedObserve
pveum acl modify /vms/150 --tokens 'mamela@pve!lab' --roles MamelaLabNamedObserve

# 段 2（access = "change"）。段 1 の ACL（MamelaLabNamedObserve）は手順 6 が外す
ensure_role MamelaLabNamedChange "VM.Audit,VM.Config.CPU,VM.Config.Disk,VM.Config.Memory,VM.Config.Options,VM.PowerMgmt"
pveum acl modify /vms/150 --users mamela@pve --roles MamelaLabNamedChange
pveum acl modify /vms/150 --tokens 'mamela@pve!lab' --roles MamelaLabNamedChange
```

スクリプト全体は `proxmox/testdata/bootstrap-named-observe.sh` と `bootstrap-named-change.sh` にあります（試験が、プロバイダーの書くものと同じことを確かめます）。

guard と plan が拒むもの（何も送りません）:

| 場合 | どうなるか | 直し方 |
|---|---|---|
| 設定で名指ししていないコンテナを、宣言で `named_lxc` にした | 引き継ぎも変更も PVE-0304 が止める（Token からも見えない） | 設定で名指しし、bootstrap を実行し直す |
| `access = "observe"` のまま引き継ぐ・変える | PVE-0304 が止める | 人が決めたら `access = "change"` にし、bootstrap を実行し直す |
| 名指ししたコンテナが別のノードに移った（migrate） | 境界の外になる。plan は `create-lxc`（しない）として、どこにあるかを示す。`change` なら PVE-0305 がすべての apply を止める | 設定と宣言の `node` を今のノードにする |
| 名指しした VMID が別の種類（VM）や別のゲストになった、なくなった | 境界の外になる。`change` なら PVE-0305 がすべての apply を止める | 名指しを外し、bootstrap を実行し直す（ACL が外れる） |
| 名指ししたコンテナが Mamela の pool に入れられた | pool の権限が届いてしまうので、PVE-0302 が止める。引き継ぎもしない（`move-lxc`） | pool から外す |
| 名指しした VMID が VMID の範囲の中 | 設定を拒む | pool に入れて `Proxmox.lxc` で宣言する |
| `type = "qemu"` | 設定を拒む（まだ対応していない） | 名指しを外す |
| 名指ししたコンテナのノードのメモリ・ストレージに余裕がない | そのノードで PVE-0201・0202 が止める | 余裕を作るか、宣言を小さくする |
| 名指ししたコンテナを `delete-lxc` で消す | 拒む。guard も PVE-0304 で止め、Token に `VM.Allocate` もない | 人が手で消す（バックアップ、宣言と名指しを外して bootstrap、`pct destroy <vmid>`） |

`mamela provider script <pome-dir> proxmox permissions` は、名指ししたゲストの権限と、それが要る操作も示します。

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
emela pome add github.com/ieyoukan/mamela-proxmox/pome@v0.2.3
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

# 境界の外の既存のコンテナを、1 つずつ名指しで境界に足す（任意。「境界の外のコンテナを名指しで引き継ぐ」）
# [[provider.proxmox.guard.named]]
# node = "pve2"
# vmid = 150
# type = "lxc"
# access = "observe"     # observe（VM.Audit だけ）か change（引き継ぎと変更の権限も）
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
- **bootstrap:** pool、イメージを置けるストレージ、パスごとのロール、操作用と監査用のユーザーと Token、ACL を作ります。名指ししたゲストには、その `/vms/<vmid>` にだけ、設定の `access` の段の ACL を付けます。不要になった ACL は外します。何度実行しても同じ状態になります。権限は、guard が確かめるのと同じマニフェストから導くので、ずれません。
- **cleanup:** どの VM も使っていないアップロード（`mamela-` で始まるイメージと seed）を一覧にします。`--free` を付けたときだけ seed を消します。イメージは次のテンプレートの元なので、`--free --images` のときだけ消します（消すと、次にテンプレートを作るときにもう一度アップロードします）。

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
  - 種類ごとの知識（一覧、差分、効いているか、明示操作、取り込み）は `proxmox/vm.go`、`proxmox/template.go`、`proxmox/lxc.go` にあります。
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
  - 人が作ったコンテナを、宣言した VMID で引き継ぎ（引き継ぐ前はプレビューだけ）、宣言のとおりに変えること。中断と再試行、境界の外・別の宣言のもの・VM の VMID・digest の変化・縮小・移動を拒むこと（`fakepve/lxc.go`）
  - 引き継いだコンテナを消すこと（動いていれば止めてから。止まらなければ消さない）。中断と再試行（なくなっていれば済み）、名指ししたもの・マーカーのないもの・別の名前のもの・境界の外・VM の VMID・保護・ロック・digest の変化を拒むこと、guard の前後（`proxmox/delete_lxc_test.go`）
  - 名指ししたコンテナ（別のノード、pool の外）を、その場所のまま引き継いで変えること。名指ししていないもの・`observe` の段・移されたもの・種類の違うものを拒み、guard の後の検査が隣のゲストの変化を見つけること。bootstrap の出力（`proxmox/testdata`、`go test ./proxmox -run TestNamedScripts -update` で書き直す）
- **観測の互換性:** 稼働中の Control Plane の記録は、観測した IDs で VM を保証します。`TestObservedIDsKeepTheirShape` が、IDs の形（宣言した VM は node・vmid・storage、それ以外は node・vmid）を固定します。コンテナの IDs（node・vmid。引き継ぐ前も後も同じ）は `TestLXCObservedIDsKeepTheirShape` が固定します。
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
