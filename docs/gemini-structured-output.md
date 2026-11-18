# Gemini Chat Completions 结构化输出

Gemini 平台账号通过 `/v1/chat/completions` 接收 OpenAI 兼容的 `response_format`。网关在 Gemini 请求构造完成后写入 `generationConfig`，保留中间协议转换前的原始 Schema。

| 客户端格式 | Gemini 请求 |
| --- | --- |
| 未指定、`null` 或 `{"type":"text"}` | 保持原有文本输出行为 |
| `{"type":"json_object"}` | `responseMimeType: "application/json"` |
| `{"type":"json_schema","json_schema":{"name":"result","schema":{...}}}` | 上述 MIME 类型，加原始 `responseJsonSchema` |

`json_schema.schema` 必须是 JSON 对象。Schema 属性顺序、数字字面量和 `additionalProperties` 等约束保留原样，不经过工具 Schema 清洗。流式和非流式使用同一映射；Code Assist 包装也保留原始 Schema 字节。

以下情况在发送上游之前返回 HTTP `400`、`invalid_request_error`：

- `response_format` 不是对象、缺少合法 `type`，或使用未知类型。
- `json_schema` 缺少对象类型的 `schema`。
- JSON 输出同时携带工具，或映射后的上游模型为图片输出模型。当前兼容路径拒绝这些组合。

如果排队心跳已提交 SSE 响应头，则发送 `invalid_request_error`（code 为 `invalid_response_format`）流内错误和 `[DONE]`，不调用上游。客户端应检查流式终态，不能仅凭 HTTP 200 判断成功。

具体模型和供应商对 JSON Schema 子集的支持以其能力为准。网关不会将约束改为 prompt，也不会因输出不符合业务要求而自动重试。客户端应在完整响应组装后校验 Schema 和业务结果，并处理拒绝、截断或取消。

范围仅为 Gemini 平台的 Chat Completions 入口；其他入口沿用现有行为，无新增配置或数据库迁移。

来源：[ranxi2001 PR #886](https://github.com/ranxi2001/sub2api/pull/886)、[Wei-Shaw PR #8662](https://github.com/Wei-Shaw/sub2api/pull/8662)。
