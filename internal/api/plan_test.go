package api

import (
	"testing"
	"time"

	"deploy_platform/internal/store"
)

func TestPlanFor(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	day := 24 * time.Hour
	cases := []struct {
		name   string
		user   store.User
		status string
		left   int
	}{
		{"fresh trial", store.User{TrialStartedAt: at(0)}, PlanTrial, 30},
		{"half a day in", store.User{TrialStartedAt: at(-12 * time.Hour)}, PlanTrial, 30},
		{"25 days in", store.User{TrialStartedAt: at(-25 * day)}, PlanTrial, 5},
		{"last hour", store.User{TrialStartedAt: at(-30*day + time.Hour)}, PlanTrial, 1},
		{"exactly over", store.User{TrialStartedAt: at(-30 * day)}, PlanExpired, 0},
		{"long over", store.User{TrialStartedAt: at(-90 * day)}, PlanExpired, 0},
		{"active after trial", store.User{TrialStartedAt: at(-90 * day), BillingStatus: store.BillingActive}, PlanActive, 0},
		{"active during trial", store.User{TrialStartedAt: at(-2 * day), BillingStatus: store.BillingActive}, PlanActive, 28},
		{"no clock yet", store.User{}, PlanTrial, 30},
	}
	for _, tc := range cases {
		got := planFor(tc.user, now)
		if got.Status != tc.status || got.DaysLeft != tc.left {
			t.Errorf("%s: got %s/%d, want %s/%d", tc.name, got.Status, got.DaysLeft, tc.status, tc.left)
		}
	}
}
