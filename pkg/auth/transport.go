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
// Authenticate() on the shared client before each request, writes
// RackspaceSpotClient.Token while other goroutines are reading it in
// authHeader(). Karpenter runs its reconcilers with large worker counts, so
// those reads and writes genuinely overlap, and a torn read of a string header
// is not a hypothetical. This transport still uses Authenticate(), but on a
// private copy of the client that only it touches, under its own mutex, and
// overwrites the Authorization header on the way out. The value the SDK put
// there is ignored.
//
// Renewal needs a refresh token, and the SDK will just as happily be
// configured with a bare access token: SPOT_ACCESS_TOKEN lands in the client's
// Token field and SPOT_REFRESH_TOKEN in RefreshToken, so an empty RefreshToken
// says there is nothing to renew from. The transport then falls back to
// injecting the startup token unchanged, which is the pre-existing behaviour,
// minus the misleading permissions error: once that token is demonstrably
// spent, calls fail saying so instead of coming back as "access denied".
//
// Failures leave here typed, and marked for the SDK's retry loop. The SDK
// retries any error from http.Client.Do as if it were a dropped connection --
// up to ten attempts over as long as ten minutes -- and gives up at once only
// on a 4xx response. A token that cannot be had is no more likely to appear on
// the next attempt than a 4xx is to turn into a 200, so both go back wrapped in
// backoff.Permanent: see AuthError and APIError.
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v5"
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

// maxErrorBody caps how much of a 4xx response body an APIError carries. The
// body is there to explain the status, and an error message is not the place
// for an arbitrarily large one.
const maxErrorBody = 4 << 10

// Transport injects a current bearer token into every outbound Rackspace
// request. The zero value is not usable; build one with Install.
type Transport struct {
	base http.RoundTripper

	// apiOrigin is the only scheme+host the token is handed to. Its zero
	// value means the base URL could not be parsed, and RoundTrip refuses to
	// send anything rather than guess.
	apiOrigin origin

	// sdk is a private copy of the SDK client, used only to call Authenticate.
	// Authenticate writes sdk.Token, so the copy is what keeps that write off
	// the shared client the rest of the process reads. Its HTTPClient is the
	// original, un-wrapped one: routing the refresh through this Transport
	// would recurse.
	sdk *rxtspot.RackspaceSpotClient

	// canRefresh is whether a refresh token was configured at all. Without
	// one there is nothing to redeem for a new id_token; see Token.
	canRefresh bool

	// oauthStatus is the status code of the last token exchange, or 0 if it
	// got no response. Authenticate reports a rejected refresh token and an
	// unreachable endpoint as the same untyped error, and only one of them is
	// worth retrying.
	oauthStatus int

	// mu guards sdk and oauthStatus. Authenticate both reads and writes
	// sdk.Token, and the exchange it makes writes oauthStatus.
	mu sync.Mutex
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
	base := client.HTTPClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}

	t := &Transport{
		base:       base,
		canRefresh: client.RefreshToken != "",
	}

	oauthClient := *client.HTTPClient
	oauthClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		// Authenticate only runs under t.mu, and Do calls this on the same
		// goroutine, so the write is covered by that lock.
		resp, err := base.RoundTrip(req)
		t.oauthStatus = 0
		if resp != nil {
			t.oauthStatus = resp.StatusCode
		}
		return resp, err
	})
	if oauthClient.Timeout == 0 {
		oauthClient.Timeout = oauthTimeout
	}
	sdk := *client
	sdk.HTTPClient = &oauthClient
	t.sdk = &sdk

	if u, err := url.Parse(client.BaseURL); err == nil && u.Scheme != "" && u.Host != "" {
		t.apiOrigin = originOf(u)
	}

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
		var authErr *AuthError
		if errors.As(err, &authErr) && !authErr.Temporary {
			return nil, backoff.Permanent(err)
		}
		return nil, err
	}

	// RoundTrip must not mutate the request it is handed.
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+token)
	resp, err := t.base.RoundTrip(clone)
	if err != nil || resp.StatusCode < 400 || resp.StatusCode >= 500 {
		return resp, err
	}

	// A 4xx comes back as an error rather than a response. That is against
	// the letter of the RoundTripper contract, which wants every response
	// returned whatever its status, but it is the only way the status code
	// survives the SDK -- see APIError. The SDK already treats a 4xx as final,
	// so marking it permanent keeps its retry behaviour as it was.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
	return nil, backoff.Permanent(&APIError{
		StatusCode: resp.StatusCode,
		Method:     req.Method,
		Path:       req.URL.Path,
		Body:       strings.TrimSpace(string(body)),
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

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
// spent and there is a refresh token to spend. The expiry check, the OAuth
// exchange and its error handling are the SDK's Authenticate.
func (t *Transport) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Without a refresh token there is nothing to renew with, so the startup
	// token is all there is: hand it over and let Rackspace judge it, exactly
	// as the SDK did before. The one case worth intercepting is a token we can
	// read and know is spent -- the 401 it earns comes back as "access denied:
	// you do not have permission to ...", which is what sent people hunting
	// for a permissions problem in the first place. Authenticate would instead
	// post an empty refresh token and report a bare 400.
	if !t.canRefresh {
		tok := t.sdk.Token
		if tok == "" {
			return "", &AuthError{Err: errors.New("no token configured and no refresh token to obtain one (set SPOT_REFRESH_TOKEN)")}
		}
		if exp, ok := expiry(tok); ok && !time.Now().Before(exp) {
			return "", &AuthError{Err: fmt.Errorf("token expired at %s and there is no refresh token to renew it (set SPOT_REFRESH_TOKEN)",
				exp.UTC().Format(time.RFC3339))}
		}
		return tok, nil
	}

	// Authenticate returns the cached token until it is within 60s of expiring,
	// so a burst of concurrent requests past the expiry redeems the refresh
	// token once: the rest queue on mu and then hit the cache.
	t.oauthStatus = 0
	tok, err := t.sdk.Authenticate(ctx)
	if err != nil {
		// No response at all, or a 5xx, may clear up. A 4xx is Rackspace
		// refusing the refresh token, and a 200 that Authenticate could not
		// use will be just as unusable next time.
		return "", &AuthError{Err: err, Temporary: t.oauthStatus == 0 || t.oauthStatus >= 500}
	}
	return tok, nil
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
