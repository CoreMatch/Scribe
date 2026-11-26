# 协议端点与数据映射

## 认证与引导

| 用途 | 标准端点 | 说明 |
|---|---|---|
| 强制重新输入密码 | `POST /authserver/validate` 或 `POST /authserver/refresh` 返回无效令牌 | 启动器随后调用 authenticate |
| 新旧竞速与迁移凭据 | `POST /authserver/authenticate` | 请求包含 username/password/clientToken，可要求 `requestUser=true` |
| 已绑定用户刷新 | `POST /authserver/refresh` | 只接受 accessToken/clientToken，不接受密码 |
| 已绑定用户校验 | `POST /authserver/validate` | 按绑定路由 |
| 退出登录 | `POST /authserver/invalidate` | 按绑定路由，不触发迁移 |
| 签名登出 | `POST /authserver/signout` | 不触发迁移 |

标准要点：

- 无效密码：HTTP `403`，`ForbiddenOperationException`，消息 `Invalid credentials. Invalid username or password.`。
- 无效令牌：HTTP `403`。
- 临时无效令牌仍可能允许 refresh；Scribe 必须同时使 validate/refresh 失败，才能强制 authenticate。
- `accessToken` 与 `clientToken` 是任意字符串，不能从旧令牌推断 Core 凭据。
- `requestUser=true` 只保证 user ID 与可选 properties，不保证邮箱或 Core username。

## 最小迁移端点集

1. `POST /authserver/authenticate`
2. `GET /sessionserver/session/minecraft/profile/{uuid}?unsigned=false`
3. `GET /` 或部署时固定签名公钥；`X-Authlib-Injector-API-Location` 必须指向 Scribe 的公共 API Root
4. 从 profile property 的 textures URL 下载皮肤/披风

实现资产写入时使用 Armorial 迁移内部端点：

- `POST /internal/migrations/commit`

Scribe 不直接调用公开纹理写端点。Armorial 在迁移提交内部复用正常材质存储与上传管线，生成 textures property、使用 Armorial 签名密钥签名，并提供公共纹理 URL。Scribe 不代理纹理，也不维护签名私钥。

`GET /sessionserver/session/minecraft/profile/{uuid}?unsigned=false` 仅用于从旧上游获取本次迁移快照，不属于 Scribe 首版公开路由。`POST /sessionserver/session/minecraft/join`、`GET .../hasJoined` 与 `POST /api/profiles/minecraft` 不是完成最小迁移所必需，不进入首版迁移快照流程。

## 数据映射

| 标准数据 | 用途 | 限制 |
|---|---|---|
| `user.id` | 身份关联与审计 | 不等于 Core UID，也不必然等于 profile UUID |
| `user.properties` | 可选保留属性 | 仅迁移协议明确允许的键 |
| `selectedProfile.id` | 目标 Yggdrasil profile UUID | 正常路径保持原值 |
| `selectedProfile.name` | Core username 与 profile name | 冲突时 fail closed |
| login identifier | 邮箱或 Core username 候选 | 不能从邮箱反推 username |
| textures SKIN/CAPE | 当前皮肤与披风 | 仅当前绑定资产，不包含历史版本 |
| `model` | `default` 或 `slim` | 必须与像素资产一起规范化 |

## 资产编码

textures property 是 Base64 JSON，至少包含：

- `timestamp`
- `profileId`
- `profileName`
- `textures.SKIN.url`
- `textures.SKIN.metadata.model`（可选）
- `textures.CAPE.url`（可选）

property signature 是 Base64 编码的 SHA1withRSA。Scribe 验证旧 profile 的签名与身份；迁移后的 textures property 由 Armorial 重写并签名。纹理 URL 文件名被客户端当作纹理哈希，规范化后应生成稳定文件名。

## 标准状态码

- 认证失败：`403`
- profile 不存在：`204`
- 无有效授权的纹理上传：`401`
- 客户端强制重新认证依赖“令牌无效”，不是标准错误码的新扩展
