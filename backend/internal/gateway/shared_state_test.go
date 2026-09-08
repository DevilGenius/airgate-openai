package gateway

import (
	"context"
	"github.com/DevilGenius/airgate-sdk/devkit/testhost"
	sdk "github.com/DevilGenius/airgate-sdk/sdkgo"
	"sync"
	"testing"
	"time"
)

func TestSharedSessionSurvivesProcessReplacementAndLateWrites(t *testing.T) {
	host := &testhost.State{}
	a, b := newSessionStateMemoryStore(time.Hour, 10), newSessionStateMemoryStore(time.Hour, 10)
	a.shared = &sdk.RuntimeStateClient{Host: host}
	b.shared = &sdk.RuntimeStateClient{Host: host}
	a.Update("s", func(s *openAISessionState) { s.LastResponseID = "before" })
	b.Store("s", &openAISessionState{SessionKey: "s", LastResponseID: "stale snapshot", LastSeenAt: time.Now()})
	a.Update("s", func(s *openAISessionState) { s.LastResponseID = "late old response" })
	b.Update("s", func(s *openAISessionState) { s.LastTurnState = "new process turn" })
	got, err := b.loadShared(context.Background(), "s")
	if err != nil || got.LastResponseID != "late old response" || got.LastTurnState != "new process turn" {
		t.Fatalf("state lost at cutover: %+v %v", got, err)
	}
	var wg sync.WaitGroup
	for _, store := range []*sessionStateMemoryStore{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if store.Update("s", func(s *openAISessionState) { s.AccountID++ }) == nil {
					t.Error("CAS update failed")
					return
				}
			}
		}()
	}
	wg.Wait()
	got, err = a.loadShared(context.Background(), "s")
	if err != nil || got.AccountID != 100 {
		t.Fatalf("concurrent update lost: %+v %v", got, err)
	}
	if len(a.items) != 0 || len(b.items) != 0 {
		t.Fatal("process-local cache populated in shared mode")
	}
}

func TestSharedOAuthCanOnlyBeConsumedOnceAcrossProcesses(t *testing.T) {
	host := &testhost.State{}
	a, b := &OpenAIGateway{host: host}, &OpenAIGateway{host: host}
	ctx := context.Background()
	if err := a.saveOAuthSession(ctx, "state", &pkceSession{verifier: "test verifier", callbackURL: "http://localhost/callback", createdAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	session, found, err := b.consumeOAuthSession(ctx, "state")
	if err != nil || !found || session.verifier != "test verifier" {
		t.Fatalf("OAuth lost: %+v %t %v", session, found, err)
	}
	if _, found, err := a.consumeOAuthSession(ctx, "state"); err != nil || found {
		t.Fatal("OAuth was consumed twice", err)
	}
}
