package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolvePublicProfileTargetRejectsAmbiguousFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/public-search" {
			t.Fatalf("expected /public-search path, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"profiles": [
				{"name":"Other One","pseudonym":"other-one","referral":"other-one","proxyWallet":"0x1111111111111111111111111111111111111111"},
				{"name":"Other Two","pseudonym":"other-two","referral":"other-two","proxyWallet":"0x2222222222222222222222222222222222222222"}
			]
		}`))
	}))
	defer server.Close()

	client := NewRestClient("")
	client.GammaURL = server.URL

	if _, _, err := client.ResolvePublicProfileTarget(context.Background(), "@target"); err == nil {
		t.Fatal("expected ambiguous target to be rejected")
	}
}
