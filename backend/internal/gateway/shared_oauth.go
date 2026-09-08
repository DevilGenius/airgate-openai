package gateway

import (
	"context"
	"encoding/json"
	"time"

	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
)

type persistedPKCE struct {
	Verifier    string
	CallbackURL string
	CreatedAt   time.Time
}

func (g *OpenAIGateway) saveOAuthSession(ctx context.Context, state string, session *pkceSession) error {
	if g.host == nil {
		oauthSessions.Store(state, session)
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return (&sdk.RuntimeStateClient{Host: g.host}).Store(ctx, "oauth:"+state, persistedPKCE{session.verifier, session.callbackURL, session.createdAt})
}

func (g *OpenAIGateway) consumeOAuthSession(ctx context.Context, state string) (*pkceSession, bool, error) {
	if g.host == nil {
		raw, ok := oauthSessions.LoadAndDelete(state)
		if !ok {
			return nil, false, nil
		}
		return raw.(*pkceSession), true, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	raw, found, err := (&sdk.RuntimeStateClient{Host: g.host}).Take(ctx, "oauth:"+state)
	if err != nil || !found {
		return nil, false, err
	}
	var session persistedPKCE
	if err := json.Unmarshal([]byte(raw), &session); err != nil {
		return nil, false, err
	}
	return &pkceSession{verifier: session.Verifier, callbackURL: session.CallbackURL, createdAt: session.CreatedAt}, true, nil
}
