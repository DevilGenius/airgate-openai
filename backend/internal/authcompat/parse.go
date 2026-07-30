package authcompat

import (
	"errors"
	"sort"
	"strings"
)

const metaFileName = "_auths.meta.json"

type authMeta struct {
	byFile  map[string]map[string]any
	byEmail map[string]map[string]any
}

// Parse 将上传人指定格式的兼容凭据文件解析为 OpenAI 账号草稿。
// 单文件失败不会阻止其他文件继续解析，所有内容都只在内存中处理。
func Parse(format Format, files []InputFile) (Result, error) {
	format, err := normalizeFormat(format)
	if err != nil {
		return Result{}, err
	}
	if len(files) == 0 {
		return Result{}, errors.New("没有可解析的文件")
	}

	sorted := append([]InputFile(nil), files...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return strings.ToLower(sorted[i].Name) < strings.ToLower(sorted[j].Name)
	})

	meta := authMeta{byFile: map[string]map[string]any{}, byEmail: map[string]map[string]any{}}
	metaIssues := []Issue{}
	if format == FormatCodex {
		meta, metaIssues = readAuthMeta(sorted)
	}
	result := Result{Format: string(format), Accounts: []Account{}, Issues: metaIssues}
	for _, file := range sorted {
		if format == FormatCodex && strings.EqualFold(inputFileBaseName(file.Name), metaFileName) {
			continue
		}
		if len(file.Content) == 0 {
			result.Issues = append(result.Issues, Issue{File: file.Name, Level: "warning", Message: "文件为空"})
			continue
		}

		var value any
		if err := decodeJSON(file.Content, &value); err != nil {
			result.Issues = append(result.Issues, Issue{File: file.Name, Level: "warning", Message: "JSON 解析失败: " + err.Error()})
			continue
		}
		accounts, issues := parseValue(format, value, file.Name, meta)
		result.Accounts = append(result.Accounts, accounts...)
		result.Issues = append(result.Issues, issues...)
	}

	if len(result.Accounts) == 0 {
		return result, errors.New("未识别到可导入的 OpenAI 账号")
	}
	return result, nil
}

func normalizeFormat(format Format) (Format, error) {
	normalized := Format(strings.ToLower(strings.TrimSpace(string(format))))
	switch normalized {
	case FormatSub2API, FormatCPA, FormatCodex, FormatCockpit, FormatAgentIdentity, FormatAccountJSON:
		return normalized, nil
	default:
		return "", errors.New("请选择支持的凭据格式")
	}
}

func readAuthMeta(files []InputFile) (authMeta, []Issue) {
	meta := authMeta{byFile: map[string]map[string]any{}, byEmail: map[string]map[string]any{}}
	issues := []Issue{}
	for _, file := range files {
		if !strings.EqualFold(inputFileBaseName(file.Name), metaFileName) {
			continue
		}
		var source map[string]any
		if err := decodeJSON(file.Content, &source); err != nil {
			issues = append(issues, Issue{File: file.Name, Level: "warning", Message: "元数据解析失败: " + err.Error()})
			continue
		}
		for _, raw := range asSlice(source["accounts"]) {
			entry := asMap(raw)
			if entry == nil {
				continue
			}
			if fileName := strings.ToLower(firstString(entry["file"])); fileName != "" {
				meta.byFile[fileName] = entry
			}
			if email := strings.ToLower(firstString(entry["email"])); email != "" {
				meta.byEmail[email] = entry
			}
		}
	}
	return meta, issues
}

func parseValue(format Format, value any, fileName string, meta authMeta) ([]Account, []Issue) {
	accounts := []Account{}
	issues := []Issue{}
	appendAccount := func(source map[string]any, index int) {
		prepared := source
		if format == FormatCodex {
			prepared = prepareAuthFileSource(source, fileName, meta)
		}
		account, err := normalizeAccount(format, prepared, fallbackName(fileName))
		if err != nil {
			issues = append(issues, Issue{File: fileName, Index: index, Level: "warning", Message: err.Error()})
			return
		}
		accounts = append(accounts, account)
	}

	switch typed := value.(type) {
	case map[string]any:
		if rawAccounts := asSlice(typed["accounts"]); rawAccounts != nil {
			for index, raw := range rawAccounts {
				source := asMap(raw)
				if source == nil {
					issues = append(issues, Issue{File: fileName, Index: index, Level: "warning", Message: "账号不是 JSON 对象"})
					continue
				}
				appendAccount(source, index)
			}
			return accounts, issues
		}
		if rawAuths := asSlice(typed["auths"]); rawAuths != nil {
			for index, raw := range rawAuths {
				source := asMap(raw)
				if source == nil {
					issues = append(issues, Issue{File: fileName, Index: index, Level: "warning", Message: "凭据不是 JSON 对象"})
					continue
				}
				appendAccount(source, index)
			}
			return accounts, issues
		}
		appendAccount(typed, 0)
	case []any:
		for index, raw := range typed {
			source := asMap(raw)
			if source == nil {
				issues = append(issues, Issue{File: fileName, Index: index, Level: "warning", Message: "账号不是 JSON 对象"})
				continue
			}
			appendAccount(source, index)
		}
	default:
		issues = append(issues, Issue{File: fileName, Level: "warning", Message: "文件内容不是 JSON 对象或数组"})
	}
	return accounts, issues
}

func prepareAuthFileSource(source map[string]any, fileName string, meta authMeta) map[string]any {
	baseFileName := inputFileBaseName(fileName)
	lowerFile := strings.ToLower(baseFileName)
	if !strings.HasSuffix(lowerFile, ".auth.json") {
		return source
	}
	entry := meta.byFile[lowerFile]
	if entry == nil {
		email := strings.ToLower(strings.TrimSuffix(baseFileName, ".auth.json"))
		entry = meta.byEmail[email]
	}
	if entry == nil {
		return source
	}
	prepared := map[string]any{"auth": source}
	if account := asMap(entry["account"]); account != nil {
		mergeMissing(prepared, cloneMap(account))
	}
	if email := firstString(entry["email"]); email != "" {
		if _, exists := prepared["email"]; !exists {
			prepared["email"] = email
		}
	}
	return prepared
}

func normalizeAccount(format Format, source map[string]any, fallback string) (Account, error) {
	if source == nil {
		return Account{}, errors.New("账号内容为空")
	}
	if format == FormatAgentIdentity {
		account, handled, err := normalizeAgentIdentity(source, fallback)
		if !handled {
			return Account{}, errors.New("所选内容不是 Agent Identity 凭据")
		}
		return account, err
	}

	authSource := source
	if embedded := asMap(source["auth"]); embedded != nil {
		authSource = embedded
	}
	credentialsSource := asMap(source["credentials"])
	if credentialsSource == nil {
		credentialsSource = source
	}
	tokenSource := firstTokenSource(authSource, credentialsSource, source)
	sources := []map[string]any{tokenSource, authSource, credentialsSource, source}

	credentials := map[string]string{}
	copyCredentialFields(credentials, sources)
	if credentials["api_key"] != "" {
		email := normalizeEmail(credentials["email"], source["email"], source["name"], fallback)
		if email != "" {
			credentials["email"] = email
		}
		name := firstString(source["name"], credentials["account_name"], email, fallback)
		account := Account{
			Name:           name,
			Type:           "apikey",
			Credentials:    credentials,
			Priority:       intValue(source["priority"], 50),
			MaxConcurrency: intValue(firstNonNil(source["max_concurrency"], source["concurrency"]), 10),
			RateMultiplier: floatValue(source["rate_multiplier"], 1),
		}
		if email != "" {
			account.Email = &email
		}
		return account, nil
	}
	if credentials["access_token"] == "" && credentials["refresh_token"] == "" && credentials["session_token"] == "" {
		return Account{}, errors.New("缺少 access_token、refresh_token 或 session_token")
	}

	accessPayload := decodeJWTPayload(credentials["access_token"])
	idPayload := decodeJWTPayload(credentials["id_token"])
	accessAuth := openAIAuth(accessPayload)
	idAuth := openAIAuth(idPayload)
	profile := openAIProfile(accessPayload)
	userSource := nestedMap(source, "user")
	accountSource := nestedMap(source, "account")
	metaSource := nestedMap(source, "meta")
	providerSource := nestedMap(source, "providerSpecificData")

	email := normalizeEmail(
		credentials["email"], userSource["email"], idPayload["email"], authSource["email"], source["email"],
		metaSource["label"], source["label"], providerSource["email"], profile["email"], accessPayload["email"],
		source["name"], fallback,
	)
	accountID := firstString(
		credentials["chatgpt_account_id"], credentials["account_id"], accountSource["id"],
		accessAuth["chatgpt_account_id"], idAuth["chatgpt_account_id"],
	)
	userID := firstString(
		credentials["chatgpt_user_id"], userSource["id"], accessAuth["chatgpt_user_id"], accessAuth["user_id"],
		idAuth["chatgpt_user_id"], idAuth["user_id"], accessPayload["sub"], idPayload["sub"],
	)
	planType := firstString(
		credentials["plan_type"], accountSource["plan_type"], accountSource["planType"],
		accessAuth["chatgpt_plan_type"], idAuth["chatgpt_plan_type"],
	)
	setCredential(credentials, "chatgpt_account_id", accountID)
	setCredential(credentials, "account_id", accountID)
	setCredential(credentials, "chatgpt_user_id", userID)
	setCredential(credentials, "plan_type", planType)
	if email != "" {
		credentials["email"] = email
	}

	name := firstString(source["name"], credentials["account_name"], email, fallback)
	account := Account{
		Name:           name,
		Type:           "oauth",
		Credentials:    credentials,
		Priority:       intValue(source["priority"], 50),
		MaxConcurrency: intValue(firstNonNil(source["max_concurrency"], source["concurrency"]), 10),
		RateMultiplier: floatValue(source["rate_multiplier"], 1),
	}
	if email != "" {
		account.Email = &email
	}
	return account, nil
}

func firstTokenSource(sources ...map[string]any) map[string]any {
	for _, source := range sources {
		if source == nil {
			continue
		}
		for _, key := range []string{"tokens", "token", "credentials"} {
			if nested := asMap(source[key]); nested != nil {
				return nested
			}
		}
	}
	for _, source := range sources {
		if source != nil {
			return source
		}
	}
	return map[string]any{}
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func candidateMaps(source map[string]any) []map[string]any {
	candidates := []map[string]any{source}
	for _, key := range []string{"user", "account", "meta", "profile", "credentials", "agent_identity", "agentIdentity", "tokens", "token", "auth"} {
		if nested := asMap(source[key]); nested != nil {
			candidates = append(candidates, nested)
			for _, childKey := range []string{"credentials", "agent_identity", "agentIdentity", "tokens", "token"} {
				if child := asMap(nested[childKey]); child != nil {
					candidates = append(candidates, child)
				}
			}
		}
	}
	return candidates
}

func isAgentIdentity(source map[string]any) bool {
	for _, candidate := range candidateMaps(source) {
		mode := strings.ToLower(firstString(candidate["auth_mode"], candidate["authMode"]))
		mode = strings.ReplaceAll(mode, "_", "")
		if mode == "agentidentity" || firstString(candidate["agent_runtime_id"], candidate["agentRuntimeId"]) != "" || firstString(candidate["agent_private_key"], candidate["agentPrivateKey"]) != "" {
			return true
		}
	}
	return false
}

func normalizeAgentIdentity(source map[string]any, fallback string) (Account, bool, error) {
	if !isAgentIdentity(source) {
		return Account{}, false, nil
	}
	candidates := candidateMaps(source)
	credentials := map[string]string{"auth_mode": "agentIdentity"}
	copyCredentialFields(credentials, candidates)
	credentials["auth_mode"] = "agentIdentity"
	if credentials["agent_runtime_id"] == "" || credentials["agent_private_key"] == "" {
		return Account{}, true, errors.New("Agent Identity 缺少 agent_runtime_id 或 agent_private_key")
	}
	emailValues := make([]any, 0, len(candidates)+3)
	for _, candidate := range candidates {
		emailValues = append(emailValues, candidate["email"])
	}
	emailValues = append(emailValues, source["name"], source["email"], fallback)
	email := normalizeEmail(emailValues...)
	if email != "" {
		credentials["email"] = email
	}
	name := firstString(source["name"], credentials["account_name"], email, fallback)
	account := Account{
		Name:           name,
		Type:           "oauth",
		Credentials:    credentials,
		Priority:       intValue(source["priority"], 50),
		MaxConcurrency: intValue(firstNonNil(source["max_concurrency"], source["concurrency"]), 10),
		RateMultiplier: floatValue(source["rate_multiplier"], 1),
	}
	if email != "" {
		account.Email = &email
	}
	return account, true, nil
}
