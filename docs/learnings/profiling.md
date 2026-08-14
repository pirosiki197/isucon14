# 計測

道具ごとに**答えられる問いが違う**。順番を守ると迷わない。

| 問い | 道具 |
|---|---|
| どのエンドポイントが遅いか | alp (nginx アクセスログ) |
| どのクエリが重いか | pt-query-digest (slow query log) |
| CPU を食っているのはアプリか DB か | `systemctl show -p CPUUsageNSec` の差分 |
| CPU か I/O か | vmstat の `us`/`sy`/`wa`/`id` |
| なぜそのクエリが遅いのか | `EXPLAIN` と `SHOW GLOBAL STATUS` の差分 |

## digest と alp の使い分け

**digest だけ見ていると詰まる。** 単価の高いクエリがある間は digest で答えが出るが、
それを潰し切ると上位が「2ms のクエリが 3 万回」に変わる。こうなると
「1 リクエストが何回クエリを投げているか」が問題であり、
テーブル横断で集計する digest では N+1 が分解されて見えない。**そこで alp に切り替える。**

逆に alp だけでも足りない。「このエンドポイントが遅い」までしか分からず、
中で何が起きているかは digest が要る。**両方を突き合わせる。**

### digest が平坦になったら役目は終わり

終盤、上位 12 本がすべて単価 0.2〜0.4ms で並び、最大でも全体の 17% という状態になった。
こうなると digest からは打ち手が読み取れない。**次に見るのは alp のリクエスト数**で、
「どのエンドポイントがこの本数を生んでいるか」を突き合わせる。

今回それで、`SELECT rides` が**2 つのホットエンドポイントの両方に乗っている**ことが分かり、
削る場所が確定した。digest だけ見ていると「どのハンドラから来た本数か」が分からない。

## 測る前にログを空にする

**当たり前だが今回間違えた。** slow log は毎回空にしていたのに nginx のアクセスログを
空にしておらず、alp が**複数走行の累積**を集計していた。
リクエスト数がクエリ数と噛み合わないことで気付いた（1.4M リクエストに対して 304k クエリ）。

計測前に**その走行で見るログすべて**を空にする。

```bash
sudo truncate -s 0 /var/log/mysql/mysql-slow.log
sudo truncate -s 0 /var/log/nginx/access.log
```

数字が計算に合わないときは、まず**集計範囲を疑う**。

## alp

nginx のデフォルト (combined) は `$request_time` を含まないので、**ログ形式の変更が必須**。

```nginx
log_format ltsv "time:$time_iso8601\tmethod:$request_method\turi:$uri"
                "\tstatus:$status\tsize:$body_bytes_sent"
                "\treqtime:$request_time\tapptime:$upstream_response_time";
access_log /var/log/nginx/access.log ltsv;
```

```bash
alp ltsv --file /var/log/nginx/access.log --sort sum --reverse \
  -m "/api/app/rides/[0-9A-Z]{26}/evaluation,/api/chair/rides/[0-9A-Z]{26}/status" \
  -o count,method,uri,avg,p99,sum
```

- `--sort sum` で見る。**avg が大きいものより、sum が大きいものを先に潰す**
  （今回 `nearby-chairs` は avg 0.83s と最悪だったが、呼び出し 123 回で全体の 3.5% しかなく、後回しが正解だった）
- `-m` で ID を含む URI をまとめないと、同じエンドポイントが散らばって埋もれる

## pt-query-digest

```bash
# 計測ごとに空にする。long_query_time = 0 だとベンチ 1 回で 50MB 増える
sudo truncate -s 0 /var/log/mysql/mysql-slow.log
# ベンチ実行
sudo pt-query-digest --limit 20 /var/log/mysql/mysql-slow.log
```

`long_query_time = 0` で全クエリを記録する。N+1 は「速いクエリが大量」なので、
閾値を上げると**見たいものが消える**。

出力は `Rows examined` と `Rows sent` の比を見る。
1 行返すのに数千行読んでいれば、インデックスが無いか効いていない。

### ヘッダの concurrency が効く

```
# Overall: 304.36k total, 124 unique, 3.14k QPS, 0.96x concurrency
```

**`concurrency` = 全クエリの実行時間の合計 ÷ ログの経過時間**、つまり平均で何本が
同時に実行中だったか。リトルの法則そのもので `QPS × 平均レイテンシ` と一致する。

読み方の要点は 2 つ。

- **vCPU 数を超えたら、超過分は待っている。** 2 vCPU で 5.9x なら 4 本は CPU 待ち
- **合計実行時間が「vCPU 数 × 経過時間」を超えたら、それは仕事ではなく待ち**。
  今回 2 vCPU・93 秒（=186 CPU 秒）に対して合計 548 秒だった。物理的にあり得ないので、
  待ちであると断定できる

同じクエリの単価が 0.3ms → 2.5ms に増えても、`Rows examined` が 1 行のままなら
**クエリは重くなっていない。行列が伸びただけ**。ここを混同すると、
既に最適なクエリをさらに最適化しようとして時間を失う。

## 計装は入れたままでよい

「slow log を全クエリ記録すると重いはず」と考えて、`slow_query_log = OFF` と
nginx の `access_log off` で測り直した。結果は **125.1s → 123.6s（1.2%）**。
`wa=0.4%` の通り**シーケンシャルな追記は安い**。

つまり**最後まで計装を入れたままチューニングして構わない**。
最終計測前に切る手間も要らないし、切る前提で設計を歪める必要もない。

## プロセス別 CPU

「CPU が足りない」まで分かっても、**アプリと DB のどちらかで打ち手が変わる**。
pprof を入れるかどうかもこれで決まる。

```bash
# ベンチ前後で差分を取る
for s in isuride-go mysql nginx alloy; do
  echo "$s $(systemctl show $s -p CPUUsageNSec --value)"
done
```

今回は **mysql 80.5s / アプリ 26.4s** で、DB がアプリの 3 倍だった。
この比率なら pprof を入れてアプリを削っても上限が低い。**先に DB を見る。**

**列挙し忘れたプロセスは計上されない。** 最初は `isuride-go` / `mysql` / `nginx` しか
見ておらず、合計が vmstat と合わなかった。`systemd-journald` を足したら 4.0s あった
（アプリが全リクエストをログに吐いており、`/var/log/journal` が 1.4GB まで育っていた）。

合計が vmstat の `us + sy` と合うかを必ず確認する。合わないなら見ていないプロセスがいる。

## us と sy の比

```
us=60.5%  sy=31.2%  id=3.5%  wa=0.4%
```

`sy` はカーネル空間、つまり**システムコール・TCP/IP スタック・割り込み**。
一般的な Web アプリなら `us : sy` は 4:1〜5:1 程度で、**2:1 は異常に高い**。

高いときの意味は「リクエスト数が多く、1 リクエストあたりの計算が軽い」。
今回は MySQL 接続が `127.0.0.1` の TCP で、1 クエリごとにループバックの
プロトコル処理が走っていた。5 本投げれば往復 10 回。

**ポーリング間隔を 3.5 倍にしてリクエストを 3 分の 1 に減らしても、`sy` の比率は動かなかった。**
これが 1 リクエストあたりの固定費だからで、総量が減っても比率は変わらない。
下げる方向は 2 つだけ。

- **システムコールの回数を減らす**（1 リクエストのクエリ本数を減らす、キャッシュする）
- **1 回を安くする**（MySQL 接続を unix ドメインソケットに、nginx の upstream に keepalive）

## vmstat の落とし穴

```bash
ssh host 'nohup sh -c "vmstat 1 160 > /tmp/vm.txt 2>&1" >/dev/null 2>&1 &'
```

`cmd1 && cmd2 && nohup vmstat &` と書くと **`&` がチェーン全体に掛かり**、
ssh 切断で巻き込まれて計測がずれる。vmstat の起動は独立したコマンドにする。
一度これで「idle 98%」というあり得ない値を取って、危うく誤った結論を出しかけた。

負荷走行中だけを見たいので、集計時に idle が低いサンプルに絞ると実態に近い。

## ボトルネックの切り分け

「遅い」の原因を推測で決めない。`SHOW GLOBAL STATUS` の**ベンチ前後の差分**で確定できる。

```sql
SHOW GLOBAL STATUS WHERE Variable_name IN (
  "Innodb_os_log_fsyncs",      -- redo log の fsync
  "Innodb_data_fsyncs",        -- データの fsync
  "Innodb_row_lock_waits",     -- ロック競合の回数
  "Innodb_row_lock_time",      -- ロック待ちの合計 ms
  "Innodb_buffer_pool_reads",  -- ディスクから読んだ回数
  "Innodb_buffer_pool_read_requests",
  "Com_update", "Com_commit"
);
```

読み方:

- `row_lock_waits` が小さい → **ロック競合ではない**
- `buffer_pool_reads / read_requests` が極小 → **メモリ不足ではない**
- `fsyncs` が更新回数と 1:1 → **fsync がボトルネック**

今回これで「UPDATE が遅いのはロック競合だろう」という**推測が明確に否定された**
（ロック待ちは合計 313ms、対して総時間 167 秒）。原因は fsync だった。
測らずにテーブル分割していたら、効果ゼロの大改修をしていた。
