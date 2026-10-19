package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/parkerbrown98/gotalk-server/internal/pushgw"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

func notification(kind string, data map[string]any) store.Notification {
	raw, _ := json.Marshal(data)
	return store.Notification{ID: uuid.New(), UserID: uuid.New(), Kind: kind, Data: raw, CreatedAt: time.Now()}
}

func TestBuildPushPayload(t *testing.T) {
	alice := &store.User{Username: "alice", DisplayName: "Alice"}
	place, topic, post := uuid.New(), uuid.New(), uuid.New()

	n := notification(NotifyTopicReply, map[string]any{"topic_title": "Launch", "excerpt": "Congrats!", "place_name": "News"})
	n.PlaceID, n.TopicID, n.PostID = &place, &topic, &post
	p := buildPushPayload(n, alice, "https://forum.example")
	assert.Equal(t, "Alice replied in Launch", p.Title)
	assert.Equal(t, "Congrats!", p.Body)
	assert.Equal(t, "replies", p.Category)
	assert.Equal(t, topic.String(), p.ThreadID)
	assert.Equal(t, map[string]string{"place_id": place.String(), "topic_id": topic.String(), "post_id": post.String()}, p.Target)
	assert.Equal(t, "https://forum.example", p.Instance)

	ch, msg := uuid.New(), uuid.New()
	n = notification(NotifyMention, map[string]any{"channel_name": "general", "excerpt": "@bob hi"})
	n.PlaceID, n.ChannelID, n.MessageID = &place, &ch, &msg
	assert.Equal(t, "Someone mentioned you in #general", buildPushPayload(n, nil, "").Title, "deleted actors are anonymous")

	n = notification(NotifyDirectMessage, map[string]any{"channel_name": "", "excerpt": "lunch?"})
	n.ChannelID = &ch
	p = buildPushPayload(n, alice, "")
	assert.Equal(t, "Alice", p.Title)
	assert.Equal(t, ch.String(), p.Target["conversation_id"])
	n = notification(NotifyDirectMessage, map[string]any{"channel_name": "Weekend plans"})
	n.ChannelID = &ch
	assert.Equal(t, "Alice in Weekend plans", buildPushPayload(n, alice, "").Title)

	n = notification(NotifyReaction, map[string]any{"emoji": "🎉"})
	n.PostID = &post
	assert.Equal(t, "Alice reacted 🎉 to your post", buildPushPayload(n, alice, "").Title)
	n = notification(NotifyModeration, map[string]any{"place_name": "News", "action": "member.warn", "reason": "Be kind"})
	p = buildPushPayload(n, alice, "")
	assert.Equal(t, "Moderation notice from News", p.Title)
	assert.Equal(t, "member warn: Be kind", p.Body)
	assert.Equal(t, "New notification", buildPushPayload(notification("future_kind", nil), alice, "").Title)

	n = notification(NotifyMention, map[string]any{"topic_title": strings.Repeat("t", 300), "excerpt": strings.Repeat("é", 500)})
	p = buildPushPayload(n, alice, "")
	assert.LessOrEqual(t, len([]rune(p.Title)), maxPushTitle)
	assert.LessOrEqual(t, len([]rune(p.Body)), maxPushBody)
	assert.True(t, strings.HasSuffix(p.Body, "…"))
}

func TestFitPushPayload(t *testing.T) {
	p := pushPayload{V: 1, Title: "t", Body: strings.Repeat("ü", 3000), Target: map[string]string{}}
	raw, err := fitPushPayload(p)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(raw), pushgw.MaxPlaintext)
	var back pushPayload
	require.NoError(t, json.Unmarshal(raw, &back), "cut on a rune boundary, still valid JSON")
	assert.True(t, strings.HasSuffix(back.Body, "…"))

	small := pushPayload{V: 1, Title: "hello", Body: "world"}
	raw, err = fitPushPayload(small)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "world", "small payloads are untouched")
}

func TestPushQuiet(t *testing.T) {
	m := func(h, min int) *int16 { v := int16(h*60 + min); return &v }
	night := store.PushPreference{QuietHoursStart: m(22, 0), QuietHoursEnd: m(7, 0), Timezone: "Europe/Berlin"}
	berlin, _ := time.LoadLocation("Europe/Berlin")
	at := func(h, min int) time.Time { return time.Date(2026, 1, 15, h, min, 0, 0, berlin) }
	assert.True(t, pushQuiet(night, at(23, 30)))
	assert.True(t, pushQuiet(night, at(6, 59)))
	assert.False(t, pushQuiet(night, at(7, 0)))
	assert.False(t, pushQuiet(night, at(12, 0)))
	assert.True(t, pushQuiet(night, at(23, 30).UTC()), "evaluated in the person's time zone")

	lunch := store.PushPreference{QuietHoursStart: m(12, 0), QuietHoursEnd: m(13, 0), Timezone: "UTC"}
	assert.True(t, pushQuiet(lunch, time.Date(2026, 1, 15, 12, 30, 0, 0, time.UTC)))
	assert.False(t, pushQuiet(lunch, time.Date(2026, 1, 15, 13, 30, 0, 0, time.UTC)))

	until := time.Now().Add(time.Hour)
	assert.True(t, pushQuiet(store.PushPreference{PausedUntil: &until}, time.Now()))
	assert.False(t, pushQuiet(store.PushPreference{PausedUntil: &until}, until.Add(time.Second)))
	assert.False(t, pushQuiet(store.PushPreference{}, time.Now()))
}

func TestCollapseKeyAndCategories(t *testing.T) {
	k1 := collapseKey([]byte("secret"), "thread-1")
	assert.Regexp(t, `^t[A-Za-z0-9_-]{22}$`, k1)
	assert.Equal(t, k1, collapseKey([]byte("secret"), "thread-1"))
	assert.NotEqual(t, k1, collapseKey([]byte("secret"), "thread-2"))
	assert.NotEqual(t, k1, collapseKey([]byte("other"), "thread-1"), "keyed per instance")

	for _, kind := range []string{NotifyMention, NotifyReply, NotifyTopicReply, NotifyNewTopic, NotifyReaction, NotifySolution, NotifyModeration, NotifyDirectMessage} {
		assert.Contains(t, PushCategories, pushCategory(kind), kind)
	}
	assert.Equal(t, pushgw.UrgencyHigh, pushUrgency(NotifyDirectMessage))
	assert.Equal(t, pushgw.UrgencyLow, pushUrgency(NotifyReaction))
	assert.Equal(t, pushgw.UrgencyNormal, pushUrgency(NotifyReply))
	assert.Equal(t, 6*3600, pushTTL(NotifyNewTopic))
}

func TestPushSubscriptionValidation(t *testing.T) {
	key := make([]byte, 65)
	in := PushSubscriptionInput{SubscriptionID: "sub_abcdefgh", AppID: "gotalk", Platform: "web", P256dh: key, Auth: make([]byte, 16)}
	assert.Error(t, in.validate("https://push.example"), "the key must be on the curve")
	assert.True(t, validGatewaySubscriptionID("sub_abcdefgh"))
	assert.False(t, validGatewaySubscriptionID("sub_short"))
	assert.False(t, validGatewaySubscriptionID("dev_abcdefgh"))
	other := PushSubscriptionInput{GatewayURL: "https://elsewhere.example"}
	assert.Error(t, other.validate("https://push.example"))
	assert.Equal(t, "https://push.example", NormalizeGatewayURL("https://push.example/"))
}
