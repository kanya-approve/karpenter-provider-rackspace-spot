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

const testRegion = "us-central-ord-1"

// spotAPI is a stand-in for the two Rackspace endpoints a refresh touches.
// gate, when non-nil, holds the serverclasses response open so a refresh can be
// observed while it is in flight.
//
// Refreshes are counted on /regions rather than /serverclasses. ListServerClasses
// checks the region exactly once per call, whereas /serverclasses is requested
// twice per refresh -- once by ListServerClasses and again by fetchDisks, which
// reads the raw payload for the disk field the SDK drops. Counting /regions
// therefore counts refreshes, not requests.
type spotAPI struct {
	gate      chan struct{}
	refreshes atomic.Int32 // one per load() that actually fetched
	inFlight  atomic.Int32 // serverclasses requests that have begun
}

// handler serves the two endpoints a refresh touches: /regions, which
// ListServerClasses validates the region against, and /serverclasses, which
// both ListServerClasses and fetchDisks read.
func (s *spotAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/apis/ngpc.rxt.io/v1/regions", func(w http.ResponseWriter, _ *http.Request) {
		s.refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"` + testRegion + `"},"spec":{"description":"test"}}]}`))
	})
	mux.HandleFunc("/apis/ngpc.rxt.io/v1/serverclasses", func(w http.ResponseWriter, _ *http.Request) {
		s.inFlight.Add(1)
		if s.gate != nil {
			<-s.gate
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

// newTestProvider wires a provider to an httptest server backed by api, so the
// tests exercise the real SDK path. Retries are kept short so a test that does
// hit one does not inherit the production backoff.
func newTestProvider(t *testing.T, api *spotAPI) (*DefaultProvider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)

	client := &rxtspot.RackspaceSpotClient{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Token:      "test-token",
		RetryConfig: rxtspot.RetryConfig{
			MaxRetries:          1,
			RetryWaitMax:        time.Second,
			RandomizationFactor: 0,
			Multiplier:          2,
			InitialInterval:     time.Millisecond,
		},
	}
	return NewProvider(client), srv
}

// The regression: load used to hold the same mutex that guards the capacity
// measurements, so a refresh in flight blocked every node reporting capacity.
// The SDK's retry budget is minutes, so that stall could be minutes long.
func TestUpdateFromNodeNotBlockedByInFlightRefresh(t *testing.T) {
	api := &spotAPI{gate: make(chan struct{})}
	p, _ := newTestProvider(t, api)

	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		if _, err := p.load(context.Background(), testRegion); err != nil {
			t.Errorf("load: %v", err)
		}
	}()

	// Wait until the refresh is genuinely in flight and parked in the handler.
	waitFor(t, func() bool { return api.inFlight.Load() >= 1 })

	updated := make(chan struct{})
	go func() {
		defer close(updated)
		p.UpdateFromNode("gp.vs1.large-ord", corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("15Gi"),
		})
	}()

	select {
	case <-updated:
	case <-time.After(2 * time.Second):
		close(api.gate)
		t.Fatal("UpdateFromNode blocked on the in-flight refresh")
	}

	close(api.gate)
	<-refreshDone

	if got := p.discoveredFor("gp.vs1.large-ord")[corev1.ResourceMemory]; got.IsZero() {
		t.Error("measurement recorded during the refresh was lost")
	}
}

// discoveredFor is on the read path for List/Get, and must likewise not wait
// on a refresh it is not the cause of.
func TestDiscoveredForNotBlockedByInFlightRefresh(t *testing.T) {
	api := &spotAPI{gate: make(chan struct{})}
	p, _ := newTestProvider(t, api)

	go func() { _, _ = p.load(context.Background(), testRegion) }()
	waitFor(t, func() bool { return api.inFlight.Load() >= 1 })

	done := make(chan struct{})
	go func() { defer close(done); p.discoveredFor("gp.vs1.large-ord") }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(api.gate)
		t.Fatal("discoveredFor blocked on the in-flight refresh")
	}
	close(api.gate)
}

// Dropping the shared lock must not cost the dedup it was providing: a burst
// of callers on a cold cache should still produce one request, not one each.
func TestConcurrentLoadIssuesOneRequest(t *testing.T) {
	api := &spotAPI{gate: make(chan struct{})}
	p, _ := newTestProvider(t, api)

	var wg sync.WaitGroup
	for range 25 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.load(context.Background(), testRegion); err != nil {
				t.Errorf("load: %v", err)
			}
		}()
	}
	// Let them all pile up on the fetch lock before releasing the handler.
	waitFor(t, func() bool { return api.inFlight.Load() >= 1 })
	time.Sleep(50 * time.Millisecond)
	close(api.gate)
	wg.Wait()

	if got := api.refreshes.Load(); got != 1 {
		t.Errorf("expected 25 concurrent loads to share 1 refresh, got %d", got)
	}
}

// A warm entry must still be served from cache rather than refetched.
func TestLoadServesFromCache(t *testing.T) {
	api := &spotAPI{}
	p, _ := newTestProvider(t, api)

	for range 5 {
		if _, err := p.load(context.Background(), testRegion); err != nil {
			t.Fatalf("load: %v", err)
		}
	}
	if got := api.refreshes.Load(); got != 1 {
		t.Errorf("expected 1 refresh for 5 loads within the TTL, got %d", got)
	}
}

// A stale entry must be refetched once the TTL lapses.
func TestLoadRefreshesAfterTTL(t *testing.T) {
	api := &spotAPI{}
	p, _ := newTestProvider(t, api)
	p.refreshAfter = 10 * time.Millisecond

	if _, err := p.load(context.Background(), testRegion); err != nil {
		t.Fatalf("load: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := p.load(context.Background(), testRegion); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := api.refreshes.Load(); got != 2 {
		t.Errorf("expected a refetch after the TTL, got %d refreshes", got)
	}
}

// End to end through the public method, so the translated result is exercised
// alongside the locking changes.
func TestListReturnsTranslatedServerClasses(t *testing.T) {
	api := &spotAPI{}
	p, _ := newTestProvider(t, api)

	its, err := p.List(context.Background(), testRegion)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(its) != 1 || its[0].Name != "gp.vs1.large-ord" {
		t.Fatalf("unexpected instance types: %+v", its)
	}
	if cpu := its[0].Capacity[corev1.ResourceCPU]; cpu.Value() != 4 {
		t.Errorf("cpu = %v, want 4", cpu.Value())
	}
}

// waitFor polls cond until it holds, failing the test if it never does.
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
