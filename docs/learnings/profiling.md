# 計測

道具ごとに**答えられる問いが違う**。順番を守ると迷わない。

| 問い | 道具 |
|---|---|
| どのエンドポイントが遅いか | alp (nginx アクセスログ) |
| どのクエリが重いか | pt-query-digest (slow query log) |
| CPU を食っているのはアプリか DB か | `systemctl show -p CPUUsageNSec` の差分 |
| CPU か I/O か | vmstat の `us`/`sy`/`wa`/`id` |
| なぜそのクエリが遅いのか | `EXPLAIN` と `SHOW GLOBAL STATUS` の差分 |

## 使い分けの勘所

**digest だけ見ていると詰まる。** 単価の高いクエリがある間は digest で答えが出るが、
それを潰し切ると上位が「2ms のクエリが 3 万回」に変わる。こうなると
「1 リクエストが何回クエリを投げているか」が問題であり、
テーブル横断で集計する digest では N+1 が分解されて見えない。**そこで alp に切り替える。**

逆に alp だけでも足りない。「このエンドポイントが遅い」までしか分からず、
中で何が起きているかは digest が要る。**両方を突き合わせる。**

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
