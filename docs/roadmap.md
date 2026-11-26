# Scribe Roadmap

状态：Scribe 侧 M1–M5 核心流程已实现；M0 契约收尾与 M6/M7 运行化仍在推进。本文档定义实施顺序与验收标准。

## 当前实施决策

- 首版为单实例临时部署，不提供水平扩容。
- Scribe 的“兼容”仅指对范围内 Yggdrasil HTTP 端点做透明代理，不实现认证、角色、材质或 Minecraft 服务端业务。
- 首版公开范围保持根接口与 authserver；session-server 不进入首版。
- 用户未表达异议时，Armorial 凭据有效但无 profile 且旧上游有可迁移 profile 的组合默认执行迁移。
- Armorial 在迁移提交内部复用正常材质管线并负责签名与公共纹理 URL；Scribe 不代理纹理或维护签名私钥。
- 首版只在启动时恢复迁移；token 摘要绑定按可配置保留期清理。
- 迁移结果未明确前 signout 保持阻断。

## M0：契约冻结与设计评审

交付：

- 冻结竞速规则、迁移范围、状态机与错误码。
- 在 HA-Contract 注册迁移 commit/reconcile/rollback 端点。
- 明确占位邮箱、Core username、固定 profile UUID 与 `skin_sync=false`。
- 为计划端点添加临时 drift exemption，并记录移除条件。

验收：

- OpenAPI validity 通过。
- business API drift 通过。
- 错误码全部先注册后使用。
- ADR、知识库与契约无冲突。

## M1：Scribe 基础网关与永久绑定

交付：

- 代理根元数据、authenticate、refresh、validate、invalidate、signout。
- 强制重新认证策略。
- 绑定存储与原子 CAS 更新。
- 超时、限流、熔断、统一错误映射。

验收：

- 已绑定用户只访问胜出上游。
- 状态未知不绑定。
- 失败请求不会切换既有绑定。
- 标准 403/204/401 语义保持不变。

## M2：认证竞速与迁移门闸

交付：

- 新旧 authenticate 并行执行。
- 按 `selectedProfile` 判定胜者。
- `NEW_WINS`、`MIGRATE_REUSE_CORE`、`MIGRATE_CREATE_CORE`、`BLOCK_RETRY`、`NO_BINDING` 状态机。
- 未完成迁移期间阻断该用户其他请求。

验收：

- 覆盖 ADR 竞速表全部分支。
- 新上游超时、5xx、畸形响应均 fail closed。
- 已完成 Armorial profile 的用户不会被迁移。
- 同用户重复认证不会重复迁移。

## M3：快照、规范化与资产迁移

交付：

- 获取 signed profile 与 textures property。
- SSRF 安全纹理下载。
- PNG 校验、元数据剥离、RGBA 规范化与像素摘要。
- 资产冲突矩阵。
- 由 Armorial 在迁移提交内部复用正常材质上传管线。
- 验证迁移后 profile 的签名与资产。

验收：

- 相同像素不同编码不会误判冲突。
- 资产冲突时不写库并保持阻断。
- 迁移后的 profile 通过 unsigned=false 签名验证。
- Armorial 提供的纹理 URL 在旧上游内部化后仍可访问。

## M4：Redis 暂存与 Armorial 原子提交

交付：

- Redis 快照、资产、提交摘要与迁移日志。
- Armorial `POST /internal/migrations/commit`。
- Core 用户创建/复用及占位邮箱语义。
- 固定 UUID profile 创建。
- 返回新的 Armorial authenticate 会话与权威 user 信息。

验收：

- 提交前数据库无半成品。
- 明文密码不进入 Redis 或日志；Core 仅保存正常凭据哈希。
- 同 migration_id 同摘要幂等，不同摘要冲突。
- 同 UUID、同 username、同资产的成功迁移可重复验证。
- `skin_sync=false` 不被注册逻辑覆盖。

## M5：Reconcile、Rollback 与恢复

交付：

- Armorial reconcile/rollback 端点。
- Core 按 migration_id 的补偿端点。
- Scribe 启动扫描与恢复；首版不运行周期性恢复 worker。
- 资源权属校验和告警。

验收：

- 在 apply 每个阶段注入故障均可恢复。
- 未提交迁移最终无残留资源。
- 既有 Core 账号不会被删除。
- 已提交迁移禁止回滚。
- 重复 reconcile/rollback 安全幂等。

## M6：安全加固与运行化

交付：

- TLS、证书校验、限流与资源预算。
- Redis ACL/TLS/TTL。
- token 摘要绑定保留期与启动清理。
- 指标、日志脱敏与告警。
- 运维 runbook 和故障演练。

验收：

- 安全评审无高危问题。
- 故障注入证明 fail closed。
- 日志扫描不出现明文密码、令牌或私钥。
- 过期 token 绑定不再参与路由，且启动清理可重复执行。

## M7：灰度上线与迁移观测

交付：

- 小流量启用。
- 按迁移结果、冲突、失败阶段、恢复时长建立仪表盘。
- 回滚到纯代理模式的紧急开关；紧急开关不删除已提交迁移。
- 收集标准协议兼容性反馈。

验收：

- 启动器无需修改配置。
- 成功迁移用户后续请求全部路由 Armorial。
- 迁移失败用户仍被阻断或按明确策略处理，不出现静默数据分叉。
- 契约测试与 drift 检查持续通过。

## 明确延后

- 公共 session-server 端点。
- 占位邮箱认领与找回密码。
- 多 profile 迁移。
- 非 Yggdrasil 数据迁移。
- WinnerProxy 正版身份吸收。
- 删除账号恢复。
- UUID 重生成。
