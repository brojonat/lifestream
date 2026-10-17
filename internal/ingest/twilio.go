// Package ingest turns requests from the outside world into raw events:
// Twilio SMS webhooks and generic JSON pushed with a user token. Handlers
// append and return; they never wait on the agent.
package ingest

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/brojonat/lifestream/internal/store"
)

const emptyTwiML = `<?xml version="1.0" encoding="UTF-8"?><Response></Response>`

// Twilio handles POST /twilio/sms. publicURL is the service's base URL as
// Twilio sees it (scheme and host, no path); the signature covers the full
// URL Twilio requested.
func Twilio(st *store.Store, authToken, publicURL string, log *slog.Logger) http.HandlerFunc {
	base := strings.TrimSuffix(publicURL, "/")
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if !ValidTwilioSignature(authToken, base+r.URL.RequestURI(), r.PostForm, r.Header.Get("X-Twilio-Signature")) {
			log.Warn("twilio signature rejected", "url", base+r.URL.RequestURI())
			http.Error(w, "invalid signature", http.StatusForbidden)
			return
		}
		ctx := r.Context()
		from := r.PostForm.Get("From")
		user, err := st.UserByPhone(ctx, from)
		if errors.Is(err, store.ErrNotFound) {
			// Answer like a success so the sender learns nothing.
			log.Warn("sms from unknown sender dropped", "from", from)
			twiml(w)
			return
		}
		if err != nil {
			log.Error("looking up sender", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Store what Twilio sent, unchanged (one value per field).
		fields := make(map[string]string, len(r.PostForm))
		for k, v := range r.PostForm {
			fields[k] = v[0]
		}
		payload, _ := json.Marshal(fields)
		e, created, err := st.Append(ctx, store.Event{
			Owner:          user.ID,
			Source:         "twilio.sms",
			Type:           "message.received",
			IdempotencyKey: r.PostForm.Get("MessageSid"),
			Payload:        payload,
		})
		if err != nil {
			log.Error("appending sms", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		log.Info("sms received", "event", e.ID, "owner", user.ID, "created", created)
		twiml(w)
	}
}

func twiml(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Write([]byte(emptyTwiML))
}

// ValidTwilioSignature checks X-Twilio-Signature: base64 HMAC-SHA1, keyed
// by the auth token, over the URL followed by every POST parameter as
// key+value, keys sorted (values sorted, duplicates dropped). Like Twilio's
// SDKs, it also accepts the URL with or without an explicit default port.
func ValidTwilioSignature(authToken, rawURL string, params url.Values, signature string) bool {
	if authToken == "" || signature == "" {
		return false
	}
	for _, u := range urlVariants(rawURL) {
		if hmac.Equal([]byte(TwilioSignature(authToken, u, params)), []byte(signature)) {
			return true
		}
	}
	return false
}

// TwilioSignature computes the signature Twilio would send for a request.
func TwilioSignature(authToken, rawURL string, params url.Values) string {
	var b strings.Builder
	b.WriteString(rawURL)
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		vals := append([]string(nil), params[k]...)
		sort.Strings(vals)
		for i, v := range vals {
			if i > 0 && v == vals[i-1] {
				continue
			}
			b.WriteString(k)
			b.WriteString(v)
		}
	}
	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(b.String()))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// urlVariants returns the URL as given plus the form with the port removed
// (or the default port added), since proxies differ on which one Twilio
// signed.
func urlVariants(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil {
		return []string{raw}
	}
	alt := *u
	if u.Port() != "" {
		alt.Host = u.Hostname()
	} else if u.Scheme == "https" {
		alt.Host = u.Host + ":443"
	} else {
		alt.Host = u.Host + ":80"
	}
	return []string{raw, alt.String()}
}
