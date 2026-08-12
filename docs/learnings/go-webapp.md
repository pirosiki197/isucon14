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

**「アプリが速くなるほど最適値は短くなる」は逆だった。** 30ms → 100ms で +57%、
さらに 100ms → 350ms で +28%。**速くなるほど最適値は伸びた。**

理由は、CPU が飽和している環境ではリクエスト数がそのまま上限を決めるから。
実際、間隔を伸ばすと「マッチ待ちの不満」は 6.3% → 24.8% に**悪化**したのに、
スコアは上がった。**待ち時間の悪化より、CPU が空いてスループットが増える効果が大きい**。

```
50ms   38636
100ms  43889
200ms  48189
350ms  約 55k   ← ここから先は平坦
```

短い側に振ると急激に悪化し、長い側は平坦。**迷ったら長めに振る**のが安全。
なお 350 / 450 / 600ms の差は変動幅の中で、最適値は決められなかった。

## 認証クエリをメモリに載せる

**全リクエストに 1 本乗るクエリは、単価が安くても全体では最大になる。**
今回、認証だけで全クエリの 24%（`chairs` 15% + `users` 8%）だった。

キャッシュできるかは**行が変化するか**で決まる。まず確認する。

```bash
grep -n "UPDATE users\|UPDATE owners\|DELETE FROM users" *.go
```

`users` と `owners` は INSERT のみで一度も更新されなかった。**完全に不変なので
古くなりようがない**。これは 20 行で入る、事故の余地がほぼ無い変更。

### 可変な行は「不変な部分だけ」を型を分けて持つ

`chairs` は座標・カウンタ・`is_active` が走行中に変わり続けるので、
**行ごとキャッシュしてはいけない**。一方でハンドラが実際に読んでいたのは
`chair.ID` が 5 箇所と `chair.PendingRides` が 1 箇所だけだった。
つまり**最大のクエリは、トークンを ID に変換するためだけに存在していた**。

不変なカラムだけを別の型に切り出すと、可変な値を誤って読めなくなる。

```go
// chairs 本体とは別の型にする。Chair を入れると
// PendingRides などをゼロ値で読んでしまい、静かに壊れる
type chairIdentity struct {
    ID      string `db:"id"`
    OwnerID string `db:"owner_id"`
    Name    string `db:"name"`
    Model   string `db:"model"`
}
```

これで椅子の認証が 1 走行 46,000 回 → 約 340 回になり、**スコアは +9%**。
変動幅 17% を超えたので、これは効果として信じられる。

### 必ず踏む 2 つの罠

- **`POST /api/initialize` でキャッシュを捨てる。** しかも**リセットスクリプトの実行後**に。
  実行中に引き当てた行も無効なので、前に捨てると取りこぼす
- **ミス時は DB にフォールバックする。** 走行中に登録されるユーザーや椅子があるので、
  キャッシュに無いだけで 401 を返してはいけない

## カウンタ化はクエリと違って自己修復しない

集計クエリは毎回真の状態を読むので**自己修復する**。カウンタに置き換えると、
**加算を 1 回取りこぼした時点で永久にずれる**。

だから「いつ加算するか」を、クエリ版で**先に**確定させてから移すとよい。
今回はクエリ版で境界（ユーザーへの通知時点）を特定してからカウンタ化したが、
それでも加算位置を 3 回間違えた。

走行後に真値と突き合わせる検算を用意しておくと、ずれを即座に検出できる。

```sql
-- カウンタと、元の集計式の結果が一致するか
SELECT SUM(c.completed_rides <> IFNULL(o.cnt, 0)) AS drift FROM chairs c LEFT JOIN (...) o ...
```

## SIGQUIT を捕まえる（最初にやる）

ISUCON の unit は `ExecStop=/bin/kill -s QUIT $MAINPID` になっていることがある。
Go は **SIGQUIT を受けると全ゴルーチンのスタックを吐いて status 2 で即死**するので、
`signal.NotifyContext` で `SIGINT`/`SIGTERM` だけ捕まえていても機能しない。

```go
signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
```

**厄介なのは、`Restart=on-failure` がこれを隠すこと。** 今回、
**一日中デプロイのたびに死んで復活していたのに症状が出なかった**。
リトライ上限に達した回だけサービスが `failed` のまま止まり、
`POST /api/initialize` が nginx の 502 になって初めて露見した。

デプロイの直後に確認する癖をつけると早く気付ける。

```bash
sudo systemctl restart isuride-go && sleep 2 && \
  sudo journalctl -u isuride-go --since '1 min ago' | grep -c SIGQUIT
```

この項目は序盤に気付いていたのに「オプション」として後回しにした。
**放置した既知の問題は、無関係な作業中に別の症状として出てくる。**
1 行で直るものは見つけた時点で直す。
