package push

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"

	"go.uber.org/zap"

	"telesrv/internal/domain"
)

// Notification is the JSON object the official clients render after decrypting
// the `p` field. Field names and meaning come from
// https://core.telegram.org/api/push-updates#notification-structure.
type Notification struct {
	LocKey  string         `json:"loc_key"`
	LocArgs []string       `json:"loc_args,omitempty"`
	Custom  map[string]any `json:"custom,omitempty"`
	UserID  int64          `json:"user_id,omitempty"`
	Sound   string         `json:"sound,omitempty"`
}

// plaintext serialises the notification as the client expects: a 32-bit
// little-endian length followed by the JSON body.
func (n Notification) plaintext() ([]byte, error) {
	body, err := json.Marshal(n)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 4+len(body))
	binary.LittleEndian.PutUint32(out[:4], uint32(len(body)))
	copy(out[4:], body)
	return out, nil
}

// Service seals and delivers a notification to a user's registered devices. It is
// provider-agnostic: the Sender decides APNs vs FCM.
type Service struct {
	sender Sender
	log    *zap.Logger
}

// NewService builds a push Service; a nil logger is replaced with a no-op logger.
func NewService(sender Sender, log *zap.Logger) *Service {
	if log == nil {
		log = zap.NewNop()
	}
	return &Service{sender: sender, log: log}
}

// Send encrypts the notification with the token's secret and delivers it. A token
// without a usable secret cannot receive an FCM push (the client reads only `p`),
// so it is skipped with errSecretTooShort. A permanent provider rejection is
// returned as ErrTokenUnregistered for the caller to delete the row.
func (s *Service) Send(ctx context.Context, token domain.PushToken, n Notification) error {
	if s == nil || s.sender == nil {
		return errors.New("push: no sender configured")
	}
	if len(token.Secret) < 128 {
		return errSecretTooShort
	}
	plaintext, err := n.plaintext()
	if err != nil {
		return err
	}
	sealed, err := encryptPushPayload(token.Secret, plaintext)
	if err != nil {
		return err
	}
	return s.sender.Send(ctx, token.Token, map[string]string{"p": sealed})
}
