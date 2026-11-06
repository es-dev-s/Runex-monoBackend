package api

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"time"

	"deploy_platform/internal/store"
)

// TrialDays is the length of the free trial every new account starts with.
const TrialDays = 30

// Plan statuses exposed to the dashboard.
const (
	PlanTrial   = "trial"   // inside the free trial
	PlanActive  = "active"  // support marked the account as paying
	PlanExpired = "expired" // trial over and not active: compute is stopped
)

type planInfo struct {
	Status         string    `json:"status"`
	TrialDays      int       `json:"trialDays"`
	TrialStartedAt time.Time `json:"trialStartedAt"`
	TrialEndsAt    time.Time `json:"trialEndsAt"`
	DaysLeft       int       `json:"daysLeft"`
}

// planFor derives the account plan from the stored trial clock and billing flag.
func planFor(u store.User, now time.Time) planInfo {
	start := now
	if u.TrialStartedAt != nil && !u.TrialStartedAt.IsZero() {
		start = *u.TrialStartedAt
	}
	ends := start.Add(TrialDays * 24 * time.Hour)
	info := planInfo{
		Status:         PlanTrial,
		TrialDays:      TrialDays,
		TrialStartedAt: start.UTC(),
		TrialEndsAt:    ends.UTC(),
	}
	if left := ends.Sub(now); left > 0 {
		info.DaysLeft = int(math.Ceil(left.Hours() / 24))
	}
	switch {
	case u.BillingStatus == store.BillingActive:
		info.Status = PlanActive
	case !now.Before(ends):
		info.Status = PlanExpired
	}
	return info
}

const trialEndedMessage = "Your free trial has ended. Contact support to run this project again."

// requireCompute rejects actions that build, start, or run containers when the
// caller's trial is over and the account is not active. It reads the account
// fresh from the database so a support change applies on the next request.
func (s *Server) requireCompute(w http.ResponseWriter, r *http.Request) bool {
	user, err := s.store.UserByID(r.Context(), userFrom(r).ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load your account")
		return false
	}
	if planFor(user, time.Now()).Status == PlanExpired {
		writeErr(w, http.StatusPaymentRequired, "trial_ended", trialEndedMessage)
		return false
	}
	return true
}

// computeAllowedFor is the background twin of requireCompute, for work that has
// no request (GitHub push redeploys).
func (s *Server) computeAllowedFor(ctx context.Context, projectID string) bool {
	userID, err := s.store.ProjectOwner(ctx, projectID)
	if err != nil || userID == "" {
		// Ownerless or unreadable: nobody can vouch for a plan, so do not run it.
		return false
	}
	user, err := s.store.UserByID(ctx, userID)
	if err != nil {
		// Fail closed: never start compute for an account we cannot verify.
		return false
	}
	return planFor(user, time.Now()).Status != PlanExpired
}

// WatchTrials stops and disconnects compute for accounts whose trial ended
// without an active plan. Code, variables, domains, and project records stay.
// It runs at startup and then every minute, so a deploy that finished after the
// deadline is caught on the next pass.
func (s *Server) WatchTrials(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	s.enforceTrials(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.enforceTrials(ctx)
		}
	}
}

func (s *Server) enforceTrials(ctx context.Context) {
	ids, err := s.store.ListTrialEndedUsers(ctx, TrialDays)
	if err != nil {
		slog.Warn("trial sweep", "err", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		s.stopUserCompute(ctx, id)
	}
}

const trialStoppedReason = "Stopped because the free trial ended. Contact support to run it again."

func (s *Server) stopUserCompute(ctx context.Context, userID string) {
	nodes, err := s.store.ListNodesForUser(ctx, userID)
	if err != nil {
		return
	}
	for _, node := range nodes {
		// Cancel builds first so a finishing job cannot start a container after we stop it.
		if node.Status == "building" || s.engine.Jobs.Busy(node.ID) {
			s.engine.CancelDeploy(node)
			// A redeploy keeps the previous container serving; stop it too.
			if node.Kind == "service" {
				_ = s.engine.StopService(ctx, node.ProjectID, node.ID)
			}
			continue
		}
		switch node.Kind {
		case "service", "postgresql", "redis":
		default:
			continue
		}
		if node.Status != "running" {
			continue
		}
		// Same path as an allocation stop: docker stop, close the database proxy,
		// mark the node stopped with a reason, and publish it to open dashboards.
		s.engine.StopOverLimit(ctx, node, trialStoppedReason)
		slog.Info("trial ended: stopped node", "user", userID, "project", node.ProjectID, "node", node.ID)
	}
}
