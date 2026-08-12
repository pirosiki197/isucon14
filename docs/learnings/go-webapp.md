# Go の webapp

## コネクションプール

**`database/sql` の `SetMaxIdleConns` はデフォルト 2。** これを設定しないと、
アイドル接続が 2 本しか保たれず、**クエリのたびに TCP 接続と MySQL 認証をやり直す**。

```go
db.SetMaxOpenConns(100)   // MySQL の max_connections (既定 151) の内側に
db.SetMaxIdleConns(100)   // MaxOpen と同じにするのが要点
db.SetConnMaxLifetime(0)
```

トランザクションを使っている間は 1 リクエストが 1 本を掴んで離さないので表面化しない。
**トランザクションを外した途端に問題になる**ので、セットで入れる。

今回これを忘れて、接続がピーク 105 本まで膨らみ、3 本が確立中にタイムアウトして
`dial tcp: operation was canceled` でベンチのエラーになった。

## interpolateParams

```go
dbConfig.InterpolateParams = true
```

デフォルトではプレースホルダごとに prepared statement を作るため、
`ADD PREPARE` が**クエリと同じ回数**発生する（今回 56,633 回）。
クライアント側で展開すればこの往復が丸ごと消える。

## 読み取りにトランザクションを張らない

読むだけのハンドラが `db.Beginx()` していると、**コミットのたびに fsync が走る**。
今回 `COMMIT` が pt-query-digest の首位（27,119 回）だった。

外す判断基準は「複数の書き込みに原子性が要るか」。
単一の `UPDATE` しかないなら autocommit で足りる。

外すときの注意:

- `*sqlx.Tx` を受け取っているヘルパは、`*sqlx.DB` も渡せるインターフェースに変える
  ```go
  type executableGet interface {
      Get(dest any, query string, args ...any) error
      GetContext(ctx context.Context, dest any, query string, args ...any) error
  }
  ```
- `FOR UPDATE` / `FOR SHARE` はトランザクション外では意味が無いので一緒に消す
- 上記のコネクションプール設定を必ずセットで入れる

## N+1

ループの中でクエリを投げている箇所を探す。digest では「速いクエリが大量」に見えるので、
**alp でエンドポイント単位の時間を見ないと発見しにくい**。

集計系の N+1（ループで集めて Go 側で数える）は、SQL 一発にできることが多い。

```sql
SELECT COUNT(*) AS cnt, IFNULL(AVG(evaluation), 0) AS avg
FROM (SELECT r.id, r.evaluation FROM rides r JOIN ride_statuses s ON s.ride_id = r.id
      WHERE r.chair_id = ?
      GROUP BY r.id, r.evaluation
      HAVING SUM(s.status = 'ARRIVED') > 0 AND SUM(s.status = 'COMPLETED') > 0) t
```

`sqlx` で集計結果を構造体に受けるときは **`db` タグが要る**。
デフォルトの NameMapper は `strings.ToLower` なので、
`TotalRidesCount` は `totalridescount` を探し、`total_rides_count` にマッチしない。

## ポーリング間隔

クライアントに次のポーリング間隔を返す API なら、**その値はサーバ側で自由に変えられる**。
リクエスト数がそのまま CPU 負荷なので、伸ばすだけで効く（今回 30ms → 100ms で +57%）。

ただし状態遷移を通知経由で進める設計だと、**遷移の段数だけ遅延が上乗せされる**。
伸ばしすぎると処理が進まなくなるので、**定数に切り出して実験で決める**。
アプリが速くなるほど最適値は短くなるはずなので、都度測り直す。

なお、間隔を 3.3 倍にしてもリクエスト数は 2 割しか減らなかった。
スループットが上がって母数が増えるためで、**効いたのは 1 回あたりの単価の方**だった。

## graceful shutdown とシグナル

systemd の `ExecStop` が `kill -s QUIT` を送る構成があり、
Go はデフォルトで **SIGQUIT を受けるとスタックダンプを吐いて即死する**。
`signal.NotifyContext` で `SIGINT`/`SIGTERM` だけ捕捉していても機能しないので、
`syscall.SIGQUIT` も含める。

OpenTelemetry などで終了時にフラッシュしたいものがあると、これを取りこぼす。
