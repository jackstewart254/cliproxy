package management

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestats"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// routingStrategies are the canonical values normalizeRoutingStrategy returns.
var routingStrategies = []string{"soonest-reset", "fill-first", "round-robin", "weighted-round-robin"}

// planDateLayout is the format of the manual plan_renews_on and plan_ends_on
// auth file fields.
const planDateLayout = "2006-01-02"

// planDateFields are the auth file fields that hold a manual plan date.
var planDateFields = map[string]struct{}{"plan_renews_on": {}, "plan_ends_on": {}}

// dashboardServer describes this proxy for the dashboard header.
func (h *Handler) dashboardServer(c *gin.Context, now time.Time) gin.H {
	address := ""
	if c != nil && c.Request != nil {
		address = c.Request.Host
	}
	version := strings.TrimSpace(buildinfo.Commit)
	if version == "none" || version == "unknown" {
		version = ""
	}
	if len(version) > 8 {
		version = version[:8]
	}
	return gin.H{
		"host":    usagestats.HostName(),
		"address": address,
		"version": version,
		"now":     now,
	}
}

// dashboardAccountMode is how the router treats a credential: "off" when
// disabled, "reserve" when its priority is below zero, else "rotation".
func dashboardAccountMode(entry gin.H) string {
	if disabled, _ := entry["disabled"].(bool); disabled {
		return "off"
	}
	if priority, ok := numberValue(entry["priority"]); ok && priority < 0 {
		return "reserve"
	}
	return "rotation"
}

func numberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	case json.Number:
		parsed, errParse := typed.Float64()
		return parsed, errParse == nil
	case string:
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, errParse == nil
	default:
		return 0, false
	}
}

// dashboardAccountPlan returns the credential's plan name and renewal and end
// dates. Manual dates from the auth file win over the Codex id_token claim.
func dashboardAccountPlan(auth *coreauth.Auth, location *time.Location) gin.H {
	plan := gin.H{"type": "", "renews_on": "", "ends_on": "", "source": ""}
	if auth == nil {
		return plan
	}
	claims := extractCodexIDTokenClaims(auth)
	if planType, _ := claims["plan_type"].(string); planType != "" {
		plan["type"] = planType
	} else if planType := metadataString(auth, "plan_type"); planType != "" {
		plan["type"] = planType
	}
	if renews := metadataPlanDate(auth, "plan_renews_on"); renews != "" {
		plan["renews_on"] = renews
		plan["source"] = "manual"
	} else if renews := claimLocalDate(claims["chatgpt_subscription_active_until"], location); renews != "" {
		plan["renews_on"] = renews
		plan["source"] = "token"
	}
	plan["ends_on"] = metadataPlanDate(auth, "plan_ends_on")
	return plan
}

func metadataString(auth *coreauth.Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	value, _ := auth.Metadata[key].(string)
	return strings.TrimSpace(value)
}

// metadataPlanDate returns a manual plan date when it is a valid YYYY-MM-DD.
func metadataPlanDate(auth *coreauth.Auth, key string) string {
	value := metadataString(auth, key)
	if _, errParse := parsePlanDate(value); errParse != nil {
		return ""
	}
	return value
}

func parsePlanDate(value string) (time.Time, error) {
	if len(value) != len(planDateLayout) {
		return time.Time{}, fmt.Errorf("not a YYYY-MM-DD date")
	}
	return time.Parse(planDateLayout, value)
}

// validatePlanDateValue checks a PATCH value for a plan date field: a
// YYYY-MM-DD string, or nil to clear it.
func validatePlanDateValue(field string, value any) error {
	if value == nil {
		return nil
	}
	text, ok := value.(string)
	if ok {
		if _, errParse := parsePlanDate(text); errParse == nil {
			return nil
		}
	}
	return fmt.Errorf("%s must be a YYYY-MM-DD date or null", field)
}

// claimLocalDate turns the chatgpt_subscription_active_until claim, a
// timestamp string or Unix seconds, into a local YYYY-MM-DD date.
func claimLocalDate(value any, location *time.Location) string {
	if location == nil {
		location = time.Local
	}
	var at time.Time
	switch typed := value.(type) {
	case string:
		text := strings.TrimSpace(typed)
		if parsed, errParse := time.Parse(time.RFC3339, text); errParse == nil {
			at = parsed
		} else if parsed, errDate := parsePlanDate(text); errDate == nil {
			return parsed.Format(planDateLayout)
		} else if seconds, errNumber := strconv.ParseFloat(text, 64); errNumber == nil {
			at = unixTime(seconds)
		}
	default:
		if seconds, ok := numberValue(typed); ok {
			at = unixTime(seconds)
		}
	}
	if at.IsZero() {
		return ""
	}
	return at.In(location).Format(planDateLayout)
}

func unixTime(seconds float64) time.Time {
	if seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return time.Time{}
	}
	if seconds > 1e12 {
		// Milliseconds.
		seconds /= 1000
	}
	return time.Unix(int64(seconds), 0)
}
