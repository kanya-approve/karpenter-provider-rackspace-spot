/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package instancetype

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rxtspot "github.com/rackspace-spot/spot-go-sdk/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	testRegion = "us-central-ord-1"
	// Unsigned test JWT with {"exp":4102444800}, which expires in 2100.
	testToken = "test.eyJleHAiOjQxMDI0NDQ4MDB9.test"
)

// spotAPI stands in for the Rackspace endpoints a refresh touches. gate, when
// non-nil, holds the serverclasses response open; beforeServerClasses runs
// while the refresh is in flight.
type spotAPI struct {
	gate                chan struct{}
	beforeServerClasses func()
	inFlight            atomic.Int32
}

func (s *spotAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/apis/ngpc.rxt.io/v1/regions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"` + testRegion + `"},"spec":{"description":"test"}}]}`))
	})
	mux.HandleFunc("/apis/ngpc.rxt.io/v1/serverclasses", func(w http.ResponseWriter, _ *http.Request) {
		s.inFlight.Add(1)
		if s.gate != nil {
			<-s.gate
		}
		if s.beforeServerClasses != nil {
			s.beforeServerClasses()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{
			"metadata":{"name":"gp.vs1.large-ord"},
			"spec":{"region":"` + testRegion + `","availability":"available","category":"general",
				"minBidPricePerHour":"0.01","onDemandPricing":{"cost":"0.10"},
				"resources":{"cpu":"4","memory":"16GB","disk":"60GB"}},
			"status":{"spotPricing":{"marketPricePerHour":"0.02"}}}]}`))
	})
	return mux
}

func newTestProvider(t *testing.T, api *spotAPI) *DefaultProvider {
	t.Helper()
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)

	client := &rxtspot.RackspaceSpotClient{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Token:      testToken,
		RetryConfig: rxtspot.RetryConfig{
			MaxRetries:          1,
			RetryWaitMax:        time.Second,
			RandomizationFactor: 0,
			Multiplier:          2,
			InitialInterval:     time.Millisecond,
		},
	}
	return NewProvider(client)
}

func TestUpdateFromNodeNotBlockedByInFlightRefresh(t *testing.T) {
	api := &spotAPI{gate: make(chan struct{})}
	p := newTestProvider(t, api)

	var wg sync.WaitGroup
	release := sync.OnceFunc(func() { close(api.gate) })
	t.Cleanup(func() {
		release()
		wg.Wait()
	})
	wg.Go(func() {
		if _, err := p.load(context.Background(), testRegion); err != nil {
			t.Errorf("load: %v", err)
		}
	})

	waitFor(t, func() bool { return api.inFlight.Load() >= 1 })

	updated := make(chan struct{})
	wg.Go(func() {
		defer close(updated)
		p.UpdateFromNode("gp.vs1.large-ord", corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("15Gi"),
		})
	})

	select {
	case <-updated:
	case <-time.After(2 * time.Second):
		t.Fatal("UpdateFromNode blocked on the in-flight refresh")
	}

	release()
	wg.Wait()

	if got := p.discoveredFor("gp.vs1.large-ord")[corev1.ResourceMemory]; got.IsZero() {
		t.Error("measurement recorded during the refresh was lost")
	}
}

func TestDiscoveredForNotBlockedByInFlightRefresh(t *testing.T) {
	api := &spotAPI{}
	p := newTestProvider(t, api)
	var reads atomic.Int32
	api.beforeServerClasses = func() {
		p.discoveredFor("gp.vs1.large-ord")
		reads.Add(1)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := p.load(ctx, testRegion); err != nil {
		t.Fatalf("load could not complete while calling discoveredFor: %v", err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("discoveredFor blocked until the refresh timed out: %v", err)
	}
	if got := reads.Load(); got != 2 {
		t.Errorf("expected discoveredFor to complete during both serverclasses requests, got %d calls", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}
