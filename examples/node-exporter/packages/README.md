# node-exporter chart 离线制品目录

离线模式（`--set online_install=false`）部署时，从本目录按目标架构取制品：

```
packages/
├── x86_64/node_exporter
└── aarch64/node_exporter
```

制作（控制机执行一次，产物不入库；aarch64 把 amd64 换成 arm64）：

    curl -LO https://github.com/prometheus/node_exporter/releases/download/v<ver>/node_exporter-<ver>.linux-amd64.tar.gz
    tar -zxof node_exporter-<ver>.linux-amd64.tar.gz --strip-components=1 -C packages/x86_64/ node_exporter-<ver>.linux-amd64/node_exporter

放置后 `wdp package test/lab/node-exporter` 打出的 tgz 自带离线制品，可在无外网环境部署。
