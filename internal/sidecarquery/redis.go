package sidecarquery

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
)

func ListKeys(ctx context.Context, rdb *redis.Client, match string, cursor uint64, limit int) (KeyPage, error) {
	limit, _ = ClampPage(limit, 0)
	match = strings.TrimSpace(match)
	if match == "" {
		match = "*"
	}
	if utf8.RuneCountInString(match) > MaxQueryRunes {
		match = string([]rune(match)[:MaxQueryRunes])
	}
	keys, next, err := rdb.Scan(ctx, cursor, match, int64(limit)).Result()
	if err != nil {
		return KeyPage{}, err
	}
	page := KeyPage{Keys: []KeyItem{}, Cursor: next, HasMore: next != 0}
	pipe := rdb.Pipeline()
	typeCmds := make([]*redis.StatusCmd, len(keys))
	ttlCmds := make([]*redis.DurationCmd, len(keys))
	for i, key := range keys {
		typeCmds[i] = pipe.Type(ctx, key)
		ttlCmds[i] = pipe.TTL(ctx, key)
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return KeyPage{}, err
	}
	for i, key := range keys {
		item := KeyItem{Name: key, Type: typeCmds[i].Val()}
		if d := ttlCmds[i].Val(); d > 0 {
			item.TTL = int64(d.Seconds())
		}
		page.Keys = append(page.Keys, item)
	}
	return page, nil
}

func ReadKey(ctx context.Context, rdb *redis.Client, key string) (KeyValue, error) {
	kind, err := rdb.Type(ctx, key).Result()
	if err != nil {
		return KeyValue{}, err
	}
	out := KeyValue{Name: key, Type: kind}
	if d, err := rdb.TTL(ctx, key).Result(); err == nil && d > 0 {
		out.TTL = int64(d.Seconds())
	}
	switch kind {
	case "string":
		s, err := rdb.Get(ctx, key).Result()
		if err != nil && err != redis.Nil {
			return KeyValue{}, err
		}
		clipped, trunc := ClipCell(s)
		out.Value = clipped
		out.Truncated = trunc
	case "hash":
		vals, err := rdb.HGetAll(ctx, key).Result()
		if err != nil {
			return KeyValue{}, err
		}
		out.Value, out.Truncated = clipMap(vals)
	case "list":
		vals, err := rdb.LRange(ctx, key, 0, 49).Result()
		if err != nil {
			return KeyValue{}, err
		}
		out.Value, out.Truncated = clipSlice(vals)
	case "set":
		vals, _, err := rdb.SScan(ctx, key, 0, "", 50).Result()
		if err != nil {
			return KeyValue{}, err
		}
		out.Value, out.Truncated = clipSlice(vals)
	case "zset":
		vals, err := rdb.ZRangeWithScores(ctx, key, 0, 49).Result()
		if err != nil {
			return KeyValue{}, err
		}
		rows := make([]map[string]any, 0, len(vals))
		for _, z := range vals {
			member, trunc := ClipCell(z.Member)
			if trunc {
				out.Truncated = true
			}
			rows = append(rows, map[string]any{"member": member, "score": z.Score})
		}
		out.Value = rows
	case "stream":
		vals, err := rdb.XRangeN(ctx, key, "-", "+", 50).Result()
		if err != nil {
			return KeyValue{}, err
		}
		raw, _ := json.Marshal(vals)
		s := string(raw)
		if utf8.RuneCountInString(s) > MaxCellRunes {
			out.Value = string([]rune(s)[:MaxCellRunes])
			out.Truncated = true
		} else {
			out.Value = vals
		}
	case "none":
		return KeyValue{}, redis.Nil
	default:
		out.Value = nil
	}
	return out, nil
}

func RedisInfo(ctx context.Context, rdb *redis.Client) (keys int64, memory string, version string) {
	info, err := rdb.Info(ctx, "server", "keyspace", "memory").Result()
	if err != nil {
		return 0, "", ""
	}
	for _, line := range splitLines(info) {
		if k, v, ok := cutColon(line); ok {
			switch k {
			case "redis_version":
				version = v
			case "used_memory_human":
				memory = v
			}
		}
		if k, v, ok := cutColon(line); ok && k == "db0" {
			if n := parseKeysField(v); n >= 0 {
				keys = n
			}
		}
	}
	if n, err := rdb.DBSize(ctx).Result(); err == nil {
		keys = n
	}
	return keys, memory, version
}

func clipMap(in map[string]string) (map[string]any, bool) {
	out := map[string]any{}
	trunc := false
	n := 0
	for k, v := range in {
		if n >= 80 {
			trunc = true
			break
		}
		clipped, t := ClipCell(v)
		if t {
			trunc = true
		}
		out[k] = clipped
		n++
	}
	return out, trunc
}

func clipSlice(in []string) ([]any, bool) {
	out := make([]any, 0, len(in))
	trunc := len(in) >= 50
	for _, v := range in {
		clipped, t := ClipCell(v)
		if t {
			trunc = true
		}
		out = append(out, clipped)
	}
	return out, trunc
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			lines = append(lines, line)
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func cutColon(line string) (string, string, bool) {
	if line == "" || line[0] == '#' {
		return "", "", false
	}
	k, v, ok := splitOnce(line, ':')
	return k, v, ok
}

func splitOnce(s string, sep byte) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

func parseKeysField(v string) int64 {
	// keys=12,expires=0,avg_ttl=0
	for _, part := range splitComma(v) {
		k, n, ok := splitOnce(part, '=')
		if ok && k == "keys" {
			var out int64
			for _, r := range n {
				if r < '0' || r > '9' {
					break
				}
				out = out*10 + int64(r-'0')
			}
			return out
		}
	}
	return -1
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
