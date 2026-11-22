package rpc

import (
	"strconv"
	"strings"

	"github.com/iamxvbaba/td/tg"

	"telesrv/internal/push"
)

// buildPushNotification converts the viewer's already-built TL updates into a
// push notification, returning false when the event is not a notifiable inbound
// message.
func buildPushNotification(userID int64, updates *tg.Updates) (push.Notification, bool) {
	if updates == nil {
		return push.Notification{}, false
	}
	for _, u := range updates.Updates {
		switch m := u.(type) {
		case *tg.UpdateNewMessage:
			if msg, ok := m.Message.(*tg.Message); ok {
				return messageNotification(userID, msg, updates)
			}
		case *tg.UpdateNewChannelMessage:
			if msg, ok := m.Message.(*tg.Message); ok {
				return messageNotification(userID, msg, updates)
			}
		}
	}
	return push.Notification{}, false
}

func messageNotification(userID int64, msg *tg.Message, updates *tg.Updates) (push.Notification, bool) {
	if msg == nil || msg.Out || msg.Silent {
		return push.Notification{}, false
	}
	custom := map[string]any{"msg_id": msg.ID}
	if msg.Mentioned {
		custom["mention"] = 1
	}
	switch peer := msg.PeerID.(type) {
	case *tg.PeerUser:
		senderID := peerUserID(msg.FromID)
		if senderID == 0 || senderID == userID {
			// Self-chat / Saved Messages must not notify the user.
			return push.Notification{}, false
		}
		custom["from_id"] = senderID
		return composeMessage(push.Notification{}, "", []string{userNameByID(updates, senderID)}, msg, custom, userID), true
	case *tg.PeerChannel:
		channelID := peer.ChannelID
		title, megagroup := channelByID(updates, channelID)
		custom["channel_id"] = channelID
		if megagroup {
			// Megagroup: the notification shows both the author and the group.
			senderName := title
			if fromUserID := peerUserID(msg.FromID); fromUserID != 0 {
				senderName = userNameByID(updates, fromUserID)
				custom["chat_from_id"] = fromUserID
			} else if fromChannelID := peerChannelID(msg.FromID); fromChannelID != 0 {
				senderName = channelTitleByID(updates, fromChannelID)
				custom["chat_from_broadcast_id"] = fromChannelID
			}
			return composeMessage(push.Notification{}, "CHAT_", []string{senderName, title}, msg, custom, userID), true
		}
		return composeMessage(push.Notification{}, "CHANNEL_", []string{title}, msg, custom, userID), true
	case *tg.PeerChat:
		chatID := peer.ChatID
		title, _ := channelByID(updates, chatID)
		custom["chat_id"] = chatID
		base := []string{title}
		if senderID := peerUserID(msg.FromID); senderID != 0 {
			base = []string{userNameByID(updates, senderID), title}
		}
		return composeMessage(push.Notification{}, "CHAT_", base, msg, custom, userID), true
	}
	return push.Notification{}, false
}

// composeMessage picks the loc_key/args for a private (MESSAGE_), broadcast
// channel (CHANNEL_) or group (CHAT_) notification from the base (non-text)
// arguments.
func composeMessage(n push.Notification, prefix string, base []string, msg *tg.Message, custom map[string]any, userID int64) push.Notification {
	args := append([]string{}, base...)
	locKey := prefix + "MESSAGE_TEXT"
	if text := strings.TrimSpace(msg.Message); text != "" {
		args = append(args, text)
	} else if suffix := mediaLocSuffix(msg.Media); suffix != "" {
		locKey = prefix + "MESSAGE_" + suffix
	} else {
		locKey = prefix + "MESSAGE_NOTEXT"
	}
	n.LocKey = locKey
	n.LocArgs = args
	n.Custom = custom
	n.UserID = userID
	return n
}

func mediaLocSuffix(media tg.MessageMediaClass) string {
	switch media.(type) {
	case *tg.MessageMediaPhoto:
		return "PHOTO"
	case *tg.MessageMediaDocument:
		return "DOC"
	case *tg.MessageMediaContact:
		return "CONTACT"
	case *tg.MessageMediaPoll:
		return "POLL"
	case *tg.MessageMediaGeo, *tg.MessageMediaVenue:
		return "GEO"
	case *tg.MessageMediaGeoLive:
		return "GEOLIVE"
	default:
		return ""
	}
}

func peerUserID(p tg.PeerClass) int64 {
	if u, ok := p.(*tg.PeerUser); ok {
		return u.UserID
	}
	return 0
}

func peerChannelID(p tg.PeerClass) int64 {
	if c, ok := p.(*tg.PeerChannel); ok {
		return c.ChannelID
	}
	return 0
}

func userNameByID(updates *tg.Updates, id int64) string {
	for _, u := range updates.Users {
		if user, ok := u.(*tg.User); ok && user.ID == id {
			if name := strings.TrimSpace(user.FirstName + " " + user.LastName); name != "" {
				return name
			}
			if user.Username != "" {
				return user.Username
			}
		}
	}
	return strconv.FormatInt(id, 10)
}

func channelTitleByID(updates *tg.Updates, id int64) string {
	title, _ := channelByID(updates, id)
	if title == "" {
		return strconv.FormatInt(id, 10)
	}
	return title
}

func channelByID(updates *tg.Updates, id int64) (title string, megagroup bool) {
	for _, c := range updates.Chats {
		switch chat := c.(type) {
		case *tg.Channel:
			if chat.ID == id {
				return chat.Title, chat.Megagroup
			}
		case *tg.Chat:
			if chat.ID == id {
				return chat.Title, false
			}
		}
	}
	return "", false
}
