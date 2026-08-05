package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/DevilGenius/airgate-openai/backend/internal/authcompat"
)

const (
	compatImportMaxInputs              = 1024
	compatImportMaxBytes               = 32 << 20
	compatRefreshTokenImportConcurrent = 4
)

var compatibleImportFromRefreshToken = (*OpenAIGateway).ImportFromRefreshToken

type compatibleImportRequest struct {
	Format string `json:"format"`
	Files  []struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	} `json:"files"`
}

func (g *OpenAIGateway) handleCompatibleAccountImport(
	ctx context.Context,
	method string,
	body []byte,
) (int, http.Header, []byte, error) {
	if strings.ToUpper(strings.TrimSpace(method)) != http.MethodPost {
		return http.StatusMethodNotAllowed, nil, jsonError("method not allowed"), nil
	}
	var raw compatibleImportRequest
	if err := json.Unmarshal(body, &raw); err != nil || len(raw.Files) == 0 {
		return http.StatusBadRequest, nil, jsonError("缺少兼容导入内容"), nil
	}
	if len(raw.Files) > compatImportMaxInputs {
		return http.StatusBadRequest, nil, jsonError(fmt.Sprintf("单次最多导入 %d 项内容", compatImportMaxInputs)), nil
	}

	files := make([]authcompat.InputFile, 0, len(raw.Files))
	totalBytes := 0
	for index, file := range raw.Files {
		name := strings.TrimSpace(file.Name)
		if name == "" {
			name = fmt.Sprintf("account-%d.json", index+1)
		}
		content := []byte(file.Content)
		totalBytes += len(content)
		if totalBytes > compatImportMaxBytes {
			return http.StatusRequestEntityTooLarge, nil, jsonError(fmt.Sprintf("兼容导入内容总大小不能超过 %d MiB", compatImportMaxBytes>>20)), nil
		}
		files = append(files, authcompat.InputFile{Name: name, Content: content})
	}

	format := strings.ToLower(strings.TrimSpace(raw.Format))
	var result authcompat.Result
	var err error
	switch format {
	case "refresh_token", "rt":
		result, err = g.parseCompatibleRefreshTokens(ctx, files)
	default:
		result, err = authcompat.Parse(authcompat.Format(format), files)
	}
	if err != nil {
		return http.StatusBadRequest, nil, jsonError(err.Error()), nil
	}
	result.Accounts = authcompat.Rename(result.Accounts, time.Now())
	result.Renamed = true
	return http.StatusOK, nil, jsonMarshal(result), nil
}

type compatibleRefreshTokenInput struct {
	File         string
	Index        int
	Name         string
	RefreshToken string
	ProxyURL     string
	ClientID     string
}

type compatibleRefreshTokenResult struct {
	Account *authcompat.Account
	Issue   *authcompat.Issue
}

func (g *OpenAIGateway) parseCompatibleRefreshTokens(
	ctx context.Context,
	files []authcompat.InputFile,
) (authcompat.Result, error) {
	inputs, issues, err := compatibleRefreshTokenInputs(files)
	result := authcompat.Result{Format: "refresh_token", Accounts: []authcompat.Account{}, Issues: issues}
	if err != nil {
		return result, err
	}
	if len(inputs) == 0 {
		return result, errors.New("未识别到可导入的 refresh_token")
	}
	if len(inputs) > compatImportMaxInputs {
		return result, fmt.Errorf("单次最多导入 %d 个 refresh_token", compatImportMaxInputs)
	}

	parsed := make([]compatibleRefreshTokenResult, len(inputs))
	jobs := make(chan int)
	workers := min(compatRefreshTokenImportConcurrent, len(inputs))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				input := inputs[index]
				imported, err := compatibleImportFromRefreshToken(g, ctx, input.RefreshToken, input.ProxyURL, input.ClientID)
				if err != nil {
					parsed[index].Issue = &authcompat.Issue{
						File: input.File, Index: input.Index, Level: "warning", Message: err.Error(),
					}
					continue
				}
				if imported == nil {
					parsed[index].Issue = &authcompat.Issue{
						File: input.File, Index: input.Index, Level: "warning", Message: "refresh_token 兑换结果为空",
					}
					continue
				}
				account := compatibleRefreshTokenAccount(input, imported)
				parsed[index].Account = &account
			}
		}()
	}
	for index := range inputs {
		select {
		case jobs <- index:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return result, ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()

	for _, item := range parsed {
		if item.Account != nil {
			result.Accounts = append(result.Accounts, *item.Account)
		}
		if item.Issue != nil {
			result.Issues = append(result.Issues, *item.Issue)
		}
	}
	if len(result.Accounts) == 0 {
		return result, errors.New("所有 refresh_token 均兑换失败")
	}
	return result, nil
}

func compatibleRefreshTokenInputs(files []authcompat.InputFile) ([]compatibleRefreshTokenInput, []authcompat.Issue, error) {
	inputs := make([]compatibleRefreshTokenInput, 0, len(files))
	issues := make([]authcompat.Issue, 0)
	appendInput := func(input compatibleRefreshTokenInput) error {
		if len(inputs) >= compatImportMaxInputs {
			return fmt.Errorf("单次最多导入 %d 个 refresh_token", compatImportMaxInputs)
		}
		inputs = append(inputs, input)
		return nil
	}
	for _, file := range files {
		content := strings.TrimSpace(string(file.Content))
		if content == "" {
			issues = append(issues, authcompat.Issue{File: file.Name, Level: "warning", Message: "refresh_token 内容为空"})
			continue
		}
		if strings.HasPrefix(content, "{") {
			var item struct {
				Name         string `json:"name"`
				RefreshToken string `json:"refresh_token"`
				ProxyURL     string `json:"proxy_url"`
				ClientID     string `json:"client_id"`
			}
			if err := json.Unmarshal(file.Content, &item); err != nil || strings.TrimSpace(item.RefreshToken) == "" {
				issues = append(issues, authcompat.Issue{File: file.Name, Level: "warning", Message: "refresh_token JSON 格式无效"})
				continue
			}
			if err := appendInput(compatibleRefreshTokenInput{
				File: file.Name, Name: strings.TrimSpace(item.Name), RefreshToken: strings.TrimSpace(item.RefreshToken),
				ProxyURL: strings.TrimSpace(item.ProxyURL), ClientID: strings.TrimSpace(item.ClientID),
			}); err != nil {
				return nil, issues, err
			}
			continue
		}

		for lineIndex, token := range strings.Fields(content) {
			if err := appendInput(compatibleRefreshTokenInput{
				File: file.Name, Index: lineIndex, RefreshToken: token,
			}); err != nil {
				return nil, issues, err
			}
		}
	}
	return inputs, issues, nil
}

func compatibleRefreshTokenAccount(input compatibleRefreshTokenInput, imported *OAuthResult) authcompat.Account {
	accountType := strings.TrimSpace(imported.AccountType)
	if accountType == "" {
		accountType = "oauth"
	}
	name := strings.TrimSpace(input.Name)
	preserveName := name != ""
	if name == "" {
		name = strings.TrimSpace(imported.AccountName)
	}
	if name == "" {
		name = strings.TrimSpace(imported.Credentials["email"])
	}
	if name == "" {
		name = input.File
	}
	account := authcompat.Account{
		Name: name, Type: accountType, Credentials: imported.Credentials,
		Priority: 50, MaxConcurrency: 10, RateMultiplier: 1, PreserveName: preserveName,
	}
	if email := strings.TrimSpace(imported.Credentials["email"]); email != "" {
		account.Email = &email
	}
	return account
}
