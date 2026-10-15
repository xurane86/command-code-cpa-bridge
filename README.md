# command-code-cpa-bridge

把 **CommandCode Go / GOAT / Pro / Max** 订阅接入 [CLIProxyAPI（CPA）](https://github.com/router-for-me/CLIProxyAPI) 的原生动态库插件，并提供**中文可视化控制台**用于配置与监控。

它自带一套完整的协议转换与调度内核（多协议翻译、推理保留、目录发现、CPA 原生 Key 池调度），并附带**中文可视化控制台**：**凭据、模型、额度、日志、设置全部在网页里完成**，无需手写 YAML 就能配置和排障。

> 单一 provider 命名空间：上游模型默认以**上游原名**（如 `deepseek/deepseek-v4.1-flash`）暴露，可在控制台逐条改成任意客户端别名（如 `deepseek-v4.1-flash`）；别名与上游原名都可调用。OpenAI / Anthropic / Responses 三种客户端协议统一翻译到上游 chat-completions。

---

## 功能

### 页面模块（控制台）

| 模块 | 能力 |
| --- | --- |
| **概览** | 插件版本、已发布模型数、目录刷新时间与错误、数据目录、发布白名单/屏蔽数、一键刷新目录 |
| **凭据** | 页面内新增 / 编辑 / 删除 CommandCode API Key，启停账号，设置优先级，展示账号邮箱与掩码密钥 |
| **模型** | 查看已发现模型（客户端 ID、上游 ID、协议、上下文、是否支持思考），逐模型启用/禁用发布，**逐条编辑客户端模型名（别名映射）**，手动刷新目录，查看目录诊断，并**对任意模型发起真实测试** |
| **额度** | 每把 Key 的套餐、剩余 credits、5 小时 / 每周 / 月度窗口（**剩余量、剩余百分比**、重置时间），按卡片刷新 |
| **日志** | 请求日志（模型、来源协议、状态、耗时、首字耗时、tokens、错误）、搜索与状态筛选、分页、清空 |
| **统计** | 总请求、成功/失败、输入/输出/缓存 tokens、平均耗时，以及按模型聚合 |
| **设置** | Base URL、Catalog URL、目录刷新间隔、请求超时、最大响应字节、客户端模型前缀、三种协议开关、是否允许 http，以及**思考强度映射** |

页面各项设置会写入 `data-dir/settings.json`，覆盖 YAML 中的同名配置；凭据以 CPA auth 文件形式保存，继续复用 CPA 原生的调度、重试、冷却与统计。

### Provider 引擎

- **多协议客户端翻译**：OpenAI Chat Completions、Anthropic Messages、OpenAI Responses 请求统一翻译为上游 chat-completions，并把响应（含流式）转换回客户端协议。
- **推理保留**：上游 `reasoning` / `reasoning_details[].text` / `reasoning_content` 归一化后分别还原为 Claude `thinking` 块、Responses `reasoning` 项、OpenAI `reasoning_content`。
- **动态目录发现**：默认 `{base-url}/models`，支持本地回退与诊断（被排除模型及原因）。
- **CPA 原生 Key 池**：每个凭据注册为一条 auth 记录，由 CPA 统一负责轮换、限流、冷却与会话亲和。
- **reasoning_effort 透传与映射**：CommandCode 未声明能力表时，客户端 effort 原样转发（`xhigh` / `max` 可用）；也可在页面配置「思考强度映射」按模型或全局改写 effort。

### 模型测试

在「模型」页对任意模型点击「测试」，选择一个凭据（或自动选择）、填写提示词、可切换流式，即会**发起一次真实的上游调用**并展示：

- 结果、HTTP 状态、所用协议与凭据；
- 总耗时与（流式时的）首字耗时；
- tokens（入 / 出 / 缓存）；
- 回复内容或上游错误。

测试会消耗所选账号的少量额度。

### 思考强度映射

在「设置 → 思考强度映射」中配置 `客户端强度 → 上游强度` 规则：

- 模型留空为**默认规则**，指定模型时该模型专属规则优先；
- 上游强度填 `omit` 表示**移除** `reasoning_effort` 字段；
- 未命中任何规则时保持原样透传。

映射在请求发往上游前应用，流式与非流式一致。

### 认证方式

- **凭据 = 手动 API Key**：CommandCode 通过 API Key 接入，**不提供交互式 OAuth**；控制台与 CPA 单凭据页都不会走授权登录流程。`auth.login.*` 明确返回「请导入 API Key」，`auth.refresh` 对不过期的 Key 设为远期刷新。
- **控制台登录 = 复用管理密钥**：页面沿用 CPA 面板记住的管理员密钥（本地未记住时提示输入），这是「页面登录」而非「上游 OAuth」。密钥被拒（401/403）时会清除对应存储并重新提示。
- 与 ClinePassBridge 的做法一致：都是「API Key 导入 + 复用管理端登录」。

### 控制台交互（对齐 ClinePassBridge）

- **主题桥接**：嵌入 CPA 时跟随宿主主题（监听父级 `data-theme` 变化并即时切换），独立打开时沿用面板保存的 `cli-proxy-theme`，默认纯白。
- **独立认证模块**：`resources/auth.js` 通过页面 `/*__AUTH_MODULE__*/` 占位注入，暴露 `CCBridgeAuthHeaders / SaveAuth / ClearAuth / RememberAuth`，与 CPB 的 `cpa-auth.js` 同构；自动适配反向代理前缀与 `/v0`、`/v8` 版本段，密钥被拒时只清除被拒的那份存储。
- **「授权与密钥」面板**：设置页展示授权状态，并提供「管理密钥」「清除密钥」；授权失败弹出对话框提示，而非整页空白。

---

## 安装

1. 让 CPA 启用插件：

   ```yaml
   plugins:
     enabled: true
     dir: /data/plugins
   ```

2. 将与本机系统/架构对应的产物放入 `<cpa>/plugins/<os>/<arch>/`，文件名保持 `command-code-cpa-bridge`：

   | 平台 | 文件 |
   | --- | --- |
   | Linux amd64 / arm64 | `command-code-cpa-bridge.so` |
   | macOS amd64 / arm64 | `command-code-cpa-bridge.dylib` |
   | Windows amd64 / arm64 | `command-code-cpa-bridge.dll` |

   从 [Releases](../../releases) 下载对应 ZIP 解压，或从源码构建（见下）。

3. **不需要在 CPA 里声明这个插件**。不写 `plugins.configs.command-code-cpa-bridge` 也能完整使用：所有设置都在控制台页面里改，并落到 `data-dir`（默认 `plugins/command-code-cpa-bridge-data`，相对 CPA 进程工作目录）。

   仅当你要覆盖默认值时才需要写，例如：

   ```yaml
   plugins:
     enabled: true
     dir: /data/plugins
     configs:
       command-code-cpa-bridge:
         # 可选：覆盖插件状态目录（默认 plugins/command-code-cpa-bridge-data）
         data-dir: "/data/plugins/command-code-cpa-bridge-data"
         # 可选：上游地址（也可在页面「设置」里改）
         base-url: "https://api.commandcode.ai/provider/v1"
         # 可选：直接写死静态密钥（否则在页面「凭据」里添加），支持 ${ENV_VAR}
         api-keys:
           - value: "${COMMANDCODE_API_KEY}"
   ```

   其余都有默认值：客户端模型名默认用上游原名（可选 `model-prefix` 前缀，默认关闭）、三种协议全开、目录刷新 15m、请求超时 5m。

4. 打开控制台：`/v0/resource/plugins/command-code-cpa-bridge/console`

   首次进入若提示密钥，请填 **CPA 管理密钥**。勾选「在此浏览器记住」后，下次可自动进入；已登录 CPA 管理面板时也会尝试复用其登录信息。

5. 在「凭据」页添加至少一个 CommandCode API Key，然后在「模型」页点击「刷新目录」。

---

## 配置项

| 配置项 | 类型 | 默认 | 说明 |
| --- | --- | --- | --- |
| `data-dir` | string | `plugins/command-code-cpa-bridge-data` | 页面设置与请求日志目录（也接受 `data_dir` 写法）；相对路径基于 CPA 进程工作目录 |
| `base-url` | string | `https://api.commandcode.ai/provider/v1` | 上游地址（https；http 需 `allow-http`） |
| `catalog-url` | string | `{base-url}/models` | 目录发现地址 |
| `model-prefix.enabled` / `.value` | bool / string | `false` / `commandcode` | 可选的客户端模型名前缀；默认关闭（客户端名即上游原名），逐条别名在「模型管理」页编辑并持久化到 `settings.json` |
| `api-keys[].value` | string | *(空)* | 静态密钥，支持 `${ENV_VAR}`；留空则全由页面「凭据」管理 |
| `catalog.refresh-interval` | duration | `15m` | 目录轮询间隔（最小 `1m`） |
| `catalog.stale-while-unavailable` | bool | `true` | 刷新失败时继续提供上次快照 |
| `protocols.*` | bool | `true` | 协议开关；关闭即从目录排除对应路由的模型 |
| `request-timeout` | duration | `5m` | 上游请求超时 |
| `max-response-bytes` | int | `67108864` | 非流式响应体上限 |
| `allow-http` | bool | `false` | 允许 http:// 上游（仅本地测试） |

**整张表都是可选的**：唯一的硬性前提是宿主开启插件系统（`plugins.enabled: true` 与 `plugins.dir`）。页面「设置」中保存的值会作为覆盖层优先生效（`base-url` 变更时自动重算 `catalog-url`）。

---

## 调用示例

客户端使用 CPA 的地址与 API Key：

```bash
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer <CPA_API_KEY>" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"你好"}]}'
```

- 模型 `model` 取自「模型」页的**客户端模型 ID**。
- Anthropic 客户端用 `/v1/messages`，Responses 客户端用 `/v1/responses`，插件自动转换。
- 关闭某协议后，对应路由的模型不会出现在 `/v1/models`。

---

## 数据与安全

- **凭据**：保存在 CPA 的 auth 目录（`*_key_<sha256>.json`），密钥明文仅存于文件与上游请求头；页面只展示掩码。
- **设置 / 日志**：保存在 `data-dir`（`settings.json`、`requests.json`）。
- 容器部署时请持久化 CPA 的 auth 目录与插件的 `data-dir`。

---

## 从源码构建

需要 Go 1.26+ 与 CGO 工具链（gcc/clang）。

```bash
# Linux
CGO_ENABLED=1 go build -buildmode=c-shared -o command-code-cpa-bridge.so .
# macOS
CGO_ENABLED=1 go build -buildmode=c-shared -o command-code-cpa-bridge.dylib .
# Windows (mingw-w64)
set CGO_ENABLED=1
go build -buildmode=c-shared -o command-code-cpa-bridge.dll .
```

测试：

```bash
go test ./...
go vet ./...
```

---

## 项目结构

```
main.go                        CGO 胶水：导出 cliproxy_plugin_* 并转发到 internal/bridge
internal/
  bridge/                      插件核心
    plugin.go                  方法分发、生命周期、目录装载、Key 解析
    management.go              控制台资源页 + Management API
    credentials.go             凭据 CRUD（基于 CPA auth 文件）
    settings.go                页面设置持久化与发布过滤
    reasoning_mapping.go       思考强度映射执行
    modeltest.go               模型测试
    telemetry.go               请求日志与统计
    quota.go                   额度查询
    hostbridge.go              宿主回调桥（HTTP / 日志 / auth / 流）
    executor.go                请求执行（流式 / 非流式）
    auth.go / account_email.go 认证解析与账号邮箱同步
  translate/
    openai/                    OpenAI Chat Completions 适配
    anthropic/                 Anthropic Messages 适配
    responses/                 OpenAI Responses 适配
    common/                    共享的翻译内核与推理处理
  registry/                    上游目录发现与路由
  reasoning/                   思考预算与强度能力
  fault/                       错误分类与信封
  config/                      配置加载、校验与页面覆盖层
resources/index.html           中文单页控制台（含主题桥接与授权面板）
resources/auth.js              控制台认证模块（占位注入，对齐 CPB 的 cpa-auth.js）
```

## 许可与来源

本项目为独立项目，采用 MIT 许可。

协议转换与调度内核经改写，源自 [commandcode-go-cliproxyapi](https://github.com/mczhoucn/commandcode-go-cliproxyapi)（MIT）；控制台的「页面插件」形态参考 [ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge)（MIT）。按 MIT 许可要求保留原始版权声明，详见 [LICENSE](LICENSE) 与 [NOTICE](NOTICE)。
