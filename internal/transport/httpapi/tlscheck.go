package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"shortlink/internal/domain"
)

// tlsCheckHandler answers Caddy's on-demand TLS question: may this server
// obtain a certificate for the hostname somebody just asked for?
//
// Every hostname approved here turns into a certificate order against a
// rate-limited account that the operator's own domains also draw on. So the
// handler fails closed in every direction: a missing parameter, a malformed
// name, an unregistered one, or one whose owner's access has lapsed all answer
// 403, and only a registered, verified, currently-paid domain answers 200.
//
// Without a gate like this, anyone could point a DNS record at this server and
// make it request certificates for names they chose, exhausting the quota and
// taking HTTPS down for real customers.
type tlsCheckHandler struct {
	domains Domains
	log     *slog.Logger
}

func (h *tlsCheckHandler) check(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Caddy passes the name it is about to request a certificate for.
	raw := r.URL.Query().Get("domain")
	if raw == "" {
		http.Error(w, "domain is required", http.StatusForbidden)
		return
	}

	hostname, err := domain.NormalizeHostname(raw)
	if err != nil {
		h.log.WarnContext(ctx, "tls check refused a malformed hostname",
			slog.String("hostname", raw))
		http.Error(w, "not allowed", http.StatusForbidden)
		return
	}

	if h.domains == nil {
		http.Error(w, "not allowed", http.StatusForbidden)
		return
	}

	switch _, err := h.domains.ServableDomain(ctx, hostname); {
	case err == nil:
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, domain.ErrNotFound):
		h.log.InfoContext(ctx, "tls check refused an unknown hostname",
			slog.String("hostname", hostname))
		http.Error(w, "not allowed", http.StatusForbidden)
	default:
		// A database failure must not become permission to issue. Refusing
		// costs one unreachable customer domain until the database recovers;
		// approving could cost the certificate quota.
		h.log.ErrorContext(ctx, "tls check could not be answered",
			slog.String("hostname", hostname), slog.Any("error", err))
		http.Error(w, "not allowed", http.StatusForbidden)
	}
}
