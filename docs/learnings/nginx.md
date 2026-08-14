# nginx

初期状態は Debian の既定設定のままなので、触る余地はある。
ただし**どこが効くかは測ってからでないと分からない**。今回、効くと思った 2 つは
どちらも空振りだった。

## 測ってから決める

### 静的ファイルは件数が多くても問題にならなかった

アクセスログを見ると、`/index.html` とフロントのアセット計 9 ファイルが
**全リクエストの 52%（129,216 件）** を占めていた。件数だけ見ると本命に見える。

しかし所要時間の合計は **0.0 秒**（1 件あたり 0.5ms 未満）だった。
nginx が page cache から sendfile で返すので、実質タダ。**手を出す価値がない。**

```bash
# uri と reqtime を集計して API と静的で分ける
sudo cat /var/log/nginx/access.log | awk -F"\t" '
  {for(i=1;i<=NF;i++){if($i ~ /^uri:/) u=substr($i,5); if($i ~ /^reqtime:/) t=substr($i,9)}
   if(u ~ /^\/api\//){a++; at+=t} else {s++; st+=t}}
  END {printf "API: %d件 %.1fs / 静的: %d件 %.1fs\n", a, at, s, st}'
```

### TLS ハンドシェイクも問題ではなかった

`ssl_session_cache` が未設定だったので「毎回フルハンドシェイクなら重い」と考えたが、
**接続数を測ったら 2,304 本しかなかった**（総リクエスト 253,393 に対して
1 接続あたり 110 リクエスト）。ハンドシェイクは 2,304 回だけなので、
セッションキャッシュを入れても効果はない。

接続の再利用はログ形式に 2 つ足せば測れる。

```nginx
log_format ltsv "...\tconnreq:$connection_requests\tsslreuse:$ssl_session_reused..."
```

`$connection_requests` が 1 の行を数えれば接続数、平均を取れば 1 接続あたりの
リクエスト数になる。**これが十分大きいなら TLS 周りは触らなくてよい。**

## unix ドメインソケット

アプリとの間が loopback TCP なら、unix socket にすると TCP スタックを迂回できる。

```nginx
upstream app {
  server unix:/run/isuride/app.sock;
  keepalive 256;
  keepalive_requests 100000;
}

location /api/ {
  proxy_http_version 1.1;
  proxy_set_header Connection "";   # keepalive を効かせるのに必須
  proxy_pass http://app;
}
```

Go 側は listener を差し替えるだけ。**前回のソケットファイルが残っていると bind に
失敗する**ので消してから作り、nginx (www-data) が繋げるようにパーミッションを開ける。

```go
os.Remove(path)
ln, _ := net.Listen("unix", path)
os.Chmod(path, 0o777)
srv.Serve(ln)
```

**TCP の listener は残しておくとよい。** ローカルから `curl` で内部エンドポイントを
叩く仕組み（今回はマッチングの定期起動）があると、そちらが動かなくなる。

ソケットの置き場は systemd に作らせる。**`/run` は再起動で消える**ので、
手で `mkdir` すると再起動後に起動しなくなる。

```ini
[Service]
RuntimeDirectory=isuride     # /run/isuride を作り、終了時に消す
```

効果は `sy` が **40.2% → 33.7%**、idle が 14.2% → 23.1%。
ただしこのときスコアは動かなかった（律速が DB 側に移っていた。
[multi-server.md](multi-server.md) 参照）。**CPU が空いたことと
スコアが上がることは別**なので、両方を見る。

## worker_connections を上げるなら fd 上限も上げる

**既定の `worker_connections 768` は少ないが、単純に上げると壊れる。**

```
[alert] socket() failed (24: Too many open files) while connecting to upstream
[crit]  accept4() failed (24: Too many open files)
```

**1 接続でクライアント側と upstream 側の 2 fd を使う**ので、`worker_connections 8192`
なら 1 ワーカーあたり 16,384 以上必要。既定の fd 上限 1024 では全く足りない。
2 箇所セットで上げる。

```nginx
worker_processes auto;
worker_rlimit_nofile 65535;
events {
  worker_connections 8192;
  multi_accept on;
}
```

```ini
# /etc/systemd/system/nginx.service.d/limits.conf
[Service]
LimitNOFILE=65535
```

確認は実プロセスの limits を見る。

```bash
cat /proc/$(pgrep -f 'nginx: worker' | head -1)/limits | grep -i 'open files'
```

この失敗はアプリの 500 として現れた（`POST /api/chair/coordinate` が 500）。
**スコアだけ見ていたら「unix socket は効かなかった」と誤って結論していた。**
エラー件数を必ず一緒に見る。

## アクセスログは切らなくてよい

`access_log off` にして測ったが、CPU は 0.8 秒しか変わらなかった
（[profiling.md](profiling.md#計装は入れたままでよい)）。alp で解析できる価値のほうが
大きいので、**最後まで付けたままでよい**。ただし計測ごとに空にすること。
