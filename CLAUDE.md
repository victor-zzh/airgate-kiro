# airgate-kiro — Claude 开发指南

> 叠加在 monorepo 根 `../CLAUDE.md` 之上。完整流程见共享 skill **`develop-plugin`**；接口契约见 `../airgate-sdk/CLAUDE.md`。

- **插件身份**：id `gateway-kiro`，type `gateway`，上游 = Kiro（AWS CodeWhisperer）。
- 实现 `sdk.GatewayPlugin`：声明 models/routes/account fields，`Forward()` 转发并回 `ForwardOutcome`。

## 🚫 红线

- 只依赖 `airgate-sdk`，禁止 import core 内部；用 core 能力经 `Host.Invoke`/`InvokeStream`。
- `plugin.yaml` 由 `make manifest` 生成，不可手改。
- 前端单 `index.js` → `web/dist/index.js`，用 `@doudou-start/airgate-theme`。

## 混合现状（过渡态）

本仓当前混合了网关 + provider + UI 三层职责：

- **Protocol 转换**：`converter.go`（Anthropic ↔ Kiro 协议转换）
- **Provider 职责**（应归 provider 插件）：AWS EventStream（`eventstream.go`）、OAuth/device auth（`oauth.go`/`token.go`）、web-search（`websearch.go`）、machine ID（`machineid.go`）
- **UI 职责**（应归 UI 插件）：3 个账号 widget

> 新增/改动须按职责归位，勿加深混合。详见 `../airgate-core/docs/architecture/current/plugins.md`。

## 命令

`make dev`（独立调试）· `make manifest` · `make build` · `make ci` · `make release`
