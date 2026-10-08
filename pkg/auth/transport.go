/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package auth keeps the Rackspace Spot bearer token fresh for the lifetime of
// the process.
//
// The SDK authenticates once and then never again on its own. Its
// Authenticate() is written to refresh -- it returns the cached token unless
// the JWT is within 60s of expiring, otherwise it redeems the refresh token --
// but nothing calls it on a schedule. Every request is built by authHeader(),
// which reads the RackspaceSpotClient.Token field that only startup ever set.
// So roughly an hour after the controller boots, that id_token expires and
// every call starts coming back 401. The SDK renders that as
//
//	access denied: you do not have permission to list the region ''
//
// which reads like a permissions problem and is really an expiry problem. From
// then on the controller cannot list server classes, so it cannot provision
// anything, until someone restarts the pod. Operators have been running a cron
// job to do exactly that.
//
// The fix belongs below the SDK rather than beside it. Injecting the header in
// a RoundTripper covers every call in one place -- pools, cloudspaces,
// organizations, server classes, and any method added later -- instead of
// asking each call site to remember to refresh first.
//
// It also sidesteps a data race. The obvious alternative, calling the SDK's
// Authenticate() before each request, writes RackspaceSpotClient.Token while
// other goroutines are reading it in authHeader(). Karpenter runs its
// reconcilers with large worker counts, so those reads and writes genuinely
// overlap, and a torn read of a string header is not a hypothetical. This
// transport never touches that field: it owns its own token behind an RWMutex
// and overwrites the Authorization header on the way out. The value the SDK
// put there is ignored.
//
// Renewal needs a refresh token, and the SDK will just as happily be
// configured with a bare access token: SPOT_ACCESS_TOKEN lands in the client's
// Token field and SPOT_REFRESH_TOKEN in RefreshToken, so an empty RefreshToken
// says there is nothing to renew from. The transport then falls back to
// injecting the startup token unchanged, which is the pre-existing behaviour,
// minus the misleading permissions error: once that token is demonstrably
// spent, calls fail saying so instead of coming back as "access denied".
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	rxtspot "github.com/rackspace-spot/spot-go-sdk/api/v1"
)

// oauthTokenPath is the one endpoint this transport must not touch: it is how
// the token is obtained, so injecting a token into it would recurse.
const oauthTokenPath = "/oauth/token"

// expiryLeeway is how long before the JWT's exp we treat it as already
// expired, matching the SDK's own margin so behaviour doesn't change at the
// boundary. It has to cover both clock skew against Rackspace and the flight
// time of a request that reads the token now and is validated a moment later.
const expiryLeeway = 60 * time.Second

// oauthTimeout bounds the token exchange when the client it is made on has no
// timeout of its own -- a hung OAuth request would otherwise block every
// caller waiting behind the refresh lock.
const oauthTimeout = 30 * time.Second

// Transport injects a current bearer token into every outbound Rackspace
// request. The zero value is not usable; construct it with NewTransport.
type Transport struct {
	base         http.RoundTripper
	oauthURL     string
	refreshToken string

	// apiOrigin is the only scheme+host the token is handed to. Its zero
	// value means the base URL could not be parsed, and RoundTrip refuses to
	// send anything rather than guess.
	apiOrigin origin

	// oauthClient carries the token exchange. It is never the client this
	// Transport is installed on: routing the refresh through ourselves would
	// recurse.
	oauthClient *http.Client

	// canRefresh is whether a refresh token was configured at all. Without
	// one there is nothing to redeem for a new id_token; see Token.
	canRefresh bool

	// refresher performs the OAuth exchange. It is a field so tests can
	// substitute one without standing up an OAuth server.
	refresher func(ctx context.Context, oauthURL, refreshToken string) (string, error)

	mu    sync.RWMutex
	token string
}

// NewTransport wraps base so that every request to baseURL carries a fresh
// token. seed is the token already obtained at startup; it may be empty, in
// which case the first request triggers a refresh.
//
// Renewal happens only when a refreshToken is given. Without one the transport
// falls back to injecting seed unchanged, which is what the SDK did before this
// package existed.
func NewTransport(base http.RoundTripper, baseURL, oauthURL, refreshToken, seed string) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	t := &Transport{
		base:         base,
		oauthURL:     oauthURL,
		refreshToken: refreshToken,
		canRefresh:   refreshToken != "",
		token:        seed,
		oauthClient:  &http.Client{Transport: base, Timeout: oauthTimeout},
	}
	t.refresher = func(ctx context.Context, oauthURL, refreshToken string) (string, error) {
		return fetchIDToken(ctx, t.oauthClient, oauthURL, refreshToken)
	}
	if u, err := url.Parse(baseURL); err == nil && u.Scheme != "" && u.Host != "" {
		t.apiOrigin = originOf(u)
	}
	return t
}

// Install wires a Transport into an SDK client, preserving the transport the
// SDK already configured (its dialer, timeouts and connection pooling) and
// layering token injection on top.
//
// The client's own Token field is left as startup set it. Nothing reads it any
// more, because RoundTrip replaces the header it produces.
//
// The token exchange goes out on a shallow copy of the SDK's http.Client, so
// it keeps the timeout, redirect policy and cookie jar configured for every
// other call -- and the tuned transport underneath, with its dialer timeout
// and connection pooling, rather than a bare http.DefaultTransport. The copy
// is taken before the Transport is installed and pinned to the original base,
// so the refresh cannot route through the Transport that asked for it.
func Install(client *rxtspot.RackspaceSpotClient) *Transport {
	oauthClient := *client.HTTPClient

	t := NewTransport(client.HTTPClient.Transport, client.BaseURL, client.OAuthURL, client.RefreshToken, client.Token)
	oauthClient.Transport = t.base
	if oauthClient.Timeout == 0 {
		oauthClient.Timeout = oauthTimeout
	}
	t.oauthClient = &oauthClient

	client.HTTPClient.Transport = t
	return t
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// The token request itself must go out unmodified, or obtaining a token
	// would require already having one.
	if strings.HasSuffix(req.URL.Path, oauthTokenPath) {
		return t.base.RoundTrip(req)
	}

	// Only the Spot API gets the token. An http.Client follows redirects by
	// calling RoundTrip again with the new location, and because the header is
	// attached here rather than by the caller, the stdlib's own cross-domain
	// stripping never sees it -- a 302 pointing elsewhere would otherwise hand
	// the token to whoever answers there.
	reqOrigin := originOf(req.URL)
	if t.apiOrigin == (origin{}) {
		closeBody(req)
		return nil, fmt.Errorf("no rackspace spot base URL configured, refusing to send a token to %s://%s", reqOrigin.scheme, reqOrigin.host)
	}
	if reqOrigin != t.apiOrigin {
		return t.base.RoundTrip(req)
	}

	token, err := t.Token(req.Context())
	if err != nil {
		closeBody(req)
		return nil, fmt.Errorf("refreshing rackspace spot token: %w", err)
	}

	// RoundTrip must not mutate the request it is handed.
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(clone)
}

// closeBody discharges the half of the RoundTripper contract that says the
// body is closed even when no response comes back. http.Client closes it for
// the paths that reach the wire, but not for a RoundTripper that returns an
// error of its own, so every early return here has to do it. The SDK only ever
// hands over a bytes.Reader, where this is a no-op; a caller that passes a
// file or a pipe would otherwise leak a descriptor per failed request.
func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

// Token returns a token that is valid now, refreshing it if the current one is
// spent and there is a refresh token to spend.
func (t *Transport) Token(ctx context.Context) (string, error) {
	t.mu.RLock()
	tok := t.token
	t.mu.RUnlock()
	if !expired(tok) {
		return tok, nil
	}

	// Without a refresh token there is nothing to renew with, so the startup
	// token is all there is: hand it over and let Rackspace judge it, exactly
	// as the SDK did before. The one case worth intercepting is a token we can
	// read and know is spent -- the 401 it earns comes back as "access denied:
	// you do not have permission to ...", which is what sent people hunting
	// for a permissions problem in the first place.
	if !t.canRefresh {
		if tok == "" {
			return "", errors.New("no rackspace spot token available and no refresh token to obtain one (set SPOT_REFRESH_TOKEN)")
		}
		// expired() reports spent a leeway before exp, which is the right
		// call when a refresh is available and the wrong one here: with
		// nothing to renew with, a token that still has seconds on it is
		// worth more than a certain failure.
		if exp, ok := expiry(tok); ok && !time.Now().Before(exp) {
			return "", fmt.Errorf("rackspace spot token expired at %s and there is no refresh token to renew it (set SPOT_REFRESH_TOKEN)",
				exp.UTC().Format(time.RFC3339))
		}
		return tok, nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	// Another goroutine may have refreshed while we waited for the write lock;
	// without this, a burst of concurrent requests past the expiry would each
	// redeem the refresh token.
	if !expired(t.token) {
		return t.token, nil
	}
	fresh, err := t.refresher(ctx, t.oauthURL, t.refreshToken)
	if err != nil {
		return "", err
	}
	t.token = fresh
	return fresh, nil
}

// origin is the scheme and host that decide whether a request is talking to
// the Spot API, compared as a value so a mismatch is one equality check.
type origin struct {
	scheme string
	host   string
}

// originOf normalizes a URL down to its origin: case-insensitive scheme and
// host, with the port dropped when it is the scheme's default, so that
// https://spot.example and https://SPOT.example:443 are one origin.
func originOf(u *url.URL) origin {
	o := origin{
		scheme: strings.ToLower(u.Scheme),
		host:   strings.ToLower(u.Hostname()),
	}
	if port := u.Port(); port != "" && port != defaultPort(o.scheme) {
		o.host = net.JoinHostPort(o.host, port)
	}
	return o
}

func defaultPort(scheme string) string {
	switch scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// fetchIDToken redeems the refresh token for a new id_token on httpClient.
//
// This duplicates what the SDK's Authenticate() does, deliberately. Calling
// the SDK method instead would write to the shared client's Token field, which
// is the race this package exists to avoid.
func fetchIDToken(ctx context.Context, httpClient *http.Client, oauthURL, refreshToken string) (string, error) {
	if refreshToken == "" {
		return "", errors.New("no refresh token configured (set SPOT_REFRESH_TOKEN)")
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", rxtspot.GetClientID())
	form.Set("refresh_token", refreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthURL+oauthTokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("authentication failed: %s", resp.Status)
	}

	var body struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decoding token response: %w", err)
	}
	if body.IDToken == "" {
		return "", errors.New("no id_token in authentication response")
	}
	return body.IDToken, nil
}

// expiry returns when a JWT expires, and whether it could be read at all. A
// token we cannot parse reports false rather than a zero time, so callers can
// tell "spent" from "unreadable" -- the two want different error messages.
func expiry(token string) (time.Time, bool) {
	if token == "" {
		return time.Time{}, false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, false
	}
	if claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// expired reports whether a JWT is spent, or unusable for any other reason. A
// token we cannot read is treated as expired: refreshing costs one request,
// whereas sending a token we failed to parse costs a 401 that surfaces as a
// misleading permissions error.
func expired(token string) bool {
	exp, ok := expiry(token)
	if !ok {
		return true
	}
	return time.Now().After(exp.Add(-expiryLeeway))
}
