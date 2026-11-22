package rpc

import (
	"testing"

	"github.com/iamxvbaba/td/tg"
)

func privateUpdate(userID, senderID int64, text string, media tg.MessageMediaClass, flags func(*tg.Message)) *tg.Updates {
	msg := &tg.Message{ID: 7, PeerID: &tg.PeerUser{UserID: userID}, FromID: &tg.PeerUser{UserID: senderID}, Message: text, Media: media}
	if flags != nil {
		flags(msg)
	}
	return &tg.Updates{
		Updates: []tg.UpdateClass{&tg.UpdateNewMessage{Message: msg, Pts: 1}},
		Users:   []tg.UserClass{&tg.User{ID: senderID, FirstName: "Alice", LastName: "Doe"}},
	}
}

func TestBuildPushNotificationPrivateText(t *testing.T) {
	notif, ok := buildPushNotification(1001, privateUpdate(1001, 42, "hello there", nil, nil))
	if !ok {
		t.Fatal("private text message produced no notification")
	}
	if notif.LocKey != "MESSAGE_TEXT" {
		t.Fatalf("loc_key = %q, want MESSAGE_TEXT", notif.LocKey)
	}
	if len(notif.LocArgs) != 2 || notif.LocArgs[0] != "Alice Doe" || notif.LocArgs[1] != "hello there" {
		t.Fatalf("loc_args = %#v", notif.LocArgs)
	}
	if notif.Custom["from_id"] != int64(42) || notif.Custom["msg_id"] != 7 {
		t.Fatalf("custom = %#v", notif.Custom)
	}
	if notif.UserID != 1001 {
		t.Fatalf("user_id = %d, want 1001", notif.UserID)
	}
}

func TestBuildPushNotificationPrivateMediaAndFlags(t *testing.T) {
	notif, ok := buildPushNotification(1001, privateUpdate(1001, 42, "", &tg.MessageMediaPhoto{}, func(m *tg.Message) { m.Mentioned = true }))
	if !ok {
		t.Fatal("private media message produced no notification")
	}
	if notif.LocKey != "MESSAGE_PHOTO" || len(notif.LocArgs) != 1 {
		t.Fatalf("media notification = %+v", notif)
	}
	if notif.Custom["mention"] != 1 {
		t.Fatalf("mention flag missing: %#v", notif.Custom)
	}
}

func TestBuildPushNotificationSkips(t *testing.T) {
	cases := map[string]*tg.Updates{
		"outgoing": privateUpdate(1001, 1001, "x", nil, func(m *tg.Message) { m.Out = true }),
		"silent":   privateUpdate(1001, 42, "x", nil, func(m *tg.Message) { m.Silent = true }),
		"selfchat": privateUpdate(1001, 1001, "x", nil, nil),
	}
	for name, updates := range cases {
		if _, ok := buildPushNotification(1001, updates); ok {
			t.Fatalf("%s message produced a notification, want skip", name)
		}
	}
}

func TestBuildPushNotificationMegagroupAndChannel(t *testing.T) {
	msg := &tg.Message{ID: 8, PeerID: &tg.PeerChannel{ChannelID: 100}, FromID: &tg.PeerUser{UserID: 2}, Message: "hi group"}
	megagroup := &tg.Updates{
		Updates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: msg}},
		Chats:   []tg.ChatClass{&tg.Channel{ID: 100, Title: "Family", Megagroup: true}},
		Users:   []tg.UserClass{&tg.User{ID: 2, FirstName: "Bob"}},
	}
	notif, ok := buildPushNotification(5, megagroup)
	if !ok {
		t.Fatal("megagroup message produced no notification")
	}
	if notif.LocKey != "CHAT_MESSAGE_TEXT" || len(notif.LocArgs) != 3 ||
		notif.LocArgs[0] != "Bob" || notif.LocArgs[1] != "Family" || notif.LocArgs[2] != "hi group" {
		t.Fatalf("megagroup notification = %+v", notif)
	}
	if notif.Custom["channel_id"] != int64(100) || notif.Custom["chat_from_id"] != int64(2) {
		t.Fatalf("megagroup custom = %#v", notif.Custom)
	}

	broadcast := &tg.Updates{
		Updates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: msg}},
		Chats:   []tg.ChatClass{&tg.Channel{ID: 100, Title: "News", Broadcast: true}},
	}
	notif, ok = buildPushNotification(5, broadcast)
	if !ok {
		t.Fatal("channel post produced no notification")
	}
	if notif.LocKey != "CHANNEL_MESSAGE_TEXT" || len(notif.LocArgs) != 2 ||
		notif.LocArgs[0] != "News" || notif.LocArgs[1] != "hi group" {
		t.Fatalf("channel notification = %+v", notif)
	}
	if _, hasFrom := notif.Custom["chat_from_id"]; hasFrom {
		t.Fatalf("broadcast custom must not carry chat_from_id: %#v", notif.Custom)
	}
}
