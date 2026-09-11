package cryptopay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const token = "12345:AAtestTokenValue"

// sign reproduces what Crypto Pay does, independently of the code under test,
// so these tests check VerifyWebhook against the specification rather than
// against itself.
func sign(t *testing.T, tok string, body []byte) string {
	t.Helper()
	secret := sha256.Sum256([]byte(tok))
	mac := hmac.New(sha256.New, secret[:])
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

const paidBody = `{"update_id":1,"update_type":"invoice_paid","request_date":"2026-09-12T10:00:00Z",` +
	`"payload":{"invoice_id":777,"status":"paid","bot_invoice_url":"https://t.me/CryptoBot?start=IV1","payload":"42"}}`

func TestVerifyWebhook_AcceptsAGenuineDelivery(t *testing.T) {
	t.Parallel()

	u, err := VerifyWebhook(token, []byte(paidBody), sign(t, token, []byte(paidBody)))
	require.NoError(t, err)
	assert.Equal(t, UpdateInvoicePaid, u.UpdateType)
	assert.Equal(t, int64(777), u.Payload.InvoiceID)
	assert.Equal(t, StatusPaid, u.Payload.Status)
}

func TestVerifyWebhook_RejectsForgeries(t *testing.T) {
	t.Parallel()

	good := sign(t, token, []byte(paidBody))

	tests := map[string]struct {
		body      string
		signature string
	}{
		// The attack this check exists for: a POST to the public endpoint
		// claiming an invoice was paid.
		"no signature at all":       {body: paidBody, signature: ""},
		"garbage signature":         {body: paidBody, signature: "not-hex"},
		"signed with another token": {body: paidBody, signature: sign(t, "999:other", []byte(paidBody))},
		// A genuine signature does not transfer to a different body: an
		// intercepted delivery cannot be edited to point at another invoice.
		"body altered after signing": {body: strings.Replace(paidBody, "777", "778", 1), signature: good},
		// Re-encoding JSON changes the bytes; only the exact body verifies.
		"whitespace added": {body: paidBody + " ", signature: good},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := VerifyWebhook(token, []byte(tc.body), tc.signature)
			assert.ErrorIs(t, err, ErrBadSignature)
		})
	}
}

func TestVerifyWebhook_ToleratesSurroundingWhitespaceInTheHeader(t *testing.T) {
	t.Parallel()

	_, err := VerifyWebhook(token, []byte(paidBody), "  "+sign(t, token, []byte(paidBody))+"\n")
	assert.NoError(t, err)
}

func TestCreateInvoice_SendsAFiatPricedRequest(t *testing.T) {
	t.Parallel()

	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = w.Write([]byte(`{"ok":true,"result":{"invoice_id":555,"status":"active",` +
			`"bot_invoice_url":"https://t.me/CryptoBot?start=IV555"}}`))
	}))
	defer srv.Close()

	inv, err := New(srv.URL, token).CreateInvoice(context.Background(), InvoiceRequest{
		Amount: "10", Fiat: "USD", AcceptedAssets: []string{"USDT", "TON"},
		Description: "access", Payload: "42", ExpiresIn: time.Hour,
	})
	require.NoError(t, err)

	assert.Equal(t, int64(555), inv.InvoiceID)
	assert.Equal(t, "https://t.me/CryptoBot?start=IV555", inv.PayURL())

	require.NotNil(t, got)
	assert.Equal(t, "/api/createInvoice", got.URL.Path)
	assert.Equal(t, token, got.Header.Get("Crypto-Pay-API-Token"))

	q := got.URL.Query()
	assert.Equal(t, "fiat", q.Get("currency_type"), "priced in dollars so the coin moving cannot short the merchant")
	assert.Equal(t, "USD", q.Get("fiat"))
	assert.Equal(t, "10", q.Get("amount"))
	assert.Equal(t, "USDT,TON", q.Get("accepted_assets"))
	assert.Equal(t, "3600", q.Get("expires_in"))
}

func TestGetInvoices_JoinsIDsAndReadsItems(t *testing.T) {
	t.Parallel()

	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("invoice_ids")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"items":[{"invoice_id":1,"status":"paid"},{"invoice_id":2,"status":"expired"}]}}`))
	}))
	defer srv.Close()

	invoices, err := New(srv.URL, token).GetInvoices(context.Background(), []int64{1, 2})
	require.NoError(t, err)
	assert.Equal(t, "1,2", query)
	require.Len(t, invoices, 2)
	assert.Equal(t, StatusPaid, invoices[0].Status)
	assert.Equal(t, StatusExpired, invoices[1].Status)
}

func TestGetInvoices_SkipsTheCallForNoIDs(t *testing.T) {
	t.Parallel()

	invoices, err := New("http://127.0.0.1:1", token).GetInvoices(context.Background(), nil)
	assert.NoError(t, err, "no ids means no request, not a failed one")
	assert.Nil(t, invoices)
}

func TestClient_SurfacesTheProviderError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error":{"code":401,"name":"UNAUTHORIZED"}}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "bad").CreateInvoice(context.Background(), InvoiceRequest{Amount: "10", Fiat: "USD"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNAUTHORIZED", "the provider names the problem better than any mapping would")
}
