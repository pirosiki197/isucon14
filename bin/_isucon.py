"""servers.env のホスト表と ssh 実行。bin/setup と bin/bench が使う。

servers.env はシェルのファイル (run_bench.sh も読むし、ISUCON の env.sh と同じ流儀)
なので、パースせず bash に読ませて環境変数として受け取る。
"""

from __future__ import annotations

import os
import subprocess
import sys
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
SSH_CONNECT_TIMEOUT = os.environ.get("SSH_CONNECT_TIMEOUT", "10")


def die(msg: str) -> None:
    print(f"error: {msg}", file=sys.stderr)
    raise SystemExit(1)


def warn(msg: str) -> None:
    print(f"warn:  {msg}", file=sys.stderr)


def load_env() -> dict[str, str]:
    path = REPO / "servers.env"
    if not path.exists():
        die("servers.env が無い (servers.env.example をコピーして作る)")
    out = subprocess.run(
        ["bash", "-c", "set -a; . ./servers.env; env -0"],
        cwd=REPO,
        capture_output=True,
        text=True,
        check=True,
    ).stdout
    return dict(kv.split("=", 1) for kv in out.split("\0") if "=" in kv)


@dataclass(frozen=True)
class Host:
    name: str
    addr: str
    private_ip: str
    roles: tuple[str, ...]

    def has_role(self, role: str) -> bool:
        return role == "*" or role in self.roles


def hosts(env: dict[str, str] | None = None) -> list[Host]:
    env = env if env is not None else load_env()
    names = env.get("HOSTS", "").split()
    if not names:
        die('servers.env に HOSTS を設定してください (例: HOSTS="s1 s2 s3")')
    out = []
    for name in names:
        p = name.upper()
        # role 未指定ならホスト名そのものを role とみなす (bin/config と同じ規則)
        roles = tuple(env.get(f"{p}_ROLES", "").split()) or (name,)
        out.append(
            Host(
                name=name,
                addr=env.get(f"{p}_HOST", ""),
                private_ip=env.get(f"{p}_PRIVATE_IP", ""),
                roles=roles,
            )
        )
    return out


def resolve(targets: list[str], env: dict[str, str] | None = None) -> list[Host]:
    """ホスト名または role を受けて対象ホストを返す。空なら全ホスト。"""
    all_hosts = hosts(env)
    if not targets:
        return all_hosts
    picked: list[Host] = []
    for t in targets:
        found = [h for h in all_hosts if h.name == t] or [h for h in all_hosts if h.has_role(t)]
        if not found:
            die(f"{t} というホストも role も無い (HOSTS={' '.join(h.name for h in all_hosts)})")
        for h in found:
            if h not in picked:
                picked.append(h)
    return picked


@dataclass
class Result:
    host: Host
    rc: int
    out: str

    @property
    def ok(self) -> bool:
        return self.rc == 0


def ssh(host: Host, script: str, user: str = "isucon") -> Result:
    """bash スクリプトを stdin から流す。引用の心配が要らないのでこの形にする。"""
    if not host.addr:
        return Result(host, 255, f"{host.name.upper()}_HOST が servers.env に無い")
    p = subprocess.run(
        [
            "ssh",
            "-o",
            f"ConnectTimeout={SSH_CONNECT_TIMEOUT}",
            f"{user}@{host.addr}",
            "bash -s",
        ],
        input=script,
        capture_output=True,
        text=True,
    )
    return Result(host, p.returncode, (p.stdout + p.stderr).strip())


def ssh_all(targets: list[Host], script: str, user: str = "isucon") -> list[Result]:
    """全ホストに並列で投げる。返る順番は targets の順を保つ。"""
    with ThreadPoolExecutor(max_workers=max(1, len(targets))) as pool:
        return list(pool.map(lambda h: ssh(h, script, user), targets))
