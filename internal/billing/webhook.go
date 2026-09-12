package billing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"shortlink/internal/cryptopay"
)

// maxWebhookBody caps what is read before the signature is checked, so an
// unauthenticated caller cannot make the service buffer an arbitrary amount.
const maxWebhookBody = 64 << 10

// WebhookHandler receives Crypto Pay deliveries.
//
// The response code is the only thing the provider acts on, so it is chosen
// deliberately:
//
//	401 -- bad signature: not from Crypto Pay, and nothing is done
//	200 -- handled, or an update type this service does not care about
//	500 -- could not record the payment: the provider retries, which is safe
//	       precisely because Confirm is idempotent
func (s *Service) WebhookHandler(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		update, err := cryptopay.VerifyWebhook(token, body, r.Header.Get(cryptopay.SignatureHeader))
		if errors.Is(err, cryptopay.ErrBadSignature) {
			s.log.WarnContext(r.Context(), "rejected a webhook with a bad signature",
				slog.String("remote_addr", r.RemoteAddr))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if update.UpdateType != cryptopay.UpdateInvoicePaid || update.Payload.Status != cryptopay.StatusPaid {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Detached from the request: if the provider gives up waiting and
		// disconnects mid-commit, the payment should still be recorded. Its
		// retry then finds the invoice already paid and does nothing.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
		defer cancel()

		if err := s.Confirm(ctx, update.Payload.InvoiceID); err != nil {
			s.log.ErrorContext(ctx, "could not confirm a webhook payment",
				slog.Int64("invoice_id", update.Payload.InvoiceID), slog.Any("error", err))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	})
}
