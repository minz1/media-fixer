package server

import (
	"net/http"
	"net/url"
	"strings"
)

// requireSameOrigin rejects state-changing requests that a browser tells us
// came from another site.
//
// The dashboard sits behind Authentik forward-auth in Caddy, which is a
// session cookie — so whether a cross-site POST to
// /media/incidents/{id}/approve-escalation (which deletes media files) is
// carried with that cookie depends entirely on the SameSite attribute
// Authentik happens to set, a third-party default this service cannot see and
// does not control. This makes the guarantee local: a request the browser
// labels cross-site never reaches a handler, whatever the cookie policy is.
//
// Deliberately not a CSRF token: tokens need a session to bind to and every
// form and hx-post on the page rethreaded to carry one, for a property these
// two headers already give. If the app ever grows its own login, revisit.
func requireSameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSafeMethod(r.Method) || originIsTrusted(r) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
	})
}

// isSafeMethod reports whether the method is read-only and therefore not a
// CSRF concern (these handlers change nothing).
func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// originIsTrusted decides whether a state-changing request may proceed.
//
// Sec-Fetch-Site is the primary signal: every current browser sends it, and
// it is set by the browser itself rather than by page script, so a hostile
// page cannot forge it. "none" means a direct user action (typed URL,
// bookmark) rather than a request another page initiated.
//
// Falling back to Origin covers a client that sends neither header — curl,
// a script, an old browser. An absent Origin is allowed on purpose: CSRF
// requires a browser acting on someone's ambient credentials, and a caller
// with no Origin at all is not that. Tightening this to "Origin required"
// would break ordinary command-line use for no security gain.
func originIsTrusted(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "cross-site", "same-site":
		// same-site is refused too: a sibling host under the same registrable
		// domain is exactly the position an attacker reaches by compromising
		// any other service on it.
		return false
	}

	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	// Compared against Host rather than a configured URL because Caddy
	// forwards the original Host (header_up Host {http.request.host}), so
	// this stays correct without a second place to keep the domain in sync.
	return strings.EqualFold(u.Host, r.Host)
}
