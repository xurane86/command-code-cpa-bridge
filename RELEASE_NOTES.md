# Release Notes

## 0.2.1

- **修复控制台管理密钥无法生效**：`resources/index.html` 之前读取 `window.CCBridgeAuth` 对象，但注入的认证模块 `resources/auth.js` 只暴露了 `CCBridgeAuthHeaders / CCBridgeSaveAuth / CCBridgeClearAuth / CCBridgeRememberAuth` 这几个独立函数（与 ClinePassBridge 的 `cpa-auth.js` 同构），导致 `AUTH` 回退为空实现——请求从不携带 `Authorization` 头，保存密钥也无效，从而始终提示「管理密钥无效或已过期」。现改为直接复用这些独立函数。

## 0.2.0

- **页面交互对齐 ClinePassBridge**：主题桥接（跟随宿主 `data-theme`、独立默认纯白）、独立认证模块 `resources/auth.js`（占位注入 + 反代前缀/版本段适配）、设置页「授权与密钥」面板（管理/清除密钥、失败弹窗提示）。
- **思考强度映射**：在「设置」页配置 `客户端强度 → 上游强度` 规则，支持模型专属规则与默认规则，`omit` 可移除 `reasoning_effort` 字段。映射在请求发往上游前应用，流式与非流式一致。
- **模型测试**：在「模型」页对任意模型发起真实上游调用（可选凭据、提示词、流式），展示 HTTP 状态、协议、总耗时、首字耗时、tokens 与回复/错误。
- **项目翻新**：包结构改为自有命名空间（`translate/openai|anthropic|responses|common`、`registry`、`reasoning`、`fault`、`bridge`），清除全部上游 spec/FR/arch 注释，插件身份、模块与产物名统一为 `command-code-cpa-bridge`。

## 0.1.0

首个版本：把 CommandCode Go/GOAT/Pro/Max 订阅接入 CLIProxyAPI，并提供中文可视化控制台。

- CommandCode provider 引擎：OpenAI / Anthropic / Responses 三协议翻译，流式与非流式，推理内容保留，`reasoning_effort` 透传。
- 控制台页面：概览、凭据、模型、额度、日志、统计、设置。
- 页面内管理 CommandCode API Key（新增 / 编辑 / 删除 / 启停 / 优先级），凭据以 CPA auth 文件保存并复用 CPA 原生调度。
- 模型目录：发现、刷新、逐模型启用/禁用发布、客户端前缀配置、诊断信息。
- 额度监控：套餐、剩余 credits、5 小时 / 每周 / 月度窗口。
- 请求日志与统计：模型、协议、状态、耗时、首字耗时、tokens、错误，支持搜索筛选与清空。
- 设置：Base URL、目录刷新间隔、超时、最大响应字节、协议开关等，写入 data-dir 覆盖 YAML。

### 与直接配置上游 provider 的差异

- `api-keys` 可选，凭据可完全由页面管理。
- 新增 `data-dir` 与页面管理接口。
- 提供 `commandcode` provider key 与 auth 记录类型。
