# 竞速与迁移协议

## 认证竞速

竞速请求使用本次启动器输入的密码，旧新上游并行执行 authenticate。

| Armorial | Legacy | 结果 |
|---|---|---|
| `200` + `selectedProfile` | 任意 | `NEW_WINS`，永久绑定 Armorial |
| `200` 无 `selectedProfile` + 旧 `200` + `selectedProfile` | 可迁移 | `MIGRATE_REUSE_CORE` |
| `403` + 旧 `200` | 可迁移 | `MIGRATE_CREATE_CORE` |
| 超时 / `5xx` / 畸形 + 旧 `200` | 状态未知 | `BLOCK_RETRY` |
| `403` + 旧失败 | 无有效账号 | `NO_BINDING` |

新上游 `200` 但无 profile 表示凭据有效、账号尚无游戏 profile。用户已经向 Scribe 提供登录凭据且旧上游存在可迁移 profile 时，默认按导入处理；只有显式 opt-out 才阻断迁移。首版不要求启动器传递 migration_intent。

新上游 `403` 不等于 Core 账号不存在。提交阶段必须检查已有 Core 账号；凭据或映射不一致时 fail closed，绝不覆盖已有密码、用户名或邮箱。

迁移成功且原请求 `requestUser:true` 时，响应必须包含 Armorial commit 返回的权威 `user.id/properties`。Scribe 不得返回旧 `user.id`，也不得用 `CoreUID` 推断用户身份。

## 迁移提交包

逻辑结构：

```json
{
  "migration_id": "random UUID",
  "identifier": "email-or-login-name",
  "password": "request-memory-only",
  "username": "LegacyPlayer",
  "profile_uuid": "legacy selectedProfile.id",
  "profile_name": "LegacyPlayer",
  "profile_model": "slim",
  "user_properties": {},
  "skin": "optional multipart PNG",
  "cape": "optional multipart PNG",
  "skin_sync": false
}
```

约束：

- 如果 identifier 是邮箱，`email` 使用该邮箱。
- 如果 identifier 是用户名，省略 `email`，由 Armorial 生成唯一的 `.invalid` 占位邮箱。
- `password` 只出现在当前提交请求内存中，禁止持久化明文或写入日志；目标 Core 仅保存正常凭据哈希。
- `skin_sync` 必须为 `false`。
- 资产以规范化 PNG 提交，并附带像素摘要。
- 不提交旧 accessToken/clientToken。

## 状态机

```text
RECEIVED
  -> STAGED_IN_REDIS
  -> APPLYING
  -> COMMITTED
  -> LOCKED
```

异常：

```text
STAGED / APPLYING / UNKNOWN
  -> RECONCILE
  -> ROLLBACK_IF_NOT_COMMITTED
  -> DISCARD_STAGING
```

`reconcile` 是读取实际资源与迁移日志，不产生新的业务写入。`rollback` 只处理未提交迁移；已提交迁移返回冲突错误。

## 资产冲突矩阵

| Legacy | Armorial | 决策 |
|---|---|---|
| 有 | 无 | 上传 |
| 有 | 有且 RGBA 像素相同 | 跳过 |
| 有 | 有且不同 | `migration_asset_conflict` |
| 无 | 有 | `migration_asset_conflict` |
| 无 | 无 | 保持无资产 |

比较对象是规范化 RGBA 像素，不是 PNG 文件字节。规范化包括方向、颜色模式、颜色 profile、非必要元数据和确定性编码。

## 提交顺序

1. 校验服务身份、迁移 ID、请求大小、PNG、模型、用户名、邮箱/占位邮箱策略。
2. 将完整提交包写入 Redis，状态 `STAGED_IN_REDIS`。
3. 创建迁移日志并进入 `APPLYING`。
4. 解析或创建 Core 用户，记录是否由本次迁移创建。
5. 创建固定 UUID 的 Yggdrasil profile，记录资源 ID。
6. 按冲突矩阵写入资产，由 Armorial 正常材质管线生成 textures property 与签名，并提供公共纹理 URL。
7. 校验目标 authenticate 能取得相同 selectedProfile 与资产。
8. 写入 `COMMITTED`，返回新的目标令牌和权威 user 信息。
9. Scribe 验证成功后建立永久绑定并进入 `LOCKED`。

任一步失败进入 reconcile/rollback；未确认提交前不得向启动器返回成功。

## 幂等性

- `commit` 以 `migration_id` 幂等。
- 同 ID 同规范化提交摘要：返回已有结果。
- 同 ID 不同摘要：`migration_conflict`。
- `reconcile` 与 `rollback` 可安全重试。
- `rollback` 已回滚完成：返回既有 `rolled_back` 状态。
- `rollback` 已提交：拒绝，不删除数据。
