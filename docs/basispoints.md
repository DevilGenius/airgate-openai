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
BPS 的 response ID 不写入普通 OAuth 续接缓存。失败分类、账号冷却和换号复用普通 OAuth 的机制。
用量使用现有 token 计费，并记录 `oauth_transport=basispoints`。

## 缓存用量与计费口径

BPS Responses 的 `input_tokens` 按包含缓存读取和创建的总输入处理。插件先拆成互斥的三部分，
再传给 SDK / Core：普通输入 = 总输入 − 缓存读取 − 缓存创建。
例如总输入 1,000、读取 200、创建 300，则记普通输入 500、读取 200、创建 300。
三类 token 各按现有模型价格计费一次；Core 汇总分项成本后应用倍率，不再对总输入重复收费。
本次适配保留缓存创建统计和现有缓存创建单价，不将其清零或额外加到普通输入上。

是否计入缓存创建由**本次上游响应的实际字段**决定，不由 BPS 开关决定。普通 OAuth
或 BPS 都没有返回创建字段时，创建 token 和创建费用均为 0；未命中的输入仍按普通输入计量，
不会以 `input_tokens - cached_tokens` 推算成缓存创建。即使模型配置有缓存创建单价，也不会
在创建 token 为 0 时收费。只有明确返回创建总数或 TTL 创建明细时才记录该项。
这里的字段支持来自参考实现，不表示所有 BPS 请求都会返回这些字段；尚无本项目的真实
BPS 原始响应及账单核对证据。

两个上游解析入口共用缓存创建字段优先级：`input_tokens_details.cache_write_tokens`、
`prompt_tokens_details.cache_write_tokens`、两者的 `cache_creation_tokens` 别名、
顶层 `cache_creation_input_tokens` / `cache_write_input_tokens` / `cache_write_tokens` / `cache_creation_tokens`。
同一指标的别名只选一个，显式零值优先；只有总数字段全部缺失时才合计 5m 和 1h TTL 明细，
不把 TTL 明细再次加到创建总数上。读取和创建被限制在总输入范围内，防止负数和重复计量。

客户端字段遵循各自协议：Responses/Chat 的输入总数包含缓存；Anthropic 的 `input_tokens`
只包含普通输入，读取和创建另外列出。因此不能把 OpenAI 输入总数与其缓存明细再次相加。
本地回归覆盖 12 组缓存字段表达（包含缺失/零值）× 3 种协议 × 流式/非流式，以及 Core 分项成本和倍率计算；
普通 OAuth WebSocket、普通 SSE、Anthropic 转换及 BPS 的无创建字段对照还覆盖缓存读取为
0、部分命中、全部命中三种情况。
这些是模拟报文验证，不代表已取得真实 BPS 账单进行核销。

## 降级决策

主要回退方式是关闭分组开关。为保证开启后不破坏普通请求，仅保留以下同请求兼容处理：

| 情况 | 行为 |
| --- | --- |
| 开关关闭、API Key、Agent Identity | 原有模式，零 BPS 请求 |
| hosted tools（含搜索、生图）、内联图片、原生文件 ID、不支持的输入或工具协议 | 发请求前选择普通模式 |
| `previous_response_id` / `item_reference` 增量历史、priority 服务档位、不支持的原生控制参数 | 发请求前选择普通模式 |
| `/responses/compact`、Images API、入站 WebSocket 直通 | 保持原有模式 |
| BPS HTTP 400/403/404 明确返回 `model_not_found`、`model_not_supported`、`unsupported_model`、`model_access_denied`、`basispoints_model_access_changed` | 尚未输出时，以未被 BPS 修改的请求走一次普通模式 |
| 输出前的 401/403、429/usage_limit_reached、5xx、超时、网络异常、SSE 临时错误或断流 | 复用普通 OAuth 失败分类；由 Core 按现有预算、冷却和会话约束换号重试 |
| 非重试性请求错误 | 保留客户端错误，不换号 |
| 实际内容已输出后的错误或断流 | 终止当前流，不重放 |

不把所有 403 都解释为模型不支持，也不按模型名称维护易过期的静态白名单。
HTTPS 图片链接可以使用 BPS；此版本不实现 BPS 附件上传、磁盘中转或隐藏的工具自纠请求。
入站 WebSocket 是持久双向会话，保持原有直通语义；HTTP Responses / Chat Completions /
Anthropic Messages 均支持 BPS 的流式和非流式响应。

`internal/basispoints` 只转换协议，不选号、不维护重试循环。
网关的 `responseFailureOutcome` 统一处理普通 WS 与 BPS SSE 错误；HTTP 错误共用
`failureOutcome`。Anthropic 转换在首个实际输出前缓存创建事件和 ping，提前失败不会
提交 HTTP 200、message_start 或错误帧；输出后仍禁止换号。模型兼容回退与账号换号是不同机制。
输出前网络失败也采用普通模式的重试策略，因此存在上游已开始计算但尚未交付输出的重复消耗可能。
关停后的会话若只持有 BPS 服务端引用而没有完整历史，应补发完整历史或新建会话；
不同上游的服务端响应引用不可互换。

## 历史消息与工具契约

恢复并适配本地提交 442570ad 的协议修复，沿用当前无状态 PrepareRequest 路径：

- history_messages.go 在内容校验后，将 author/recipient 转成正文归属说明；agent_message
  降为明确标注的协作上下文，元数据不能赋予 system/developer 权限。普通消息无归属字段时直接返回，
  保留角色和原生字段；工具参数中的同名业务字段不参与归一化。
- tool_definition.go 只比较工具执行契约，统一使用已解析的参数 schema；description/defer_loading
  的变化不再被误判成冲突。保留首个权威声明，schema、strict、custom grammar 和未知执行约束的冲突仍拒绝。
- tool_images.go 在工具结果校验后，将 HTTPS 图片移到紧随其后的上下文消息；用 call_id 与位置标签
  关联原工具结果，保留文本顺序、URL 和 detail。不新增上传、磁盘中转或持久缓存。

此恢复不引入备份分支的进度超时、工具参数提前输出或另一套保活逻辑。
当前非空 references 约束、原生参数字符串外空白保护及输出后不重放策略继续生效。
这三项协议修复不是诊断设施，后续清理日志和采样时应保留。

## 验证

### 基于 0.2.80 的流式保活修复

保活补丁本身不改变请求构造、历史消息、工具整批校验、隐藏思考事件、首输出判定和重试策略；
独立的历史协议修复见上一节。
转换后的 BPS 流静默 15 秒时，以独立私有控制帧唤醒原有响应消费者；只有基线已经
开始输出的流才发送对应协议的保活，首输出前不提前提交响应。私有帧不会发给客户端，
也不改变上游原生 keepalive 的处理和用量/首字计时。

这里不判断模型是否有“语义进展”，不设首次正文或交付期限，不新增工具参数增量转换。
上游实际读空闲仍使用 0.2.80 的保护；取消和终态结束读取，保活不能延长已断开的上游。
读取泵只缓存一个待交付 SSE 帧，按字节线性处理，不扫描对话历史或重复解析累计参数。

三种客户端协议共用 basispointsKeepaliveEncoder 编码接口和 handleBasispointsKeepalive
提交策略；BPS 与图片 SSE 共用 writeSSEFrame 处理写入、短写错误和 Flush。
发送间隔、首输出前是否发送、取消与终态由原有控制器负责。WebSocket 使用自身的
Ping/Pong 控制帧机制，不套用 SSE 编码或 HTTP 提交规则。

回归覆盖持续隐藏思考不被误杀、三种客户端协议保活、首输出前失败仍可重试、取消释放、
原始帧保留、分片边界和真正上游读空闲退出。保活用于维持等待，不代表上游已生成正文。

协议回归覆盖工具整批校验、schema、无状态完整历史重建、身份隔离、结构化输出及不完整流。
无状态回归还覆盖四种工具传输的精确载荷恢复、独立请求/账号切换，以及孤立工具结果不继承旧状态。
本地 TLS 集成测试覆盖分组开关、账号类型、请求头和请求体、三种客户端协议、模型拒绝回退、
HTTP/SSE 限流及 reset 时间、输出前故障重试判决、输出后不重放、失败用量及流中断。
Core 测试覆盖配置头的防伪、开关恢复和两种 OAuth 传输一致的换号判决。
测试不访问真实 BPS；部署后的模型权益和真实上游兼容性仍需使用具备权限的 OAuth 账号验证。
