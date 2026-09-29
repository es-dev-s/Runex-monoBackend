package api

import (
	"testing"

	"deploy_platform/internal/store"
)

func TestPickVisitURLPrefersConnectedCustom(t *testing.T) {
	visit, kind := pickVisitURL("designflow.aivex.art", "https://app.salesradar.live", true)
	if visit != "https://designflow.aivex.art" || kind != primaryKindCustom {
		t.Fatalf("visit=%s kind=%s", visit, kind)
	}
	visit, kind = pickVisitURL("designflow.aivex.art", "https://app.salesradar.live", false)
	if visit != "https://designflow.aivex.art" || kind != primaryKindCustom {
		t.Fatalf("revoked platform must keep custom primary: %s %s", visit, kind)
	}
}

func TestServiceHasAddressOnlyAfterACodebase(t *testing.T) {
	if serviceHasAddress(store.Node{Kind: "service", Status: "idle"}) {
		t.Fatal("empty slot must not get a public URL")
	}
	src := "github"
	if !serviceHasAddress(store.Node{Kind: "service", Status: "ready", SourceType: &src}) {
		t.Fatal("a staged service must keep its platform URL")
	}
}

func TestPickVisitURLPlatformWhenNoCustom(t *testing.T) {
	visit, kind := pickVisitURL("", "https://app.salesradar.live", true)
	if visit != "https://app.salesradar.live" || kind != primaryKindPlatform {
		t.Fatalf("visit=%s kind=%s", visit, kind)
	}
	visit, kind = pickVisitURL("", "https://app.salesradar.live", false)
	if visit != "" || kind != "" {
		t.Fatalf("revoked platform with no custom must be empty: %s %s", visit, kind)
	}
}
