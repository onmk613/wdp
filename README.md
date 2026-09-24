# wdp

Go 实现的自动化部署工具：单二进制、零运行时依赖，
支持 **常驻 agent**（默认，mTLS）/ **local** 两种执行通道，
Helm 风格 chart 打包与多环境精准部署。

> `web-console` 分支正在演进 Web 管理端：多节点纳管（token 脚本拉取 / SSH 推装两种引导）、远程执行、chart 版本化管理（主机信息入库 SQLite）。该分支已移除 push 通道；SSH 保留作 server 推装 agent 的引导通道（server→目标机单向可达的网络）。

## 特性

- **一份多架构 bin 目录**：`./build.sh` 缺省交叉编译全平台产物（linux/darwin/windows × amd64/arm64），服务端按目标机架构自动取对应文件下发——跨平台安装零配置；agent 通道目标机零依赖
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

# 从一个 playbook 开始（语法见 docs/05-playbook任务编排.md）
wdp run site.yaml --check --diff -i inventory.yaml
```

## 文档

- 📖 **[Docs](docs/README.md)**：快速开始、inventory、playbook 全语法、
  内置模块手册、chart 应用包、CLI 参考、最佳实践与 FAQ


## 许可证

MIT License，详见 [LICENSE](LICENSE)。
