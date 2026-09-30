package customdomain

import (
	"context"
	"errors"
	"testing"

	"deploy_platform/internal/config"
)

func TestNormalize(t *testing.T) {
	got, err := Normalize("https://WWW.Example.com/path?q=1")
	if err != nil || got != "www.example.com" {
		t.Fatalf("got %q %v", got, err)
	}
	got, err = Normalize("Shop.Example.com.")
	if err != nil || got != "shop.example.com" {
		t.Fatalf("trailing dot %q %v", got, err)
	}
	if _, err := Normalize("localhost"); err != ErrInvalid {
		t.Fatalf("localhost: %v", err)
	}
	if _, err := Normalize("10.0.0.1"); err != ErrInvalid {
		t.Fatalf("ip: %v", err)
	}
	if _, err := Normalize("app.localhost"); err != ErrInvalid {
		t.Fatalf("localhost suffix: %v", err)
	}
	if _, err := Normalize("-bad.example.com"); err != ErrInvalid {
		t.Fatalf("leading hyphen: %v", err)
	}
}

func TestPlatformHost(t *testing.T) {
	cfg := config.Config{
		EdgeDomain:  "salesradar.live",
		WebURL:      "https://deploy.salesradar.live",
		PublicURL:   "https://deploy.salesradar.live",
		PublicBase:  "https://salesradar.live",
		TunnelCNAME: "abc.cfargotunnel.com",
	}
	for _, host := range []string{
		"salesradar.live",
		"horbor-frontend.salesradar.live",
		"deploy.salesradar.live",
		"cname.salesradar.live",
		"abc.cfargotunnel.com",
	} {
		if !PlatformHost(cfg, host) {
			t.Fatalf("%s must stay on the platform zone", host)
		}
	}
	if PlatformHost(cfg, "app.example.com") {
		t.Fatal("customer domains must be allowed")
	}
}

func TestRecordsApexAndSubdomain(t *testing.T) {
	apex := Records("example.com", "harbor-verify=abc", "cname.salesradar.live")
	if len(apex) != 2 || apex[0].Type != "TXT" || apex[1].Type != "CNAME" || apex[1].Host != "@" {
		t.Fatalf("apex %#v", apex)
	}
	if apex[0].Name != "_harbor-challenge.example.com" {
		t.Fatalf("txt name %s", apex[0].Name)
	}
	sub := Records("www.example.com", "harbor-verify=abc", "cname.salesradar.live")
	if len(sub) != 2 || sub[1].Type != "CNAME" || sub[1].Host != "www" {
		t.Fatalf("sub %#v", sub)
	}
	if sub[0].Host != "_harbor-challenge.www" {
		t.Fatalf("txt host %s", sub[0].Host)
	}
}

func TestAddressRecordsOnlyForApex(t *testing.T) {
	apex := AddressRecords("example.com", []string{"104.21.65.142", "104.21.65.142"}, []string{"2606:4700:3035::ac43:a3e7"})
	if len(apex) != 2 || apex[0].Type != "A" || apex[1].Type != "AAAA" || apex[0].Required || apex[0].Host != "@" {
		t.Fatalf("%#v", apex)
	}
	if AddressRecords("www.example.com", []string{"1.2.3.4"}, nil) != nil {
		t.Fatal("subdomain must not get address records")
	}
}

func TestTXTMatch(t *testing.T) {
	token := "harbor-verify=secret"
	if !TXTMatch([]string{`"harbor-verify=secret"`}, token) {
		t.Fatal("quoted")
	}
	if TXTMatch([]string{"other"}, token) {
		t.Fatal("wrong value")
	}
}

type fakeResolver struct {
	txt   map[string][]string
	cname map[string]string
	ips   map[string][]string
}

func (f fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if values, ok := f.txt[name]; ok {
		return values, nil
	}
	return nil, errors.New("nxdomain")
}

func (f fakeResolver) LookupCNAME(_ context.Context, name string) (string, error) {
	if value, ok := f.cname[name]; ok {
		return value, nil
	}
	return name, nil
}

func (f fakeResolver) LookupHost(_ context.Context, name string) ([]string, error) {
	if values, ok := f.ips[name]; ok {
		return values, nil
	}
	return nil, errors.New("nxdomain")
}

func TestVerifyRequiresTXTAndRoute(t *testing.T) {
	token := "harbor-verify=abc"
	target := "cname.salesradar.live"
	res := fakeResolver{
		txt:   map[string][]string{"_harbor-challenge.app.example.com": {token}},
		cname: map[string]string{"app.example.com": "cname.salesradar.live."},
		ips: map[string][]string{
			"app.example.com":       {"1.2.3.4"},
			"cname.salesradar.live": {"1.2.3.4"},
		},
	}
	got := Verify(context.Background(), res, "app.example.com", token, target)
	if !got.Active || !got.TXT || !got.Route {
		t.Fatalf("%+v", got)
	}

	missingCNAME := fakeResolver{txt: res.txt}
	pending := Verify(context.Background(), missingCNAME, "app.example.com", token, target)
	if pending.Active || !pending.TXT || pending.Route {
		t.Fatalf("cname missing should stay pending: %+v", pending)
	}

	proxied := fakeResolver{
		txt: res.txt,
		ips: map[string][]string{"app.example.com": {"104.21.61.116", "172.67.209.157"}},
	}
	orange := Verify(context.Background(), proxied, "app.example.com", token, target)
	if !orange.Active || !orange.Route {
		t.Fatalf("orange-cloud CNAME must verify when TXT matches: %+v", orange)
	}

	stolen := Verify(context.Background(), fakeResolver{
		cname: map[string]string{"app.example.com": target},
		ips:   res.ips,
	}, "app.example.com", token, target)
	if stolen.Active || stolen.TXT {
		t.Fatalf("TXT is required before routing: %+v", stolen)
	}
}

func TestCloudflareProxied(t *testing.T) {
	if !CloudflareProxied([]string{"104.21.61.116", "172.67.209.157"}) {
		t.Fatal("official CF anycast")
	}
	if CloudflareProxied([]string{"104.21.61.116", "1.1.1.1"}) {
		t.Fatal("mixed with a non-CF address is not an orange-cloud hostname")
	}
	if CloudflareProxied(nil) {
		t.Fatal("empty")
	}
}

func TestClassifyProbeDetectsCloudflareBans(t *testing.T) {
	banned := classifyProbe(403, "error code: 1014")
	if !banned.Banned || banned.OK {
		t.Fatalf("%+v", banned)
	}
	tunnel := classifyProbe(530, "Error 1033 Cloudflare is currently unable to resolve it")
	if !tunnel.Tunnel || tunnel.OK {
		t.Fatalf("%+v", tunnel)
	}
	ok := classifyProbe(401, `{"error":"unauthorized"}`)
	if !ok.OK || ok.Banned {
		t.Fatalf("%+v", ok)
	}
	empty404 := classifyProbe(404, "")
	if !empty404.OK || empty404.Banned || empty404.Tunnel {
		t.Fatalf("empty 404 is a reachable origin response: %+v", empty404)
	}
	originDown := classifyProbe(522, "connection timed out")
	if originDown.OK || originDown.Banned || originDown.Tunnel {
		t.Fatalf("origin timeout must not look healthy: %+v", originDown)
	}
}

func TestTokenFromSecret(t *testing.T) {
	if TokenFromSecret("abc") != "harbor-verify=abc" {
		t.Fatal("prefix")
	}
	if TokenFromSecret("harbor-verify=abc") != "harbor-verify=abc" {
		t.Fatal("already prefixed")
	}
}
