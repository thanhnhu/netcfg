package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		site   string
		want   bool
	}{
		{"chrome form post with stripped origin", "null", "same-origin", true},
		{"plain same-origin post", "https://box:8443", "same-origin", true},
		{"typed url or bookmark", "", "none", true},
		{"cross site post", "https://evil.example", "cross-site", false},
		{"sibling subdomain post", "https://other.box", "same-site", false},
		{"legacy browser, matching origin", "https://box:8443", "", true},
		{"legacy browser, foreign origin", "https://evil.example", "", false},
		{"legacy browser, opaque origin", "null", "", false},
		{"legacy browser, no origin", "", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "https://box:8443/login", nil)
			r.Host = "box:8443"
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.site != "" {
				r.Header.Set("Sec-Fetch-Site", tc.site)
			}
			if got := sameOrigin(r); got != tc.want {
				t.Errorf("sameOrigin() = %v, want %v", got, tc.want)
			}
		})
	}
}
