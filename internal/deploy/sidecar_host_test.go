package deploy

import (
	"testing"

	"deploy_platform/internal/config"
)

func TestPublicTCPHostPrefersDashboardHost(t *testing.T) {
	e := &Engine{cfg: config.Config{
		EdgeDomain: "salesradar.live",
		PublicBase: "http://deploy.salesradar.live",
		WebURL:     "http://deploy.salesradar.live",
	}}
	if got := e.publicTCPHost(); got != "deploy.salesradar.live" {
		t.Fatalf("got %s", got)
	}
}
