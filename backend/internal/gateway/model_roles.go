package gateway

import (
	"os"
	"strings"
)

const (
	defaultLongContextModel = "gpt-5.6-sol"
	longContextModelEnv     = "AIRGATE_MODEL_LONG_CONTEXT"
)

// configuredLongContextModel 返回 context window 超限后的统一重路由目标。环境变量允许在
// 模型下线或替换时直接切换，不需要修改请求缓存或调度控制逻辑。
func configuredLongContextModel() string {
	return resolveRoleTargetModel(defaultLongContextModel, longContextModelEnv)
}

func resolveRoleTargetModel(fallback string, keys ...string) string {
	return normalizeModelID(firstNonEmptyEnv(keys...), fallback)
}

func firstNonEmptyEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

// normalizeModelID 统一模型角色配置中的 provider 前缀和 @ 映射形式。
func normalizeModelID(raw string, fallback string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return fallback
	}
	if idx := strings.LastIndex(value, "@"); idx >= 0 && idx+1 < len(value) {
		value = strings.TrimSpace(value[idx+1:])
	}
	for _, prefix := range []string{"openai/", "oai/"} {
		if len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
			value = value[len(prefix):]
			break
		}
	}
	if value == "" {
		return fallback
	}
	return value
}

func modelIDsEqual(a, b string) bool {
	left := normalizeModelID(a, "")
	right := normalizeModelID(b, "")
	return left != "" && right != "" && strings.EqualFold(left, right)
}
