#!/bin/bash

set -euo pipefail

ssh isucon@13.231.124.9 "./bench run . run --addr 172.31.33.90:443 --target https://isuride.xiv.isucon.net --payment-url http://172.31.38.72:12346 --payment-bind-port 12346"
