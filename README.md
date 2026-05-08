<div align="center">
  <h1>AirGate Kiro</h1>

  <p><strong>Kiro (AWS CodeWhisperer) 反代网关插件</strong></p>

  <p>
    <a href="https://github.com/DouDOU-start/airgate-kiro/releases"><img src="https://img.shields.io/github/v/release/DouDOU-start/airgate-kiro?style=flat-square" alt="release" /></a>
    <a href="https://github.com/DouDOU-start/airgate-kiro/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/DouDOU-start/airgate-kiro/ci.yml?branch=master&style=flat-square&label=CI" alt="ci" /></a>
    <a href="https://github.com/DouDOU-start/airgate-kiro/blob/master/LICENSE"><img src="https://img.shields.io/github/license/DouDOU-start/airgate-kiro?style=flat-square" alt="license" /></a>
    <img src="https://img.shields.io/badge/Go-1.25-00ADD8?style=flat-square&logo=go" alt="go" />
    <img src="https://img.shields.io/badge/React-19-61DAFB?style=flat-square&logo=react" alt="react" />
  </p>
</div>

---

[airgate-core](https://github.com/DouDOU-start/airgate-core) 的 Kiro 网关插件，基于 [airgate-sdk](https://github.com/DouDOU-start/airgate-sdk) 实现。它将 Kiro (AWS CodeWhisperer) 服务以 Anthropic Messages API 兼容接口暴露，让 Claude Code 等客户端可以直接通过 Kiro 账号调用。

核心能力：**用 Kiro 免费账号池服务 Anthropic Messages API 客户端**，支持 OAuth 浏览器授权、BuilderID 设备授权、IdC (AWS SSO) 手动导入、API Key 四种账号接入方式。

## ✨ 核心特性

- **🔑 三种账号类型** — `oauth`（Google/GitHub 社交登录）、`idc`（AWS BuilderID / SSO）、`api_key`（Kiro API Key）同池调度
- **🔄 BuilderID 设备授权** — BuilderID 用户无需手动提取凭证，OAuth 流程自动检测并切换为 AWS SSO Device Authorization Grant，浏览器验证即完成
- **🌐 Anthropic Messages API** — 完整支持 `/v1/messages` 流式/非流式、`/v1/messages/count_tokens`、`/v1/models`
- **🔁 自动 Token 刷新** — Social OAuth 与 IdC 两种刷新路径，token 过期前自动续期
- **📡 AWS Event Stream** — Kiro 使用 AWS Event Stream 二进制协议，插件内部完成协议转换为标准 Anthropic SSE
- **💼 完整账号 Widget** — 自带前端账号表单（OAuth 引导、设备授权验证码、IdC 字段、API Key），由 core 自动嵌入管理后台
- **📦 一键发版** — git tag 触发 release workflow，矩阵构建 4 个平台二进制

## 🧩 接入位置

```text
                  ┌──────────────────────────────────────┐
                  │           AirGate Core               │
                  │   (账号 / 调度 / 计费 / 管理后台)     │
                  └────────────┬─────────────────────────┘
                               │ go-plugin (gRPC)
                               ▼
                  ┌──────────────────────────────────────┐
                  │       airgate-kiro (本仓库)           │
                  │                                      │
                  │   ┌──────────┐  ┌───────────────┐    │
                  │   │  oauth   │  │   idc / key   │    │
                  │   │(Social/  │  │  (BuilderID/  │    │
                  │   │ Device)  │  │   API Key)    │    │
                  │   └────┬─────┘  └──────┬────────┘    │
                  │        └───────────────┤             │
                  │     ┌──────────────────▼──────────┐  │
                  │     │   Anthropic → Kiro 协议转换  │  │
                  │     │  (Messages → Event Stream)  │  │
                  │     └──────────────┬──────────────┘  │
                  └────────────────────┼─────────────────┘
                                       ▼
                               Kiro / AWS Q API
```

## 🔑 账号类型

| Key | 标签 | 凭证 | 适用场景 |
|---|---|---|---|
| `oauth` | Social OAuth | `refresh_token`（授权后自动填充）| Google / GitHub 登录的 Kiro 账号 |
| `idc` | IdC (AWS SSO) | `refresh_token` + `client_id` + `client_secret`（BuilderID 自动获取，或手动填写）| AWS BuilderID / 企业 SSO 账号 |
| `api_key` | Kiro API Key | `kiro_api_key` | Kiro API Key 直连 |

### BuilderID 授权流程

1. 点击"生成授权链接" → 打开 Kiro 门户 → 选 BuilderID 登录
2. 复制回调 URL 粘贴回来 → 插件自动检测 BuilderID，注册 OIDC 客户端
3. 界面显示验证链接和验证码 → 在浏览器中打开验证链接完成授权
4. 点击"我已完成授权" → 自动获取 token 并填充凭证

## 🦠 路由

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/messages` | Anthropic Messages API |
| POST | `/v1/messages/count_tokens` | Token 计数 |
| GET  | `/v1/models` | 模型列表 |

## 🛠 技术栈

| 层 | 技术 |
|---|---|
| 后端 | Go 1.25 · gRPC · gjson/sjson · AWS Event Stream 二进制协议 |
| 前端 | React 19 · Vite · TypeScript（账号表单 Widget） |
| 插件协议 | hashicorp/go-plugin (gRPC) |
| 认证 | Kiro OAuth PKCE · AWS SSO OIDC Device Authorization Grant |
| 发布 | GitHub Actions · 矩阵构建 4 平台二进制 · GitHub Release |

## 🚀 安装与开发

### 方式 1：安装到 core（推荐）

打开 core 管理后台 → **插件管理** → 三种方式任选：

```text
1. 插件市场 → 点击「安装」               （从 GitHub Release 自动拉取）
2. 上传安装 → 拖入二进制文件              （适合内部环境）
3. GitHub 安装 → 输入 DouDOU-start/airgate-kiro
```

### 方式 2：源码运行（开发）

需要 Go 1.25+、Node 22+，以及兄弟目录 `airgate-sdk` 与 `airgate-core`：

```bash
git clone https://github.com/DouDOU-start/airgate-sdk.git
git clone https://github.com/DouDOU-start/airgate-core.git
git clone https://github.com/DouDOU-start/airgate-kiro.git
```

把本插件以 dev 模式挂到 core：

```yaml
# airgate-core/backend/config.yaml
plugins:
  dev:
    - name: gateway-kiro
      path: /absolute/path/to/airgate-kiro/backend
```

然后在 `airgate-core` 目录运行 `make dev`。

## 🏗 项目结构

```text
airgate-kiro/
├── backend/                              # Go 后端（插件主体）
│   ├── main.go                           # gRPC 插件入口
│   └── internal/gateway/
│       ├── gateway.go                    # GatewayPlugin 接口实现
│       ├── metadata.go                   # 插件元信息（运行时单源）
│       ├── forward.go                    # 转发分发
│       ├── converter.go                  # Anthropic → Kiro 协议转换
│       ├── eventstream.go                # AWS Event Stream 二进制解析
│       ├── stream.go                     # 响应流处理
│       ├── oauth.go                      # OAuth / BuilderID 设备授权
│       ├── token.go                      # Token 管理与刷新
│       ├── models.go                     # 模型定义
│       └── assets.go                     # 前端资源 embed
├── web/                                  # 前端（账号表单 Widget）
│   └── src/components/AccountForm.tsx
├── .github/workflows/
│   ├── ci.yml                            # push/PR 触发 CI
│   └── release.yml                       # v* tag 触发矩阵构建
├── plugin.yaml                           # 插件清单
└── LICENSE
```

## 📦 发版

```bash
git tag v1.0.0
git push origin v1.0.0
```

`release.yml` 会自动矩阵构建 4 个平台二进制（linux/darwin × amd64/arm64），通过 ldflags 注入版本号，上传到 GitHub Release。

## 🤝 相关仓库

- 主仓库: [airgate-core](https://github.com/DouDOU-start/airgate-core)
- 插件 SDK: [airgate-sdk](https://github.com/DouDOU-start/airgate-sdk)
- OpenAI 插件: [airgate-openai](https://github.com/DouDOU-start/airgate-openai)

## 📜 License

MIT — 详见 [LICENSE](LICENSE)。
