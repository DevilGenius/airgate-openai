package gateway

import "strings"

// parseRefreshTokenInfo uses the access token's workspace as the authority.
// An ID token may supply missing plan metadata only for the same account.
// Refresh and compatible import do not query accounts/check, whose default/paid
// workspace selection can overwrite a different account's JWT plan.
func parseRefreshTokenInfo(idToken, accessToken string) *idTokenInfo {
	info := parseIDToken(accessToken)
	id := parseIDToken(idToken)
	if info.AccountID != "" && info.AccountID == id.AccountID {
		if strings.TrimSpace(info.PlanType) == "" {
			info.PlanType = id.PlanType
			info.SubscriptionActiveUntil = id.SubscriptionActiveUntil
		}
		if info.Email == "" {
			info.Email = id.Email
		}
		if info.AccountName == "" {
			info.AccountName = id.AccountName
		}
	}
	info.PlanType = strings.TrimSpace(info.PlanType)
	if info.PlanType == "" {
		info.PlanType = "unknown"
	}
	if strings.EqualFold(info.PlanType, "free") || strings.EqualFold(info.PlanType, "unknown") {
		info.SubscriptionActiveUntil = ""
	}
	return info
}
