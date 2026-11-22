package push

import (
	"crypto/ecdh"
	"encoding/base64"
	"testing"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

// TestEncryptWebPushRFC8291Vector checks encryptWebPush against the worked
// example in RFC 8291 section 5 / Appendix A (exact salt and application-server
// key), so every intermediate value is pinned.
func TestEncryptWebPushRFC8291Vector(t *testing.T) {
	uaPublic := mustB64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	asPrivateRaw := mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw")
	authSecret := mustB64(t, "BTBZMqHH6r4Tts7J_aSIgg")
	salt := mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw")

	asPrivate, err := ecdh.P256().NewPrivateKey(asPrivateRaw)
	if err != nil {
		t.Fatalf("as private key: %v", err)
	}
	if got := base64.RawURLEncoding.EncodeToString(asPrivate.PublicKey().Bytes()); got != "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8" {
		t.Fatalf("as public key = %s, want RFC value", got)
	}

	ua, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		t.Fatalf("ua public key: %v", err)
	}
	shared, err := asPrivate.ECDH(ua)
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}
	if got := base64.RawURLEncoding.EncodeToString(shared); got != "kyrL1jIIOHEzg3sM2ZWRHDRB62YACZhhSlknJ672kSs" {
		t.Fatalf("ecdh_secret = %s, want RFC value", got)
	}

	body, err := encryptWebPush(uaPublic, authSecret, asPrivate, salt, []byte("When I grow up, I want to be a watermelon"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if got := base64.RawURLEncoding.EncodeToString(body); got != want {
		t.Fatalf("encrypted body mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestParseWebPushSubscription(t *testing.T) {
	valid := `{"endpoint":"https://fcm.googleapis.com/fcm/send/abc","keys":{"p256dh":"BCVx","auth":"BTBZ"},"vapid":true}`
	sub, err := parseWebPushSubscription(valid)
	if err != nil {
		t.Fatalf("valid subscription rejected: %v", err)
	}
	if sub.Endpoint == "" || sub.Keys.P256DH != "BCVx" || sub.Keys.Auth != "BTBZ" {
		t.Fatalf("parsed = %+v", sub)
	}
	for name, token := range map[string]string{
		"not json":  `nope`,
		"no keys":   `{"endpoint":"https://x"}`,
		"empty key": `{"endpoint":"https://x","keys":{"p256dh":"","auth":"a"}}`,
	} {
		if _, err := parseWebPushSubscription(token); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestOriginOf(t *testing.T) {
	if got := originOf("https://fcm.googleapis.com/fcm/send/abc"); got != "https://fcm.googleapis.com" {
		t.Fatalf("origin = %q", got)
	}
	if got := originOf("https://updates.push.services.mozilla.com/wpush/v2/xyz"); got != "https://updates.push.services.mozilla.com" {
		t.Fatalf("origin = %q", got)
	}
}
