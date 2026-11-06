package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"deploy_platform/internal/store"
)

func TestAdminSetPlanRequiresAdmin(t *testing.T) {
	s := &Server{}
	for name, user := range map[string]store.User{
		"anonymous":     {},
		"member":        {ID: "usr_member", Role: store.RoleUser},
		"revoked admin": {ID: "usr_old", Role: store.RoleAdmin, RevokedAt: ptrTime(time.Now())},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/admin/users/usr_target/plan", strings.NewReader(`{"plan":"paid"}`))
			req.SetPathValue("id", "usr_target")
			rec := httptest.NewRecorder()
			s.adminSetPlan(rec, withUser(req, user))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
		})
	}
}

func TestAdminSetPlanRejectsUnknownPlan(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/users/usr_target/plan", strings.NewReader(`{"plan":"enterprise"}`))
	req.SetPathValue("id", "usr_target")
	rec := httptest.NewRecorder()
	s.adminSetPlan(rec, withUser(req, store.User{ID: "usr_admin", Role: store.RoleAdmin}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestParseAdminPlan(t *testing.T) {
	cases := []struct {
		body    adminPlanBody
		status  string
		restart bool
		ok      bool
	}{
		{adminPlanBody{Plan: "paid"}, store.BillingActive, false, true},
		{adminPlanBody{Plan: "active"}, store.BillingActive, false, true},
		{adminPlanBody{Plan: "paid", RestartTrial: true}, store.BillingActive, false, true},
		{adminPlanBody{Plan: "trial"}, store.BillingTrial, false, true},
		{adminPlanBody{Plan: "trial", RestartTrial: true}, store.BillingTrial, true, true},
		{adminPlanBody{Plan: ""}, "", false, false},
		{adminPlanBody{Plan: "expired"}, "", false, false},
	}
	for _, c := range cases {
		status, restart, err := parseAdminPlan(c.body)
		if (err == nil) != c.ok || status != c.status || restart != c.restart {
			t.Fatalf("%+v -> %q %v %v", c.body, status, restart, err)
		}
	}
}

func TestPaidPlanClearsExpiredLock(t *testing.T) {
	now := time.Date(2026, 12, 20, 12, 0, 0, 0, time.UTC)
	started := now.Add(-40 * 24 * time.Hour)
	account := store.Account{TrialStartedAt: started, BillingStatus: store.BillingTrial}
	if got := accountPlan(account, now).Status; got != PlanExpired {
		t.Fatalf("unpaid after 40 days = %q, want expired", got)
	}
	expiredKey := planKeyFor(store.User{TrialStartedAt: &started, BillingStatus: store.BillingTrial}, now)

	account.BillingStatus = store.BillingActive
	if got := accountPlan(account, now).Status; got != PlanActive {
		t.Fatalf("paid after 40 days = %q, want active", got)
	}
	paidKey := planKeyFor(store.User{TrialStartedAt: &started, BillingStatus: store.BillingActive}, now)
	if paidKey == expiredKey {
		t.Fatal("plan key must change when an account is marked paid")
	}

	// A fresh trial clock unlocks too, and counts the full trial again.
	fresh := now
	if plan := planFor(store.User{TrialStartedAt: &fresh, BillingStatus: store.BillingTrial}, now); plan.Status != PlanTrial || plan.DaysLeft != TrialDays {
		t.Fatalf("restarted trial = %+v", plan)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
