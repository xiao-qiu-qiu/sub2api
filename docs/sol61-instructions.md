# GPT-6.1 Sol 默认指令

GPT-6.1 Sol 在空 `instructions` 时不再自动补入 Codex 编程助手模板。客户端提供的非空指令继续保留；需要编程助手行为时，可以显式发送自己的指令。

普通 Chat Completions 转为 Responses 并通过 API Key 转发时，客户端的 system 文本会提升至顶层 `instructions`，纯文本消息提升后不再重复放入 input。JSON object 模式和包含图片等非文本内容的 system 消息保留既有兼容处理；developer 消息仍保留在 input 中。没有 system 时使用空指令字段。

该变更适用于 GPT-6.1 Sol 及支持的推理强度、compact 别名。GPT-6.2、其他模型的默认指令、客户端显式提示词、BPS 自定义客户端指令和原生协议路径保持原有行为。供 Codex 客户端使用的模型能力描述也继续保留。

第三方 Responses 网关仍可能自行填充空 `instructions`。核对用量时，总输入 Tokens 应包含缓存读取，不能只比较非缓存 `input_tokens`。

无新增配置或数据库迁移。来源：[ranxi2001 PR #898](https://github.com/ranxi2001/sub2api/pull/898)。
