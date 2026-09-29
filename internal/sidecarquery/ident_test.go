package sidecarquery

import (
	"strings"
	"testing"
)

func TestQuoteRejectsInjection(t *testing.T) {
	if _, err := Quote(`users;drop`); err == nil {
		t.Fatal("expected reject")
	}
	if _, err := Quote(`users"`); err == nil {
		t.Fatal("expected reject")
	}
	got, err := Quote("users")
	if err != nil || got != `"users"` {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestClampPage(t *testing.T) {
	limit, offset := ClampPage(0, -4)
	if limit != DefaultLimit || offset != 0 {
		t.Fatalf("limit=%d offset=%d", limit, offset)
	}
	limit, offset = ClampPage(5000, MaxOffset+9)
	if limit != MaxLimit || offset != MaxOffset {
		t.Fatalf("limit=%d offset=%d", limit, offset)
	}
}

func TestParseSort(t *testing.T) {
	col, dir, err := ParseSort("created_at:desc")
	if err != nil || col != "created_at" || dir != "desc" {
		t.Fatalf("%s %s %v", col, dir, err)
	}
	if _, _, err := ParseSort("id;desc"); err == nil {
		t.Fatal("expected reject")
	}
}

func TestCursorValue(t *testing.T) {
	if CursorValue(int64(42)) != "42" {
		t.Fatal(CursorValue(int64(42)))
	}
	if !validCursor("") || !validCursor("abc-12") {
		t.Fatal("valid")
	}
	if validCursor("a\x00b") || validCursor(strings.Repeat("x", 129)) {
		t.Fatal("invalid")
	}
}

func TestLikePatternEscapes(t *testing.T) {
	got := LikePattern(`a%b_c\`)
	if got != `%a\%b\_c\\%` {
		t.Fatalf("got %q", got)
	}
}

func TestRowCountHint(t *testing.T) {
	if rowCountHint(0, 12) != 12 {
		t.Fatal("live stats must win over a stale 0 planner estimate")
	}
	if rowCountHint(40, 12) != 40 {
		t.Fatal("planner estimate should win when it is higher")
	}
	if rowCountHint(-1, 0) != 0 {
		t.Fatal("negative reltuples is unanalyzed")
	}
}

func TestClipCellRendersUUID(t *testing.T) {
	raw := [16]byte{0xc4, 0xca, 0x42, 0x38, 0xa0, 0xb9, 0x23, 0x82, 0x0d, 0xcc, 0x50, 0x9a, 0x6f, 0x75, 0x84, 0x9b}
	got, truncated := ClipCell(raw)
	if truncated {
		t.Fatal("a uuid is never truncated")
	}
	if got != "c4ca4238-a0b9-2382-0dcc-509a6f75849b" {
		t.Fatalf("uuid rendered as %v; a raw byte array reaches the browser as [196,202,...]", got)
	}
}

func TestValidTID(t *testing.T) {
	for _, ok := range []string{"(0,1)", "(123,45)", "(4294967295,65535)"} {
		if !validTID(ok) {
			t.Fatalf("%q should be a usable ctid cursor", ok)
		}
	}
	for _, bad := range []string{"", "123", "(1,2", "1,2)", "(a,b)", "(1,2);DROP", "(99999999999,1)"} {
		if validTID(bad) {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:                "0 bytes",
		8192:             "8192 bytes",
		10 * 1024:        "10 kB",
		1024 * 1024:      "1024 kB",
		11 * 1024 * 1024: "11 MB",
	}
	for n, want := range cases {
		if got := HumanBytes(n); got != want {
			t.Fatalf("HumanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestNeedsExactCount(t *testing.T) {
	if !needsExactCount(0) {
		t.Fatal("unanalyzed tables need COUNT(*)")
	}
	if needsExactCount(3) || needsExactCount(ExactCountMax) {
		t.Fatal("a planner hint must not trigger a table scan")
	}
	if !worthExactCount(0, 4096) {
		t.Fatal("a small unanalyzed table still needs an exact count")
	}
	if worthExactCount(250, 4096) || worthExactCount(0, ExactCountBytes+1) {
		t.Fatal("known or large tables must keep the catalog estimate")
	}
}
