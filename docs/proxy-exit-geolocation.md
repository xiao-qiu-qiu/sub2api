# 代理出口与 IPv6 归属查询

内置探针按顺序使用 HTTPS `ipwho.is`、HTTPS `api64.ipify.org`。保留
`ip-api` 解析器以兼容已有自定义配置，但内置列表不再访问 `ip-api.com`。
旧配置若显式填写了该域名，需要同步替换 URL 和解析器。

归属服务使用以下免密钥端点：

```text
https://ipwho.is/{IP}?lang=zh-CN&fields=success,message,ip,city,region,country,country_code,timezone.id
```

`{IP}` 支持 IPv4 和 IPv6；省略时查询当前出口。要求 `success: true` 和合法
`ip`，将 `city`、`region`、`country`、`country_code`、`timezone.id` 分别映射到
`ProxyExitInfo` 的城市、地区、国家、国家码和时区。错误响应可能是 HTTP 429，
也可能是 HTTP 200 加 `success: false`，均按归属失败处理。

自定义 CFtrace/ipify 探针成功后，通过**同一代理**查询已确认的 IP，补全缺失归属。
归属请求最多 3 秒，受父 context 的更短截止时间约束；保留原探针 IP、国家码和延迟。
仅接受与已确认 IP 相同的查询结果。超时、限流、无效 JSON 或查询失败均保留
已成功的连通结果。若本轮已尝试过 ipwhois，不在 ipify 回退成功后重复查询。
连通探针本身沿用单次 10 秒超时和响应大小上限。

2026-11-01 核验：[官方文档](https://ipwhois.io/documentation)说明免密钥接口支持
IPv4/IPv6 和 HTTPS，免费额度为每客户端出口 IP 每天 1,000 次，允许商业使用，
不提供 SLA；超额返回 429 和 `Retry-After`。实查 `ipwho.is` 的 AAAA 包含
`2606:4700:10::ac42:af6b`、`2606:4700:10::6814:2c85`，本机强制 `curl -6`
查询 `2001:4860:4860::8888` 成功，返回国家、地区、城市及 IANA 时区。
`api.ip.sb` 虽有 AAAA，本次直连和本地代理访问均返回 403，未选用。
