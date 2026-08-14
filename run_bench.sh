#!/bin/bash

set -euo pipefail
cd "$(dirname "$0")"

# サーバーのアドレスはリポジトリに含めない。servers.env.example を参照。
if [ -f servers.env ]; then
	# shellcheck disable=SC1091
	. ./servers.env
fi

: "${BENCH_HOST:?servers.env に BENCH_HOST を設定してください (servers.env.example 参照)}"
: "${APP_PRIVATE_IP:?servers.env に APP_PRIVATE_IP を設定してください}"
: "${BENCH_PRIVATE_IP:?servers.env に BENCH_PRIVATE_IP を設定してください}"

# --payment-url はベンチサーバー自身の内部 IP を指す。アプリサーバーを指すと
# 決済モックに届かず evaluation が全部 500 になる。
ssh "isucon@${BENCH_HOST}" "./bench run . run \
	--addr ${APP_PRIVATE_IP}:443 \
	--target https://isuride.xiv.isucon.net \
	--payment-url http://${BENCH_PRIVATE_IP}:12346 \
	--payment-bind-port 12346"
