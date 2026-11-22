package rpc

import (
	"context"
	"testing"
	"time"

	"github.com/iamxvbaba/td/clock"
	"go.uber.org/zap/zaptest"

	"telesrv/internal/domain"
	"telesrv/internal/push"
	"telesrv/internal/store/memory"
)

type bridgeCaptureSender struct {
	calls int
	err   error
}

func (s *bridgeCaptureSender) Send(context.Context, string, map[string]string) error {
	s.calls++
	return s.err
}

func TestOfflinePushBridgeDebouncesPerUser(t *testing.T) {
	log := zaptest.NewLogger(t)
	store := memory.NewPushTokenStore()
	bridge := NewOfflinePushBridge(store, push.NewService(&bridgeCaptureSender{}, log), 30*time.Second, clock.System, log)

	event := domain.UpdateEvent{Type: domain.UpdateEventNewMessage, UserID: 1001}
	updates := privateUpdate(1001, 42, "hello", nil, nil)

	bridge.PushOfflineUpdate(1001, event, updates)
	if len(bridge.jobs) != 1 {
		t.Fatalf("first update enqueued %d jobs, want 1", len(bridge.jobs))
	}
	bridge.PushOfflineUpdate(1001, event, updates)
	if len(bridge.jobs) != 1 {
		t.Fatalf("debounced update enqueued %d jobs, want 1", len(bridge.jobs))
	}
	bridge.PushOfflineUpdate(1001, domain.UpdateEvent{Type: domain.UpdateEventEditMessage, UserID: 1001}, updates)
	if len(bridge.jobs) != 1 {
		t.Fatalf("non-new-message enqueued %d jobs, want 1", len(bridge.jobs))
	}
}

func TestOfflinePushBridgeDeliversOnlyFCMAndDeletesInvalid(t *testing.T) {
	ctx := context.Background()
	log := zaptest.NewLogger(t)
	store := memory.NewPushTokenStore()
	secret := make([]byte, 256)
	for i := range secret {
		secret[i] = byte(i)
	}
	if err := store.UpsertPushToken(ctx, domain.PushToken{UserID: 1, TokenType: domain.PushTokenTypeFCM, Token: "fcm", Secret: secret}); err != nil {
		t.Fatalf("seed fcm: %v", err)
	}
	if err := store.UpsertPushToken(ctx, domain.PushToken{UserID: 1, TokenType: domain.PushTokenTypeAPNs, Token: "apns", Secret: secret}); err != nil {
		t.Fatalf("seed apns: %v", err)
	}
	capture := &bridgeCaptureSender{}
	bridge := NewOfflinePushBridge(store, push.NewService(capture, log), time.Second, clock.System, log)

	bridge.deliver(ctx, offlinePushJob{userID: 1, notif: push.Notification{LocKey: "MESSAGE_TEXT"}})
	if capture.calls != 1 {
		t.Fatalf("sent %d pushes, want 1 (FCM only)", capture.calls)
	}

	capture.err = push.ErrTokenUnregistered
	bridge.deliver(ctx, offlinePushJob{userID: 1, notif: push.Notification{LocKey: "MESSAGE_TEXT"}})
	tokens, err := store.ListPushTokensForUser(ctx, 1)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, token := range tokens {
		if token.TokenType == domain.PushTokenTypeFCM {
			t.Fatalf("invalidated FCM token was not deleted: %+v", token)
		}
	}
	if len(tokens) != 1 || tokens[0].TokenType != domain.PushTokenTypeAPNs {
		t.Fatalf("remaining tokens = %+v, want only the APNs token", tokens)
	}
}
