# IP 与账号回收站

IP 管理和账号管理工具栏均提供“回收站”入口。列表支持搜索、分页、恢复及彻底删除；彻底删除前会再次确认。

## 删除与恢复

- IP 的单条和批量删除进入回收站，原有“正在被账号使用时禁止删除”的规则继续生效。恢复保留原 ID、地址、认证信息、状态和到期配置。Mihomo 静态代理同时恢复原节点和监听端口；节点或端口冲突时返回具体错误。
- 账号删除沿用原有软删除、影子账号级联清理和定时测试清理流程，并在同一事务中存档分组关联与已有登录凭据。账号本身的协议凭据和配置保留在原记录中。
- 账号恢复使用原 ID，恢复仍存在的分组及其优先级、模型限制，并恢复存档的登录配置。恢复不触发登录或测试，也不恢复已经清理的定时测试。影子账号需先恢复母账号。
- 恢复保留原代理与登录代理设置；原 IP 仍在回收站时，请先恢复 IP。
- 彻底删除账号会清理凭据与档案。已有消费明细的账号保留最小账务记录，移出回收站且不能再恢复；没有消费明细时物理删除账号行。
- IP 或账号在回收站期间不出现在正常列表与调度候选中。恢复和彻底删除接口均只接受已删除的记录。
- 回收站列表只展示登录邮箱与是否已存档，不返回密码、TOTP、OTP URL 或 Token。登录秘密沿用现有密文存储。
- 功能上线前已软删除的记录也会出现在回收站；历史删除时未存档的分组配置无法补回。

### 未完成的 2FA 轮换

账号仍可正常移入回收站。尚处于 `queued/running` 的 `rotate` 任务会失败终止；已进入 `enrolling/prepared/activating/verifying/uncertain` 的轮换，以及尚未完成的 `verify_new/verify_old` 验证动作，转为 `uncertain`，保留原有候选密钥、旧密钥和密码密文，清除 worker 身份以隔离旧回调。未决轮换继续保留任务占位，避免被新的登录或轮换覆盖。

恢复账号不会自动登录、重新轮换或验证。系统锁定并核对原登录配置版本、身份和归档密文，确认它们仍与未决轮换的原密码、旧 TOTP 密文一致后，将该轮换的配置版本衔接到恢复后的登录配置时间。恢复后可在原 2FA 入口导出候选密钥、补回本地日志中的候选密钥，或手动验证新/旧密钥；旧 worker 不能提交结果。

若原配置版本、身份或密文不匹配，恢复返回 `409 ACCOUNT_TRASH_TOTP_CONFIG_CHANGED`，账号与候选密钥继续保留在回收站，需核对原登录配置后再恢复。彻底删除仍会擦除这些轮换和登录凭据，包括已有消费明细、仅保留账务记录的账号。

## 管理接口

以下路径均以 `/api/v1/admin` 为前缀，沿用管理员认证和审计；回收站仅向管理员开放。

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/proxies/trash` | 分页查询 IP 回收站，可传 `search` 搜名称或地址 |
| POST | `/proxies/:id/restore` | 恢复 IP |
| DELETE | `/proxies/:id/permanent` | 彻底删除回收站内的 IP |
| GET | `/accounts/trash` | 分页查询账号回收站，可传 `search` 搜名称或邮箱 |
| POST | `/accounts/:id/restore` | 恢复原账号与已存档的登录凭据 |
| DELETE | `/accounts/:id/permanent` | 彻底删除回收站内的账号及凭据档案 |

列表使用现有 `page`、`page_size` 参数和标准分页响应。IP 仍被账号、备用代理或登录配置（包括回收站档案）引用时，需要先处理引用才能彻底删除。

## 更新与人工验收

后端启动迁移包含 `323_account_trash.sql`，前端与后端应一起更新。

可用一个临时 IP 和一个临时账号验收：删除后在正常列表消失、回收站可搜索；点击恢复后原 ID 与配置仍在；再次删除并在回收站彻底删除后，该条目不再出现。带已保存登录凭据的账号应显示“已存档”，恢复后可在原登录配置入口核对凭据配置状态。

## 本轮验证

在 `frontend` 目录执行，均通过：

```powershell
node node_modules/vue-tsc/bin/vue-tsc.js --noEmit
node node_modules/eslint/bin/eslint.js src/components/admin/RecycleBinDialog.vue src/views/admin/AccountsView.vue src/views/admin/ProxiesView.vue src/api/admin/trash.ts src/i18n/locales/zh/trash.ts src/i18n/locales/en/trash.ts
node node_modules/vitest/vitest.mjs run src/i18n/__tests__/localeKeyCompleteness.spec.ts
```

在 `backend` 目录执行，均通过：

```powershell
go test -mod=readonly -tags unit ./internal/repository ./internal/service ./internal/handler/admin ./internal/server/routes -run '^TestProxyTrash' -count=1
go test -mod=readonly -tags integration ./internal/repository -run '^TestAccountTrash' -count=1 -v
```

账号数据库流程使用 Docker 临时 PostgreSQL 与独立测试数据库，覆盖凭据密文往返、分组恢复、代理依赖、历史软删除、影子账号及事务回滚。彻底删除同时验证消费明细和连接使用归因保留、连接凭据清空。未执行生产部署或浏览器点击验收。

未决 TOTP 恢复修复后，使用 Go 1.27.3 仅复验上述 `^TestAccountTrash` 集成用例：4 个用例全部通过，新增的 12 个子场景覆盖轮换各阶段、未决验证动作、旧 worker 回调隔离、本地候选密钥补回、恢复后配置版本衔接及新/旧密钥验证完成、配置版本/归档密文/身份不匹配时回滚，以及两种彻底删除路径的密钥擦除。
