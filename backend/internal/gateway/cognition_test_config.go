package gateway

import (
	"fmt"
	"regexp"
	"strings"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

func validateCognitionTestConfig(config sdk.PluginConfig) error {
	if config == nil || !config.GetBool("cognition_test_enabled") {
		return nil
	}
	if strings.TrimSpace(config.GetString("cognition_test_prompt")) == "" {
		return fmt.Errorf("降智检测 Prompt 不能为空")
	}
	pattern := config.GetString("cognition_test_regexp")
	if strings.TrimSpace(pattern) == "" {
		return fmt.Errorf("正常回复正则不能为空")
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return fmt.Errorf("正常回复正则无效: %w", err)
	}
	return nil
}
