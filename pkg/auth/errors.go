/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package auth

import (
	"fmt"
	"net/http"
)

// AuthError reports that no usable bearer token could be had, so the request
// it was meant for was never sent.
type AuthError struct {
	// Temporary is whether asking again could help: the token exchange failed
	// in transit or with a 5xx. A spent token with nothing to renew it, or a
	// refresh token Rackspace refused, fails the same way every time.
	Temporary bool
	Err       error
}

func (e *AuthError) Error() string {
	return "obtaining rackspace spot token: " + e.Err.Error()
}

func (e *AuthError) Unwrap() error { return e.Err }

// APIError is a 4xx answer from the Spot API.
//
// The SDK does build a typed error for these, but every method then passes it
// through handleAPIError, which flattens it into a sentence -- "spot node pool
// 'x' not found" -- and drops the status code. Telling a missing pool from a
// failed call was left to matching substrings of that sentence, and an error
// that merely mentioned a 404 somewhere, such as a token exchange that got
// one, read as the pool being gone. The Transport sees the response before the
// SDK does, so it reports the status here instead, and handleAPIError wraps
// anything that is not its own error type with %w, which keeps this reachable
// through errors.As.
type APIError struct {
	StatusCode int
	Method     string
	// Path is the URL path of the request that failed. One SDK call can make
	// several requests -- the organization lookup, a server class, then the
	// resource itself -- so a 404 alone does not say which of them is missing.
	Path string
	Body string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("rackspace spot API: %s %s: %d %s", e.Method, e.Path, e.StatusCode, http.StatusText(e.StatusCode))
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}
