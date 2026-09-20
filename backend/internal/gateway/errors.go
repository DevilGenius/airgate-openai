package gateway

import (
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const maxErrorResponseBodyBytes = 1 << 20

// 统一错误分类工具（跨 OpenAI / Anthropic 协议共用）。
//
// 新契约：插件用 OutcomeKind 向 Core 表达判决。classifyHTTPFailure / classifyMessage
// 是所有错误路径的唯一分类入口。

// classifyHTTPFailure 把 HTTP 状态码 + 错误文本归一化为 OutcomeKind。
// 返回的 Kind 决定 Core 如何处置账号：
//
//	429 → AccountRateLimited
//	401 → AccountDead
//	403 → AccountUnavailable（明确账号禁用 / workspace 失活仍升级为 AccountDead）
//	402 + deactivated_workspace → AccountDead
//	402 + WebSocket 握手 Payment Required → AccountDead
//	402 / 403 + 余额或配额耗尽（结构化 code 或纯文本）→ AccountQuotaExhausted
//	  （Core 语义：可 failover 换号重试，并把该账号直接 disabled）
//	400 + 消息含限流关键词 → AccountRateLimited（部分上游用 400 返回 usage_limit_reached）
//	400 + 消息含 disabled/deactivated → AccountDead
//	overloaded 语义 → FamilyTransient（走 Core 的 family 级短退避）
//	5xx → UpstreamTransient
//	其它 4xx → ClientError（客户端请求自己的问题，账号无辜）
func classifyHTTPFailure(statusCode int, message string) sdk.OutcomeKind {
	if statusCode == http.StatusBadRequest && isEncryptedContentVerificationError(message) {
		return sdk.OutcomeClientError
	}
	if statusCode >= 400 && isOverloadedText(message) {
		return sdk.OutcomeFamilyTransient
	}
	if (statusCode == 400 || statusCode == 403 || statusCode == 429) && isTemporaryRateLimitText(message) {
		return sdk.OutcomeAccountRateLimited
	}
	if statusCode == 402 &&
		(isDeactivatedWorkspaceText(message) || isWebSocketPaymentRequiredText(message)) {
		return sdk.OutcomeAccountDead
	}
	if (statusCode == 400 || statusCode == 403) && isDisabledAccountText(message) {
		return sdk.OutcomeAccountDead
	}
	switch statusCode {
	case 429:
		return sdk.OutcomeAccountRateLimited
	case 401:
		return sdk.OutcomeAccountDead
	case 403:
		return sdk.OutcomeAccountUnavailable
	}
	if statusCode >= 500 {
		return sdk.OutcomeUpstreamTransient
	}
	if statusCode >= 400 {
		return sdk.OutcomeClientError
	}
	return sdk.OutcomeSuccess
}

// classifyAnthropicBody 从 Anthropic 错误响应体（400 可能是账号级）归一化 OutcomeKind。
func classifyAnthropicBody(statusCode int, body []byte) sdk.OutcomeKind {
	msg := gjson.GetBytes(body, "error.message").String()
	if msg == "" {
		msg = string(body)
	}
	return classifyHTTPFailureResponse(statusCode, body, msg)
}

// classifyHTTPFailureResponse 在通用 HTTP 分类基础上读取结构化错误体。
// 传输层只提交完整上游响应，不感知具体账号处置策略。
func classifyHTTPFailureResponse(statusCode int, body []byte, message string) sdk.OutcomeKind {
	if statusCode >= 400 {
		if rejection, ok := parseExplicitUpstreamError(body); ok && isExplicitTextHashRejectionCode(rejection.Code) {
			return sdk.OutcomeClientError
		}
	}
	if (statusCode == http.StatusPaymentRequired || statusCode == http.StatusForbidden) &&
		(hasQuotaExhaustionCode(body) || isBalanceExhaustion(message, body)) {
		return sdk.OutcomeAccountQuotaExhausted
	}
	return classifyHTTPFailure(statusCode, message)
}

func hasQuotaExhaustionCode(body []byte) bool {
	for _, path := range []string{"error.code", "response.error.code", "code"} {
		if isQuotaExhaustionCode(gjson.GetBytes(body, path).String()) {
			return true
		}
	}
	return false
}

// isQuotaExhaustionCode 判断结构化错误码是否表示额度/余额已经耗尽。
// 必须同时命中"额度主体"（quota / balance / credit）与"耗尽语义"
// （insufficient / exhausted / exceeded / depleted），
// 这样 quota_information_unavailable 之类的非耗尽错误码不会被误判为耗尽。
func isQuotaExhaustionCode(code string) bool {
	tokens := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(code)), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	hasSubject := false
	hasExhaustion := false
	for _, token := range tokens {
		switch token {
		case "quota", "balance", "credit", "credits":
			hasSubject = true
		case "insufficient", "exhausted", "exceeded", "depleted":
			hasExhaustion = true
		}
	}
	return hasSubject && hasExhaustion
}

// balanceExhaustionScanLimit 限制对上游错误正文的回退扫描长度。
// 余额提示都是短句，而错误正文上限是 1 MiB（readLimitedErrorBody），
// 为它做整份拷贝既慢又制造大量瞬时垃圾。
const balanceExhaustionScanLimit = 4 << 10

// isBalanceExhaustion 判断上游错误是否表示余额已经耗尽。
// 先看调用方已提取的 message（各错误路径都会填，通常是正文本身或其截断），
// 命中即返回；未命中再扫正文前缀，兜住 message 为空或未取到正文的错误路径。
func isBalanceExhaustion(message string, body []byte) bool {
	if isBalanceExhaustionText(message) {
		return true
	}
	if len(body) == 0 {
		return false
	}
	if len(body) > balanceExhaustionScanLimit {
		body = body[:balanceExhaustionScanLimit]
	}
	return isBalanceExhaustionText(string(body))
}

// isBalanceExhaustionText 判断一段文本是否表示余额已经耗尽。
// 部分上游（尤其预付费中转）不返回结构化 code，只在 402 / 403 里回纯文本，例如：
//
//	Account balance is exhausted. Please recharge or redeem a card before retrying.
//
// 耗尽语义要求与余额主体相邻出现，避免把 "insufficient permissions for the
// balance endpoint" 这类无关 403 误判为余额耗尽。
// 这里刻意不认裸 quota：纯文本的 quota exceeded 仍由 isTemporaryRateLimitText
// 归入 AccountRateLimited，保持与既有 429 / 400 usage limit 路径一致。
func isBalanceExhaustionText(text string) bool {
	combined := strings.ToLower(text)
	if combined == "" {
		return false
	}
	for _, signal := range []string{
		"insufficient balance",
		"insufficient_balance",
		"insufficient credit",
		"insufficient_credit",
		"insufficient credits",
		"balance is exhausted",
		"balance has been exhausted",
		"balance exhausted",
		"balance is insufficient",
		"balance is too low",
		"balance too low",
		"balance exceeded",
		"balance depleted",
		"no balance remaining",
		"credit is exhausted",
		"credit has been exhausted",
		"credit exhausted",
		"credit is insufficient",
		"credit is too low",
		"credit exceeded",
		"credit depleted",
		"no credits remaining",
		"no credit remaining",
	} {
		if strings.Contains(combined, signal) {
			return true
		}
	}
	if !strings.Contains(combined, "balance") && !strings.Contains(combined, "credit") {
		return false
	}
	return strings.Contains(combined, "recharge") ||
		strings.Contains(combined, "redeem") ||
		strings.Contains(combined, "top up") ||
		strings.Contains(combined, "add funds")
}

func isTemporaryRateLimitText(parts ...string) bool {
	combined := strings.ToLower(strings.Join(parts, " "))
	if combined == "" {
		return false
	}
	return strings.Contains(combined, "usage limit") ||
		strings.Contains(combined, "rate limit") ||
		strings.Contains(combined, "too many requests") ||
		strings.Contains(combined, "quota exceeded") ||
		strings.Contains(combined, "insufficient quota") ||
		strings.Contains(combined, "insufficient_quota") ||
		strings.Contains(combined, "billing hard limit") ||
		strings.Contains(combined, "billing_hard_limit_reached") ||
		strings.Contains(combined, "slow down") ||
		strings.Contains(combined, "slow_down") ||
		strings.Contains(combined, "try again later") ||
		strings.Contains(combined, "try again in") ||
		strings.Contains(combined, "retry after")
}

func isOverloadedText(parts ...string) bool {
	combined := strings.ToLower(strings.Join(parts, " "))
	if combined == "" {
		return false
	}
	return strings.Contains(combined, "overloaded")
}

func isDisabledAccountText(parts ...string) bool {
	combined := strings.ToLower(strings.Join(parts, " "))
	if combined == "" {
		return false
	}
	return strings.Contains(combined, "disabled") ||
		strings.Contains(combined, "deactivated") ||
		strings.Contains(combined, "suspended") ||
		strings.Contains(combined, "not an active member of the selected workspace") ||
		strings.Contains(combined, "personal access token owner is inactive")
}

func isDeactivatedWorkspaceText(parts ...string) bool {
	combined := strings.ToLower(strings.Join(parts, " "))
	if combined == "" {
		return false
	}
	return strings.Contains(combined, "deactivated_workspace")
}

func isWebSocketPaymentRequiredText(parts ...string) bool {
	combined := strings.ToLower(strings.Join(parts, " "))
	return strings.Contains(combined, "websocket 握手失败: payment required (http 402)")
}

func isModelUnsupportedText(parts ...string) bool {
	combined := strings.ToLower(strings.Join(parts, " "))
	if combined == "" {
		return false
	}
	directSignals := []string{
		"model_not_found",
		"model_not_supported",
		"invalid_model",
		"unsupported_model",
		"no such model",
	}
	for _, signal := range directSignals {
		if strings.Contains(combined, signal) {
			return true
		}
	}
	if !strings.Contains(combined, "model") {
		return false
	}
	return strings.Contains(combined, "not supported") ||
		strings.Contains(combined, "unsupported") ||
		strings.Contains(combined, "does not support") ||
		strings.Contains(combined, "does not exist") ||
		strings.Contains(combined, "not found") ||
		strings.Contains(combined, "not available") ||
		strings.Contains(combined, "invalid model")
}

// anthropicErrorType 根据 HTTP 状态码返回 Anthropic 错误类型。
func anthropicErrorType(statusCode int) string {
	switch statusCode {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 422:
		return "invalid_model_error"
	case 429:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

func anthropicErrorJSON(errType, message string) []byte {
	return anthropicErrorJSONWithCode(errType, "", message)
}

// anthropicErrorJSONWithCode 透传可机器读的 error.code（例如 safety_rejected）。
// 仅当 code 非空时才输出该字段，避免影响既有不带 code 的回包。
func anthropicErrorJSONWithCode(errType, code, message string) []byte {
	out := `{"type":"error","error":{"type":"","message":""}}`
	out, _ = sjson.Set(out, "error.type", errType)
	out, _ = sjson.Set(out, "error.message", message)
	if code != "" {
		out, _ = sjson.Set(out, "error.code", code)
	}
	return []byte(out)
}

func openAIErrorJSON(errType, code, message string) []byte {
	out := `{"error":{"message":"","type":"","code":""}}`
	out, _ = sjson.Set(out, "error.message", message)
	out, _ = sjson.Set(out, "error.type", errType)
	out, _ = sjson.Set(out, "error.code", code)
	return []byte(out)
}

func openAIErrorTypeForStatus(statusCode int) string {
	switch {
	case statusCode == http.StatusTooManyRequests:
		return "rate_limit_error"
	case statusCode >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// extractRetryAfterHeader 从响应头提取 Retry-After。
func extractRetryAfterHeader(headers http.Header) time.Duration {
	val := headers.Get("Retry-After")
	if val == "" {
		return 0
	}
	return parseRetryDelay("try again in " + val + "s")
}

// truncate 截断字符串到指定长度。
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func readLimitedErrorBody(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, maxErrorResponseBodyBytes+1))
}
