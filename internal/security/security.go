package security

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// OriginGuard rejects browser cross-site writes. It allows same-host, reverse proxies
// (via X-Forwarded-Host or TRUSTED_ORIGIN), and Vercel frontends.
func OriginGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			u, err := url.Parse(origin)
			host := r.Header.Get("X-Forwarded-Host")
			if host == "" {
				host = r.Host
			}
			trustedOrigin := os.Getenv("TRUSTED_ORIGIN")
			matched := strings.EqualFold(u.Host, host) ||
				strings.EqualFold(u.Host, r.Host) ||
				(trustedOrigin != "" && (strings.EqualFold(origin, trustedOrigin) || strings.EqualFold(u.Host, trustedOrigin))) ||
				strings.HasSuffix(strings.ToLower(u.Host), ".vercel.app")

			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !matched {
				http.Error(w, "Invalid origin", http.StatusForbidden)
				return
			}
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" && r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			trustedOrigin := os.Getenv("TRUSTED_ORIGIN")
			isVercel := strings.HasSuffix(strings.ToLower(r.Header.Get("Origin")), ".vercel.app")
			if trustedOrigin == "" && !isVercel {
				http.Error(w, "Cross-site request blocked", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type bucket struct {
	count int
	reset time.Time
}
type Limiter struct {
	mu     sync.Mutex
	hits   map[string]bucket
	limit  int
	window time.Duration
}

func NewLimiter(limit int, window time.Duration) *Limiter {
	return &Limiter{hits: make(map[string]bucket), limit: limit, window: window}
}
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b := l.hits[key]
	if now.After(b.reset) {
		b = bucket{reset: now.Add(l.window)}
	}
	b.count++
	l.hits[key] = b
	if len(l.hits) > 10000 {
		for k, v := range l.hits {
			if now.After(v.reset) {
				delete(l.hits, k)
			}
		}
	}
	return b.count <= l.limit
}
func (l *Limiter) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.RemoteAddr
		if i := strings.LastIndex(key, ":"); i > 0 {
			key = key[:i]
		}
		if !l.Allow(key) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrfCookie is readable by the SPA so it can echo the value back in the
// X-CSRF-Token header; an attacker on another origin cannot read it.
const csrfCookie = "examflow_csrf"

// CSRFProtect issues a double-submit CSRF token on every response and requires
// it on all state-changing requests except the pre-session auth endpoints
// (login, register, password recovery request/completion).
func CSRFProtect(next http.Handler) http.Handler {
	exempt := map[string]bool{
		"/api/v1/auth/login":           true,
		"/api/v1/auth/register":        true,
		"/api/v1/auth/forgot-password": true,
		"/api/v1/auth/reset-password":  true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, cookieErr := r.Cookie(csrfCookie)
		if cookieErr != nil || cookie.Value == "" {
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				http.Error(w, "Unavailable", http.StatusInternalServerError)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     csrfCookie,
				Value:    hex.EncodeToString(b),
				Path:     "/",
				HttpOnly: false, // the SPA must read it to echo it back
				SameSite: http.SameSiteLaxMode,
				Secure:   r.TLS != nil,
				MaxAge:   604800,
			})
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions && !exempt[r.URL.Path] {
			if cookieErr != nil || r.Header.Get("X-CSRF-Token") == "" || r.Header.Get("X-CSRF-Token") != cookie.Value {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"CSRF token missing or invalid","code":"csrf"}`))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
