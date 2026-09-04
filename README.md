# wdp

Go 实现的自动化部署工具：单二进制、零运行时依赖，
支持 **push 临时 agent**（默认）/ **常驻 agent** / **SSH** 三种远程通道，
Helm 风格 chart 打包与多环境精准部署。

## 特性

- **单静态二进制**：交叉编译开箱即用；SSH 通道目标机仅需 POSIX sh，agent 通道零依赖（`./build.sh` 默认内嵌 linux/amd64+darwin/arm64 自举载荷，跨平台 push 免配置；`--slim` 可关闭）
- **声明式 playbook**：条件/循环/block 容错/委托/hook，连接按主机复用免重复握手
- **部署策略**：金丝雀/滚动分批、健康门、失败自动回滚
- **chart 应用包**：三层 values 叠加 + JSON Schema 校验、子 chart 组合与
  `tasks_from` 多入口、按文件名扩展的生命周期相位（uninstall/status/update/download…）、artifact 在线/离线一体化制品分发
- **23 个内置模块** + chart 自带脚本模块（无需改源码即可扩展）
- **零风险预演**：全相位 `--check` / `--diff`，漂移巡检 `wdp drift`
- **安全默认**：mTLS 自建 CA、短周期证书、指纹准许名单吊销

## 快速开始

```sh
go build -trimpath -o wdp ./cmd/wdp

# 示例应用包开箱即用（node-exporter / docker）
wdp run ./examples/node-exporter --check --diff -i examples/inventory.yaml
```

## 文档

- 📖 **[Docs](docs/README.md)**：快速开始、inventory、playbook 全语法、
  内置模块手册、chart 应用包、CLI 参考、最佳实践与 FAQ
- 🧪 **[可运行示例](examples/)**：`node-exporter`、`docker` 等 chart 示例


## 许可证

MIT License，详见 [LICENSE](LICENSE)。
