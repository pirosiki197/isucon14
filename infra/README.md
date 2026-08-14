# infra — アプリコード以外の設定ファイル

サーバ上の設定ファイル（`/etc` と `/home/isucon` 配下）のミラー。
**このディレクトリを見れば、どのサーバがどう設定されているかが分かる**状態を保つ。

## ホストと role

置き場所は**ホストごと**。`infra/<ホスト名>/<絶対パスの先頭 / を取ったもの>`。

```
infra/s1/etc/nginx/nginx.conf   ↔  s1 の /etc/nginx/nginx.conf
infra/s1/home/isucon/env.sh     ↔  s1 の /home/isucon/env.sh
```

**role は「そのホストが今どういう役割か」を表すラベル**で、`servers.env` に書く。

```bash
HOSTS="s1 s2 s3"
S1_ROLES="app"        # 1 台に全部載っている間は "app db"
S2_ROLES="db"
S3_ROLES="app"        # 途中でアプリを 2 台に増やした
```

role の使い道は 1 つだけで、**[PULL](PULL) と [MANIFEST](MANIFEST) の行がどのホストに適用されるか**を決めます。
`app /etc/nginx/nginx.conf` と書けば、role に `app` を持つホスト全部が対象になります。

**ディレクトリをホスト名で切って、role では切らないのが要点です。**
役割は大会中に変わる（`s3` が遊んでいる状態 → 2 台目のアプリになる）ので、
role でディレクトリを切ると、そのたびにファイルが引っ越して履歴が追えなくなります。

## pull は広く、push は狭く

| | 対象 | 定義 |
|---|---|---|
| pull（サーバ → ここ） | `/etc` 丸ごとなど | [PULL](PULL) |
| push（ここ → サーバ） | 明示したファイルだけ | [MANIFEST](MANIFEST) |

**pull を広く取るのは、取りこぼしを無くすため。** 手で書き換えたファイルを
「リポジトリに入れ忘れる」と、サーバを増やしたときや作り直したときに静かに壊れる。
丸ごと持ってきてローカルの `git status` に差分を出させれば、思い出す必要がなくなる。

**push を狭くするのは、git がパーミッションと所有者を保存しないため。**
`/etc` を丸ごと書き戻すと `sudo` が壊れたり、MySQL が設定ファイルを
（誰でも書ける権限になったせいで）黙って無視したりする。

秘密（`shadow`、ssh のホスト鍵、TLS の秘密鍵）は
[rsync-exclude](rsync-exclude) で pull の対象から外している。

## 使い方

```bash
bin/config status                # ホスト・アドレス・role・ミラー済みファイル数
bin/config pull                  # 全ホストから吸い上げ、git status を出す
bin/config pull s1               # ホスト指定
bin/config pull app              # role 指定 (app を持つホスト全部)
bin/config push s1               # MANIFEST の s1 分を配って反映コマンドを実行
bin/config push app              # app を持つホスト全部に配る
bin/config push s2 /etc/mysql/mysql.conf.d/z-isucon-tuning.cnf   # 1 ファイルだけ
bin/config copy s1 s3            # s1 の設定を s3 に複製 (ローカルのみ)
bin/config push s1 -n            # 転送も reload もせず、サーバ側との差分だけ出す
```

### 初動

1. **何も触る前に `bin/config pull` して commit する。** これが初期状態の記録になる
2. 以降は設定をいじるたびに pull する。`git status` が「MANIFEST に足すべきファイル」を教えてくれる

### 台数を増やすとき

3 台構成では、遊んでいるサーバに役割を与える場面が必ず来る。

```bash
# 1. servers.env の S3_ROLES に "app" を足す
# 2. s1 の設定を s3 に複製する (この時点ではローカルのファイルが増えるだけ)
bin/config copy s1 s3
# 3. git diff で確認する。env.sh の接続先など、ホスト固有の値をここで直す
# 4. 配る
bin/config push s3
```

`push` は**中身が変わったファイルが 1 つも無ければ reload しない**。
同じものを配り直してサービスが再起動する事故が起きない。
何が動くかを先に見たいときは `-n` を付ける（rsync の itemize がそのまま出る）。

`copy` がローカル操作で止まるのは、**差分を目で見てから配るため**。
`env.sh` のようにホストごとに違ってよいファイルを、無自覚に上書きしないようにしている。

### role を増やすとき

role は自由な文字列で、`bin/config` 側に一覧を持っていない。
新しいミドルウェア（DNS、キャッシュ、キューなど）が出てきたら、**触るのは 2 箇所だけ**。

```bash
# 1. servers.env でホストにその role を与える
S2_ROLES="db dns"

# 2. MANIFEST に行を足す (設定が /etc の下なら PULL は触らなくてよい)
dns  /etc/powerdns/pdns.conf   sudo systemctl restart pdns
dns  /etc/powerdns/pdns.d/     sudo systemctl restart pdns
```

`/etc` 全体を pull しているので、**新しいミドルウェアの設定は入れた瞬間から勝手にミラーされる**。
`/var/lib/...` のように `/etc` の外に置くものだけ、[PULL](PULL) に 1 行足す。

MANIFEST のパスは末尾に `/` を付けるとディレクトリごと配れる（中身のみ。
サーバ側にしか無いファイルは消さない）。`pdns.d/` のような drop-in ディレクトリはこれで扱う。

## ファイルではない設定

以下は rsync では捕まらないので、再実行できるスクリプトとしてここに置く。

- MySQL の `CREATE USER` / `GRANT`（複数台構成にするときに必要）
- `SET GLOBAL` で入れた実行時の値
- インデックス追加などのスキーマ変更（`sql/` 側に置く）

判断基準は「**サーバを作り直したときに、この手順だけで元に戻せるか**」。

## 次の大会で使い回す

問題固有の情報は `MANIFEST` の中身（サービス名・設定ファイル名）だけで、
`bin/config` と `PULL` と `rsync-exclude` はそのまま持っていける。

```bash
cp -r bin/config infra/{PULL,rsync-exclude} servers.env.example <新しいリポジトリ>/
```

新しい大会では `servers.env` を書いて `bin/config pull` し、
サーバを触りながら `MANIFEST` に行を足していく。
