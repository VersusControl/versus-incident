package narrative

import "strings"

// serviceLabels maps neutral service keys to plain names. Provider adapters
// map their own service names onto these keys; an unknown key falls back to
// the provider's display name.
var serviceLabels = map[string]string{
	"compute.vm":            "Virtual machines",
	"compute.container":     "Containers",
	"compute.kubernetes":    "Kubernetes clusters",
	"compute.function":      "Functions",
	"storage.block":         "Disk volumes",
	"storage.object":        "Object storage",
	"storage.file":          "File storage",
	"storage.backup":        "Backups",
	"database.relational":   "Databases",
	"database.keyvalue":     "Key-value databases",
	"database.cache":        "Caches",
	"database.search":       "Search clusters",
	"network.nat":           "Network traffic (NAT)",
	"network.transfer":      "Data transfer",
	"network.loadbalancer":  "Load balancers",
	"network.cdn":           "Content delivery",
	"network.ip":            "Public IP addresses",
	"network.endpoint":      "Private endpoints",
	"network.dns":           "DNS",
	"observability.logs":    "Logs",
	"observability.metrics": "Metrics and alarms",
	"messaging.queue":       "Queues",
	"messaging.stream":      "Streams",
	"analytics.warehouse":   "Data warehouse",
	"analytics.query":       "Query service",
	"ai.model":              "AI models",
	"security.keys":         "Encryption keys",
	"security.secrets":      "Secrets",
	"registry.container":    "Container registry",
	"commitment.savings":    "Savings commitment",
	"commitment.unused":     "Unused Savings Plan",
	"billing.support":       "Support plan",
	"billing.marketplace":   "Marketplace",
	"billing.tax":           "Tax",
	"billing.credit":        "Credits",
	"billing.refund":        "Refunds",
	"billing.fee":           "Fees",
	"billing.discount":      "Discounts",
	"other":                 "Other",
}

// ServiceLabel returns the label of a neutral service key. Summary uses the
// plain name; Details uses the provider's name when known, else the key.
func ServiceLabel(key, providerName string, d Depth) string {
	if d == Details {
		if providerName != "" {
			return providerName
		}
		return key
	}
	if l, ok := serviceLabels[key]; ok {
		return l
	}
	if providerName != "" {
		return providerName
	}
	if key == "" {
		return "Other"
	}
	return key
}

// KnownService reports whether the key has a plain-language label.
func KnownService(key string) bool {
	_, ok := serviceLabels[key]
	return ok
}

// Cause keys, shared with the FinOps core.
const (
	CauseNewResource     = "new_resource"
	CauseRemovedResource = "removed_resource"
	CauseUsageUp         = "usage_up"
	CauseUsageDown       = "usage_down"
	CausePriceChange     = "price_change"
	CauseDiscountChange  = "discount_change"
	CauseOneOff          = "one_off"
	CauseNewAccount      = "new_account"
	CauseOther           = "other"
)

// CauseLabel is the short driver prefix ("Used more", "New").
// amountSign is the sign of the driver amount; it picks the direction word
// for causes that can go either way.
func CauseLabel(cause string, amountSign int, recordType string, d Depth) string {
	if d == Details {
		switch cause {
		case CauseDiscountChange:
			if recordType != "" {
				return "discount_change (" + recordType + ")"
			}
		case CauseOneOff:
			if recordType != "" && recordType != "usage" {
				return "one_off (" + recordType + ")"
			}
		}
		return cause
	}
	switch cause {
	case CauseNewResource:
		return "New"
	case CauseRemovedResource:
		return "Removed"
	case CauseUsageUp:
		return "Used more"
	case CauseUsageDown:
		return "Used less"
	case CausePriceChange:
		if amountSign < 0 {
			return "Price went down"
		}
		return "Price changed"
	case CauseDiscountChange:
		if recordType == "credit" {
			return "Credits applied"
		}
		if amountSign > 0 {
			return "Discount covered less"
		}
		return "Discount covered more"
	case CauseOneOff:
		if recordType == "refund" {
			return "Refund"
		}
		if recordType == "tax" {
			return "Tax"
		}
		return "One-time charge"
	case CauseNewAccount:
		return "New account"
	case CauseOther:
		return "Everything else"
	}
	return capitalize(strings.ReplaceAll(cause, "_", " "))
}

// Paid call names, as the planner reports skipped calls.
var callLabels = map[string]string{
	"forecast":          "forecast",
	"discount_coverage": "discount coverage",
	"resource_detail":   "resource detail",
	"drill_down":        "usage-type detail",
	"cost_by_account":   "cost by account",
	"cost_by_usage":     "cost by usage type",
	"cost_by_team":      "cost by team",
	"cost_by_record":    "credits and fees",
}

// CallLabel names a paid call in plain words.
func CallLabel(name string) string {
	if l, ok := callLabels[name]; ok {
		return l
	}
	return strings.ReplaceAll(name, "_", " ")
}

// EffortWord and RiskWord name savings effort and risk.
func EffortWord(effort string) string {
	switch effort {
	case "minutes":
		return "Minutes"
	case "hours":
		return "Hours"
	case "days":
		return "Days"
	}
	return capitalize(effort)
}

func RiskWord(risk string) string {
	switch risk {
	case "none":
		return "None"
	case "low":
		return "Low"
	case "needs_testing":
		return "Needs testing"
	}
	return capitalize(risk)
}

// BudgetStatusWord names a budget status.
func BudgetStatusWord(status string) string {
	switch status {
	case "on_track":
		return "On track"
	case "at_risk":
		return "At risk"
	case "over_budget":
		return "Over budget"
	}
	return capitalize(strings.ReplaceAll(status, "_", " "))
}

// EstimatedMark follows any figure or owner built from an estimated share.
const EstimatedMark = "(estimated)"

// OwnerRuleText explains which ownership rule matched.
func OwnerRuleText(rule, tagKey, owner, subject string, d Depth) string {
	switch rule {
	case "tag":
		s := tagKey + "=" + owner + " tag"
		if subject != "" {
			s += " on " + subject
		}
		return s
	case "account":
		if d == Details {
			return "accounts[].owner in config"
		}
		return "account owner from settings"
	}
	if d == Details {
		return "no allowlisted " + tagKey + " tag and no account owner"
	}
	return "no team tag and no account owner"
}
