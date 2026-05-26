/*
Copyright 2026 kanya-approve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package pricing

import (
	"strconv"
	"strings"

	rxtspot "github.com/rackspace-spot/spot-go-sdk/api/v1"
)

// Provider exposes per-hour prices for Rackspace Spot ServerClasses, both
// spot (market) and on-demand, plus the minimum acceptable bid.
//
// In the MVP these come straight from ServerClass fields surfaced by the SDK.
// The plan calls for layering the public S3 percentile feed
// (https://ngpc-prod-public-data.s3.us-east-2.amazonaws.com/percentiles.json)
// on top to drive smarter bid placement; that is intentionally deferred until
// a smart-bidder consumer exists.
type Provider interface {
	SpotPrice(sc *rxtspot.ServerClass) float64
	OnDemandPrice(sc *rxtspot.ServerClass) float64
	MinBidPrice(sc *rxtspot.ServerClass) float64
}

type DefaultProvider struct{}

func NewProvider() *DefaultProvider {
	return &DefaultProvider{}
}

func (DefaultProvider) SpotPrice(sc *rxtspot.ServerClass) float64 {
	return parse(sc.CurrentMarketPricePerHour)
}

func (DefaultProvider) OnDemandPrice(sc *rxtspot.ServerClass) float64 {
	return parse(sc.OnDemandPricePerHour)
}

func (DefaultProvider) MinBidPrice(sc *rxtspot.ServerClass) float64 {
	return parse(sc.MinBidPricePerHour)
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
