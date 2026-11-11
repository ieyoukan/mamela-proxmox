# Proxmox VE で Mamela を安全に動かす（lab）

> この文書は公開リポジトリにある。サイト固有の値（ホスト名、IP、既存ゲストの一覧、Token）は書かない。それらは、gitignore した `mamela.site.toml`・`mamela.providers.toml` と、手元の宣言に置く。
>
> Mamela 本体の考え方は [Mamela の ADR 0006](https://github.com/ieyoukan/mamela/blob/main/docs/adr/0006-proxmox-out-of-core.md) にある。Proxmox の知識はすべてこのリポジトリにあり、Mamela 本体は guard・明示操作・スクリプトという汎用の仕組みでそれを強制する。

## 1. 目的と前提

- 既存のワークロードが動いている Proxmox クラスタの中で、**既存のゲスト（VM / LXC）を一切壊さずに** Mamela で VM を作り、その上を管理する。
- 境界は宣言と設定で決め、毎回の apply の前後に機械的に確かめる。
- 実機テストは明示的に opt-in したときだけ動かす（`MAMELA_LAB=1`）。CI の標準ジョブでは動かさない。

## 2. 安全の考え方: 境界を三重にする

| 層 | 仕組み | 何を防ぐか |
|---|---|---|
| 1. Proxmox の権限 | Mamela 専用の pool・ユーザー・権限分離した Token。権限は pool と、必要なストレージ・ブリッジにだけ付ける | Mamela が既存ゲストを見る・変える・消すこと自体を API が拒む |
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
| `mamela.providers.toml` | `[provider.proxmox]`: API の URL、`auth = "proxmox"`、操作用 Token（`token_id`、`token_file`）、`tls_fingerprint`、`catalog = "builtin:proxmox"`、guard 用の監査 Token（`guard_token_id`、`guard_token_file`）。`[provider.proxmox.guard]`: 境界と安全の閾値 | gitignore |
| 宣言 | `Proxmox.Site`（node、pool、ストレージ、bridge、tag、VMID の範囲）と `Proxmox.vm` | 実アドレスを含むなら gitignore した場所 |

設定の例は [README](../README.md#使い方) にある。

- **宣言の Site と guard の設定は同じ値にする。** guard は、各変更が `[provider.proxmox.guard]` の境界（ノード、VMID の範囲、pool、ストレージ、ブリッジ、タグ）の中にあることを確かめる（PVE-0304）。宣言に何が書かれていても、運用者の境界の外には出ない。
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
  - 監査用の別ユーザーと Token（組み込みロール `PVEAuditor` を `/` に。guard だけが使う）
- 消すのは、操作用ユーザーと Token の ACL のうち、もう要らないものだけ。もう一度実行しても同じ状態に収束する。

### 権限はマニフェストから導く

- 権限は手で決めない。各操作に要る権限は、プロバイダーのマニフェスト（needs）にある。bootstrap と guard は、同じマニフェストから付与と検査の内容を導く。
- 各 API 呼び出しに Proxmox VE 9.2 が確かめる権限は、権限カタログ（`proxmox/permissions.go`）にある。根拠は API スキーマの permissions と、ハンドラの中のチェック（`PVE/API2/Qemu.pm` の `create_vm`・`check_storage_access`・`check_vm_modify_config_perm`・`check_vm_create_serial_perm`、`PVE/Storage.pm` の `check_volume_access`、`PVE/GuestHelpers.pm` の `assert_tag_permissions`）。
- 試験で、各操作の needs がカタログの権限を満たすことを確かめる。新しい呼び出しは、権限を書かずには足せない。
- guard は、操作用 Token の実効権限がこの一覧（と、pool に属する範囲内の `/vms/<id>`）を一つでも超えれば PVE-0101 で、足りなければ PVE-0102 で止める。
- `/`、`/vms`、他の pool、他のノードには何も付けない。

## 5. VM の作り方

テンプレートは使わない。宣言の `image`（例 `ubuntu-24.04`）から、毎回次のように作る。

1. **イメージはこのプロバイダーのコードに固定する**（`proxmox.Images`。URL、sha256、最小ディスク）。ないものは拒む。イメージの更新は、レビューされるコードの変更（新しい版）になる。
2. **一度だけアップロードする。** Mamela がダウンロードして sha256 を確かめ、イメージ用ストレージに `import` としてアップロードする。Proxmox にも sha256 を確かめさせる。ファイル名に sha256 を含める。
3. **seed をアップロードする。** Mamela が作る NoCloud の ISO（mamelet、一度きりの登録トークン、ネットワーク）。プロバイダーは中身を見ない。
4. **イメージから直接 VM を作る**（`import-from`）。止めたまま作り、マーカーを書く。
5. ディスクを宣言の大きさまで広げ、タグを付ける。タグは作成時に付けられない（Proxmox は pool に入れる前にタグの権限を確かめる）。
6. **起動は late の変更にする。** Mamela は、その apply のすべての作成を終えてから起動する。初回起動が別の VM のディスクの取り込みと重なると、I/O 待ちで緊急モードに落ちることがあるためである。

### 中断と再試行

- Mamela は、作成を送る前に記録する（VMID）。応答が失われても、同じ VMID に作り直すことはない。
- 作成が途中で止まった VM（マーカーはあるがタグがない）は「途中」と報告する。Mamela は、記録した VM なら残りの変更（`grow-vm-disk`、`tag-vm`、`start-vm`）を提案する。
- 起動の変更は、作成と同時でも、止まった VM を見つけたときでも同じ内容にしてある。作成の応答が失われた後も、承認した plan のまま続けられる。
- しないこと（差分として示し、Mamela が拒む）: ディスクの縮小、イメージの変更、ネットワークの変更。

## 6. guard（apply の前後）

`mamela apply` は、このプロバイダーの変更を送る前後に guard を呼ぶ。単独でも実行できる。

~~~bash
mamela guard <pome-dir>          # 前の検査だけ（読み取りのみ）。block があれば終了コード 1
~~~

- **前:** クォーラム、ノード、操作用 Token の権限、メモリとストレージの余裕、イメージを置けるストレージ、境界（範囲内の他人のゲスト、pool 内の範囲外のゲスト、VM の数）、各変更が境界の中にあること。境界の外のゲストを記録する（baseline）。
- **後:** 境界の外のゲストが、消えず、止まらず、設定も変わらず、再起動もしていないこと。
- **block があれば:** 前なら何も送らない。後なら apply を失敗として報告する。人が確かめてから次に進む。
- コードの一覧は [README](../README.md#guard適用の前後の安全確認) にある。

## 7. 明示操作（削除、再起動、引き継ぎ）

plan には現れない操作である。人が名前を打ち直したときだけ行う。対象は、Mamela が作成を記録した VM に限る（引き継ぎは除く）。

~~~bash
mamela act delete-vm web <pome-dir>                      # 何を送るかを表示するだけ
mamela act --confirm web delete-vm web <pome-dir>        # 名前を打ち直したときだけ実行する
mamela act --confirm web reboot-vm web <pome-dir>        # guest に再起動を頼む（ACPI）
mamela act --confirm web reset-vm web <pome-dir>         # 応答しない guest をリセットする
~~~

- **流れ:** 表示 → guard（前）→ 記録 → 許可を発行して送る → guard（後）。
- **送る直前の確認:** VMID の VM がまだその名前のマーカーを持つことを確かめる。違えば何も送らない。
- **削除:** 止めてから、ディスクごと消す（`destroy-unreferenced-disks=1`）。バックアップやレプリケーションのジョブから外す `purge` は、クラスタ全体の設定を変えるので使わない。済めば Mamela は作成の記録を消す。
- **起動で止まった VM:** mamelet が 15 分を超えて報告しなければ、plan は MAM-PLAN-0110 で止める。コンソールで確かめてから `reboot-vm`、応答しなければ `reset-vm` を使う。

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

## 9. 後片付け

アップロードしたイメージと seed は、Mamela の権限では消さない。ストレージ上のファイルを消すには、そのストレージ全体への `Datastore.Allocate` が要り、同じストレージにある他のゲストのバックアップやテンプレートまで消せてしまうためである。

~~~bash
mamela provider script <pome-dir> proxmox cleanup > cleanup.sh
less cleanup.sh
bash cleanup.sh            # 使われていない mamela-* の一覧を出すだけ
bash cleanup.sh --free     # 確かめてから消す
~~~

## 10. 運用ルール

- 実機テストは `MAMELA_LAB=1` を明示したときだけ動かす。
- サイト固有の値、既存ゲストの一覧、Token をコミットしない。ログやテストの fixture にも残さない。
- 予算やしきい値を緩める変更は、理由を書いたうえで人がレビューする。
