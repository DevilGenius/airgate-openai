package gateway

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

const (
	openAIAuthModeAgentIdentity = "agentIdentity"
	agentIdentityAuthAPIBaseURL = "https://auth.openai.com/api/accounts"
	agentIdentityTaskTimeout    = 30 * time.Second
)

// Kept as a variable so protocol tests can point registration at a local
// httptest server without changing production configuration.
var openAIAgentIdentityAuthAPIBaseURL = agentIdentityAuthAPIBaseURL

var agentIdentityJSONWhitespaceStripper = strings.NewReplacer(
	" ", "",
	"\t", "",
	"\r", "",
	"\n", "",
)

var (
	errAgentIdentityTaskRegistrationRequestFailed   = errors.New("agent identity task 注册请求失败")
	errAgentIdentityTaskRegistrationResponseInvalid = errors.New("agent identity task 注册响应格式无效")
	errAgentIdentityTaskRegistrationTaskIDMissing   = errors.New("agent identity 注册响应缺少 task_id")
)

type agentIdentityTaskRegistrationHTTPError struct {
	StatusCode int
}

func (e *agentIdentityTaskRegistrationHTTPError) Error() string {
	if e == nil {
		return "agent identity task 注册返回无效 HTTP 状态"
	}
	return fmt.Sprintf("agent identity task 注册返回 HTTP %d", e.StatusCode)
}

func isTransientAgentIdentityTaskRegistrationError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errAgentIdentityTaskRegistrationRequestFailed) ||
		errors.Is(err, errAgentIdentityTaskRegistrationResponseInvalid) ||
		errors.Is(err, errAgentIdentityTaskRegistrationTaskIDMissing) {
		return true
	}
	var statusErr *agentIdentityTaskRegistrationHTTPError
	if !errors.As(err, &statusErr) || statusErr == nil {
		return false
	}
	return statusErr.StatusCode == http.StatusRequestTimeout ||
		statusErr.StatusCode == http.StatusTooEarly ||
		statusErr.StatusCode == http.StatusTooManyRequests ||
		(statusErr.StatusCode >= http.StatusInternalServerError && statusErr.StatusCode < 600)
}

func isTransientAgentIdentityAuthenticationError(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		isTransientAgentIdentityTaskRegistrationError(err)
}

type agentIdentityKey struct {
	runtimeID  string
	privateKey ed25519.PrivateKey
	taskID     string
}

type agentIdentityTaskRegistrationResponse struct {
	TaskID               string `json:"task_id"`
	TaskIDCamel          string `json:"taskId"`
	EncryptedTaskID      string `json:"encrypted_task_id"`
	EncryptedTaskIDCamel string `json:"encryptedTaskId"`
}

type agentIdentityRuntime struct {
	mu sync.Mutex
	// taskID is a process-local hot-path cache. It is intentionally independent
	// of RouteGraph/database reads after the account snapshot has been received.
	taskID map[string]string
	// reportedTaskID prevents a stale RouteGraph snapshot from causing the
	// same task_id to be persisted once per hot-path request.
	reportedTaskID map[string]string
	locks          map[string]*sync.Mutex
}

var processAgentIdentityRuntime agentIdentityRuntime

func currentAgentIdentityRuntime() *agentIdentityRuntime {
	processAgentIdentityRuntime.initialize()
	return &processAgentIdentityRuntime
}

func (r *agentIdentityRuntime) initialize() {
	r.mu.Lock()
	if r.taskID == nil {
		r.taskID = make(map[string]string)
	}
	if r.reportedTaskID == nil {
		r.reportedTaskID = make(map[string]string)
	}
	if r.locks == nil {
		r.locks = make(map[string]*sync.Mutex)
	}
	r.mu.Unlock()
}

func (r *agentIdentityRuntime) lockFor(key string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.locks == nil {
		r.locks = make(map[string]*sync.Mutex)
	}
	lock := r.locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		r.locks[key] = lock
	}
	return lock
}

type agentIdentityRequestState struct {
	mu      sync.Mutex
	updated map[string]string
}

type agentIdentityRequestStateKey struct{}

const (
	agentIdentityStateAuthenticationError     = "\x00agent_identity_authentication_error"
	agentIdentityStateAuthenticationTransient = "\x00agent_identity_authentication_transient"
)

type agentIdentityAuthenticationFailure struct {
	reason    string
	transient bool
}

func withAgentIdentityRequestState(ctx context.Context) context.Context {
	return context.WithValue(ctx, agentIdentityRequestStateKey{}, &agentIdentityRequestState{})
}

func agentIdentityRequestStateFromContext(ctx context.Context) *agentIdentityRequestState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(agentIdentityRequestStateKey{}).(*agentIdentityRequestState)
	return state
}

func recordAgentIdentityUpdatedCredential(ctx context.Context, key, value string) bool {
	state := agentIdentityRequestStateFromContext(ctx)
	if state == nil || strings.TrimSpace(key) == "" {
		return false
	}
	state.mu.Lock()
	if state.updated == nil {
		state.updated = make(map[string]string)
	}
	state.updated[key] = value
	state.mu.Unlock()
	return true
}

func recordAgentIdentityAuthenticationError(ctx context.Context, err error) {
	state := agentIdentityRequestStateFromContext(ctx)
	if state == nil || err == nil {
		return
	}
	state.mu.Lock()
	if state.updated == nil {
		state.updated = make(map[string]string)
	}
	state.updated[agentIdentityStateAuthenticationError] = err.Error()
	if isTransientAgentIdentityAuthenticationError(err) {
		state.updated[agentIdentityStateAuthenticationTransient] = "1"
	} else {
		delete(state.updated, agentIdentityStateAuthenticationTransient)
	}
	state.mu.Unlock()
}

func agentIdentityRequestResult(ctx context.Context) (map[string]string, agentIdentityAuthenticationFailure, bool) {
	state := agentIdentityRequestStateFromContext(ctx)
	if state == nil {
		return nil, agentIdentityAuthenticationFailure{}, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	var updated map[string]string
	var failure agentIdentityAuthenticationFailure
	hasAuthenticationFailure := false
	if len(state.updated) > 0 {
		updated = make(map[string]string, len(state.updated))
		for key, value := range state.updated {
			switch key {
			case agentIdentityStateAuthenticationError:
				failure.reason = value
				hasAuthenticationFailure = true
			case agentIdentityStateAuthenticationTransient:
				failure.transient = value == "1"
			default:
				updated[key] = value
			}
		}
		if len(updated) == 0 {
			updated = nil
		}
	}
	return updated, failure, hasAuthenticationFailure
}

func mergeAgentIdentityUpdatedCredentials(outcome *sdk.ForwardOutcome, ctx context.Context) {
	if outcome == nil {
		return
	}
	updated, authFailure, hasAuthFailure := agentIdentityRequestResult(ctx)
	if hasAuthFailure && outcome.Kind != sdk.OutcomeSuccess {
		if authFailure.transient {
			outcome.Kind = sdk.OutcomeUpstreamTransient
			outcome.Upstream = sdk.UpstreamResponse{StatusCode: http.StatusBadGateway}
		} else {
			outcome.Kind = sdk.OutcomeAccountDead
			outcome.Upstream = sdk.UpstreamResponse{StatusCode: http.StatusUnauthorized}
		}
		outcome.FailoverScope = sdk.FailoverScopeNone
		outcome.RetryAfter = 0
		if strings.TrimSpace(outcome.Reason) == "" {
			outcome.Reason = authFailure.reason
		}
	}
	if len(updated) == 0 {
		return
	}
	if outcome.UpdatedCredentials == nil {
		outcome.UpdatedCredentials = make(map[string]string, len(updated))
	}
	for key, value := range updated {
		outcome.UpdatedCredentials[key] = value
	}
}

func normalizeAgentIdentityMode(value string) string {
	mode := strings.ToLower(strings.TrimSpace(value))
	mode = strings.ReplaceAll(mode, "_", "")
	return mode
}

func isOpenAIAgentIdentityCredentials(credentials map[string]string) bool {
	if credentials == nil {
		return false
	}
	mode := credentials["auth_mode"]
	if strings.TrimSpace(mode) == "" {
		mode = credentials["authMode"]
	}
	if normalizeAgentIdentityMode(mode) == normalizeAgentIdentityMode(openAIAuthModeAgentIdentity) {
		return true
	}
	// Be tolerant of hand-edited/imported credentials that contain the
	// unmistakable key pair but omitted auth_mode.
	runtimeID := credentials["agent_runtime_id"]
	if strings.TrimSpace(runtimeID) == "" {
		runtimeID = credentials["agentRuntimeId"]
	}
	privateKey := credentials["agent_private_key"]
	if strings.TrimSpace(privateKey) == "" {
		privateKey = credentials["agentPrivateKey"]
	}
	return strings.TrimSpace(runtimeID) != "" && strings.TrimSpace(privateKey) != ""
}

func isOpenAIAgentIdentityAccount(account *sdk.Account) bool {
	return account != nil && isOpenAIAgentIdentityCredentials(account.Credentials)
}

func isOpenAIOAuthCredentials(credentials map[string]string) bool {
	return strings.TrimSpace(credentials["access_token"]) != "" ||
		isOpenAIAgentIdentityCredentials(credentials)
}

func agentIdentityPrivateKey(credentials map[string]string) (ed25519.PrivateKey, error) {
	raw := strings.TrimSpace(credentials["agent_private_key"])
	if raw == "" {
		raw = strings.TrimSpace(credentials["agentPrivateKey"])
	}
	if raw == "" {
		return nil, errors.New("agent identity 缺少 agent_private_key")
	}
	der, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		if der, err = base64.RawStdEncoding.DecodeString(raw); err != nil {
			if der, err = base64.RawURLEncoding.DecodeString(raw); err != nil {
				if der, err = base64.URLEncoding.DecodeString(raw); err != nil {
					return nil, errors.New("agent identity 的 agent_private_key 不是有效 Base64")
				}
			}
		}
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, errors.New("agent identity 的 agent_private_key 不是有效 PKCS#8")
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("agent identity 的 agent_private_key 不是 Ed25519 私钥")
	}
	return privateKey, nil
}

func agentIdentityKeyFromCredentials(credentials map[string]string, taskID string) (agentIdentityKey, error) {
	privateKey, err := agentIdentityPrivateKey(credentials)
	if err != nil {
		return agentIdentityKey{}, err
	}
	runtimeID := strings.TrimSpace(credentials["agent_runtime_id"])
	if runtimeID == "" {
		runtimeID = strings.TrimSpace(credentials["agentRuntimeId"])
	}
	if runtimeID == "" {
		return agentIdentityKey{}, errors.New("agent identity 缺少 agent_runtime_id")
	}
	if strings.TrimSpace(taskID) == "" {
		taskID = strings.TrimSpace(credentials["task_id"])
		if taskID == "" {
			taskID = strings.TrimSpace(credentials["taskId"])
		}
	}
	return agentIdentityKey{
		runtimeID:  runtimeID,
		privateKey: privateKey,
		taskID:     strings.TrimSpace(taskID),
	}, nil
}

func buildAgentAssertion(key agentIdentityKey, now time.Time) (string, error) {
	if key.runtimeID == "" || key.taskID == "" {
		return "", errors.New("agent identity 缺少 runtime_id 或 task_id")
	}
	timestamp := now.UTC().Format(time.RFC3339)
	payload := []byte(key.runtimeID + ":" + key.taskID + ":" + timestamp)
	signature, err := key.privateKey.Sign(nil, payload, crypto.Hash(0))
	if err != nil {
		return "", errors.New("agent identity assertion 签名失败")
	}
	envelope, err := json.Marshal(map[string]string{
		"agent_runtime_id": key.runtimeID,
		"task_id":          key.taskID,
		"timestamp":        timestamp,
		"signature":        base64.StdEncoding.EncodeToString(signature),
	})
	if err != nil {
		return "", errors.New("agent identity assertion 序列化失败")
	}
	return "AgentAssertion " + base64.RawURLEncoding.EncodeToString(envelope), nil
}

func signAgentTaskRegistration(key agentIdentityKey, now time.Time) (string, string, error) {
	if key.runtimeID == "" {
		return "", "", errors.New("agent identity 缺少 agent_runtime_id")
	}
	timestamp := now.UTC().Format(time.RFC3339)
	signature, err := key.privateKey.Sign(nil, []byte(key.runtimeID+":"+timestamp), crypto.Hash(0))
	if err != nil {
		return "", "", errors.New("agent identity task 注册签名失败")
	}
	return timestamp, base64.StdEncoding.EncodeToString(signature), nil
}

func decryptAgentTaskID(key agentIdentityKey, encoded string) (string, error) {
	encoded = strings.TrimSpace(encoded)
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		if ciphertext, err = base64.RawStdEncoding.DecodeString(encoded); err != nil {
			if ciphertext, err = base64.RawURLEncoding.DecodeString(encoded); err != nil {
				return "", errors.New("agent identity encrypted task_id 不是有效 Base64")
			}
		}
	}
	seed := key.privateKey.Seed()
	digest := sha512.Sum512(seed)
	var curvePrivate [32]byte
	copy(curvePrivate[:], digest[:32])
	curvePrivate[0] &= 248
	curvePrivate[31] &= 127
	curvePrivate[31] |= 64
	curvePublicBytes, err := curve25519.X25519(curvePrivate[:], curve25519.Basepoint)
	if err != nil {
		return "", errors.New("agent identity 解密密钥派生失败")
	}
	var curvePublic [32]byte
	copy(curvePublic[:], curvePublicBytes)
	plaintext, ok := box.OpenAnonymous(nil, ciphertext, &curvePublic, &curvePrivate)
	if !ok {
		return "", errors.New("agent identity encrypted task_id 解密失败")
	}
	taskID := strings.TrimSpace(string(plaintext))
	if taskID == "" {
		return "", errors.New("agent identity 注册响应没有有效 task_id")
	}
	return taskID, nil
}

func agentIdentityCacheKey(account *sdk.Account) string {
	if account != nil && account.ID != 0 {
		return "account:" + strconv.FormatInt(account.ID, 10)
	}
	if account != nil {
		runtimeID := strings.TrimSpace(account.Credentials["agent_runtime_id"])
		if runtimeID == "" {
			runtimeID = strings.TrimSpace(account.Credentials["agentRuntimeId"])
		}
		return "runtime:" + runtimeID
	}
	return "runtime:"
}

func (g *OpenAIGateway) registerAgentIdentityTask(ctx context.Context, account *sdk.Account) (string, error) {
	if account == nil {
		return "", errors.New("agent identity account 为空")
	}
	key, err := agentIdentityKeyFromCredentials(account.Credentials, "")
	if err != nil {
		return "", err
	}
	timestamp, signature, err := signAgentTaskRegistration(key, time.Now())
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]string{
		"timestamp": timestamp,
		"signature": signature,
	})
	if err != nil {
		return "", errors.New("agent identity task 注册请求序列化失败")
	}
	endpoint := strings.TrimRight(strings.TrimSpace(openAIAgentIdentityAuthAPIBaseURL), "/") +
		"/v1/agent/" + url.PathEscape(key.runtimeID) + "/task/register"
	registerCtx, cancel := context.WithTimeout(ctx, agentIdentityTaskTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(registerCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", errors.New("agent identity task 注册请求构造失败")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := g.buildHTTPClient(account)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errAgentIdentityTaskRegistrationRequestFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", &agentIdentityTaskRegistrationHTTPError{StatusCode: resp.StatusCode}
	}
	var result agentIdentityTaskRegistrationResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&result); err != nil {
		return "", fmt.Errorf("%w: %v", errAgentIdentityTaskRegistrationResponseInvalid, err)
	}
	if taskID := strings.TrimSpace(result.TaskID); taskID != "" {
		return taskID, nil
	}
	if taskID := strings.TrimSpace(result.TaskIDCamel); taskID != "" {
		return taskID, nil
	}
	encrypted := strings.TrimSpace(result.EncryptedTaskID)
	if encrypted == "" {
		encrypted = strings.TrimSpace(result.EncryptedTaskIDCamel)
	}
	if encrypted == "" {
		return "", errAgentIdentityTaskRegistrationTaskIDMissing
	}
	taskID, err := decryptAgentTaskID(key, encrypted)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errAgentIdentityTaskRegistrationResponseInvalid, err)
	}
	return taskID, nil
}

func (g *OpenAIGateway) ensureAgentIdentityTask(ctx context.Context, account *sdk.Account, force bool) (string, bool, error) {
	if !isOpenAIAgentIdentityAccount(account) {
		return "", false, errors.New("不是 Agent Identity account")
	}
	if g == nil {
		return "", false, errors.New("OpenAI gateway 为空")
	}
	runtime := currentAgentIdentityRuntime()
	cacheKey := agentIdentityCacheKey(account)
	accountLock := runtime.lockFor(cacheKey)
	accountLock.Lock()
	defer accountLock.Unlock()
	storedTaskID := strings.TrimSpace(account.Credentials["task_id"])
	if storedTaskID == "" {
		storedTaskID = strings.TrimSpace(account.Credentials["taskId"])
	}
	runtime.mu.Lock()
	cached := strings.TrimSpace(runtime.taskID[cacheKey])
	runtime.mu.Unlock()
	if force && cached != "" && cached != storedTaskID {
		// Another concurrent request may have completed recovery while this
		// request was waiting for the per-account lock. Reuse its task instead
		// of registering a second task.
		updated := false
		runtime.mu.Lock()
		shouldReport := runtime.reportedTaskID[cacheKey] != cached
		runtime.mu.Unlock()
		if shouldReport {
			if recordAgentIdentityUpdatedCredential(ctx, "task_id", cached) {
				runtime.mu.Lock()
				runtime.reportedTaskID[cacheKey] = cached
				runtime.mu.Unlock()
				updated = true
			}
		}
		return cached, updated, nil
	}
	if !force {
		if cached != "" {
			updated := false
			runtime.mu.Lock()
			shouldReport := cached != storedTaskID && runtime.reportedTaskID[cacheKey] != cached
			runtime.mu.Unlock()
			if shouldReport {
				if recordAgentIdentityUpdatedCredential(ctx, "task_id", cached) {
					runtime.mu.Lock()
					runtime.reportedTaskID[cacheKey] = cached
					runtime.mu.Unlock()
					updated = true
				}
			}
			return cached, updated, nil
		}
		existing := strings.TrimSpace(account.Credentials["task_id"])
		if existing == "" {
			existing = strings.TrimSpace(account.Credentials["taskId"])
		}
		if existing != "" {
			runtime.mu.Lock()
			runtime.taskID[cacheKey] = existing
			runtime.mu.Unlock()
			return existing, false, nil
		}
	}
	taskID, err := g.registerAgentIdentityTask(ctx, account)
	if err != nil {
		return "", false, err
	}
	previous := strings.TrimSpace(account.Credentials["task_id"])
	if previous == "" {
		previous = strings.TrimSpace(account.Credentials["taskId"])
	}
	runtime.mu.Lock()
	runtime.taskID[cacheKey] = taskID
	shouldReport := taskID != previous && runtime.reportedTaskID[cacheKey] != taskID
	runtime.mu.Unlock()
	updated := false
	if shouldReport {
		if recordAgentIdentityUpdatedCredential(ctx, "task_id", taskID) {
			runtime.mu.Lock()
			runtime.reportedTaskID[cacheKey] = taskID
			runtime.mu.Unlock()
			updated = true
		}
	}
	return taskID, updated, nil
}

// agentIdentityTaskIDFromAuthHeaders runs only on an invalid-task response; the
// normal forwarding path does not decode assertions.
func agentIdentityTaskIDFromAuthHeaders(headers http.Header) (string, error) {
	const prefix = "AgentAssertion "
	authorization := strings.TrimSpace(headers.Get("Authorization"))
	if !strings.HasPrefix(authorization, prefix) {
		return "", errors.New("agent identity assertion 缺失")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(authorization, prefix)))
	if err != nil {
		return "", errors.New("agent identity assertion 不是有效 Base64URL")
	}
	var envelope struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return "", errors.New("agent identity assertion 格式无效")
	}
	taskID := strings.TrimSpace(envelope.TaskID)
	if taskID == "" {
		return "", errors.New("agent identity assertion 缺少 task_id")
	}
	return taskID, nil
}

// invalidateAgentIdentityTask evicts only the task used by the rejected
// request. A stale concurrent failure therefore cannot delete a newer task
// that another request has already registered.
func (g *OpenAIGateway) invalidateAgentIdentityTask(account *sdk.Account, failedAuthHeaders http.Header) {
	if g == nil || account == nil {
		return
	}
	runtime := currentAgentIdentityRuntime()
	failedTaskID, err := agentIdentityTaskIDFromAuthHeaders(failedAuthHeaders)
	if err != nil {
		return
	}
	cacheKey := agentIdentityCacheKey(account)
	accountLock := runtime.lockFor(cacheKey)
	accountLock.Lock()
	defer accountLock.Unlock()
	runtime.mu.Lock()
	if strings.TrimSpace(runtime.taskID[cacheKey]) == failedTaskID {
		delete(runtime.taskID, cacheKey)
		if runtime.reportedTaskID[cacheKey] == failedTaskID {
			delete(runtime.reportedTaskID, cacheKey)
		}
	}
	runtime.mu.Unlock()
}

func (g *OpenAIGateway) buildOpenAIAuthHeaders(ctx context.Context, account *sdk.Account, forceTaskRecovery bool) (http.Header, error) {
	if account == nil {
		return nil, errors.New("account 为空")
	}
	if isOpenAIAgentIdentityAccount(account) {
		taskID, _, err := g.ensureAgentIdentityTask(ctx, account, forceTaskRecovery)
		if err != nil {
			recordAgentIdentityAuthenticationError(ctx, err)
			return nil, err
		}
		key, err := agentIdentityKeyFromCredentials(account.Credentials, taskID)
		if err != nil {
			recordAgentIdentityAuthenticationError(ctx, err)
			return nil, err
		}
		assertion, err := buildAgentAssertion(key, time.Now())
		if err != nil {
			recordAgentIdentityAuthenticationError(ctx, err)
			return nil, err
		}
		return http.Header{"Authorization": []string{assertion}}, nil
	}
	token := strings.TrimSpace(account.Credentials["access_token"])
	if token == "" {
		return nil, errors.New("OAuth account 缺少 access_token")
	}
	return http.Header{"Authorization": []string{"Bearer " + token}}, nil
}

func isAgentIdentityTaskInvalidHTTPResponse(statusCode int, body []byte) bool {
	if statusCode != http.StatusUnauthorized {
		return false
	}
	lower := strings.ToLower(string(body))
	compact := agentIdentityJSONWhitespaceStripper.Replace(lower)
	for _, marker := range []string{
		`"code":"invalid_task_id"`,
		`"code":"task_not_found"`,
		`"code":"task_expired"`,
		`"error":"invalid_task_id"`,
	} {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	for _, marker := range []string{
		"invalid task_id",
		"invalid task id",
		"task_id is invalid",
		"task id is invalid",
		"task not found",
		"task expired",
		"unknown task_id",
		"unknown task id",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isAgentIdentityTaskInvalidWSError(statusCode int, err error) bool {
	if err == nil || statusCode != http.StatusUnauthorized {
		return false
	}
	return isAgentIdentityTaskInvalidHTTPResponse(statusCode, []byte(err.Error()))
}

func isAgentIdentityTaskInvalidWSResult(result WSResult) bool {
	if result.Err == nil {
		return false
	}
	var failure *responsesFailureError
	if errors.As(result.Err, &failure) && failure != nil {
		code := strings.ToLower(strings.TrimSpace(failure.Code))
		if strings.Contains(code, "invalid_task") || strings.Contains(code, "task_not_found") || strings.Contains(code, "task_expired") {
			return true
		}
	}
	return isAgentIdentityTaskInvalidHTTPResponse(http.StatusUnauthorized, []byte(result.Err.Error()))
}

func (g *OpenAIGateway) refreshAgentIdentityToken(ctx context.Context, credentials map[string]string) (*tokenRefreshInfo, error) {
	account := &sdk.Account{
		Credentials: credentials,
		ProxyURL:    credentials["proxy_url"],
	}
	taskID, _, err := g.ensureAgentIdentityTask(ctx, account, false)
	if err != nil {
		return nil, err
	}
	extra := map[string]string{
		"plan_type":                 credentials["plan_type"],
		"subscription_active_until": credentials["subscription_active_until"],
	}
	storedTaskID := strings.TrimSpace(credentials["task_id"])
	if storedTaskID == "" {
		storedTaskID = strings.TrimSpace(credentials["taskId"])
	}
	if taskID != storedTaskID {
		extra["task_id"] = taskID
	}
	return &tokenRefreshInfo{ExpiresAt: credentials["subscription_active_until"], Extra: extra}, nil
}
