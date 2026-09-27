# OAuth Basispoints

在管理后台的 OpenAI 分组编辑页开启「OAuth Basispoints 模式」。默认关闭；
关闭后，新请求恢复原有 OAuth 转发，不需要重启、数据库迁移或修改账号。
已开始的请求继续使用进入请求时的配置。

配置使用现有 `plugin_settings.openai.basispoints`，值为字符串 `"true"` / `"false"`。
Core 删除客户端伪造的 `X-Airgate-*` 控制头，再从实际选中分组签发
`X-Airgate-Plugin-Openai-Basispoints`。配置随分组读取、更新和路由切换传递；
API Key 和 Agent Identity 账号不进入 BPS。

## 模块边界

- `backend/internal/basispoints`：独立协议适配、Excel 请求头、能力判定、工具信封、
  JSON Schema 校验和 SSE 转换；无数据库、SDK 或账户依赖。保留参考实现归属及许可证。
- `backend/internal/gateway/basispoints.go`：复用现有 OAuth 凭据生命周期、账号代理连接池、
  首字节/流空闲超时、客户端协议转换和计费；负责是否尝试 BPS。
- Core 分组表单：只保存现有插件配置，不新增 SDK 字段或数据库列。

BPS 使用固定 `/basispoints/api/responses` 地址和 Excel 客户端画像。
请求保留映射后的模型，以 `model_selection: explicit`、`stream: true`、`store: false`
发送完整历史。FUNCTION、FUNCTION_CODE、FUNCTION_CMD 和 CUSTOM 工具都通过
`run_officejs` 信封传输；代理不执行 Office 或工具代码。
普通文本保持增量；工具调用整批校验通过后才交付。结构化输出在终态校验通过后交付。

`OpenAIGateway` 不持有 BPS Adapter 或会话缓存。`PrepareRequest` 是无副作用的准备入口，
返回单次请求专用的 `PreparedRequest`，其正文只通过只读 reader 暴露，流转换状态随请求释放。
请求每次携带工具声明和完整历史；工具信封从本次历史重建，不隐式继承其他请求的工具目录或调用。
只有工具结果、没有对应工具调用的请求直接走普通模式，行为不再取决于缓存是否命中。
上游 task/turn/prompt-cache 标识仍由账户、用户、API Key、分组和会话确定性派生；
这是用于上游缓存的身份种子，并不代表本地保存了会话状态。
BPS 的 response ID 不写入普通 OAuth 续接缓存，失败不改变普通 OAuth 的权益冷却。
用量使用现有 token 计费，并记录 `oauth_transport=basispoints`。

## 降级决策

主要回退方式是关闭分组开关。为保证开启后不破坏普通请求，仅保留以下同请求兼容处理：

| 情况 | 行为 |
| --- | --- |
| 开关关闭、API Key、Agent Identity | 原有模式，零 BPS 请求 |
| hosted tools（含搜索、生图）、内联图片、原生文件 ID、不支持的输入或工具协议 | 发请求前选择普通模式 |
| `previous_response_id` / `item_reference` 增量历史、priority 服务档位、不支持的原生控制参数 | 发请求前选择普通模式 |
| `/responses/compact`、Images API、入站 WebSocket 直通 | 保持原有模式 |
| BPS HTTP 400/403/404 明确返回 `model_not_found`、`model_not_supported`、`unsupported_model`、`model_access_denied`、`basispoints_model_access_changed` | 尚未输出时，以未被 BPS 修改的请求走一次普通模式 |
| 普通 401/403、429、5xx、超时、网络异常、SSE 内错误或流中断 | 返回错误；不自动切模式、切账号或重放 |

不把所有 403 都解释为模型不支持，也不按模型名称维护易过期的静态白名单。
HTTPS 图片链接可以使用 BPS；此版本不实现 BPS 附件上传、磁盘中转或隐藏的工具自纠请求。
入站 WebSocket 是持久双向会话，保持原有直通语义；HTTP Responses / Chat Completions /
Anthropic Messages 均支持 BPS 的流式和非流式响应。

自动重放流中断或网络超时可能产生重复生成与计费，因此不支持。
关停后的会话若只持有 BPS 服务端引用而没有完整历史，应补发完整历史或新建会话；
不同上游的服务端响应引用不可互换。

## 验证

协议回归覆盖工具整批校验、schema、无状态完整历史重建、身份隔离、结构化输出及不完整流。
无状态回归还覆盖四种工具传输的精确载荷恢复、独立请求/账号切换，以及孤立工具结果不继承旧状态。
本地 TLS 集成测试覆盖分组开关、账号类型、请求头和请求体、三种客户端协议、模型拒绝回退、
错误不重放、用量及流中断。Core 测试覆盖配置头的防伪与开关恢复。
测试不访问真实 BPS；部署后的模型权益和真实上游兼容性仍需使用具备权限的 OAuth 账号验证。
