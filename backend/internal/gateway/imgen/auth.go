package imgen

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// AuthHeaderProvider 为每个 chatgpt.com 请求生成 Authorization 头。
// Agent Identity 使用它逐请求签名；普通 OAuth 可使用静态 Bearer token。
type AuthHeaderProvider func() (string, error)

// AuthorizationError 表示请求尚未发送前，动态 Authorization 头构建失败。
// 调用方可以通过 errors.As 区分本地认证失败和上游实际返回的 401。
type AuthorizationError struct {
	Err error
}

func (e *AuthorizationError) Error() string {
	if e == nil || e.Err == nil {
		return "构建 Authorization 头失败"
	}
	return "构建 Authorization 头失败: " + e.Err.Error()
}

func (e *AuthorizationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// IsAuthorizationError 判断错误是否来自动态认证头构建阶段。
func IsAuthorizationError(err error) bool {
	var authErr *AuthorizationError
	return errors.As(err, &authErr)
}

type authorizationScope struct {
	cancel context.CancelCauseFunc
}

// authorizationState 将认证提供器、静态 token 和单次生成的失败传播放在
// 同一组件内，避免图片生成、轮询、上传等业务步骤分别判断认证错误。
type authorizationState struct {
	provider      AuthHeaderProvider
	fallbackToken string
	mu            sync.Mutex
	scope         *authorizationScope
}

func (a *authorizationState) begin(cancel context.CancelCauseFunc) *authorizationScope {
	if a == nil {
		return nil
	}
	scope := &authorizationScope{cancel: cancel}
	a.mu.Lock()
	a.scope = scope
	a.mu.Unlock()
	return scope
}

func (a *authorizationState) end(scope *authorizationScope) {
	if a == nil || scope == nil {
		return
	}
	a.mu.Lock()
	if a.scope == scope {
		a.scope = nil
	}
	a.mu.Unlock()
}

func (a *authorizationState) fail(cause error) error {
	authErr := &AuthorizationError{Err: cause}
	if a == nil {
		return authErr
	}
	a.mu.Lock()
	scope := a.scope
	a.mu.Unlock()
	if scope != nil && scope.cancel != nil {
		scope.cancel(authErr)
	}
	return authErr
}

func (a *authorizationState) resolve() (string, error) {
	authHeader := ""
	if a != nil && a.provider != nil {
		var err error
		authHeader, err = a.provider()
		if err != nil {
			return "", a.fail(err)
		}
	}
	if strings.TrimSpace(authHeader) == "" && a != nil && a.fallbackToken != "" {
		authHeader = "Bearer " + a.fallbackToken
	}
	if strings.TrimSpace(authHeader) == "" {
		return "", a.fail(errors.New("authorization 头为空"))
	}
	return authHeader, nil
}

func (a *authorizationState) validate() error {
	_, err := a.resolve()
	return err
}
