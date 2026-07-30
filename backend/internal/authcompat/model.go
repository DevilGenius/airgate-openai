package authcompat

// InputFile 是兼容导入器接收的内存文件。解析器不会自行访问文件系统。
type InputFile struct {
	Name    string
	Content []byte
}

// Account 是兼容格式归一化后的 OpenAI 账号草稿。
// Platform 由调用方固定为 openai，因此不进入无依赖解析模型。
type Account struct {
	Name           string            `json:"name"`
	Email          *string           `json:"email,omitempty"`
	Type           string            `json:"type"`
	Credentials    map[string]string `json:"credentials"`
	Priority       int               `json:"priority"`
	MaxConcurrency int               `json:"max_concurrency"`
	RateMultiplier float64           `json:"rate_multiplier"`
}

// Issue 描述某个输入文件或账号未能完整导入的原因。
type Issue struct {
	File    string `json:"file"`
	Index   int    `json:"index,omitempty"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// Result 是一次兼容解析的完整结果。
type Result struct {
	Format   string    `json:"format"`
	Accounts []Account `json:"accounts"`
	Issues   []Issue   `json:"issues,omitempty"`
	Renamed  bool      `json:"renamed"`
}
