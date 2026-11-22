package rpc

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/iamxvbaba/td/clock"
	"github.com/iamxvbaba/td/tg"
	"go.uber.org/zap"

	"telesrv/internal/domain"
	"telesrv/internal/push"
	"telesrv/internal/store"
)

// OfflinePusher delivers a push notification for a durable update that reached no
// live session. It is called from an outbox worker, so an implementation must
// return quickly and do any network work asynchronously.
type OfflinePusher interface {
	PushOfflineUpdate(userID int64, event domain.UpdateEvent, update *tg.Updates)
}

const (
	offlinePushWorkers         = 4
	offlinePushQueueSize       = 256
	defaultOfflinePushDebounce = 30 * time.Second
)

// OfflinePushBridge builds notifications from the already-enriched viewer updates,
// coalesces bursts per user, and sends them from a small worker pool so an FCM
// round-trip never blocks the outbox.
type OfflinePushBridge struct {
	tokens   store.PushTokenStore
	service  *push.Service
	clock    clock.Clock
	debounce time.Duration
	log      *zap.Logger

	jobs chan offlinePushJob
	mu   sync.Mutex
	last map[int64]time.Time
}

type offlinePushJob struct {
	userID int64
	notif  push.Notification
}

// NewOfflinePushBridge builds the outbox push bridge. A nil clock/log falls back to
// the system clock / no-op logger; a non-positive debounce uses the default.
func NewOfflinePushBridge(tokens store.PushTokenStore, service *push.Service, debounce time.Duration, clk clock.Clock, log *zap.Logger) *OfflinePushBridge {
	if clk == nil {
		clk = clock.System
	}
	if log == nil {
		log = zap.NewNop()
	}
	if debounce <= 0 {
		debounce = defaultOfflinePushDebounce
	}
	return &OfflinePushBridge{
		tokens:   tokens,
		service:  service,
		clock:    clk,
		debounce: debounce,
		log:      log,
		jobs:     make(chan offlinePushJob, offlinePushQueueSize),
		last:     make(map[int64]time.Time),
	}
}

// PushOfflineUpdate decides whether an outbox event warrants a push, builds it,
// applies the per-user debounce, and enqueues it. It never blocks.
func (p *OfflinePushBridge) PushOfflineUpdate(userID int64, event domain.UpdateEvent, update *tg.Updates) {
	if p == nil || p.tokens == nil || p.service == nil || update == nil {
		return
	}
	if event.Type != domain.UpdateEventNewMessage {
		return
	}
	notif, ok := buildPushNotification(userID, update)
	if !ok {
		return
	}
	if !p.allow(userID) {
		return
	}
	select {
	case p.jobs <- offlinePushJob{userID: userID, notif: notif}:
	default:
		p.log.Debug("offline push queue full; dropping", zap.Int64("user_id", userID))
	}
}

func (p *OfflinePushBridge) allow(userID int64) bool {
	now := p.clock.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if last, ok := p.last[userID]; ok && now.Sub(last) < p.debounce {
		return false
	}
	p.last[userID] = now
	return true
}

// Run starts the send workers and blocks until ctx is cancelled.
func (p *OfflinePushBridge) Run(ctx context.Context) {
	if p == nil || p.tokens == nil || p.service == nil {
		return
	}
	var wg sync.WaitGroup
	for i := 0; i < offlinePushWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.runWorker(ctx)
		}()
	}
	wg.Wait()
}

func (p *OfflinePushBridge) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-p.jobs:
			p.deliver(ctx, job)
		}
	}
}

func (p *OfflinePushBridge) deliver(ctx context.Context, job offlinePushJob) {
	tokens, err := p.tokens.ListPushTokensForUser(ctx, job.userID)
	if err != nil {
		p.log.Warn("list push tokens", zap.Int64("user_id", job.userID), zap.Error(err))
		return
	}
	for _, token := range tokens {
		if token.TokenType != domain.PushTokenTypeFCM {
			continue
		}
		if err := p.service.Send(ctx, token, job.notif); err != nil {
			if errors.Is(err, push.ErrTokenUnregistered) {
				if _, delErr := p.tokens.DeletePushTokenByValue(ctx, token.TokenType, token.Token); delErr != nil {
					p.log.Warn("delete invalidated push token", zap.Error(delErr))
				}
				continue
			}
			p.log.Debug("offline push send failed", zap.Int64("user_id", job.userID), zap.Error(err))
		}
	}
}
