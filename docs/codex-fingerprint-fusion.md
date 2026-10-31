# Codex 指纹与请求外观设置

本功能从 `KlN-4096/sub2api` 的 `klno` 分支（`bdff1129ffbd22947612f83cd09d23bf3fa47f5d`）选择性移植，保留本项目的连接授权隔离、嵌套 WebSocket 元数据、数字精度、票据和代理生命周期。没有引入该分支的 gwpool 系统或数据库迁移。

## 使用入口与默认值

在 OpenAI OAuth / setup-token 账号的创建、编辑面板，展开“高级指纹设置”。批量修改必须勾选“应用”才会写入。影子账号的身份配置来自凭据账号，应在母账号设置。

存量账号继续使用 v1 身份方案，客户端预设沿用已有 UA 和版本同步逻辑，所有新增请求外观开关默认关闭。保存其他账号设置不会自动启用这些功能。`codex_fingerprint_mode` 的 off / device / session / full 含义保持原有规则；建议从 device 开始，保留不同客户端的独立会话。

## v2 身份方案

显式设置 `extra.codex_identity_scheme = "v2"`，启用以下改进：

- 账号命名空间派生后，保持原本相等的 session / 根 thread / 默认 cache key 之间的关系。
- 保留 UUIDv7 的版本和时间戳、`<thread>:<window_number>` 及 `<source>:<parent_thread>` 的复合结构。
- 同时处理顶层与 `response.client_metadata`，同步头部和内嵌 turn metadata，并隔离 parent / root / context-window 身份。
- HTTP、透传和原生 WebSocket 复用同一份派生结果；compact 保留独立设备头，显式 cache key 按账号隔离。
- 模型映射后，对齐已存在的 turn metadata model；不会强行让 reasoning effort 与请求体相等。

v1 → v2 会切换派生身份，建议在新会话开始前切换。同一会话运行期间不要反复切换。UUIDv7 的创建时间仍保留；v2 的目标是协议关系一致，不是抹去所有请求关联。

## 客户端预设

`extra.codex_client_profile` 可显式选择 CLI / Desktop，以及 Windows / macOS / Linux。UA、originator 和 core version 同源生成；`ForceCodexCLI` 继续拥有原有优先级。未配置预设时不会自动切换为 Desktop。

- `versionMode: "latest"`：core version 跟随现有后台设置及自动同步链。
- `versionMode: "fixed"`：必须指定合法的 `codexVersion`。
- Desktop 必须另填真实的 `desktopVersion`，不以 core version 冒充应用版本。
- `cliEntry: "exec"` 仅用于 CLI；可选填写系统版本、架构和终端。

不额外下载客户端制品，也不进行系统版本或出口地理位置探测。

## 独立请求外观开关

以下键放在 `extra.codex_wire_options` 对象中，只有布尔值 `true` 才启用，与 v2 方案相互独立。

| 键 | 行为与边界 |
| --- | --- |
| `body_order` | 按已知 Codex 结构重排 HTTP Responses、compact 和 WS response.create 的顶层字段；保留值的原始字节，未知字段保留原相对顺序。 |
| `request_compression` | 仅原生 HTTP `/responses` 请求使用 zstd；compact、搜索、WS 不压缩。在底层完成路由、票据和出口时区处理后才转换为传输字节，重试可重新读取相同内容。插件继续接收明文 JSON，由插件负责自身编码。 |
| `environment_timezone` | 使用 `extra.codex_wire_timezone` 指定的 IANA 时区，仅改写有创建时间与内容分类证据的环境消息。日期按该消息的创建时间换算，不改普通提问、路径、shell 或工具输出。 |
| `web_search_location` | 使用显式的 `extra.codex_wire_user_location` 改写已提供的搜索位置。需要 city / region / country / timezone 四项完整；无有效配置时保留用户原位置。会改变搜索地域。 |
| `settings_user` | 主请求已发送后，异步补发只读 `settings/user` GET。按凭据 / 线程去重 2 小时，超时 10 秒；存储与异步任务有上限。不是同步 preflight，响应不用于推理与计费。 |
| `guardian_credits` | 为符合条件的普通 ChatGPT 请求设置 `guardian_credits_requested` 标记；PAT、Agent Identity 和 Guardian 评审自身请求排除。涉及上游 credits 请求语义，按需开启。 |
| `cookie_replay` | 仅缓存 ChatGPT HTTPS/WSS 主机的 Cloudflare / 路由 Cookie，按凭据和出口隔离；已有 Cookie、受管授权连接和绑定票据优先。数据只保存在进程内。 |

绑定 Harvest 身份的请求继续遵守原票据身份，新增外观处理不覆盖它。环境、位置、辅助 GET 和 Cookie 功能均不会随设备收敛自动启用。

配置示例：

```json
{
  "codex_fingerprint_mode": "device",
  "codex_identity_scheme": "v2",
  "codex_client_profile": {
    "client": "cli",
    "platform": "windows",
    "versionMode": "latest"
  },
  "codex_wire_options": {
    "body_order": true,
    "request_compression": false,
    "environment_timezone": false,
    "web_search_location": false,
    "settings_user": false,
    "guardian_credits": false,
    "cookie_replay": false
  }
}
```

## TLS 缓存修复与验证

Anthropic TLS 模板缓存键新增账号和完整 Profile 摘要。相同账号、相同模板可复用连接；同名模板修改参数后会使用新 transport，proxy 隔离模式也不会共享不同账号的 TLS 人格。原有代理超时、取消及 HTTPS proxy 回退保持既有实现。

本次改进覆盖应用层身份及请求编码；Codex 仍沿用已配置的 Go HTTP/WS 或插件传输栈。它没有增加一套完整的 Rust / reqwest TLS、HTTP 头顺序或 HTTP/2 SETTINGS 仿真。

上线后先用少量账号、固定模型和出口验收：确认普通 Responses、compact、WS 多轮和换账号可用，再逐个启用编码或辅助行为；观察实际回答、错误、限流和延迟。代码与离线测试不能证明封禁率或模型质量一定改善。
