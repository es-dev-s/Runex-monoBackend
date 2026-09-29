package sidecarquery

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const (
	DefaultLimit  = 50
	MaxLimit      = 100
	MaxOffset     = 1_000_000
	MaxCellRunes  = 4096
	MaxQueryRunes = 80
	MaxColumns    = 64
	QueryTimeout  = 8 * time.Second
	// Exact COUNT(*) only while the catalog has no estimate and the relation
	// is still small. A full count of every table, or of a million-row table,
	// is what made the studio wait even when Postgres is on this machine.
	ExactCountMax   = 1_000_000
	ExactCountBytes = 1 << 20
	// FilteredCountBytes bounds a search that wants a real match count. The
	// count has to scan, so above this the studio reports the range it has
	// rather than spending a second pass on a number nobody waited for.
	FilteredCountBytes = 32 << 20
)

var (
	identRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	tidRE   = regexp.MustCompile(`^\(\d{1,10},\d{1,5}\)$`)
)

func ClampPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	if offset < 0 {
		offset = 0
	}
	if offset > MaxOffset {
		offset = MaxOffset
	}
	return limit, offset
}

func Quote(name string) (string, error) {
	if !identRE.MatchString(name) || len(name) > 63 {
		return "", fmt.Errorf("invalid identifier")
	}
	return pgx.Identifier{name}.Sanitize(), nil
}

func ParseSort(raw string) (column, dir string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "asc", nil
	}
	column, dir, ok := strings.Cut(raw, ":")
	if !ok {
		column, dir = raw, "asc"
	}
	dir = strings.ToLower(strings.TrimSpace(dir))
	if dir != "asc" && dir != "desc" {
		return "", "", fmt.Errorf("invalid sort")
	}
	if _, err := Quote(column); err != nil {
		return "", "", err
	}
	return column, dir, nil
}

func LikePattern(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	if utf8.RuneCountInString(q) > MaxQueryRunes {
		runes := []rune(q)
		q = string(runes[:MaxQueryRunes])
	}
	q = strings.ReplaceAll(q, `\`, `\\`)
	q = strings.ReplaceAll(q, `%`, `\%`)
	q = strings.ReplaceAll(q, `_`, `\_`)
	return "%" + q + "%"
}

// HumanBytes renders a byte count the way pg_size_pretty does, so sizes read
// the same now that the listing computes them from the catalog rather than
// asking Postgres to format them.
func HumanBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	if n < 10*1024 {
		return fmt.Sprintf("%d bytes", n)
	}
	value := float64(n)
	for _, unit := range []string{"kB", "MB", "GB", "TB"} {
		value /= 1024
		if value < 10*1024 || unit == "TB" {
			return fmt.Sprintf("%.0f %s", value, unit)
		}
	}
	return fmt.Sprintf("%d bytes", n)
}

// uuidText renders Postgres's 16-byte uuid in the canonical form. Without this
// the driver's raw [16]byte reached the dashboard as a JSON array of numbers,
// so a uuid column displayed as "[196,202,66,...]" instead of its value.
func uuidText(b [16]byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, c := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hexDigits[c>>4], hexDigits[c&0x0f])
	}
	return string(out)
}

func ClipCell(v any) (any, bool) {
	switch t := v.(type) {
	case nil:
		return nil, false
	case [16]byte:
		return uuidText(t), false
	case []byte:
		s := string(t)
		if utf8.RuneCountInString(s) > MaxCellRunes {
			return string([]rune(s)[:MaxCellRunes]), true
		}
		return s, false
	case string:
		if utf8.RuneCountInString(t) > MaxCellRunes {
			return string([]rune(t)[:MaxCellRunes]), true
		}
		return t, false
	default:
		return v, false
	}
}
