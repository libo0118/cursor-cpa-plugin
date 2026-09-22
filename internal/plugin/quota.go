package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"cursorplugin/internal/cursorauth"
)

// Cursor Dashboard money values are cents; the CPA summary contract uses USD.
type cursorQuotaNumber float64

func (n *cursorQuotaNumber) UnmarshalJSON(raw []byte) error {
	text := string(raw)
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return errors.New("invalid Cursor quota number")
		}
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return errors.New("invalid Cursor quota number")
	}
	*n = cursorQuotaNumber(value)
	return nil
}

type cursorPeriodQuota struct {
	BillingCycleEnd *cursorQuotaNumber
	PlanUsage       *struct {
		TotalSpend       *cursorQuotaNumber
		Limit            *cursorQuotaNumber
		Remaining        *cursorQuotaNumber
		TotalPercentUsed *cursorQuotaNumber
	}
	SpendLimitUsage *struct {
		IndividualLimit     *cursorQuotaNumber
		IndividualRemaining *cursorQuotaNumber
		PooledLimit         *cursorQuotaNumber
		PooledRemaining     *cursorQuotaNumber
	}
}

type cursorGrantQuota struct {
	HasCreditGrants bool
	TotalCents      *cursorQuotaNumber
	UsedCents       *cursorQuotaNumber
}

func (handler *Handler) fetchQuota(ctx context.Context, raw []byte) (any, error) {
	var request struct {
		StorageJSON    []byte `json:"storage_json"`
		HostCallbackID string `json:"host_callback_id"`
		Provider       string `json:"provider"`
	}
	if err := json.Unmarshal(raw, &request); err != nil || (request.Provider != "" && request.Provider != "cursor") {
		return nil, errors.New("invalid Cursor quota request")
	}
	credentials, err := cursorauth.ParseCredentials(request.StorageJSON)
	if err != nil {
		return nil, errors.New("Cursor quota credentials are unavailable")
	}
	current, err := handler.quotaDashboard(ctx, request.HostCallbackID, credentials.AccessToken, "GetCurrentPeriodUsage")
	if err != nil {
		return nil, err
	}
	var period cursorPeriodQuota
	if err := json.Unmarshal(current, &period); err != nil {
		return nil, errors.New("invalid Cursor current-period quota response")
	}
	plan := ""
	if rawPlan, err := handler.quotaDashboard(ctx, request.HostCallbackID, credentials.AccessToken, "GetPlanInfo"); err == nil {
		var info struct{ PlanInfo struct{ PlanName string } }
		if json.Unmarshal(rawPlan, &info) == nil {
			plan = info.PlanInfo.PlanName
		}
	}
	var grants *cursorGrantQuota
	if rawGrants, err := handler.quotaDashboard(ctx, request.HostCallbackID, credentials.AccessToken, "GetCreditGrantsBalance"); err == nil {
		var grant cursorGrantQuota
		if json.Unmarshal(rawGrants, &grant) == nil && grant.HasCreditGrants {
			grants = &grant
		}
	}
	return normalizeCursorQuota(period, grants, plan)
}

func (handler *Handler) quotaDashboard(ctx context.Context, callbackID, token, method string) ([]byte, error) {
	if handler.host == nil || callbackID == "" || token == "" {
		return nil, errors.New("Cursor quota host transport is unavailable")
	}
	response, err := handler.host.Call(ctx, "host.http.do", map[string]any{
		"host_callback_id": callbackID,
		"method":           "POST",
		"url":              "https://api2.cursor.sh/aiserver.v1.DashboardService/" + method,
		"headers": map[string][]string{
			"Authorization":            {"Bearer " + token},
			"Content-Type":             {"application/json"},
			"Connect-Protocol-Version": {"1"},
		},
		"body": []byte("{}"),
	})
	if err != nil {
		return nil, fmt.Errorf("Cursor quota %s transport failed", method)
	}
	var result struct {
		StatusCode int
		Body       []byte
	}
	if err := json.Unmarshal(response, &result); err != nil {
		return nil, errors.New("invalid Cursor quota transport response")
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return nil, fmt.Errorf("Cursor quota %s failed (HTTP %d)", method, result.StatusCode)
	}
	if len(result.Body) == 0 || len(result.Body) > 1024*1024 {
		return nil, errors.New("invalid Cursor quota response size")
	}
	return result.Body, nil
}

func normalizeCursorQuota(period cursorPeriodQuota, grants *cursorGrantQuota, plan string) (map[string]any, error) {
	groups := make([]map[string]any, 0, 4)
	summary := make([]map[string]any, 0, 12)
	reset := ""
	if end := period.BillingCycleEnd; end != nil && *end >= 946684800000 && *end < 4102444800000 {
		reset = time.UnixMilli(int64(*end)).UTC().Format(time.RFC3339)
	}
	add := func(key, label string, used, limit, remaining, percent *cursorQuotaNumber, resetTime string) {
		// Only derive a value when both required operands are actually present.
		if remaining == nil && used != nil && limit != nil && *limit > 0 {
			value := cursorQuotaNumber(math.Max(0, float64(*limit-*used)))
			remaining = &value
		}
		if used == nil && limit != nil && remaining != nil && *remaining <= *limit {
			value := *limit - *remaining
			used = &value
		}
		for _, metric := range []struct {
			suffix string
			value  *cursorQuotaNumber
		}{
			{"used", used}, {"limit", limit}, {"remaining", remaining},
		} {
			if metric.value != nil {
				summary = append(summary, map[string]any{
					"key": key + "_" + metric.suffix, "label": label + " " + metric.suffix,
					"value": float64(*metric.value) / 100, "format": "currency", "currency": "USD",
				})
			}
		}
		fraction := 0.0
		known := false
		if limit != nil && *limit > 0 && remaining != nil {
			fraction = float64(*remaining / *limit)
			known = true
		} else if percent != nil {
			fraction = 1 - float64(*percent)/100
			known = true
		}
		if known {
			fraction = math.Max(0, math.Min(1, fraction))
			bucket := map[string]any{"window": key, "remainingFraction": fraction}
			if resetTime != "" {
				bucket["resetTime"] = resetTime
			}
			groups = append(groups, map[string]any{"displayName": label, "buckets": []map[string]any{bucket}})
		}
	}
	if p := period.PlanUsage; p != nil {
		add("plan", "Plan", p.TotalSpend, p.Limit, p.Remaining, p.TotalPercentUsed, reset)
	}
	if p := period.SpendLimitUsage; p != nil {
		if p.IndividualLimit != nil || p.IndividualRemaining != nil {
			add("on_demand_individual", "On-demand (individual)", nil, p.IndividualLimit, p.IndividualRemaining, nil, reset)
		}
		if p.PooledLimit != nil || p.PooledRemaining != nil {
			add("on_demand_team", "On-demand (team)", nil, p.PooledLimit, p.PooledRemaining, nil, reset)
		}
	}
	if grants != nil && grants.HasCreditGrants {
		add("credit_grants", "Credit grants", grants.UsedCents, grants.TotalCents, nil, nil, "")
	}
	if len(groups) == 0 && len(summary) == 0 {
		return nil, errors.New("Cursor did not return usable quota data")
	}
	result := map[string]any{"groups": groups, "summary": summary}
	if plan != "" {
		result["subscription"] = map[string]string{"plan": plan}
	}
	return result, nil
}
