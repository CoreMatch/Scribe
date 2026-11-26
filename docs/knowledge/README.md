# Scribe 项目知识库

本知识库记录 Scribe 当前已确认的设计事实，不记录未经确认的实现猜测。权威外部契约位于 `/home/lnb/Desktop/HA/HA-Contract`；本地文档与契约冲突时，以契约为准。

## 索引

1. [系统边界与不变量](001-system-boundary.md)
2. [协议端点与数据映射](002-protocol-inventory.md)
3. [竞速与迁移协议](003-migration-protocol.md)
4. [安全与运行约束](004-security-and-operations.md)

## 运行文档

- [Scribe 运维 Runbook](../runbook.md)
- [Prometheus 告警规则](../../deploy/alerts.yml)
- [Grafana 迁移仪表盘](../../deploy/grafana/dashboards/scribe.json)

## 核心结论

- Scribe 是惰性迁移中间层，不改变启动器 API Root 配置。
- “兼容”仅指对范围内 Yggdrasil HTTP 端点做透明代理，不实现认证、角色、材质或 Minecraft 服务端业务。
- 首版公开范围保持根接口与 authserver；session-server 不进入首版。
- 旧上游完全黑箱，只使用标准 Yggdrasil/authlib-injector 外部形状。
- 新上游固定为 Armorial。
- 首次认证竞速，胜出后永久绑定。
- 用户未表达异议时，凭据有效但无 profile 的 Armorial 账号默认按导入处理。
- 只迁移本次 `selectedProfile` 与当前皮肤/披风。
- 密码不持久化；提交前不得写入数据库。
- Scribe 只访问 Yggdrasil 服务，不直接访问 Core。
- Armorial 在迁移提交内部复用正常材质管线、签发 textures property 并提供公共纹理 URL；Scribe 不代理纹理或维护签名私钥。
- 资产或凭据冲突必须 fail closed。
- 通过迁移日志、reconcile 和 rollback 保证失败后无残留脏数据。
- 首版单实例部署，只在启动时恢复迁移。
