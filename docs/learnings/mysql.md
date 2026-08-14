# MySQL

## インデックス

ISUCON の初期実装は**主キー以外のインデックスが無い**ことが多い。
まずスキーマと `WHERE` 句を突き合わせて、機械的に貼る。ここが一番効く。

```sql
SHOW INDEX FROM <table>;   -- PRIMARY だけなら候補
```

`WHERE a = ? ORDER BY b DESC LIMIT 1` という形なら **`(a, b)` の複合**にする。
`a` だけでも絞れるが、`b` を含めると `ORDER BY` の filesort も消えて
`LIMIT 1` がインデックス先頭 1 件で済む。

今回の実績:

| 対象 | 効果 |
|---|---|
| `ride_statuses(ride_id, created_at)` | 21.8ms → 0.3ms (約 70 倍)。全体の 49% を占めていたものが 1.2% に |
| `chair_locations(chair_id, created_at)` | 92ms/回のクエリが digest から消滅 |
| `chairs(access_token)` | 認証が全リクエストに乗っていたため、**全エンドポイントが 3 割速くなった** |

**`access_token` のような認証カラムを見落としやすい。** 全リクエストが通るので、
1 本のインデックスで全体が底上げされる。`users` や `owners` は `UNIQUE` 制約が
付いていてインデックスがあるのに、`chairs` だけ無い、といった非対称がよくある。

### インデックスが効かないケース

`WHERE` が無ければインデックスは使われない。**全件必要なら順に読む方が速い**と判断される。

```sql
-- WHERE が最外側にしかなく、内側は全件処理される
FROM chairs
  LEFT JOIN (SELECT ... FROM chair_locations) d ON d.chair_id = chairs.id
WHERE owner_id = ?
```

この形は**絞り込みをサブクエリの内側に押し込む**と一撃で直る。

```sql
FROM chair_locations WHERE chair_id IN (SELECT id FROM chairs WHERE owner_id = ?)
```

今回はこれだけで 1.75s → 63ms（28 倍）。走査対象が 27,688 行 → 3,057 行になり、
**貼ってあったインデックスがここで初めて使われるようになった**。
「インデックスを貼ったのに速くならない」ときは `EXPLAIN` で `key: NULL` / `type: ALL` を疑う。

## fsync 設定

デフォルトは耐久性最優先で、**1 コミットあたり redo log と binlog で 2 回 fsync する**。
更新が多いワークロードではこれが支配的になる。

```ini
[mysqld]
innodb_flush_log_at_trx_commit = 0   # 毎コミットの fsync をやめ 1 秒ごとにまとめる
sync_binlog                    = 0
innodb_doublewrite             = OFF
disable_log_bin                      # レプリカがいないなら binlog 自体不要
```

効果: fsync が **3 万回 → 939 回**。首位だった `UPDATE chairs`（全体の 48%）が
digest から消滅し、全体の Exec time が半減した。

**クラッシュすると直近 1 秒程度の更新が失われる。** ISUCON は計測ごとに
`initialize` でデータを作り直すので問題ないが、本番システムでは絶対にやらない。

### 効かなかったもの

`innodb_buffer_pool_size` はデフォルト 128MB のままにした。
`Innodb_buffer_pool_reads` が 401 万リクエスト中 **12 回**、ヒット率 99.9997% で、
**データが既に収まっていた**ため。定番だからと闇雲に上げず、まず統計を見る。

## スキーマ変更の落とし穴

初期データの INSERT が**カラム名を省略している**ことがある。

```sql
INSERT INTO `chairs` VALUES ('01ABC...', '01DEF...', 'name', ...);
```

この状態で `CREATE TABLE` にカラムを足すと**値の数が合わずデータ投入が失敗する**。
しかも `init.sh` の出力を捨てていると気づかず、テーブルが空のままベンチが走る。

対策は**データ投入後に `ALTER TABLE ADD COLUMN` する**こと。
インデックス追加は INSERT に影響しないので `CREATE TABLE` に書いてよい。

```bash
mysql < 1-schema.sql
mysql < 3-initial-data.sql      # カラム名省略の INSERT
mysql < 4-denormalize.sql       # ここで ALTER TABLE ADD COLUMN
```

### バックフィルで updated_at を壊さない

`ON UPDATE CURRENT_TIMESTAMP` が付いた列は、**関係ない列を UPDATE しただけで書き換わる**。
初期データの `updated_at` を期間絞り込みに使っているクエリがあると、
バックフィル一発で売上集計などが壊れる。

**自分自身を明示的に代入すると自動更新が抑止される。**

```sql
UPDATE rides r JOIN ... 
SET r.latest_status = l.status,
    r.updated_at    = r.updated_at;   -- これで ON UPDATE が発火しない
```

確認は簡単で、バックフィル後に `SELECT MIN(updated_at), MAX(updated_at)` を見る。
初期データの日付のままなら成功、実行日時になっていたら壊している。

## 複数テーブルをまとめて更新して隙間を消す

フラグとカウンタのように**同時に進まないと矛盾する 2 つの値**を別々の文で更新すると、
その隙間を読んだリクエストが矛盾した値を返す。今回これで検証エラーを 132 件出した。

MySQL の複数テーブル UPDATE で 1 文にまとめれば、隙間は原理的に消える。

```sql
UPDATE ride_statuses s
       JOIN rides r  ON r.id = s.ride_id
       JOIN chairs c ON c.id = r.chair_id
SET s.app_sent_at      = CURRENT_TIMESTAMP(6),
    c.completed_rides  = c.completed_rides + 1,
    c.total_evaluation = c.total_evaluation + IFNULL(r.evaluation, 0)
WHERE s.id = ? AND s.app_sent_at IS NULL
```

`WHERE ... IS NULL` を付けておくと、**同時に 2 リクエストが来ても二重に数えない**
（更新できた側だけがカウントを進める）。トランザクションを張るより軽く、
「どちらを先に更新するか」を悩む必要もなくなる。

## 非正規化

「読むたびに集計」を「書くたびに更新」へ移す。
履歴テーブルを毎回集計している箇所が候補。

今回は座標履歴 (`chair_locations`) から総移動距離を窓関数で毎回計算していたのを、
`chairs` に `total_distance` を持たせて座標受信時に加算する形にした。
履歴が伸びても悪化しなくなる。

注意点:

- **初期データからの移行 SQL が要る**。アプリは以降 `chairs` しか更新しないので、
  初期状態を作っておかないと不整合になる
- 移行後は**元の計算と結果が一致するか必ず検証する**
  （[workflow.md](workflow.md#結果が変わらないことを確認してから置き換える)）
- 集約条件を「簡単にできそう」と単純化しない。今回 `COMPLETED` があれば
  `ARRIVED` もあるはずと考えたが、実データに 2 件例外があった。
  **状態遷移の不変条件は、実データで数えて確かめる**
