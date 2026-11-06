package security

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOriginGuard(t *testing.T) {
	handler := OriginGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, tc := range []struct {
		origin, site string
		want         int
	}{{"https://example.com", "same-origin", 204}, {"https://evil.test", "cross-site", 403}, {"", "cross-site", 403}} {
		r := httptest.NewRequest("POST", "https://example.com/api", nil)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Sec-Fetch-Site", tc.site)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("origin %q site %q: got %d want %d", tc.origin, tc.site, w.Code, tc.want)
		}
	}
}
func TestLimiter(t *testing.T) {
	l := NewLimiter(2, time.Minute)
	if !l.Allow("first") || !l.Allow("first") || l.Allow("first") || !l.Allow("other") {
		t.Fatal("limiter did not isolate clients")
	}
}
