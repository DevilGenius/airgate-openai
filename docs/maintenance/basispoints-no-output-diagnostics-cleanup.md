# BAS/BPS 无输出问题：诊断设施与移除清单

本文登记此次无输出、工具参数持续生成及 max_output_tokens 排查所添加的日志、采样、临时工具和数据，供后续逐项移除。**建立清单不代表问题已修复，也不代表已执行清理。**

**当前清理约束：按用户要求，采样探针、开关、已有样本和诊断日志先保留；确认实际会话中的修复无误后再清理。** 探针和开关在一次临时撤下后已恢复，没有删除参数样本。仍沿用每流 64 KiB、标记 30 分钟有效期；过期不等于已删除，后续复现前需检查标记是否仍有效。

代码核验基线为 `c91cc58`（流式传输摘要报告），其上还有未提交的本机采样探针。此前的 `129e650`、`2abffb0` 是保活及写入抽象，不属于此次诊断清理对象。**不要整体 revert c91cc58**：其中同时包含应保留的 incomplete 终态修复。

下文代码路径默认相对于 airgate-openai 仓库；标注“工作区”的路径相对于 `C:/Users/quantal/workspace/projects/airgate`。后续若新增诊断设施，必须同步更新本清单；移除完成后再勾选。

## 1. 仓库内的日志与观察接口

| 编号 | 项目及位置 | 行为与边界 | 后续移除范围 |
| --- | --- | --- | --- |
| L01 | `backend/internal/gateway/basispoints.go`：`openBasispoints` 中的 `basispoints_stream_finished` | 每条转换流结束后汇总一次。正常 completed 为 Debug，其余终态或读写错误为 Warn；携带 model、account_id、stream 统计及日志上下文 request_id。通过 logger.Enabled 判断后才格式化。 | 删除传给 prepared.Stream 的日志回调及不再使用的 slog 导入；与 L03 的签名修改同时进行。不要删除既有 basispoints_request_started。 |
| L02 | 同文件 `tryBasispointsOAuth`：`openai.incomplete_reason` 用量元数据 | max_output_tokens 时写入 Usage.Metadata，最终可进入 Core usage_logs.usage_metadata。它不是逐事件日志。 | 若要求移除此次全部诊断字段，只删除该 setUsageMetadata 分支，并调整对应测试断言。不要删除其他用量字段或批量清空历史 usage_logs。 |
| L03 | `backend/internal/basispoints/stream_summary.go`；`stream.go`；`prepared_request.go` | StreamSummary、summaryTokens、observeUpstream、observeDownstream，以及 report 回调、计时和参数传递。由单个转换协程维护固定计数；不保存正文或参数，不增加逐事件日志。 | 删除摘要类型/实现、观察调用、统计专用计时和回调；同步收拢 PreparedRequest.Stream、Bridge.stream、transformWithRepairs 及调用方签名，按实际使用情况清理导入。 |
| L04 | `backend/internal/basispoints/stream_summary_test.go` | 字节/错误不变性、缺报 reasoning 用量与显式零的区别、慢日志不阻塞 EOF，以及 BenchmarkStreamSummaryToolDelta。 | L03 完全移除后删除专属测试文件；若保留部分观察能力，应相应保留其测试。 |
| L05 | `backend/internal/basispoints/prepared_request_test.go`、`backend/internal/gateway/basispoints_keepalive_test.go` | PreparedRequest.Stream 调用新增了诊断回调参数，通常传 nil。 | 随 L03 调整参数；不要删除历史回放或三种协议的保活测试。 |

### 摘要字段的含义

| 字段组 | 内容 |
| --- | --- |
| `UpstreamEvents / UpstreamBytes` | 上游 JSON 事件数量与载荷字节数；不是网络流量计数，快照也计入。 |
| `TextDeltaEvents / TextDeltaBytes` | 正文增量数量和字节数。 |
| `ReasoningDeltaEvents / ReasoningDeltaBytes` | 可见推理增量数量和字节数；没有这些事件不等于没有隐藏推理。 |
| `ToolDeltaEvents / ToolDeltaBytes` | 原生工具参数增量数量和字节数；大量增量可能仅组成一个调用。 |
| `NativeToolsDone / DeliveredTools / DownstreamEvents` | 原生工具 done 事件、转换器已写出的工具 done 事件、转换后事件数；不是客户端实际执行工具的确认。后续注入的保活不在这些计数内。 |
| `UpstreamTerminal / DownstreamTerminal / IncompleteReason` | 上游与转换后的终态、白名单内的不完整原因。取消时可能没有终态。 |
| `TerminalTools / UnfinishedTools / TerminalToolBytes` | 上游终态快照中的工具数量、状态非 completed 的工具数量及参数字节数。 |
| `OutputTokens / ReasoningTokens / ReasoningTokensReported` | 上游终态用量及 reasoning 用量是否明确上报；取消、缺报时的零不能解释为实际消耗为零。 |
| `DurationMs / ReadOrWriteError` | 转换流处理耗时及是否读写出错；不含此前的排队、响应头等待时间。错误也可能来自客户端取消。 |

## 2. 临时本机参数采样探针

| 编号 | 位置 | 后续移除范围 |
| --- | --- | --- |
| S01 | `backend/internal/basispoints/local_capture.go` | 整个文件；这是含本机绝对路径的临时探针，不能进入发布版本。 |
| S02 | `stream.go` 的 `transformWithRepairs` | 三处接入：openLocalArgumentCapture(b.scope)、defer localCapture.close()、localCapture.observe(kind, payload)。与 S01 同批删除，避免遗留未定义符号。 |
| S03 | 工作区 `airgate-core/backend/tmp/bps-live-capture/enabled` | 启用标记。清理时首先移除，以停止新请求开启采样。 |
| S04 | 同目录 `args-<scope前12字符>-*.partial` | 原生工具参数样本。确认没有仍在写入的诊断流后，移除此次生成的文件。 |

采样机制与限制：

- 标记存在且修改时间距当前不超过 30 分钟时，新转换流才开启采样；修改时间在未来也不启用。
- **当前没有账号白名单**：标记有效时，经过此本机探针的 BAS/BPS 转换流均可能被采样。
- 每条流一个文件，最多保留前 64 KiB 参数，使用 4 KiB 缓冲并按阈值/结束刷新。不保存完整 SSE，不执行生成的工具。
- 仅采集 response.function_call_arguments.delta 和 response.custom_tool_call_input.delta；同一流若有多个调用，其参数会顺序拼接，不能把整个文件 JSON 解析失败直接当成协议错误。
- 空文件可能只是没有工具参数；文件也可能仍在写入或已截到 64 KiB。分析时必须结合流状态。
- 标记删除或过期只影响新采样，**不会立即关闭既有文件、删除已有样本或中断用户请求**。每流容量有界，但没有目录总量上限或自动文件清理。
- 样本是原始参数，可能包含代码、路径及敏感字面量；它与 L01 的无正文计数日志不同。不要提交样本、贴出完整内容或误认为已脱敏。

## 3. 临时工具、重放文件与外部产物

| 编号 | 位置 | 用途及后续处理 |
| --- | --- | --- |
| T01 | `backend/tmp/bps-replay/main.go`、`observe.go` | 直连原 BPS 上游的有界重放：读取现有配置和账号身份，使用实际代理组 slot；每次最多 120 秒，参数样本最多 64 KiB。会消耗真实上游用量，但不执行模型生成的工具。停止本次探针进程后移除。 |
| T02 | `backend/tmp/bps-replay/captures/*/tool-args.partial` | 五次直连重放产生的本机参数样本；按敏感原始数据处理，诊断结束后删除。 |
| T03 | `backend/tmp/bps-replay/normalize_overlay.txt`、`overlay.json`、`padding_overlay.txt`、`padding_overlay.json` | 两组 Go overlay 分别验证请求规范化差异和真实失败样本的空白检测；清理时移除这四个临时文件。虚拟目标 `backend/internal/gateway/bps_replay_overlay_test.go`、`backend/internal/basispoints/bps_padding_overlay_test.go` 没有实体文件，不应误删真实测试。 |
| T04 | 工作区 `airgate-core/backend/tmp/bps-live-status/` | main.go：只读账号/Redis 槽位/用量诊断及模式入口；incident.go：提取已有错误追踪；replay.go：经本地 Core 完整链路重放；shape.go：分析样本结构。重放模式会发起真实请求，读取诊断/shape 模式不会执行模型工具。工具闲置后移除本次目录。 |
| T05 | 工作区 `airgate-core/backend/tmp/bps-incident/<request_id>/` | client.json 原始请求、identity.txt 哈希身份、可选 normalized.json 和 headers.json。来自已有 Core 错误追踪，可能含完整会话内容；不应提交。归档必要的脱敏结论后，删除下列此次请求目录。 |
| T06 | 工作区 `review/bps-baseline-fix/capture-summary.go` | 早期 PktMon 抓包的 Go 摘要工具；不再需要时移除。 |
| T07 | 同目录 `live-8386-032905.etl`、`live-8386-032905.pcapng` | 早期有界抓包，核验时均已删除。捕获结束时已停止当次 PktMon 并移除当次过滤器；不要为了清理而停止其他人的后续捕获。 |
| T08 | 同目录 `0909-plus-6-incomplete-20260928.md`、`live-tool-generation-20260928.md`、`161a891-failures.log` | 排查结论及早期回退证据。单独审阅后决定脱敏归档或移除，不与原始样本混为一类。 |

T05 本次请求目录：

- `10dbaec1-8764-40f5-884b-7e3dbbd5fb6e`
- `325d5a44-285f-4b7d-958e-d9c0249d20b0`
- `8ccb7f34-286b-41ce-9a52-370332042705`
- `d3944636-ab4c-4dfb-b3c8-519944898ed6`

上述 tmp 工具/数据可能被 Git 忽略，不能只凭 git status 干净就认为清理完毕。重放设置的 120 秒是**诊断请求自身的时限**，没有作为生产请求总超时加入网关。

## 4. 保留项：不要随诊断清理回滚

- `request.go` 的非空 references 指令，以及 `tools.go`、`function_code_transport.go`、`function_cmd_transport.go` 中历史回放的非空 references。原生工具要求至少一项，不能恢复旧的 references=[] 指令或示例。
- `argument_padding.go` 及 `stream.go` 的 argumentPadding.observe 接入：这是功能性保护，不是诊断观察器。只跟踪已识别的原生 run_officejs 调用，对 JSON 字符串外连续空白设 4 KiB 上限；字符串中的代码空白不计入。不要与 localCapture.observe、summary.observeUpstream/Downstream 一并删除。
- `native_references_test.go`、`argument_padding_test.go` 的协议与结构保护回归测试。
- `basispoints.go` 对流式 Responses 的 max_output_tokens 终态判定：保留原始 response.incomplete 和实际用量，返回 OutcomeStreamAborted，不透明重放，不追加伪造成功终态或成功 DONE。Chat 的 length 和非流式 incomplete JSON 语义也应保留。
- `backend/internal/gateway/basispoints_incomplete_test.go` 的行为回归测试；移除 L02 时只调整该元数据相关断言。
- 正确的流关闭与取消顺序、上游资源释放、既有整体工具批次校验、保活和 Core 重试边界。
- 既有 basispoints_request_started、basispoints_native_route、forward_request_*、http_request 等正常日志，以及 backend.out.log、backend.err.log、frontend.out.log 的常规轮转机制。
- Core 的错误追踪能力和数据库记录原本已启用，本次只是利用它们提取快照；不要顺带清空 monitor_request_events、monitor_request_trace、usage_logs，或修改 API Key、OAuth 凭据、代理绑定及 Redis 槽位。
- `airgate-core/.dev/config.yaml` 仅被诊断工具读取，不属于新增诊断文件，不应删除或重置。

## 5. 按顺序执行的移除清单

- [ ] 确认已获得必要的失败证据，先保存脱敏结论；确认本次重放/捕获进程状态，不批量终止所有 Go、Node 或后端进程。
- [ ] 删除 S03 标记；等待已开启的采样流自然结束或由请求所有者明确中断，确认文件句柄不再写入。
- [ ] 同批移除 S01、S02；确认代码中不再引用 localArgumentCapture、openLocalArgumentCapture、localCaptureDirectory、localCapture。
- [ ] 移除 L01、L03；同步调整 PreparedRequest.Stream、Bridge.stream、transformWithRepairs 的签名及所有调用方。
- [ ] 移除 L04 中的专属测试，调整 L05 的调用参数；保留保活、历史回放、错误传播和 incomplete 行为测试。
- [ ] 如需移除全部本次诊断字段，移除 L02 的写入及相应测试断言；确认第 4 节的功能修复仍保留。
- [ ] 停止并移除 T01、T03、T04；按明确的本次路径清除 S04、T02、T05，避免递归清理整个 backend/tmp 或 review。
- [x] T07 的两份原始抓包文件已删除；后续如新增抓包，另行登记。
- [ ] 审阅并处理 T06、T08；本清单保留作为交接和清理记录。
- [ ] 搜索诊断符号与路径残留，检查 tracked/untracked/ignored 文件，确认没有临时绝对路径或原始样本进入待提交内容。
- [ ] 代码清理后运行 basispoints、gateway 的相关 Go 测试，重点核验取消释放、EOF、保活、incomplete 用量与不重放行为；提交/发布前再按仓库规则进行所需检查。
- [ ] 在此文档记录清理提交、证据归档位置和仍有意保留的项目；未经勾选或说明，不将诊断清理标为完成。

## 6. 已确认样本与验证进度

- 失败请求 `d3944636-ab4c-4dfb-b3c8-519944898ed6`，插件请求 `ca3df4ca-b82e-4e8d-a8ea-c7b66d32ce47`：401.210 秒、33,159 个工具参数增量、99,409 字节参数，没有原生工具完成事件。
- 失败样本为 S04 下的 `args-40e31dbcfcff-1341145830.partial`（64 KiB）。code 字段已完整，references 数组刚打开后持续出现空格、制表符和换行；不是仍在生成代码。此文件是原始证据，验证完成前保留，不提交。
- 样本 SHA-256：`C436EDFDC35054057505DA7A750D889F01C6D950D468546D1E2DA3AC2C3021D7`。
- 对真实样本运行 padding overlay 回归，新结构保护在读取到第 5,329 字节以内识别该循环。basispoints 和 gateway 测试通过；1 KiB 字符串扫描微基准约 496.9 ns、0 分配，不代表整条请求的总成本。
- 修复后的原请求经本地 Core→插件链路重放约 19.9 秒完成（请求 `61dd4043-4467-4682-969d-7c6e6d09c090`），一个完整工具调用被转换并下发；诊断客户端没有执行该工具。单次成功不能替代实际会话的持续验证，因此采样继续保留。

本清单不授权自动清理。验证期间保留诊断设施；确认修复无误后再按列表移除，功能修复与诊断代码应分开处理。
