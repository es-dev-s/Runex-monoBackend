package runtime

import "testing"

func TestParseSize(t *testing.T) {
	if got := ParseSize("512MiB"); got != 512*1024*1024 {
		t.Fatalf("512MiB: got %d", got)
	}
	if got := ParseSize("100B"); got != 100 {
		t.Fatalf("100B: got %d", got)
	}
	if ParseSize("0B") != 0 || ParseSize("") != 0 {
		t.Fatal("zero")
	}
	mib := ParseSize("23.54MiB")
	if mib < 24_000_000 || mib > 25_000_000 {
		t.Fatalf("23.54MiB: got %d", mib)
	}
	gib := ParseSize("1.2GiB")
	if gib < 1_200_000_000 || gib > 1_400_000_000 {
		t.Fatalf("1.2GiB: got %d", gib)
	}
}

func TestParseMemUsage(t *testing.T) {
	used, limit := parseMemUsage("23.54MiB / 512MiB")
	if used != ParseSize("23.54MiB") {
		t.Fatalf("used %d", used)
	}
	if limit != ParseSize("512MiB") {
		t.Fatalf("limit %d", limit)
	}
}
