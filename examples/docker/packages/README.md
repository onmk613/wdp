# docker chart 离线制品目录

离线模式（`--set online_install=false`）部署时，从本目录按目标架构取制品：

```
packages/
├── x86_64/    （containerd, containerd-shim-runc-v2, ctr, docker,
│               docker-init, docker-proxy, dockerd, runc,
│               docker-compose, docker-buildx）
└── aarch64/   （同名清单）
```

制作（控制机执行一次，产物不入库）：

    curl -LO https://download.docker.com/linux/static/stable/x86_64/docker-<ver>.tgz
    tar -zxof docker-<ver>.tgz && cp docker/* packages/x86_64/
    curl -L -o packages/x86_64/docker-compose \
      https://github.com/docker/compose/releases/download/<cver>/docker-compose-linux-x86_64
    curl -L -o packages/x86_64/docker-buildx \
      https://github.com/docker/buildx/releases/download/<bver>/buildx-<bver>.linux-amd64

注意架构命名差异：compose 用 x86_64/aarch64，buildx 用 amd64/arm64（同在线下载 URL）。

放置后 `wdp package test/docker` 打出的 tgz 自带离线制品，可在无外网环境部署。
