package sidecarquery

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

func PostgresVersion(ctx context.Context, conn *pgx.Conn) string {
	var v string
	if err := conn.QueryRow(ctx, "SHOW server_version").Scan(&v); err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

// listTablesSQL sizes every table from the catalog instead of calling
// pg_total_relation_size, which stats the filesystem once per table, toast and
// index fork. On a database with 202 tables that function alone was 106ms of a
// 114ms listing, and the cost grows with the table count. Summing relpages
// reads only shared memory, so the listing stays flat as a database grows. The
// figure tracks the last VACUUM or ANALYZE, the same basis as reltuples beside
// it; the exact size is measured per table when one is opened.
const listTablesSQL = `
	SELECT n.nspname,
	       c.relname,
	       GREATEST(c.reltuples, 0)::bigint,
	       COALESCE(s.n_live_tup, 0)::bigint,
	       (c.relpages
	         + COALESCE(t.relpages, 0)
	         + COALESCE(idx.pages, 0)
	         + COALESCE(tidx.pages, 0))::bigint * current_setting('block_size')::bigint
	FROM pg_class c
	JOIN pg_namespace n ON n.oid = c.relnamespace
	LEFT JOIN pg_class t ON t.oid = c.reltoastrelid
	LEFT JOIN pg_stat_all_tables s ON s.relid = c.oid
	LEFT JOIN LATERAL (
	  SELECT sum(ic.relpages) AS pages
	  FROM pg_index x
	  JOIN pg_class ic ON ic.oid = x.indexrelid
	  WHERE x.indrelid = c.oid
	) idx ON true
	LEFT JOIN LATERAL (
	  SELECT sum(ic.relpages) AS pages
	  FROM pg_index x
	  JOIN pg_class ic ON ic.oid = x.indexrelid
	  WHERE x.indrelid = c.reltoastrelid
	) tidx ON true
	WHERE c.relkind IN ('r', 'p')
	  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
	  AND n.nspname NOT LIKE 'pg_toast%'
	  AND n.nspname NOT LIKE 'pg_temp%'
	ORDER BY n.nspname, c.relname
	LIMIT 500
`

func ListTables(ctx context.Context, conn *pgx.Conn) ([]Table, error) {
	rows, err := conn.Query(ctx, listTablesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Table{}
	for rows.Next() {
		var t Table
		var live int64
		if err := rows.Scan(&t.Schema, &t.Name, &t.EstimatedRows, &live, &t.SizeBytes); err != nil {
			return nil, err
		}
		t.EstimatedRows = rowCountHint(t.EstimatedRows, live)
		t.Size = HumanBytes(t.SizeBytes)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if !worthExactCount(out[i].EstimatedRows, out[i].SizeBytes) {
			continue
		}
		n, ok := exactTableCount(ctx, conn, out[i].Schema, out[i].Name, "", nil)
		if !ok {
			continue
		}
		out[i].EstimatedRows = n
		out[i].Exact = true
	}
	return out, nil
}

func TableRows(ctx context.Context, conn *pgx.Conn, schema, table string, limit, offset int, sort, q, after string) (RowPage, error) {
	limit, offset = ClampPage(limit, offset)
	if !validCursor(after) {
		return RowPage{}, fmt.Errorf("invalid identifier")
	}
	schemaQ, err := Quote(schema)
	if err != nil {
		return RowPage{}, err
	}
	tableQ, err := Quote(table)
	if err != nil {
		return RowPage{}, err
	}
	cols, err := tableColumns(ctx, conn, schema, table)
	if err != nil {
		return RowPage{}, err
	}
	if len(cols) == 0 {
		return RowPage{Columns: []Column{}, Rows: []map[string]any{}, Offset: offset, Limit: limit}, nil
	}
	if len(cols) > MaxColumns {
		cols = cols[:MaxColumns]
	}
	sortCol, sortDir, err := ParseSort(sort)
	if err != nil {
		return RowPage{}, err
	}
	if sortCol != "" {
		ok := false
		for _, col := range cols {
			if col.Name == sortCol {
				ok = true
				break
			}
		}
		if !ok {
			return RowPage{}, fmt.Errorf("unknown sort column")
		}
	}

	pk, hasPK := singlePK(cols)
	searching := strings.TrimSpace(q) != ""
	keyset := hasPK && !searching && (sortCol == "" || sortCol == pk.Name)
	// A table with no single-column key used to fall back to OFFSET, which
	// makes Postgres read and throw away every skipped row: page 2000 of a
	// million-row log table took over two seconds. Every heap table has ctid,
	// the physical row pointer, and a ctid range scan jumps straight to the
	// right block. It only orders rows physically, so it is used only when the
	// caller has not asked for a sort.
	byCtid := !hasPK && !searching && sortCol == ""
	if byCtid {
		keyset = true
	}
	if keyset && !byCtid && sortCol == "" {
		sortCol, sortDir = pk.Name, "asc"
	}
	if !keyset && sortCol == "" && hasPK {
		sortCol, sortDir = pk.Name, "asc"
	} else if !keyset && sortCol == "" && len(cols) > 0 {
		sortCol, sortDir = cols[0].Name, "asc"
	}

	args := []any{}
	where := ""
	fetchOffset := offset
	fetchLimit := limit
	if byCtid {
		fetchOffset = 0
		fetchLimit = limit + 1
		from := after
		if from == "" {
			// Every page carries a lower bound, including the first. No row has
			// offset zero, so this excludes nothing, and it is what makes the
			// planner choose a Tid Range Scan: that node walks blocks upward
			// from the bound and stops at the limit, so the rows arrive in
			// physical order without an ORDER BY. Asking for the order
			// explicitly instead cost a parallel scan and a sort of the whole
			// table — 333ms a page against 0.1ms for the range scan.
			from = "(0,0)"
		}
		if !validTID(from) {
			return RowPage{}, fmt.Errorf("invalid cursor")
		}
		args = append(args, from)
		where = fmt.Sprintf(" WHERE ctid > $%d::tid", len(args))
	} else if keyset {
		fetchOffset = 0
		fetchLimit = limit + 1
		if after != "" {
			quoted, err := Quote(pk.Name)
			if err != nil {
				return RowPage{}, err
			}
			op := ">"
			if sortDir == "desc" {
				op = "<"
			}
			args = append(args, after)
			where = fmt.Sprintf(" WHERE %s %s $%d", quoted, op, len(args))
		}
	} else if pattern := LikePattern(q); pattern != "" {
		var textCol string
		for _, col := range cols {
			if isTextish(col.Type) {
				textCol = col.Name
				break
			}
		}
		if textCol != "" {
			quoted, err := Quote(textCol)
			if err != nil {
				return RowPage{}, err
			}
			args = append(args, pattern)
			where = fmt.Sprintf(" WHERE %s::text ILIKE $%d ESCAPE '\\'", quoted, len(args))
		}
	}

	order := ""
	if byCtid {
		// Deliberately unsorted; see the lower bound above.
	} else if sortCol != "" {
		quoted, err := Quote(sortCol)
		if err != nil {
			return RowPage{}, err
		}
		order = " ORDER BY " + quoted + " " + strings.ToUpper(sortDir)
	}

	// The ctid is fetched as a leading extra column so the next cursor can be
	// read off the last row. It is stripped before the page is returned.
	selection := "*"
	if byCtid {
		selection = "ctid::text AS runex_row_cursor, *"
	}
	query := fmt.Sprintf(
		"SELECT %s FROM %s.%s%s%s LIMIT %d OFFSET %d",
		selection, schemaQ, tableQ, where, order, fetchLimit, fetchOffset,
	)
	rows, err := conn.Query(ctx, query, args...)
	if err != nil {
		return RowPage{}, err
	}
	defer rows.Close()

	page := RowPage{
		Columns: cols,
		Rows:    []map[string]any{},
		Offset:  offset,
		Limit:   limit,
	}
	// Cursors for ctid paging live outside the row data, so they are collected
	// alongside the rows rather than read back out of the returned map.
	cursors := []string{}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return RowPage{}, err
		}
		descs := rows.FieldDescriptions()
		start := 0
		if byCtid {
			if len(values) > 0 {
				cursors = append(cursors, CursorValue(values[0]))
			}
			start = 1
		}
		row := map[string]any{}
		for i := start; i < len(descs); i++ {
			var val any
			if i < len(values) {
				val = values[i]
			}
			clipped, trunc := ClipCell(val)
			if trunc {
				page.Truncated = true
			}
			row[string(descs[i].Name)] = clipped
		}
		page.Rows = append(page.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return RowPage{}, err
	}
	page.Keyset = keyset
	if keyset && len(page.Rows) > limit {
		page.HasMore = true
		page.Rows = page.Rows[:limit]
		cursors = cursors[:min(len(cursors), limit)]
	} else if !keyset {
		page.HasMore = len(page.Rows) == limit
	}
	if keyset && len(page.Rows) > 0 {
		if byCtid {
			if n := len(cursors); n > 0 {
				page.NextAfter = cursors[n-1]
			}
		} else if hasPK {
			page.NextAfter = CursorValue(page.Rows[len(page.Rows)-1][pk.Name])
		}
	}
	// A first page that did not fill holds the entire result, so the total is
	// already known. This is the common case for a search that matches a
	// handful of rows and for small tables, and it costs no extra query.
	if offset == 0 && after == "" && !page.HasMore {
		page.EstimatedRows = int64(len(page.Rows))
		page.Exact = true
		return page, nil
	}

	hint, sizeBytes := tableEstimate(ctx, conn, schema, table)

	if searching {
		// Counting matches means scanning them, and the planner's estimate
		// describes the whole table, not the matches, so reporting it would be
		// wrong. Pay for a real count only while the relation is small enough
		// that the scan is cheap; otherwise report nothing and let the caller
		// show the range it actually has.
		if sizeBytes > 0 && sizeBytes <= FilteredCountBytes {
			if n, ok := exactTableCount(ctx, conn, schema, table, where, args); ok {
				page.EstimatedRows = n
				page.Exact = true
			}
		}
		return page, nil
	}

	page.EstimatedRows = hint
	if worthExactCount(hint, sizeBytes) {
		if n, ok := exactTableCount(ctx, conn, schema, table, where, args); ok {
			page.EstimatedRows = n
			page.Exact = true
		}
	}
	return page, nil
}

// validTID guards the ctid cursor. The value is passed as a parameter and cast
// server side, so this is about failing clearly rather than about injection.
func validTID(s string) bool {
	return tidRE.MatchString(s)
}

func singlePK(cols []Column) (Column, bool) {
	var found []Column
	for _, col := range cols {
		if col.PK {
			found = append(found, col)
		}
	}
	if len(found) != 1 {
		return Column{}, false
	}
	return found[0], true
}

func validCursor(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 32 || s[i] == 127 {
			return false
		}
	}
	return true
}

func CursorValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case int64:
		return strconv.FormatInt(t, 10)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case int:
		return strconv.Itoa(t)
	case uint64:
		return strconv.FormatUint(t, 10)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(t)
	}
}

func tableColumns(ctx context.Context, conn *pgx.Conn, schema, table string) ([]Column, error) {
	rows, err := conn.Query(ctx, `
		SELECT a.attname,
		       pg_catalog.format_type(a.atttypid, a.atttypmod),
		       NOT a.attnotnull,
		       COALESCE((
		         SELECT true
		         FROM pg_index i
		         WHERE i.indrelid = a.attrelid
		           AND i.indisprimary
		           AND a.attnum = ANY (i.indkey)
		       ), false)
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1
		  AND c.relname = $2
		  AND a.attnum > 0
		  AND NOT a.attisdropped
		ORDER BY a.attnum
	`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Column{}
	for rows.Next() {
		var col Column
		if err := rows.Scan(&col.Name, &col.Type, &col.Nullable, &col.PK); err != nil {
			return nil, err
		}
		out = append(out, col)
	}
	return out, rows.Err()
}

func tableEstimate(ctx context.Context, conn *pgx.Conn, schema, table string) (int64, int64) {
	var estimate, live, sizeBytes int64
	_ = conn.QueryRow(ctx, `
		SELECT GREATEST(c.reltuples, 0)::bigint,
		       COALESCE(s.n_live_tup, 0)::bigint,
		       pg_total_relation_size(c.oid)::bigint
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_stat_all_tables s ON s.relid = c.oid
		WHERE n.nspname = $1 AND c.relname = $2
	`, schema, table).Scan(&estimate, &live, &sizeBytes)
	return rowCountHint(estimate, live), sizeBytes
}

func rowCountHint(reltuples, live int64) int64 {
	if live > reltuples {
		return live
	}
	if reltuples < 0 {
		return 0
	}
	return reltuples
}

func needsExactCount(hint int64) bool {
	return hint == 0
}

func worthExactCount(hint, sizeBytes int64) bool {
	if hint > 0 {
		return false
	}
	if sizeBytes > ExactCountBytes {
		return false
	}
	return true
}

func exactTableCount(ctx context.Context, conn *pgx.Conn, schema, table, where string, args []any) (int64, bool) {
	schemaQ, err := Quote(schema)
	if err != nil {
		return 0, false
	}
	tableQ, err := Quote(table)
	if err != nil {
		return 0, false
	}
	var n int64
	query := fmt.Sprintf("SELECT COUNT(*)::bigint FROM %s.%s%s", schemaQ, tableQ, where)
	if err := conn.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		return 0, false
	}
	return n, true
}

func isTextish(t string) bool {
	t = strings.ToLower(t)
	return strings.Contains(t, "char") || strings.Contains(t, "text") || strings.Contains(t, "json") || strings.Contains(t, "uuid")
}
