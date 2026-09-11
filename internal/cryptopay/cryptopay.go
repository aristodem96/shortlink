// Package cryptopay is a minimal client for the Crypto Pay API, the merchant
// API behind Telegram's @CryptoBot.
//
// It is hand written rather than pulled in as a dependency because the service
// uses two methods and one webhook, and a payment path is exactly the place
// where every line should be readable in one sitting.
package cryptopay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Network base URLs. Testnet is a full parallel system with test coins, which is
// what makes the whole payment flow testable without spending money.
const (
	MainnetURL = "https://pay.crypt.bot"
	TestnetURL = "https://testnet-pay.crypt.bot"
)

// SignatureHeader carries the webhook signature.
const SignatureHeader = "Crypto-Pay-Api-Signature"

// Invoice statuses as the API reports them.
const (
	StatusActive  = "active"
	StatusPaid    = "paid"
	StatusExpired = "expired"
)

// Invoice is the subset of the API's invoice object this service reads.
type Invoice struct {
	InvoiceID         int64  `json:"invoice_id"`
	Status            string `json:"status"`
	BotInvoiceURL     string `json:"bot_invoice_url"`
	MiniAppInvoiceURL string `json:"mini_app_invoice_url"`
	Payload           string `json:"payload"`
}

// PayURL is the link the buyer opens to pay. bot_invoice_url opens the invoice
// inside @CryptoBot, which is the natural hand-off from another Telegram bot.
func (i Invoice) PayURL() string {
	if i.BotInvoiceURL != "" {
		return i.BotInvoiceURL
	}
	return i.MiniAppInvoiceURL
}

// Client talks to one Crypto Pay app.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a client for the given network base URL and app token.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		// A payment provider that hangs must not hang the bot with it.
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

// InvoiceRequest describes a fiat-priced invoice.
type InvoiceRequest struct {
	// Amount in the fiat currency, e.g. "10".
	Amount string
	// Fiat is the pricing currency, e.g. "USD". Pricing in fiat means the
	// buyer pays the equivalent in crypto at the current rate, so the merchant
	// is not exposed to the coin moving between invoice and payment.
	Fiat string
	// AcceptedAssets limits which coins the buyer may pay with.
	AcceptedAssets []string
	Description    string
	// Payload is echoed back on the invoice. It is informational only: the
	// service trusts its own record of who an invoice belongs to, never this.
	Payload   string
	ExpiresIn time.Duration
}

// CreateInvoice issues a new invoice.
func (c *Client) CreateInvoice(ctx context.Context, req InvoiceRequest) (Invoice, error) {
	params := url.Values{
		"currency_type": {"fiat"},
		"fiat":          {req.Fiat},
		"amount":        {req.Amount},
		"description":   {req.Description},
		"payload":       {req.Payload},
		// The buyer must be identifiable to the payment provider; anonymous
		// payment adds nothing here and complicates support.
		"allow_anonymous": {"false"},
	}
	if len(req.AcceptedAssets) > 0 {
		params.Set("accepted_assets", strings.Join(req.AcceptedAssets, ","))
	}
	if req.ExpiresIn > 0 {
		params.Set("expires_in", strconv.Itoa(int(req.ExpiresIn.Seconds())))
	}

	var inv Invoice
	if err := c.call(ctx, "createInvoice", params, &inv); err != nil {
		return Invoice{}, err
	}
	return inv, nil
}

// GetInvoices looks up invoices by id.
func (c *Client) GetInvoices(ctx context.Context, ids []int64) ([]Invoice, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	joined := make([]string, len(ids))
	for i, id := range ids {
		joined[i] = strconv.FormatInt(id, 10)
	}

	var result struct {
		Items []Invoice `json:"items"`
	}
	if err := c.call(ctx, "getInvoices", url.Values{"invoice_ids": {strings.Join(joined, ",")}}, &result); err != nil {
		return nil, err
	}
	return result.Items, nil
}

// envelope is the API's response wrapper.
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func (c *Client) call(ctx context.Context, method string, params url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/"+method+"?"+params.Encode(), nil)
	if err != nil {
		return fmt.Errorf("cryptopay: build %s request: %w", method, err)
	}
	req.Header.Set("Crypto-Pay-API-Token", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cryptopay: %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("cryptopay: read %s response: %w", method, err)
	}

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("cryptopay: %s: unexpected response (HTTP %d)", method, resp.StatusCode)
	}
	if !env.OK {
		// The error object is passed through verbatim: it names the problem
		// (a bad token, an unsupported asset) better than any mapping would.
		return fmt.Errorf("cryptopay: %s failed: %s", method, string(env.Error))
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("cryptopay: decode %s result: %w", method, err)
	}
	return nil
}

// Update is a webhook delivery.
type Update struct {
	UpdateID   int64   `json:"update_id"`
	UpdateType string  `json:"update_type"`
	Payload    Invoice `json:"payload"`
}

// UpdateInvoicePaid is the only update type this service acts on.
const UpdateInvoicePaid = "invoice_paid"

// ErrBadSignature means a webhook was not sent by Crypto Pay, or was altered.
var ErrBadSignature = errors.New("cryptopay: webhook signature mismatch")

// VerifyWebhook checks a delivery and returns the parsed update.
//
// The signature is an HMAC-SHA256 of the raw body, keyed with the SHA-256 of
// the app token. It must be computed over the exact bytes received: parsing the
// JSON and re-encoding it would reorder keys and change whitespace, and a
// genuine delivery would then fail to verify.
//
// Without this check the webhook endpoint -- which is public by necessity --
// would grant paid access to anybody who POSTed {"status": "paid"} to it.
func VerifyWebhook(token string, body []byte, signature string) (Update, error) {
	secret := sha256.Sum256([]byte(token))

	mac := hmac.New(sha256.New, secret[:])
	mac.Write(body)
	expected := mac.Sum(nil)

	got, err := hex.DecodeString(strings.TrimSpace(signature))
	// hmac.Equal compares in constant time. A plain == would return early at
	// the first differing byte, and the timing of that difference leaks how
	// much of a forged signature was right.
	if err != nil || !hmac.Equal(got, expected) {
		return Update{}, ErrBadSignature
	}

	var u Update
	if err := json.Unmarshal(body, &u); err != nil {
		return Update{}, fmt.Errorf("cryptopay: decode webhook: %w", err)
	}
	return u, nil
}
