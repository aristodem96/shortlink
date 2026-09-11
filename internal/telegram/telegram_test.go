package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const token = "123456:SECRET-bot-token"

// TestTransportErrorsNeverContainTheToken guards against the token -- which is
// full control of the bot -- being written into logs. net/http puts the
// request URL in its errors, and every Bot API URL contains the token.
func TestTransportErrorsNeverContainTheToken(t *testing.T) {
	t.Parallel()

	// Nothing listens on port 1, so the dial fails immediately.
	c := NewWithBaseURL("http://127.0.0.1:1", token)

	err := c.SendMessage(context.Background(), 1, "hi")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "SECRET-bot-token")
	assert.NotContains(t, err.Error(), token)
}

func TestSendMessage_SendsHTMLWithButtons(t *testing.T) {
	t.Parallel()

	var (
		path string
		body map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()

	err := NewWithBaseURL(srv.URL, token).SendMessage(context.Background(), 42, "<b>hi</b>",
		Button{Text: "Pay", URL: "https://t.me/pay"})
	require.NoError(t, err)

	assert.Equal(t, "/bot"+token+"/sendMessage", path)
	assert.Equal(t, float64(42), body["chat_id"])
	assert.Equal(t, "HTML", body["parse_mode"])

	markup, ok := body["reply_markup"].(map[string]any)
	require.True(t, ok, "buttons must travel as reply_markup")
	rows := markup["inline_keyboard"].([]any)
	button := rows[0].([]any)[0].(map[string]any)
	assert.Equal(t, "Pay", button["text"])
	assert.Equal(t, "https://t.me/pay", button["url"])
}

func TestGetUpdates_DecodesMessages(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":9,"message":{"message_id":1,` +
			`"from":{"id":7000000001,"first_name":"A"},"chat":{"id":7000000001,"type":"private"},"text":"/buy"}}]}`))
	}))
	defer srv.Close()

	updates, err := NewWithBaseURL(srv.URL, token).GetUpdates(context.Background(), 0)
	require.NoError(t, err)
	require.Len(t, updates, 1)
	assert.Equal(t, int64(9), updates[0].UpdateID)
	// User ids above 2^31 are common now; they must not overflow.
	assert.Equal(t, int64(7000000001), updates[0].Message.From.ID)
	assert.Equal(t, ChatPrivate, updates[0].Message.Chat.Type)
}

func TestCall_SurfacesTelegramErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`))
	}))
	defer srv.Close()

	err := NewWithBaseURL(srv.URL, token).SendMessage(context.Background(), 1, "<b>broken")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "can't parse entities")
}
