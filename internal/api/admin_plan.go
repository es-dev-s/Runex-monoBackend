package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"deploy_platform/internal/store"

	"github.com/jackc/pgx/v5"
)

// Plans an administrator can put an account on. Runex has no payment
// processor: support switches an account to paid after the customer pays.
const (
	AdminPlanTrial = "trial" // free trial on its existing clock
	AdminPlanPaid  = "paid"  // paid and active: nothing is gated
)

type adminPlanBody struct {
	Plan         string `json:"plan"`
	RestartTrial bool   `json:"restartTrial"`
}

// parseAdminPlan maps the request to the stored billing status. "active" is
// accepted as an alias for paid. A fresh trial clock only applies to trial.
func parseAdminPlan(body adminPlanBody) (status string, restart bool, err error) {
	switch body.Plan {
	case AdminPlanPaid, PlanActive:
		return store.BillingActive, false, nil
	case AdminPlanTrial:
		return store.BillingTrial, body.RestartTrial, nil
	default:
		return "", false, errors.New(`Choose "paid" or "trial".`)
	}
}

// accountPlan derives the admin directory's plan view for one account.
func accountPlan(account store.Account, now time.Time) planInfo {
	started := account.TrialStartedAt
	user := store.User{BillingStatus: account.BillingStatus}
	if !started.IsZero() {
		user.TrialStartedAt = &started
	}
	return planFor(user, now)
}

// planKeyFor changes whenever the plan the dashboard shows would change.
func planKeyFor(u store.User, now time.Time) string {
	plan := planFor(u, now)
	return plan.Status + "|" + plan.TrialEndsAt.Format(time.RFC3339)
}

// adminSetPlan switches an account between the free trial and paid. The change
// is saved in Postgres and applies at once: compute checks read the account
// fresh, the session cache is dropped, and the user's open dashboard is told.
func (s *Server) adminSetPlan(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var body adminPlanBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "JSON body is required")
		return
	}
	status, restart, err := parseAdminPlan(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_plan", err.Error())
		return
	}
	id := r.PathValue("id")
	if err := s.store.SetBillingPlan(r.Context(), id, status, restart, actor.ID); errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "That account does not exist")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not update that plan")
		return
	}
	slog.Info("admin set plan", "admin", actor.ID, "user", id, "billing_status", status, "restart_trial", restart)
	s.forgetUserSessions(id)
	s.wakeAdmins()
	s.writeAdminUser(w, r, id)
}
