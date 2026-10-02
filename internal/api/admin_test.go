package api

import (
	"testing"
	"time"

	"deploy_platform/internal/store"
)

func TestRevokeGuardRefusesSelf(t *testing.T) {
	actor := store.User{ID: "usr_admin", Role: store.RoleAdmin}
	if err := revokeGuard(actor, actor, 2); err == nil {
		t.Fatal("an administrator must not be able to revoke their own access")
	}
}

func TestRevokeGuardRefusesLastAdmin(t *testing.T) {
	actor := store.User{ID: "usr_a", Role: store.RoleAdmin}
	other := store.User{ID: "usr_b", Role: store.RoleAdmin}
	if err := revokeGuard(actor, other, 1); err == nil {
		t.Fatal("the last active administrator must stay")
	}
	if err := revokeGuard(actor, other, 2); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeGuardAllowsMember(t *testing.T) {
	actor := store.User{ID: "usr_admin", Role: store.RoleAdmin}
	member := store.User{ID: "usr_member", Role: store.RoleUser}
	if err := revokeGuard(actor, member, 1); err != nil {
		t.Fatal(err)
	}
}

func TestResolveAdminSeed(t *testing.T) {
	id, err := resolveAdminSeed(nil, nil, "gojo", "owner@example.com")
	if err != nil || id != "" {
		t.Fatalf("missing account should be created, got %q %v", id, err)
	}

	same := &store.User{ID: "usr_1", Username: "Gojo", Email: "owner@example.com"}
	id, err = resolveAdminSeed(same, same, "gojo", "owner@example.com")
	if err != nil || id != "usr_1" {
		t.Fatalf("matching account should be updated, got %q %v", id, err)
	}

	taken := &store.User{ID: "usr_2", Username: "other", Email: "owner@example.com"}
	if _, err := resolveAdminSeed(taken, nil, "gojo", "owner@example.com"); err == nil {
		t.Fatal("an email that belongs to someone else must not be taken")
	}

	left := &store.User{ID: "usr_a", Username: "gojo", Email: "a@example.com"}
	right := &store.User{ID: "usr_b", Username: "other", Email: "b@example.com"}
	if _, err := resolveAdminSeed(right, left, "gojo", "b@example.com"); err == nil {
		t.Fatal("split username and email must not be merged")
	}
}

func TestDaysOnPlatformCountsTheCurrentDay(t *testing.T) {
	created := time.Date(2026, 10, 1, 23, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 2, 1, 0, 0, 0, time.UTC)
	if days := daysOnPlatform(created, now); days != 2 {
		t.Fatalf("days = %d", days)
	}
	if days := daysOnPlatform(now, now); days != 1 {
		t.Fatalf("same day = %d", days)
	}
}

func TestAssembleDirectoryUsageAndDomains(t *testing.T) {
	accounts := []store.Account{{
		ID: "usr_1", Username: "ada", Email: "ada@example.com", Role: store.RoleUser,
		CreatedAt: time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC), SignedIn: true,
	}}
	workloads := []store.AdminWorkload{
		{UserID: "usr_1", ProjectID: "prj_1", ProjectName: "Site", NodeID: "nod_1", Kind: "service", PlatformSlug: "site"},
		{UserID: "usr_1", ProjectID: "prj_1", ProjectName: "Site", NodeID: "nod_2", Kind: "postgresql"},
		{UserID: "usr_1", ProjectID: "prj_1", ProjectName: "Site", NodeID: "nod_3", Kind: "service", PlatformSlug: "old", PlatformRevoked: true},
	}
	hosts := []store.AdminHost{{UserID: "usr_1", ProjectID: "prj_1", Hostname: "ada.example", Status: "active"}}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	got := assembleDirectory(accounts, workloads, hosts, func(id string) (int64, float64) {
		if id == "nod_1" {
			return 1024, 1.5
		}
		if id == "nod_2" {
			return 2048, 0.5
		}
		return 0, 0
	}, func(slug string) string {
		return "https://" + slug + ".runex.cloud"
	}, now)
	if len(got) != 1 {
		t.Fatalf("accounts = %d", len(got))
	}
	user := got[0]
	if user.Days != 1 || user.RAMUsedBytes != 3072 || user.CPUPercent != 2 {
		t.Fatalf("usage days=%d ram=%d cpu=%v", user.Days, user.RAMUsedBytes, user.CPUPercent)
	}
	if len(user.Projects) != 1 || len(user.Projects[0].Domains) != 2 {
		t.Fatalf("projects %+v", user.Projects)
	}
	if user.Projects[0].Domains[0].Host != "site.runex.cloud" || user.Projects[0].Domains[1].Host != "ada.example" {
		t.Fatalf("domains %+v", user.Projects[0].Domains)
	}
}

func TestPublicUserIncludesRole(t *testing.T) {
	body := publicUser(store.User{ID: "usr_1", Username: "gojo", Email: "a@b.co", Role: store.RoleAdmin, CreatedAt: time.Unix(0, 0).UTC()})
	if body["role"] != store.RoleAdmin {
		t.Fatalf("role = %v", body["role"])
	}
	if _, ok := body["passwordHash"]; ok {
		t.Fatal("public user must not include a password hash")
	}
}
