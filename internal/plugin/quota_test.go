package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCursorQuotaAmountsAndUnknownValues(t *testing.T) {
	var period cursorPeriodQuota
	if err := json.Unmarshal([]byte(`{"billingCycleEnd":"1758931200000","planUsage":{"totalSpend":"1234","limit":2000,"remaining":0},"spendLimitUsage":{"individualRemaining":1250}}`), &period); err != nil {
		t.Fatal(err)
	}
	got, err := normalizeCursorQuota(period, nil, "Pro")
	if err != nil {
		t.Fatal(err)
	}
	metrics := got["summary"].([]map[string]any)
	values := map[string]float64{}
	for _, metric := range metrics {
		values[metric["key"].(string)] = metric["value"].(float64)
	}
	if values["plan_used"] != 12.34 || values["plan_limit"] != 20 {
		t.Fatalf("wrong USD conversion: %v", values)
	}
	if remaining, ok := values["plan_remaining"]; !ok || remaining != 0 {
		t.Fatal("real zero lost")
	}
	if _, ok := values["on_demand_individual_limit"]; ok {
		t.Fatal("unknown limit invented")
	}
	groups := got["groups"].([]map[string]any)
	if len(groups) != 1 {
		t.Fatalf("unknown limit must not create a bar: %v", groups)
	}
	bucket := groups[0]["buckets"].([]map[string]any)[0]
	if bucket["remainingFraction"] != 0.0 || bucket["resetTime"] != "2025-09-27T00:00:00Z" {
		t.Fatalf("wrong period: %v", bucket)
	}
	if _, err := normalizeCursorQuota(cursorPeriodQuota{}, nil, "Pro"); err == nil {
		t.Fatal("empty quota accepted")
	}
	for _, raw := range []string{`{"planUsage":{"limit":"NaN"}}`, `{"planUsage":{"totalSpend":-1}}`} {
		if json.Unmarshal([]byte(raw), &period) == nil {
			t.Fatal("invalid amount accepted")
		}
	}
}

func TestCursorQuotaCreditGrantsAndOverage(t *testing.T) {
	var grants cursorGrantQuota
	if err := json.Unmarshal([]byte(`{"hasCreditGrants":true,"totalCents":"2000","usedCents":"2500"}`), &grants); err != nil {
		t.Fatal(err)
	}
	got, err := normalizeCursorQuota(cursorPeriodQuota{}, &grants, "")
	if err != nil {
		t.Fatal(err)
	}
	bucket := got["groups"].([]map[string]any)[0]["buckets"].([]map[string]any)[0]
	if bucket["remainingFraction"] != 0.0 {
		t.Fatal("overage must be exhausted")
	}
	if _, ok := bucket["resetTime"]; ok {
		t.Fatal("grant reset fabricated")
	}
}

type cursorQuotaTestHost struct{ fail bool }

func (h cursorQuotaTestHost) Call(_ context.Context, method string, request any) (json.RawMessage, error) {
	if h.fail {
		return nil, errors.New("secret-upstream-body")
	}
	return json.Marshal(map[string]any{"StatusCode": 401, "Body": []byte("secret-upstream-body")})
}

func TestCursorQuotaErrorsDoNotExposeCredentials(t *testing.T) {
	for _, fail := range []bool{false, true} {
		handler := &Handler{host: cursorQuotaTestHost{fail: fail}}
		_, err := handler.quotaDashboard(context.Background(), "callback", "private-token", "GetCurrentPeriodUsage")
		if err == nil || strings.Contains(err.Error(), "secret-upstream") || strings.Contains(err.Error(), "private-token") {
			t.Fatalf("unsafe error: %v", err)
		}
	}
}
