/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rxtspot "github.com/rackspace-spot/spot-go-sdk/api/v1"
)

// jwt builds a token whose exp is offset from now. Only the payload is real;
// nothing here verifies a signature.
func jwt(t *testing.T, offset time.Duration) string {
	t.Helper()
	payload, err := json.Marshal(struct {
		Exp int64 `json:"exp"`
	}{Exp: time.Now().Add(offset).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

// recorder captures the Authorization header of every request that reaches it.
type recorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.seen = append(r.seen, req.Header.Get("Authorization"))
	r.mu.Unlock()
	return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}, Request: req}, nil
}

func (r *recorder) headers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func newTestTransport(t *testing.T, seed string, mint func(int) string) (*Transport, *recorder, *atomic.Int32) {
	t.Helper()
	rec := &recorder{}
	var calls atomic.Int32
	tr := NewTransport(rec, "https://spot.example", "https://login.example", "refresh-token", seed)
	tr.refresher = func(context.Context, string, string) (string, error) {
		return mint(int(calls.Add(1))), nil
	}
	return tr, rec, &calls
}

func do(t *testing.T, tr *Transport, url string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
}

// The bug this package fixes: once the startup token expires the SDK keeps
// sending it, and every call comes back 401. A request made after expiry must
// carry a new token, not the stale one.
func TestRoundTripRefreshesExpiredToken(t *testing.T) {
	stale := jwt(t, -time.Minute)
	fresh := jwt(t, time.Hour)
	tr, rec, calls := newTestTransport(t, stale, func(int) string { return fresh })

	do(t, tr, "https://spot.example/apis/ngpc.rxt.io/v1/serverclasses")

	if got := calls.Load(); got != 1 {
		t.Errorf("expected exactly 1 refresh, got %d", got)
	}
	if got, want := rec.headers()[0], "Bearer "+fresh; got != want {
		t.Errorf("request carried the stale token; got %q want %q", got, want)
	}
}

// A token still comfortably in date must be reused. If it were not, every
// request would redeem the refresh token and we would trade an expiry bug for
// a rate-limit one.
func TestRoundTripReusesValidToken(t *testing.T) {
	valid := jwt(t, time.Hour)
	tr, rec, calls := newTestTransport(t, valid, func(int) string { return jwt(t, time.Hour) })

	for range 5 {
		do(t, tr, "https://spot.example/apis/ngpc.rxt.io/v1/serverclasses")
	}

	if got := calls.Load(); got != 0 {
		t.Errorf("expected no refresh for a valid token, got %d", got)
	}
	for i, h := range rec.headers() {
		if want := "Bearer " + valid; h != want {
			t.Errorf("request %d: got %q want %q", i, h, want)
		}
	}
}

// The SDK treats a token as expired 60s before its exp, and so must we --
// otherwise a token that passes our check can still fail validation in flight.
func TestRoundTripRefreshesWithinLeeway(t *testing.T) {
	nearly := jwt(t, 30*time.Second) // inside the 60s leeway
	fresh := jwt(t, time.Hour)
	tr, _, calls := newTestTransport(t, nearly, func(int) string { return fresh })

	do(t, tr, "https://spot.example/apis/ngpc.rxt.io/v1/serverclasses")

	if got := calls.Load(); got != 1 {
		t.Errorf("token inside the leeway window should refresh; got %d refreshes", got)
	}
}

// Injecting a token into the token request would require already having one.
func TestRoundTripSkipsOAuthEndpoint(t *testing.T) {
	tr, rec, calls := newTestTransport(t, jwt(t, -time.Minute), func(int) string {
		t.Error("refreshing while fetching a token would recurse")
		return ""
	})

	do(t, tr, "https://login.example/oauth/token")

	if got := calls.Load(); got != 0 {
		t.Errorf("expected no refresh on the oauth path, got %d", got)
	}
	if h := rec.headers()[0]; h != "" {
		t.Errorf("oauth request should go out unauthenticated, got %q", h)
	}
}

// Karpenter reconciles with large worker counts, so a token expiring under
// load means many goroutines discover it at once. They must produce one
// refresh between them, and -race must stay quiet.
func TestTokenRefreshesOnceUnderConcurrency(t *testing.T) {
	fresh := jwt(t, time.Hour)
	tr, _, calls := newTestTransport(t, jwt(t, -time.Minute), func(n int) string {
		time.Sleep(10 * time.Millisecond) // widen the window for a double refresh
		return fresh
	})

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			do(t, tr, "https://spot.example/apis/ngpc.rxt.io/v1/serverclasses")
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("50 concurrent requests should share 1 refresh, got %d", got)
	}
}

// RoundTrip is documented as not mutating the request it is given.
func TestRoundTripDoesNotMutateRequest(t *testing.T) {
	tr, _, _ := newTestTransport(t, jwt(t, time.Hour), func(int) string { return "" })
	req, err := http.NewRequest(http.MethodGet, "https://spot.example/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if h := req.Header.Get("Authorization"); h != "" {
		t.Errorf("caller's request was mutated: Authorization = %q", h)
	}
}

// A malformed or unparseable token is worth one wasted refresh; sending it
// yields a 401 the SDK reports as a permissions error, which is what sent
// people looking in the wrong place to begin with.
func TestExpiredTreatsUnusableTokensAsExpired(t *testing.T) {
	for name, tok := range map[string]string{
		"empty":          "",
		"not a jwt":      "opaque-token",
		"two segments":   "a.b",
		"bad base64":     "a.!!!.c",
		"payload no exp": base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`)) + ".s",
	} {
		if !expired(tok) {
			t.Errorf("%s: expected to be treated as expired", name)
		}
	}
	if expired(jwt(t, time.Hour)) {
		t.Error("a valid token was reported expired")
	}
}

// Install must leave the SDK's own transport in the chain -- it carries the
// dialer, timeouts and connection pooling the SDK configured.
func TestInstallPreservesUnderlyingTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	rec := &recorder{}
	client := &http.Client{Transport: rec}
	tr := NewTransport(client.Transport, srv.URL, "https://login.example", "rt", jwt(t, time.Hour))
	client.Transport = tr

	if _, err := client.Get(srv.URL); err != nil {
		t.Fatal(err)
	}
	if len(rec.headers()) != 1 {
		t.Fatalf("request did not reach the wrapped transport: %d calls", len(rec.headers()))
	}
}

// A refresh that fails must surface as an error rather than sending a request
// with no usable credentials.
func TestRoundTripPropagatesRefreshFailure(t *testing.T) {
	rec := &recorder{}
	tr := NewTransport(rec, "https://spot.example", "https://login.example", "rt", jwt(t, -time.Minute))
	tr.refresher = func(context.Context, string, string) (string, error) {
		return "", fmt.Errorf("refresh token revoked")
	}
	req, _ := http.NewRequest(http.MethodGet, "https://spot.example/x", nil)
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("expected an error when the refresh fails")
	}
	if len(rec.headers()) != 0 {
		t.Error("request should not have been sent without a token")
	}
}

// A redirect is followed by calling RoundTrip again with the new location, and
// the Authorization header is attached here rather than by the caller, so the
// stdlib never gets the chance to strip it on the way across. A response that
// points somewhere else must not take the Rackspace token with it.
func TestRoundTripWithholdsTokenFromOtherOrigins(t *testing.T) {
	valid := jwt(t, time.Hour)
	for name, target := range map[string]string{
		"another host":   "https://evil.example/apis/ngpc.rxt.io/v1/serverclasses",
		"another scheme": "http://spot.example/apis/ngpc.rxt.io/v1/serverclasses",
		"another port":   "https://spot.example:8443/apis/ngpc.rxt.io/v1/serverclasses",
		"subdomain":      "https://spot.example.evil.example/apis/ngpc.rxt.io/v1/serverclasses",
	} {
		t.Run(name, func(t *testing.T) {
			tr, rec, calls := newTestTransport(t, valid, func(int) string { return valid })

			do(t, tr, target)

			if h := rec.headers()[0]; h != "" {
				t.Errorf("token leaked to %s: Authorization = %q", target, h)
			}
			if got := calls.Load(); got != 0 {
				t.Errorf("a request we do not authenticate should not refresh; got %d", got)
			}
		})
	}
}

// The API origin is compared after normalization, so spellings that name the
// same origin still get the token -- otherwise every call would go out
// unauthenticated and fail as a permissions error.
func TestRoundTripSendsTokenToEquivalentOrigins(t *testing.T) {
	valid := jwt(t, time.Hour)
	for name, target := range map[string]string{
		"exact":          "https://spot.example/apis/ngpc.rxt.io/v1/serverclasses",
		"uppercase host": "https://SPOT.EXAMPLE/apis/ngpc.rxt.io/v1/serverclasses",
		"default port":   "https://spot.example:443/apis/ngpc.rxt.io/v1/serverclasses",
	} {
		t.Run(name, func(t *testing.T) {
			tr, rec, _ := newTestTransport(t, valid, func(int) string { return valid })

			do(t, tr, target)

			if got, want := rec.headers()[0], "Bearer "+valid; got != want {
				t.Errorf("%s: got %q want %q", target, got, want)
			}
		})
	}
}

// Without a base URL there is no origin to trust, and sending the token to
// whatever host the request names would be a guess. Fail loudly instead: an
// unconfigured base URL cannot produce a working request anyway.
func TestRoundTripRefusesWithoutBaseURL(t *testing.T) {
	rec := &recorder{}
	tr := NewTransport(rec, "", "https://login.example", "rt", jwt(t, time.Hour))

	req, err := http.NewRequest(http.MethodGet, "https://spot.example/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("expected an error when no base URL is configured")
	}
	if len(rec.headers()) != 0 {
		t.Error("request should not have been sent")
	}
}

// countingTransport stands in for the transport the SDK tunes -- its dialer
// timeout, idle connection limits and TLS settings live here. It counts what
// passes through and forwards the rest.
type countingTransport struct {
	next  http.RoundTripper
	count atomic.Int32
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.count.Add(1)
	return c.next.RoundTrip(req)
}

// The token exchange is the one call that must work before anything else can,
// so it should not be the one call made on a bare client. It goes out on a
// copy of the SDK's -- same timeout, same tuned transport underneath -- and
// not through the Transport itself, which would recurse.
func TestInstallRefreshesOnACopyOfTheSDKClient(t *testing.T) {
	fresh := jwt(t, time.Hour)
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			t.Errorf("the token exchange carried a token of its own: %q", h)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": fresh})
	}))
	defer oauth.Close()

	base := &countingTransport{next: http.DefaultTransport}
	sdk := &rxtspot.RackspaceSpotClient{
		BaseURL:      "https://spot.example",
		OAuthURL:     oauth.URL,
		RefreshToken: "rt",
		Token:        jwt(t, -time.Minute),
		HTTPClient:   &http.Client{Transport: base, Timeout: 7 * time.Second},
	}

	tr := Install(sdk)

	if sdk.HTTPClient.Transport != tr {
		t.Fatal("Install did not wire the Transport into the SDK client")
	}
	if got, want := tr.oauthClient.Timeout, 7*time.Second; got != want {
		t.Errorf("oauth client dropped the SDK's timeout: got %v want %v", got, want)
	}
	if tr.oauthClient.Transport != base {
		t.Error("oauth client must keep the SDK's transport rather than the token injector or a bare default")
	}

	tok, err := tr.Token(t.Context())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != fresh {
		t.Errorf("got %q want the freshly minted token", tok)
	}
	if base.count.Load() == 0 {
		t.Error("the refresh bypassed the transport the SDK configured")
	}
}

// A caller may hand the SDK an http.Client with no timeout of its own. The
// refresh runs under the write lock, so a hung exchange stalls every request
// behind it; keep the bound the bare client used to provide.
func TestInstallBoundsAnUntimedClient(t *testing.T) {
	sdk := &rxtspot.RackspaceSpotClient{
		BaseURL:    "https://spot.example",
		OAuthURL:   "https://login.example",
		HTTPClient: &http.Client{Transport: &recorder{}},
	}

	tr := Install(sdk)

	if got := tr.oauthClient.Timeout; got != oauthTimeout {
		t.Errorf("oauth client left unbounded: timeout = %v", got)
	}
	if sdk.HTTPClient.Timeout != 0 {
		t.Error("Install mutated the SDK's own client instead of a copy")
	}
}

// closeCounter is a request body that records having been closed.
type closeCounter struct {
	closed atomic.Int32
}

func (c *closeCounter) Read([]byte) (int, error) { return 0, io.EOF }
func (c *closeCounter) Close() error {
	c.closed.Add(1)
	return nil
}

// A RoundTripper owns the request body once it is handed one, and http.Client
// does not close it when RoundTrip returns an error of its own. Every early
// return has to, or a caller passing a file or a pipe leaks a descriptor per
// failed request.
func TestRoundTripClosesBodyOnError(t *testing.T) {
	t.Run("no base url", func(t *testing.T) {
		tr := NewTransport(&recorder{}, "", "https://login.example", "rt", jwt(t, time.Hour))
		body := &closeCounter{}
		req, err := http.NewRequest(http.MethodPost, "https://spot.example/x", body)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tr.RoundTrip(req); err == nil {
			t.Fatal("expected an error")
		}
		if got := body.closed.Load(); got != 1 {
			t.Errorf("body closed %d times, want 1", got)
		}
	})

	t.Run("refresh failed", func(t *testing.T) {
		tr := NewTransport(&recorder{}, "https://spot.example", "https://login.example", "rt", jwt(t, -time.Minute))
		tr.refresher = func(context.Context, string, string) (string, error) {
			return "", fmt.Errorf("refresh token revoked")
		}
		body := &closeCounter{}
		req, err := http.NewRequest(http.MethodPost, "https://spot.example/x", body)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tr.RoundTrip(req); err == nil {
			t.Fatal("expected an error")
		}
		if got := body.closed.Load(); got != 1 {
			t.Errorf("body closed %d times, want 1", got)
		}
	})
}

// The SDK also accepts a bare access token: SPOT_ACCESS_TOKEN with no
// SPOT_REFRESH_TOKEN. There is nothing to renew from, so the startup token
// must still go out rather than every request failing on an exchange that
// cannot happen. The token here is one we cannot read, so we have no evidence
// it is spent -- it goes out and Rackspace judges it.
func TestTokenWithoutARefreshTokenPassesTheSeedThrough(t *testing.T) {
	rec := &recorder{}
	tr := NewTransport(rec, "https://spot.example", "https://login.example", "", "opaque-access-token")
	tr.refresher = func(context.Context, string, string) (string, error) {
		t.Error("there is no refresh token to redeem")
		return "", nil
	}

	do(t, tr, "https://spot.example/apis/ngpc.rxt.io/v1/serverclasses")

	if got, want := rec.headers()[0], "Bearer opaque-access-token"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// A token we can read and know is spent, with no way to renew it, is worth an
// error that says so: sending it earns a 401 the SDK renders as "access
// denied: you do not have permission to ...", which is what sent people
// looking for a permissions problem to begin with.
func TestTokenWithoutARefreshTokenReportsAnExpiredSeed(t *testing.T) {
	rec := &recorder{}
	tr := NewTransport(rec, "https://spot.example", "https://login.example", "", jwt(t, -time.Minute))

	_, err := tr.Token(t.Context())
	if err == nil {
		t.Fatal("expected an error for a spent token that cannot be renewed")
	}
	if !strings.Contains(err.Error(), "SPOT_REFRESH_TOKEN") {
		t.Errorf("error should name the missing setting; got %v", err)
	}
	if len(rec.headers()) != 0 {
		t.Error("nothing should have been sent")
	}
}

// A seed token inside the renewal leeway is not spent yet. With a refresh
// token that window is when we renew early; without one there is nothing to
// renew with, so the seconds still on the token beat a certain failure.
func TestTokenWithoutARefreshTokenSendsASeedInsideTheLeeway(t *testing.T) {
	rec := &recorder{}
	nearly := jwt(t, 30*time.Second) // inside the 60s leeway
	tr := NewTransport(rec, "https://spot.example", "https://login.example", "", nearly)

	do(t, tr, "https://spot.example/apis/ngpc.rxt.io/v1/serverclasses")

	if got, want := rec.headers()[0], "Bearer "+nearly; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
