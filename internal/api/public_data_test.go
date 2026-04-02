package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

type rewriteRoundTripper struct {
	base   http.RoundTripper
	target *url.URL
}

func (r rewriteRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = r.target.Scheme
	clone.URL.Host = r.target.Host
	return r.base.RoundTrip(clone)
}

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

func TestGetPublicActivitySnapshotWithFallbackMarksCachedPositions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/trades":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		case "/positions":
			time.Sleep(150 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"conditionId":"cond-1","outcome":"Up","size":5}]`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	targetURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	originalClient := httpClient
	httpClient = &http.Client{
		Transport: rewriteRoundTripper{
			base:   server.Client().Transport,
			target: targetURL,
		},
	}
	defer func() { httpClient = originalClient }()

	client := NewRestClient("")
	cached := []Position{{ConditionID: "cond-1", Outcome: "Up", Size: 3}}
	snapshot := client.GetPublicActivitySnapshotWithFallback(
		context.Background(),
		"0x1111111111111111111111111111111111111111",
		[]string{"cond-1"},
		10,
		0.01,
		10,
		cached,
		true,
		time.Second,
		300*time.Millisecond,
	)

	if !snapshot.PositionsCached {
		t.Fatal("expected snapshot to report cached positions")
	}
	if snapshot.PositionsErr != nil {
		t.Fatalf("expected cached positions to clear position error, got %v", snapshot.PositionsErr)
	}
	if len(snapshot.Positions) != 1 || snapshot.Positions[0].Size != 3 {
		t.Fatalf("expected cached position size 3, got %+v", snapshot.Positions)
	}
}
