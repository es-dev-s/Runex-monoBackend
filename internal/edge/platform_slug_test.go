package edge

import "testing"

func TestValidatePlatformSlug(t *testing.T) {
	got, err := ValidatePlatformSlug("Pawan")
	if err != nil || got != "pawan" {
		t.Fatalf("got %q err %v", got, err)
	}
	if _, err := ValidatePlatformSlug("deploy"); err == nil {
		t.Fatal("deploy must stay reserved")
	}
	if _, err := ValidatePlatformSlug("a"); err == nil {
		t.Fatal("too short")
	}
	if _, err := ValidatePlatformSlug("www"); err == nil {
		t.Fatal("www reserved")
	}
}

func TestHostSlugsPrefersExplicit(t *testing.T) {
	nodes := []Named{
		{ID: "svc_1", Title: "Other", PreferredSlug: "pawan"},
		{ID: "svc_2", Title: "Pawan"},
	}
	got := HostSlugs(nodes)
	if got["svc_1"] != "pawan" {
		t.Fatalf("explicit slug %q", got["svc_1"])
	}
	if got["svc_2"] == "pawan" {
		t.Fatalf("auto title must not steal explicit slug: %q", got["svc_2"])
	}
}
