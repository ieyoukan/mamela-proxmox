# Proxmox VE で Mamela を安全に動かす（lab）

> この文書は公開リポジトリにある。サイト固有の値（ホスト名、IP、既存ゲストの一覧、Token）は書かない。それらは、gitignore した `mamela.site.toml`・`mamela.providers.toml` と、手元の宣言に置く。
>
> Mamela 本体の考え方は [Mamela の ADR 0006](https://github.com/ieyoukan/mamela/blob/main/docs/adr/0006-proxmox-out-of-core.md) にある。Proxmox の知識はすべてこのリポジトリにあり、Mamela 本体は guard・明示操作・スクリプトという汎用の仕組みでそれを強制する。

## 1. 目的と前提

- 既存のワークロードが動いている Proxmox クラスタの中で、**既存のゲスト（VM / LXC）を一切壊さずに** Mamela で VM を作り、その上を管理する。
- 人が手で作ったコンテナ（LXC）は、人が引き継いだもの（`adopt-lxc`、§10）だけを管理する。Mamela はコンテナを作らない。宣言から外しても消さない。消すのは、人が名前を打ち直したとき（`delete-lxc`、§10.2）だけで、名指ししたもの（§10.1）は消さない。
- 境界の外（別のノード、pool の外）にある既存のコンテナは、運用者が 1 つずつ名指ししたものだけを、その場所のまま引き継ぐ（§10.1）。範囲や pool は広げない。
- 境界は宣言と設定で決め、毎回の apply の前後に機械的に確かめる。
- 実機テストは明示的に opt-in したときだけ動かす（`MAMELA_LAB=1`）。CI の標準ジョブでは動かさない。

## 2. 安全の考え方: 境界を三重にする

| 層 | 仕組み | 何を防ぐか |
|---|---|---|
| 1. Proxmox の権限 | Mamela 専用の pool・ユーザー・権限分離した Token。権限は pool と、必要なストレージ・ブリッジにだけ付ける（名指ししたゲストには、その `/vms/<vmid>` にだけ） | Mamela が既存ゲストを見る・変える・消すこと自体を API が拒む |
| 2. 所有の確認 | VM の説明（description）のマーカー、タグ、pool、VMID の範囲。さらに Mamela は、自分が作成を記録した VM しか自分のものと認めない（Mamela の ADR 0004） | 権限設定の誤りがあっても、管理外のものは `Foreign` として変えない |
| 3. guard | apply の前後に、このプロバイダーの guard が読み取りだけで確かめる（§6） | 資源の枯渇による巻き添え、境界の外への変更、管理外ゲストの想定外の変化を止める |

### 共有資源による巻き添え

権限で守れないのは **ホストの共有資源** である。

- **メモリ:** ホストの空きメモリが尽きると、OOM killer が既存の大きな VM を止め得る。
- **LVM thin pool:** 満杯になると、同じ pool にディスクを持つ全ゲストが I/O エラーになる。
- **クラスタのクォーラム:** ノードの停止でクォーラムを失い得る。

そのため guard は、変更が使うメモリとディスクを足しても余裕が残ることを確かめる（PVE-0201、PVE-0202）。Mamela の site の 1 台あたりの上限（vCPU、メモリ、ディスク）も、plan と apply で確かめられる。

### 推奨: 本番クラスタの外にある専用ホスト

| 構成 | 隔離の強さ | 注意点 |
|---|---|---|
| A. クラスタ外の専用ホスト（推奨） | 強い。メモリ・ストレージ・クォーラムを本番と共有しない | 専用ホストを用意する手間がかかる |
| B. 本番クラスタ内の専用ノード | 中。資源は分かれるが、クォーラムと `/etc/pve` を共有する | ノードの停止や設定ミスがクラスタ全体に影響し得る |
| C. 本番ノードの空き資源 | 弱い。予算としきい値だけが頼り | 資源に余裕がないと実行できない |

どの構成でも、§4 の pool・Token・ACL は同じように作る。

## 3. 設定

| ファイル | 中身 | 置き場所 |
|---|---|---|
| `mamela.site.toml` | Mamela の site（`mamela-site/v1`）。Control Plane、VM のログインユーザーと SSH 鍵、ネットワークの規則（DHCP、固定アドレスの範囲）、1 台あたりの上限 | gitignore |
| `mamela.providers.toml` | `[provider.proxmox]`: API の URL、`auth = "proxmox"`、操作用 Token（`token_id`、`token_file`）、`tls_fingerprint`、`catalog = "pome:proxmox"`（この Pome が同梱するカタログ）、guard 用の監査 Token（`guard_token_id`、`guard_token_file`）。`[provider.proxmox.guard]`: 境界と安全の閾値。`[[provider.proxmox.guard.named]]`（任意）: 境界に名指しで足す既存のゲスト（§10.1） | gitignore |
| 宣言 | `Proxmox.Site`（node、pool、ストレージ、bridge、tag、VMID の範囲）と `Proxmox.vm` | 実アドレスを含むなら gitignore した場所 |

設定の例は [README](../README.md#使い方) にある。

- **宣言の Site と guard の設定は同じ値にする。** guard は、各変更が `[provider.proxmox.guard]` の境界（ノード、VMID の範囲、pool、ストレージ、ブリッジ、タグ）の中にあることを確かめる（PVE-0304）。宣言に何が書かれていても、運用者の境界の外には出ない。
- **名指ししたコンテナは、宣言と設定の両方に書く。** 宣言（`Proxmox.named_lxc`）だけでは境界に入らない。guard は、設定（`[[provider.proxmox.guard.named]]`）で名指しされていないゲストへの変更を止める（PVE-0304）。
- **Token の秘密は書かない。** ファイルの場所だけを書く。秘密のファイルは mode 0600 にする（グループや他人が読めるファイルを Mamela は拒む）。

## 4. 一度だけの準備（人が root で実行）

pool・ロール・ユーザー・Token・ACL の作成と、ストレージの content type の追加には root 権限が要る。Mamela にその権限は持たせない。このプロバイダーが、冪等な bash スクリプトを**書くだけ**なので、人が内容を確認してからクラスタのノードで root として実行する。

~~~bash
mamela provider script <pome-dir> proxmox permissions             # 操作用 Token の権限と、それが要る操作
mamela provider script <pome-dir> proxmox bootstrap > bootstrap.sh
less bootstrap.sh                                                 # 一行ずつ確かめてから、ノードで root として実行
~~~

- スクリプトは、Pome（Pome.lock と sumdb）で固定された WASM のプロバイダーが、`mamela.providers.toml` の秘密でない値だけから書く。書く間、プロバイダーは Proxmox に何も送れない。
- 作るもの:
  - pool
  - イメージ用ストレージの content type に `import` を足す（既存の content type は残す）
  - パスごとのロール（`MamelaLabVM`、`MamelaLabDisk`、`MamelaLabImage`、`MamelaLabNet`）
  - 操作用のユーザーと権限分離した Token（ACL はロールの付くパスだけ）
  - 名指ししたゲストの ACL（その `/vms/<vmid>` にだけ。設定の `access` の段のロール `MamelaLabNamedObserve` か `MamelaLabNamedChange`。§10.1）
  - 監査用の別ユーザーと Token（組み込みロール `PVEAuditor` を `/` に。guard だけが使う）
- 消すのは、操作用ユーザーと Token の ACL のうち、もう要らないものだけ。もう一度実行しても同じ状態に収束する。

### 権限はマニフェストから導く

- 権限は手で決めない。各操作に要る権限は、プロバイダーのマニフェスト（needs）にある。bootstrap と guard は、同じマニフェストから付与と検査の内容を導く。
- 各 API 呼び出しに Proxmox VE 9.2 が確かめる権限は、権限カタログ（`proxmox/permissions.go`）にある。根拠は API スキーマの permissions と、ハンドラの中のチェック（`PVE/API2/Qemu.pm` の `create_vm`・`check_storage_access`・`check_vm_modify_config_perm`・`check_vm_create_serial_perm`、`PVE/Storage.pm` の `check_volume_access`、`PVE/GuestHelpers.pm` の `assert_tag_permissions`）。
- 試験で、各操作の needs がカタログの権限を満たすことを確かめる。新しい呼び出しは、権限を書かずには足せない。
- guard は、操作用 Token の実効権限がこの一覧（と、pool に属する範囲内の `/vms/<id>`）を一つでも超えれば PVE-0101 で、足りなければ PVE-0102 で止める。名指ししたゲストの `/vms/<vmid>` も、設定の `access` の段とちょうど同じでなければ止める。
- `/`、`/vms`、他の pool、他のノードには何も付けない。名指ししたゲストには、その `/vms/<vmid>` にだけ付ける。

## 5. VM の作り方

`Proxmox.vm` は、VM と、そのイメージのテンプレート（`ProxmoxTemplate <image>-<node>`）を宣言する。VM は、テンプレートの linked clone として数秒で作る。テンプレートは、宣言の `image`（例 `ubuntu-24.04`）から一度だけ、次のように作る（テンプレートを使わない宣言の VM も同じ手順で作る）。

1. **イメージはこのプロバイダーのコードに固定する**（`proxmox.Images`。URL、sha256、最小ディスク）。ないものは拒む。イメージの更新は、レビューされるコードの変更（新しい版）になる。
2. **一度だけアップロードする。** Mamela がダウンロードして sha256 を確かめ、イメージ用ストレージに `import` としてアップロードする。Proxmox にも sha256 を確かめさせる。ファイル名に sha256 を含める。
3. **イメージから直接作る**（`import-from`）。止めたまま作り、マーカーを書く。タグを付け、テンプレートにする。

VM は、次のように作る。

1. **seed をアップロードする。** Mamela が作る NoCloud の ISO（mamelet、一度きりの登録トークン、ネットワーク）。プロバイダーは中身を見ない。
2. **テンプレートを複製する**（linked clone、`full=0`）。止めたまま作り、マーカーを書く。
3. vCPU とメモリを設定し、seed を付け、ディスクを宣言の大きさまで広げ、タグを付ける。タグは作成時に付けられない（Proxmox は pool に入れる前にタグの権限を確かめる）。
4. **起動は late の変更にする。** Mamela は、その apply のすべての作成を終えてから起動する。初回起動が別の VM のディスクの取り込みと重なると、I/O 待ちで緊急モードに落ちることがあるためである。
   - 止めておく宣言（`Proxmox.vm_stopped`、spec の `stopped`）の VM は起動しない。動いていれば、late の `shutdown-vm` でゲストに電源を切るよう頼む（ACPI）。応えないゲストは、明示操作 `stop-vm` で止める。

### 中断と再試行

- Mamela は、作成を送る前に記録する（VMID）。応答が失われても、同じ VMID に作り直すことはない。
- 作成が途中で止まった VM（マーカーはあるがタグがない）は「途中」と報告する。Mamela は、記録した VM なら残りの変更（`grow-vm-disk`、`tag-vm`、`start-vm`）を提案する。
- 起動の変更は、作成と同時でも、止まった VM を見つけたときでも同じ内容にしてある。作成の応答が失われた後も、承認した plan のまま続けられる。
- しないこと（差分として示し、Mamela が拒む）: ディスクの縮小、イメージの変更、ネットワークの変更、別のノード・pool・範囲外の VMID への移動（`move-vm`）。

## 6. guard（apply の前後）

`mamela apply` は、このプロバイダーの変更を送る前後に guard を呼ぶ。単独でも実行できる。

~~~bash
mamela guard <pome-dir>          # 前の検査だけ（読み取りのみ）。block があれば終了コード 1
~~~

- **前:** クォーラム、ノード、操作用 Token の権限、メモリとストレージの余裕（名指ししたゲストの変更は、そのゲストのノードで）、イメージを置けるストレージ、境界（範囲内の他人のゲスト、pool 内の範囲外のゲスト、VM の数、名指ししたゲストが名指しした場所にあること）、各変更が境界の中にあること。境界の外のゲストを記録する（baseline）。
- **後:** 境界の外のゲストが、消えず、止まらず、設定も変わらず、再起動もしていないこと。名指しした場所にある名指ししたゲストは境界の中なので、ここでは確かめない。同じノードの隣のゲストは確かめる。
- **block があれば:** 前なら何も送らない。後なら apply を失敗として報告する。人が確かめてから次に進む。
- コードの一覧は [README](../README.md#guard適用の前後の安全確認) にある。

## 7. 明示操作（削除、再起動、引き継ぎ）

plan には現れない操作である。人が名前を打ち直したときだけ行う。対象は、Mamela が作成を記録した VM に限る（引き継ぎは除く）。

~~~bash
mamela act delete-vm web <pome-dir>                      # 何を送るかを表示するだけ
mamela act --confirm web delete-vm web <pome-dir>        # 名前を打ち直したときだけ実行する
mamela act --confirm web reboot-vm web <pome-dir>        # guest に再起動を頼む（ACPI）
mamela act --confirm web reset-vm web <pome-dir>         # 応答しない guest をリセットする
mamela act --confirm web stop-vm web <pome-dir>          # すぐに止める（電源を抜くのと同じ。shutdown に応えない guest 向け）
~~~

- **流れ:** 表示 → guard（前）→ 記録 → 許可を発行して送る → guard（後）。
- **送る直前の確認:** VMID の VM がまだその名前のマーカーを持つことを確かめる。違えば何も送らない。
- **削除:** 止めてから、ディスクごと消す（`destroy-unreferenced-disks=1`）。バックアップやレプリケーションのジョブから外す `purge` は、クラスタ全体の設定を変えるので使わない。済めば Mamela は作成の記録を消す。
- **起動で止まった VM:** mamelet が 15 分を超えて報告しなければ、plan は MAM-PLAN-0110 で止める。コンソールで確かめてから `reboot-vm`、応答しなければ `reset-vm` を使う。
- **停止:** `stop-vm` で止めても、動かす宣言の VM は、次の plan が `start-vm` でまた起動する。止めておくなら、宣言を `Proxmox.vm_stopped` にする。
- **コンテナの削除:** 引き継いだコンテナは `delete-lxc` で消す（§10.2）。VM と違い、宣言から外しても plan に削除は現れない。

## 8. 旧組み込みプロバイダーの VM を引き継ぐ（adopt-vm）

Mamela 0.2 までの組み込みの Proxmox プロバイダーが作った VM は、説明に `Managed by Mamela. Do not edit the lines below.` のマーカーを持つ。このプロバイダーは、それを `adoptable`（引き継げる）と報告する。

1. 宣言を `Proxmox.vm` で書き直す。VM の名前、イメージ、ネットワークは旧宣言と同じにする。
2. `mamela.providers.toml` に `[provider.proxmox]` と guard の設定を書き、bootstrap を実行し直す（旧版の付けた `VM.GuestAgent.Audit` などが外れる）。
3. `mamela guard <pome-dir>` で境界を確かめる。
4. VM ごとに引き継ぐ。このプロバイダーのマーカーとタグを書き、Mamela は作成を記録する。

   ~~~bash
   mamela act adopt-vm k3s-server <pome-dir>
   mamela act --confirm k3s-server adopt-vm k3s-server <pome-dir>
   ~~~

5. `mamela plan` で、各 VM が `in sync`（または意図した差分）になることを確かめる。

引き継げるのは、pool と VMID の範囲にあり、旧マーカーの状態が `ready` で、宣言にある名前の VM だけである。

## 9. Terraform で作った VM を移す（mamela import terraform）

Terraform（bpg/proxmox の `proxmox_virtual_environment_vm`、Telmate/proxmox の `proxmox_vm_qemu`）が作った VM を、作り直さずに Mamela へ移せる。詳しい条件は [README](../README.md#terraform-からの乗り換えmamela-の-adr-0009) にある。

lab のクラスタには、別の Terraform が管理する本番のゲストもある。そのため、次のことを守る。

- **境界の外は取り込まない:** 取り込むのは、guard の設定の node・pool・VMID の範囲の中にある VM だけである。state に本番のゲストが入っていても、プロバイダーが理由とともに外す。範囲の外の VM の VMID は、その場で変えられない。
- **取り込みは何も変えない:** `mamela import terraform` は、Proxmox に何も送らない。宣言（`src/imported.emel`）を書き、引き継ぐ対象を SQLite に記録するだけである。
- **引き継ぎは 1 台ずつ:** `adopt-vm` は、VM の説明にマーカーを書き、タグを guard の `tag` に置き換える。それ以外は変えない。送る前に、境界の中にあること、設定が観測したときのまま（digest）であることを確かめる。guard も前後に走る。
- **`terraform state rm` は、表示されたアドレスにだけ使う:** 引き継いだ VM だけを Terraform から外す。取り込まなかったもの（境界の外）は、Terraform が管理し続ける。

~~~bash
mamela guard <pome-dir>                                                    # まず境界を確かめる
terraform show -json > /tmp/state.json                                     # Terraform のディレクトリで
mamela import terraform --dry-run --state /tmp/state.json --providers mamela.providers.toml <pome-dir>
mamela import terraform --state /tmp/state.json --providers mamela.providers.toml <pome-dir>
mamela plan <pome-dir>                                                     # 取り込んだ VM は「引き継ぐ対象」（MAM-PLAN-0114）
mamela act adopt-vm "ProxmoxVM <name>" <pome-dir>                          # 送るものを確かめる
mamela act --confirm <name> adopt-vm "ProxmoxVM <name>" <pome-dir>         # 引き継ぐ
terraform state rm '<address>'                                             # Terraform に忘れさせる
mamela plan <pome-dir>                                                     # 宣言との差を確かめる
~~~

## 10. 人が作ったコンテナを引き継ぐ（adopt-lxc）

手で作ったコンテナ（LXC）を、作り直さずに Mamela へ移せる。Terraform は要らない。宣言で VMID を名指しし、人が 1 つずつ引き継ぐ。値や手順は [README](../README.md#コンテナlxc) にある。

lab のクラスタには、人が大切にしているコンテナもある。そのため、次のことを守る。

- **境界の外は引き継がない:** 引き継げるのは、宣言（と guard の設定）の node・pool・VMID の範囲の中にあり、Mamela の印のないコンテナだけである（§10.1 のとおり名指ししたものを除く）。pool に入れるのは人がする（Mamela の Token にその権限はない）。範囲の中で pool の外にあるゲストがあると、guard の PVE-0301 がすべての apply を止める。
- **宣言して plan するだけでは何も変わらない:** plan は「引き継ぐ対象」（MAM-PLAN-0114）と、引き継いだら変わることのプレビューを示すだけである。
- **引き継ぎは 1 つずつ、書き足すだけ:** `adopt-lxc` は、説明の最後にマーカーの行を、タグに guard の `tag` を足す。今ある説明とタグは残る。それ以外は変えず、再起動もしない。観測したときの digest が前提で、その後に誰かがコンテナを変えていれば何も送らない。guard も前後に走る。
- **書いた値だけを変える:** 宣言に書かない値（コア数、メモリ、スワップ、ルートファイルシステム、起動時の起動、電源）は、引き継いだ後も変えない。ルートファイルシステムは広げるだけで、`disk_storage` にあるものに限る。
- **消さない:** 宣言から外しても、コンテナは消さない（マーカーとタグは残る）。消すのは `delete-lxc` だけである（§10.2）。

~~~bash
mamela guard <pome-dir>                                                    # まず境界を確かめる
mamela plan <pome-dir>                                                     # 引き継ぐ対象（MAM-PLAN-0114）と、変わること（プレビュー）
mamela act adopt-lxc "ProxmoxLXC <name>" <pome-dir>                        # 送るものを確かめる
mamela act --confirm <name> adopt-lxc "ProxmoxLXC <name>" <pome-dir>       # 引き継ぐ
mamela plan <pome-dir>                                                     # 宣言との差を確かめる
~~~

### 10.1 境界の外のコンテナを名指しで引き継ぐ

lab のクラスタには、境界の外に、人が大切にしているコンテナもある（別のノードにある、pool に入っていない、VMID が範囲の外）。範囲や pool を広げると、隣の無関係なゲストまで境界に入る。そこで、引き継ぐコンテナだけを、運用者が guard の設定で 1 つずつ名指しする。名指ししたものだけが、名指しした場所にある間だけ境界に入る。

- **名指しは 2 か所に書く:** 設定（`mamela.providers.toml` の `[[provider.proxmox.guard.named]]`: `node`、`vmid`、`type = "lxc"`、`access`）と、宣言（`Proxmox.named_lxc(site, node, ct)`）。宣言だけでは、guard が変更を止める（PVE-0304）。
- **pool には入れない:** 名指ししたコンテナは、その場所のまま引き継ぐ。Mamela の pool に入っていれば、pool の権限が届いてしまうので、guard の PVE-0302 が止める。VMID の範囲の中のゲストは名指しできない（pool に入れる）。
- **権限は 2 段階、人が決める:** bootstrap は、名指ししたゲストの `/vms/<vmid>` にだけ ACL を付ける。段は設定の `access` で決める。
  - 段 1 `access = "observe"`: `VM.Audit` だけ。plan は引き継ぐ対象と、変わること（プレビュー）を示す。引き継ぎも変更も guard が止める。
  - 段 2 `access = "change"`: 引き継ぎと変更に要る権限も足す（`VM.Config.CPU`・`VM.Config.Disk`・`VM.Config.Memory`・`VM.Config.Options`・`VM.PowerMgmt`）。
  - guard は、Token の権限が段とちょうど同じことを確かめる（PVE-0101、PVE-0102）。段を戻すか名指しを外して bootstrap を実行し直すと、要らなくなった ACL が外れる。
- **名指ししたとおりの場所にあること:** guard は、名指ししたゲストが同じ VMID・種類・ノードにあることを確かめる（PVE-0305）。移された（migrate）、別のゲストになった、なくなったゲストは境界の外として扱い、何も変えない。`access = "change"` のままなら、Token がその VMID のものを変えられてしまうので、すべての apply を止める。設定（と宣言）の `node` を直すか、名指しを外して bootstrap を実行し直す。
- **余裕はそのノードで数える:** 名指ししたコンテナの変更が使うメモリとストレージは、そのコンテナのノードで確かめる（PVE-0201、PVE-0202。閾値は同じ）。そのノードが止まっていれば PVE-0003 で止める。
- **隣のゲストは今までどおり:** 名指ししていないゲストは、同じノードにあっても `Foreign` で、読むだけである。apply の前後に、変わっていないことを guard が確かめる（PVE-0400〜0405）。
- **消さない:** 名指ししたコンテナは、人が大切にしている本番のゲストである。Mamela は消さない。`delete-lxc` は拒み、guard も PVE-0304 で止める。段 2 でも、bootstrap は削除の権限（`VM.Allocate`）を付けない（Proxmox も拒む）。消すときは、人が手で消す（§10.2 の最後）。
- **VM はまだ名指しできない:** VM を宣言の VMID で引き継ぐ仕組みがまだないためである（`type = "qemu"` は設定ごと拒む）。

~~~bash
# 段 1: 見るだけ（設定に access = "observe" で名指しする。宣言は Proxmox.named_lxc）
mamela provider script <pome-dir> proxmox bootstrap > bootstrap.sh
less bootstrap.sh                                                     # 手順 5b を確かめてから、ノードで root として実行
mamela guard <pome-dir>                                               # PVE-0305: 名指ししたとおりの場所にあるか
mamela plan <pome-dir>                                                # 引き継ぐ対象（MAM-PLAN-0114）と、変わること（プレビュー）

# 段 2: 引き継ぐと人が決めたら（access = "change" に書き換える）
mamela provider script <pome-dir> proxmox bootstrap > bootstrap.sh
less bootstrap.sh
mamela guard <pome-dir>
mamela act adopt-lxc "ProxmoxLXC <name>" <pome-dir>                   # 送るものを確かめる
mamela act --confirm <name> adopt-lxc "ProxmoxLXC <name>" <pome-dir>  # 引き継ぐ
mamela plan <pome-dir>
~~~

bootstrap が名指ししたゲストに実行するのは、次の行だけである（VMID 150、操作用のユーザー `mamela@pve` と Token `mamela@pve!lab` の例）。

~~~bash
# 段 1（access = "observe"）: 手順 3 と 5b
ensure_role MamelaLabNamedObserve "VM.Audit"
pveum acl modify /vms/150 --users mamela@pve --roles MamelaLabNamedObserve
pveum acl modify /vms/150 --tokens 'mamela@pve!lab' --roles MamelaLabNamedObserve

# 段 2（access = "change"）: 手順 3 と 5b。段 1 の MamelaLabNamedObserve は手順 6 が外す
ensure_role MamelaLabNamedChange "VM.Audit,VM.Config.CPU,VM.Config.Disk,VM.Config.Memory,VM.Config.Options,VM.PowerMgmt"
pveum acl modify /vms/150 --users mamela@pve --roles MamelaLabNamedChange
pveum acl modify /vms/150 --tokens 'mamela@pve!lab' --roles MamelaLabNamedChange
~~~

実際の VMID とノードは、公開しない設定と宣言にだけ書く。

### 10.2 引き継いだコンテナを消す（delete-lxc）

引き継いだコンテナは、宣言から外しても消えない（plan は MAM-PLAN-0308 として残すと示す）。消すのは、人が名前を打ち直したときだけである。lab のクラスタでは、次のことを守る。

- **消せるのは境界の中だけ:** guard の node・pool・VMID の範囲の中にあり、このプロバイダーのマーカーをその名前で持つ（Mamela が引き継いだ）コンテナだけである。境界の外、マーカーのないもの、別の名前のマーカー、VM の VMID、テンプレートは拒む。guard も前に確かめる（PVE-0304）。
- **名指ししたものは消さない:** §10.1 で名指ししたコンテナは、プロバイダーが拒み、guard も止め、Token に `VM.Allocate` もない。
- **バックアップは人が取る:** Mamela はバックアップを取らない。消す前に `vzdump` で取る。
- **強制しない:** 動いていれば、まずきれいに止める（`shutdown`、`timeout=120`、`forceStop=0`）。止まらなければ失敗し、何も消さない。理由を確かめ、消してよければ `stop-lxc` で止めてから、もう一度実行する。
- **観測したときのコンテナだけ:** 送る直前（止める前と、消す前）に毎回、同じ VMID・ノード・pool・マーカー・設定の digest であることを確かめる。違えば、それ以上送らない。保護（`protection`）やロックがあれば、止める前に拒む（Proxmox が、止めたあとで削除を拒むため）。
- **`purge` で、クラスタのジョブからも外れる:** バックアップ、レプリケーションのジョブと HA のリソースから、その VMID を外す。その VMID だけを挙げていたバックアップのジョブは、ジョブごと消える。`--confirm` なしの表示にもそう書く。VM の削除（§7）と違って `purge` を使うのは、人が作ったコンテナは人がジョブや HA に入れていることが多く、`purge` がないと、Proxmox が削除を拒む（HA、レプリケーション。止めたあとで）か、消えた VMID がバックアップのジョブに残る（同じ VMID の次のゲストがバックアップされる）ためである。
- **guard が前後に走る:** 後で、境界の外のゲストが、消えず、止まらず、変わっていないことを確かめる（PVE-0400〜0405）。
- **中断したら:** 表示された `--recover` で、送り直さずに確かめる。止まっただけで残っていれば、もう一度 `--confirm` で実行する（消すことだけを送る）。もうなければ、済んだとみなす。

~~~bash
vzdump <vmid> --storage <backup-storage>                                   # 要るなら先にバックアップを取る（ノードで root）
mamela guard <pome-dir>                                                    # まず境界を確かめる
mamela act delete-lxc "ProxmoxLXC <name>" <pome-dir>                       # 送るもの（shutdown、DELETE）と効果（restart、lose-data）を確かめる
mamela act --confirm <name> delete-lxc "ProxmoxLXC <name>" <pome-dir>      # 消す（guard が前後に走る）
mamela plan <pome-dir>                                                     # 宣言に残っていれば create-lxc（しない）が出る。宣言からも消す
~~~

名指ししたコンテナを消すときは、人が手で消す。

1. `vzdump <vmid>` でバックアップを取る。
2. 宣言（`Proxmox.named_lxc`）と、設定の `[[provider.proxmox.guard.named]]` を外す。
3. bootstrap を root で実行し直す（その `/vms/<vmid>` の ACL が外れる）。
4. そのノードで `pct shutdown <vmid>`、`pct destroy <vmid>` を実行する。

## 11. 後片付け

アップロードしたイメージと seed は、Mamela の権限では消さない。ストレージ上のファイルを消すには、そのストレージ全体への `Datastore.Allocate` が要り、同じストレージにある他のゲストのバックアップやテンプレートまで消せてしまうためである。

~~~bash
mamela provider script <pome-dir> proxmox cleanup > cleanup.sh
less cleanup.sh
bash cleanup.sh            # 使われていない mamela-* の一覧を出すだけ
bash cleanup.sh --free     # 確かめてから seed を消す（--free --images でイメージも）
~~~

## 12. 運用ルール

- 実機テストは `MAMELA_LAB=1` を明示したときだけ動かす。
- サイト固有の値、既存ゲストの一覧、Token をコミットしない。ログやテストの fixture にも残さない。
- 予算やしきい値を緩める変更は、理由を書いたうえで人がレビューする。
