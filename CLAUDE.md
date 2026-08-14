# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## このリポジトリについて

ISUCON14 の Web アプリ「ISURIDE」(椅子の配車サービス) の Go 参照実装。ベンチマーカーは含まれず、`webapp` 相当のみ。**パフォーマンスチューニングが目的のコードベース**であり、初期実装は意図的に非効率に書かれている。

## コマンド

すべて `go/` ディレクトリで実行する。

```bash
cd go
go build -o isuride .   # ビルド (.gitignore で isuride は除外済み)
go run .                # 起動 (:8080)
go vet ./...
```

テストは存在しない。

**起動時のカレントディレクトリは必ず `go/`**。`POST /api/initialize` が相対パス `../sql/init.sh` を exec するため、他の場所から起動すると初期化が失敗する。

DB 接続は環境変数 `ISUCON_DB_{HOST,PORT,USER,PASSWORD,NAME}` (既定: `127.0.0.1:3306`, `isucon`/`isucon`, `isuride`)。`sql/init.sh` は `/home/isucon/env.sh` があれば読み込み、`ENV=local-dev` のときは DB 再投入をスキップする。

決済ゲートウェイのスタブは `payment_mock/`(`:12345` で待受)。実際の URL は `POST /api/initialize` のリクエストボディで渡され `settings` テーブルに保存される。

API 仕様は `openapi.yaml`、フロントエンドのビルド済み成果物は `public/`(通常 nginx が配信)。

## アーキテクチャ

`go/` は単一の `package main`。ルーティングは `main.go` の `setup()` に集約され、利用者(app) / オーナー(owner) / 椅子(chair) / internal の 4 系統に分かれ、ファイル名もこれに対応する(`app_handlers.go` など)。

- 認証は Cookie (`app_session` / `owner_session` / `chair_session`) → `middlewares.go` が毎リクエスト `access_token` で SELECT し、`context.WithValue(ctx, "user"|"owner"|"chair", ...)` で渡す(文字列キー)。ハンドラ側は `ctx.Value("user").(*User)` で取り出す。
- ルータは chi だが、パターンは Go 1.22 の `"POST /api/..."` 形式で書かれており、パスパラメータは `r.PathValue("ride_id")` で取る。
- DB アクセスは sqlx のグローバル `db`。多くのハンドラが `db.Beginx()` でトランザクションを張り `defer tx.Rollback()` する。

### ドメインモデルの要点

**ライドの状態は追記型**。`rides` に現在ステータス列はなく、`ride_statuses` に `MATCHING → ENROUTE → PICKUP → CARRYING → ARRIVED → COMPLETED` の 6 行が順次 INSERT される。「現在の状態」は常に `ORDER BY created_at DESC LIMIT 1`(`getLatestRideStatus`)で求める。

**通知はポーリング**。`GET /api/app/notification` と `GET /api/chair/notification` は「まだ送っていない最古のステータス」(`app_sent_at IS NULL` / `chair_sent_at IS NULL`)を 1 件返し、返した行に送信時刻を刻む。レスポンスの `retry_after_ms` が次回ポーリング間隔。

**椅子の位置も追記型**。`chair_locations` に INSERT され続け、最新位置は `ORDER BY created_at DESC LIMIT 1`。オーナー向けの `total_distance` は `chair_locations` 全体に対する `LAG()` ウィンドウ関数でその場計算している(`ownerGetChairs`)。

**マッチングは外部からの定期起動**。`GET /api/internal/matching` をインスタンス内から一定間隔で叩く前提。現状は「最も待たせているライド 1 件 × ランダムな空き椅子」を最大 10 回試行するだけの実装(`internal_handlers.go` のコメントも参照)。

**運賃**: `initialFare`(500) + `farePerDistance`(100) × マンハッタン距離。クーポン割引は従量部分にのみ適用され、初乗り運賃は割り引かれない(`calculateDiscountedFare`)。クーポンは `coupons.used_by` にライド ID を書いて消費し、`CP_NEW2024`(初回登録)を最優先、以降は付与順。

**決済**: `appPostRideEvaluatation` が評価登録と同一トランザクション内から決済 API を同期呼び出しする(`payment_gateway.go`、最大 5 回リトライ)。DB トランザクションを保持したまま外部 HTTP を待つ構造になっている。

### チューニング時に効いてくる既知の構造

- `sql/1-schema.sql` には主キーとユニーク制約以外のインデックスが一切ない。`chair_locations.chair_id`、`ride_statuses.ride_id`、`rides.chair_id` などは未設定。
- N+1 が随所にある。特に `appGetNearbyChairs`(全椅子 × 全ライド × 最新ステータス × 最新位置)、`getChairStats`(ライドごとにステータス全件)、`ownerGetSales`(椅子ごとにライド取得)、`appGetRides` / `appPostRides`(ライドごとに `getLatestRideStatus`)。
- `internal_handlers.go` の椅子選択は `ORDER BY RAND()`。
- スキーマや初期化フローを変える場合、`sql/1-schema.sql`(DDL)と `sql/3-initial-data.sql.gz`(初期データ、gzip)の整合に注意する。ベンチマーカーは `POST /api/initialize` 経由で毎回 `sql/init.sh` を走らせる。

## 大会をまたいで使える知見

`docs/learnings/` に、この問題固有ではない知見をまとめてある。計測の順序、インデックスの当て方、MySQL の fsync 設定、Go 側の定石、進め方。新しく効いた手や踏んだ罠はここに追記する。

## サーバーでの計測と運用

アドレスは**リポジトリに含めない**。`servers.env`(gitignore 済み、`servers.env.example` がテンプレート)に書いてあるので、必要なら読む。

| 変数 | 役割 |
|---|---|
| `APP_HOST` / `APP_PRIVATE_IP` | アプリ + nginx。2 vCPU / 3.8GB |
| `DB_HOST` / `DB_PRIVATE_IP` | MySQL |
| `BENCH_HOST` / `BENCH_PRIVATE_IP` | ベンチマーカー。2 vCPU |

いずれも `ssh isucon@<host>` で入れて `sudo` は NOPASSWD。2 vCPU しかないので、計測エージェント自体の負荷も無視できない。

systemd サービスは `isuride-go`(`/home/isucon/webapp/go/isuride`)、`isuride-matcher`、`mysql`、`nginx`、`alloy`。環境変数は `/home/isucon/env.sh`(`EnvironmentFile` として読まれる)。

### Go のバージョンに注意

サーバーには Go が 2 つある。**`go.mod` の `go 1.26` を満たすのは `/usr/local/go/bin/go`(1.26.5)だけ**で、`~/local/golang/bin/go` は 1.23.2 でビルドできない。`.profile` は `/usr/local/go/bin` を PATH 先頭に置くよう直してあるが、**非対話の `ssh host 'go ...'` では `.profile` も `.bashrc` も読まれない**ため、スクリプトからはフルパスで呼ぶ。

### デプロイ

```bash
. ./servers.env
ssh "isucon@$APP_HOST" 'cd /home/isucon/webapp && git pull && cd go && /usr/local/go/bin/go build -o isuride . && sudo systemctl restart isuride-go'
```

### ベンチマーク

ローカルから `bash run_bench.sh`。`servers.env` を読んでベンチサーバーに ssh し、`bench run` を実行する。`--payment-url` は**ベンチサーバー自身の内部 IP**(`BENCH_PRIVATE_IP`)を指す必要がある。ここをアプリサーバーの IP にすると決済モックに届かず `evaluation` が全部 500 になる。

### slow query の解析

`long_query_time = 0` で全クエリを記録する(`/etc/mysql/mysql.conf.d/z-isucon-slow.cnf`)。ベンチ 1 回で約 54MB 増えるので、計測ごとに空にしてから回す。

```bash
. ./servers.env
ssh "isucon@$DB_HOST" 'sudo truncate -s 0 /var/log/mysql/mysql-slow.log'
bash run_bench.sh
ssh "isucon@$DB_HOST" 'sudo pt-query-digest --limit 20 /var/log/mysql/mysql-slow.log > /tmp/digest.txt; sed -n "1,12p;/# Profile/,/^$/p" /tmp/digest.txt'
```

計測しないときは `sudo mysql -e "SET GLOBAL slow_query_log = OFF;"` で即座に止まる(設定ファイルは残るので再起動で復活する)。

### OpenTelemetry / Grafana Cloud

アプリは HTTP・SQL・決済 API を計装済み(`go/otel.go`)。送信先は Alloy の OTLP receiver(`127.0.0.1:4318`)で、そこから Grafana Cloud Tempo に転送される。**Alloy 側のトレース設定は Fleet Management から配信されており、`/etc/alloy/config.alloy` には現れない**ので、ローカルの設定ファイルだけ見て「トレース未設定」と判断しないこと。

制御はすべて `env.sh` の `OTEL_*` 標準環境変数で行う。`OTEL_TRACES_SAMPLER_ARG` が 0.1(10% サンプリング)。計装を切るなら `OTEL_SDK_DISABLED=true` を足して再起動する。

slow query log は**あえて Loki に送っていない**。1 リクエストあたり 32KB・ベンチ 1 回で 54MB 出るため Grafana Cloud Free の 50GB/月 を圧迫する上、トレース側に `db.query.text` として SQL が既に載っており情報が重複するため。解析は `pt-query-digest` でサーバー上で行う。

### この環境固有の壊れている点

- **nginx の TLS 証明書が 2025-02-01 に失効している**。`isuride-matcher` は元々 `https://isuride.xiv.isucon.net/api/internal/matching` を叩いていたが、証明書検証エラー(curl exit 60)で毎回失敗し、マッチングが一度も動かなかった。現在は `http://localhost:8080/api/internal/matching` の直叩きに変更済み(TLS と nginx のオーバーヘッドも省ける)。元の unit は同ディレクトリに `.bak.*` で残してある。
- Alloy の UI が `127.0.0.1:12345` を使う。`payment_mock` の待受ポートと同じなので、ローカルで決済モックを動かす構成にするなら衝突する。
