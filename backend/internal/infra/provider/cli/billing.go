package cli

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func parseBilling(data []byte) (account.Billing, error) {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return account.Billing{}, fmt.Errorf("解析 Billing: %w", err)
	}
	original := root
	if nested, ok := root["config"].(map[string]any); ok {
		root = nested
	}
	planCode, planName := planValues(root)
	if planCode == "" || planName == "" {
		outerCode, outerName := planValues(original)
		if planCode == "" {
			planCode = outerCode
		}
		if planName == "" {
			planName = outerName
		}
	}
	result := account.Billing{
		PlanCode:             planCode,
		PlanName:             planName,
		MonthlyLimit:         numberValue(firstValue(root, "monthlyLimit", "monthly_limit")),
		Used:                 numberValue(firstValue(root, "used", "totalUsed", "includedUsed")),
		OnDemandCap:          numberValue(firstValue(root, "onDemandCap", "on_demand_cap", "maxAmountPerMonth")),
		OnDemandUsed:         numberValue(firstValue(root, "onDemandUsed", "on_demand_used")),
		PrepaidBalance:       numberValue(firstValue(root, "prepaidBalance", "prepaid_balance")),
		CreditUsagePercent:   numberValue(firstValue(root, "creditUsagePercent", "credit_usage_percent")),
		IsUnifiedBillingUser: boolValue(firstValue(root, "isUnifiedBillingUser", "is_unified_billing_user")),
		TopUpMethod:          stringValue(firstValue(root, "topUpMethod", "top_up_method")),
		BillingPeriodStart:   stringValue(firstValue(root, "billingPeriodStart", "billing_period_start")),
		BillingPeriodEnd:     stringValue(firstValue(root, "billingPeriodEnd", "billing_period_end")),
	}
	if currentPeriod, ok := root["currentPeriod"].(map[string]any); ok {
		result.UsagePeriodType = stringValue(currentPeriod["type"])
		result.UsagePeriodStart = stringValue(currentPeriod["start"])
		result.UsagePeriodEnd = stringValue(currentPeriod["end"])
	}
	if history, ok := root["history"].([]any); ok {
		result.History = make([]account.BillingHistoryEntry, 0, len(history))
		for _, raw := range history {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			cycle, _ := entry["billingCycle"].(map[string]any)
			result.History = append(result.History, account.BillingHistoryEntry{
				Year: int(numberValue(cycle["year"])), Month: int(numberValue(cycle["month"])),
				IncludedUsed: numberValue(entry["includedUsed"]), OnDemandUsed: numberValue(entry["onDemandUsed"]), TotalUsed: numberValue(entry["totalUsed"]),
			})
		}
	}
	if result.CreditUsagePercent == 0 {
		switch {
		case result.OnDemandCap > 0:
			result.CreditUsagePercent = result.OnDemandUsed / result.OnDemandCap * 100
		case result.MonthlyLimit > 0:
			result.CreditUsagePercent = result.Used / result.MonthlyLimit * 100
		}
	}
	return result, nil
}

func boolValue(value any) bool {
	result, _ := value.(bool)
	return result
}

func planValues(values map[string]any) (string, string) {
	code := stringValue(firstValue(values, "planCode", "plan_code", "subscriptionTier", "subscription_tier", "tier"))
	name := stringValue(firstValue(values, "planName", "plan_name", "subscriptionName", "subscription_name"))
	for _, key := range []string{"plan", "subscription", "membership"} {
		value, ok := values[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if name == "" {
				name = typed
			}
		case map[string]any:
			if code == "" {
				code = stringValue(firstValue(typed, "code", "id", "tier", "slug"))
			}
			if name == "" {
				name = stringValue(firstValue(typed, "name", "displayName", "display_name", "label"))
			}
		}
	}
	return code, name
}

func firstValue(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value
		}
	}
	return nil
}

func numberValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case string:
		parsed, _ := strconv.ParseFloat(typed, 64)
		return parsed
	case map[string]any:
		return numberValue(typed["val"])
	default:
		return 0
	}
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}
