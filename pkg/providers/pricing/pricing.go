/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package pricing

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	rxtspot "github.com/rackspace-spot/spot-go-sdk/api/v1"
)

// FeedURL is Rackspace's public unauthenticated percentile feed. It carries
// 20/50/80 percentile spot prices + current market price per ServerClass per
// region, updated continuously.
const FeedURL = "https://ngpc-prod-public-data.s3.us-east-2.amazonaws.com/percentiles.json"

// initialPricesJSON is a snapshot of the live S3 feed taken at build time.
// Used as a cold-start fallback and when the live fetch fails.
// Refreshed by the update-pricing workflow.
//
//go:embed initial-prices.json
var initialPricesJSON []byte

// Percentiles is what we surface to callers — the four numbers from a single
// (region, ServerClass) entry in the feed.
type Percentiles struct {
	P20         float64
	P50         float64
	P80         float64
	MarketPrice float64
}

// Provider exposes per-hour prices for Rackspace Spot ServerClasses.
//
// SpotPrice / OnDemandPrice / MinBidPrice still read straight from a
// ServerClass for backward compatibility — they're inexpensive and Karpenter
// already has the ServerClass at scheduling time.
//
// Percentiles fetches from the live S3 feed (with caching + embedded
// fallback) and is the right input to smart-bid logic — bidding at P80 +
// market-clearing buffer gives stable wins across momentary spot price
// spikes.
type Provider interface {
	SpotPrice(sc *rxtspot.ServerClass) float64
	OnDemandPrice(sc *rxtspot.ServerClass) float64
	MinBidPrice(sc *rxtspot.ServerClass) float64
	Percentiles(ctx context.Context, region, serverClass string) (Percentiles, error)
}

type DefaultProvider struct {
	httpClient   *http.Client
	refreshAfter time.Duration

	mu        sync.RWMutex
	cached    *feed
	cachedAt  time.Time
	cacheErr  error // sticky error suppression: log once per refresh window
	loadedEmb bool  // true after the first successful embed parse
}

func NewProvider() *DefaultProvider {
	return &DefaultProvider{
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		refreshAfter: 5 * time.Minute,
	}
}

func (*DefaultProvider) SpotPrice(sc *rxtspot.ServerClass) float64 {
	return parse(sc.CurrentMarketPricePerHour)
}

func (*DefaultProvider) OnDemandPrice(sc *rxtspot.ServerClass) float64 {
	return parse(sc.OnDemandPricePerHour)
}

func (*DefaultProvider) MinBidPrice(sc *rxtspot.ServerClass) float64 {
	return parse(sc.MinBidPricePerHour)
}

func (p *DefaultProvider) Percentiles(ctx context.Context, region, serverClass string) (Percentiles, error) {
	f, err := p.load(ctx)
	if err != nil {
		return Percentiles{}, err
	}
	r, ok := f.Regions[region]
	if !ok {
		return Percentiles{}, fmt.Errorf("region %q not in pricing feed", region)
	}
	sc, ok := r.ServerClasses[serverClass]
	if !ok {
		return Percentiles{}, fmt.Errorf("server class %q not in pricing feed for region %q", serverClass, region)
	}
	return Percentiles{
		P20:         sc.P20,
		P50:         sc.P50,
		P80:         sc.P80,
		MarketPrice: parse(sc.MarketPrice),
	}, nil
}

// load returns the cached feed, refreshing via HTTP after refreshAfter
// elapses. On any HTTP failure it falls through to the embedded snapshot.
func (p *DefaultProvider) load(ctx context.Context) (*feed, error) {
	p.mu.RLock()
	if p.cached != nil && time.Since(p.cachedAt) < p.refreshAfter {
		f := p.cached
		p.mu.RUnlock()
		return f, nil
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached != nil && time.Since(p.cachedAt) < p.refreshAfter {
		return p.cached, nil
	}

	if live, err := p.fetchLive(ctx); err == nil {
		p.cached = live
		p.cachedAt = time.Now()
		p.cacheErr = nil
		return live, nil
	} else {
		p.cacheErr = err
	}

	if p.cached != nil { // we have a stale-but-valid cache; serve it
		return p.cached, nil
	}

	emb, err := p.loadEmbedded()
	if err != nil {
		return nil, fmt.Errorf("live fetch failed (%v) and embedded fallback unreadable: %w", p.cacheErr, err)
	}
	p.cached = emb
	p.cachedAt = time.Now()
	p.loadedEmb = true
	return emb, nil
}

func (p *DefaultProvider) fetchLive(ctx context.Context) (*feed, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, FeedURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("pricing feed returned %d", resp.StatusCode)
	}
	var f feed
	if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
		return nil, fmt.Errorf("decoding pricing feed: %w", err)
	}
	return &f, nil
}

func (p *DefaultProvider) loadEmbedded() (*feed, error) {
	var f feed
	if err := json.Unmarshal(initialPricesJSON, &f); err != nil {
		return nil, fmt.Errorf("parsing embedded snapshot: %w", err)
	}
	return &f, nil
}

// feed mirrors the S3 schema (subset of fields we care about).
type feed struct {
	Regions map[string]regionEntry `json:"regions"`
}

type regionEntry struct {
	Generation    string                       `json:"generation"`
	ServerClasses map[string]serverClassEntry  `json:"serverclasses"`
}

type serverClassEntry struct {
	P20         float64 `json:"20_percentile"`
	P50         float64 `json:"50_percentile"`
	P80         float64 `json:"80_percentile"`
	MarketPrice string  `json:"market_price"`
	CPU         string  `json:"cpu,omitempty"`
	Memory      string  `json:"memory,omitempty"`
	DisplayName string  `json:"display_name,omitempty"`
	Category    string  `json:"category,omitempty"`
	Description string  `json:"description,omitempty"`
}

// parse handles Rackspace's "$0.001000"-style strings (currency prefix +
// leading/trailing whitespace) and returns 0 when the value can't be parsed.
func parse(s string) float64 {
	s = strings.TrimPrefix(strings.TrimSpace(s), "$")
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}
