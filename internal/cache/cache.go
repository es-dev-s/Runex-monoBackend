package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	KeyGitHubApp    = "aivex:v1:gh:app"
	KeyGitHubSync   = "aivex:v1:gh:installs"
	reposKeyPrefix  = "aivex:v1:gh:repos:"
	branchKeyPrefix = "aivex:v1:gh:branches:"
	deliveryKeyPfx  = "aivex:v1:gh:delivery:"
)

// DeliveryKey scopes a GitHub webhook delivery ID so the same delivery is only
// ever acted on once, no matter how many times GitHub retries it.
func DeliveryKey(deliveryID string) string {
	return deliveryKeyPfx + strings.ToLower(strings.TrimSpace(deliveryID))
}

func ReposKey(installationID int64) string {
	return fmt.Sprintf("%s%d", reposKeyPrefix, installationID)
}

func BranchesKey(installationID int64, fullName string) string {
	return fmt.Sprintf("%s%d:%s", branchKeyPrefix, installationID, strings.ToLower(strings.TrimSpace(fullName)))
}

type Store struct {
	rdb *redis.Client
	mu  sync.Mutex
	mem map[string]memEntry
}

type memEntry struct {
	value   []byte
	expires time.Time
}

func Open(ctx context.Context, url string) (*Store, error) {
	store := &Store{mem: map[string]memEntry{}}
	if url == "" {
		return store, nil
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	client := redis.NewClient(opt)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	store.rdb = client
	return store, nil
}

func (s *Store) Backend() string {
	if s != nil && s.rdb != nil {
		return "redis"
	}
	return "memory"
}

func (s *Store) Ping(ctx context.Context) error {
	if s != nil && s.rdb != nil {
		return s.rdb.Ping(ctx).Err()
	}
	return nil
}

func (s *Store) Close() {
	if s != nil && s.rdb != nil {
		_ = s.rdb.Close()
	}
}

func (s *Store) GetJSON(ctx context.Context, key string, dest any) bool {
	raw, ok := s.get(ctx, key)
	if !ok {
		return false
	}
	return json.Unmarshal(raw, dest) == nil
}

func (s *Store) SetJSON(ctx context.Context, key string, value any, ttl time.Duration) {
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	s.set(ctx, key, raw, ttl)
}

// Claim atomically takes ownership of a key, returning true only for the first
// caller within the TTL. It is the deduplication primitive for at-least-once
// event sources such as GitHub webhooks, which redeliver on any non-2xx reply
// or manual retry. Backed by SETNX when Redis is available so the guarantee
// holds across processes; otherwise it is per-process.
func (s *Store) Claim(ctx context.Context, key string, ttl time.Duration) bool {
	if s == nil {
		return true
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	if s.rdb != nil {
		ok, err := s.rdb.SetNX(ctx, key, []byte("1"), ttl).Result()
		if err != nil {
			// A cache outage must not block deploys; fall through to the
			// in-process guard rather than dropping the event.
			return s.claimMem(key, ttl)
		}
		return ok
	}
	return s.claimMem(key, ttl)
}

func (s *Store) claimMem(key string, ttl time.Duration) bool {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if item, ok := s.mem[key]; ok && now.Before(item.expires) {
		return false
	}
	for k, item := range s.mem {
		if now.After(item.expires) {
			delete(s.mem, k)
		}
	}
	s.mem[key] = memEntry{value: []byte("1"), expires: now.Add(ttl)}
	return true
}

func (s *Store) Delete(ctx context.Context, key string) {
	if s == nil {
		return
	}
	if s.rdb != nil {
		_ = s.rdb.Del(ctx, key).Err()
	}
	s.mu.Lock()
	delete(s.mem, key)
	s.mu.Unlock()
}

func (s *Store) get(ctx context.Context, key string) ([]byte, bool) {
	if s == nil {
		return nil, false
	}
	if s.rdb != nil {
		raw, err := s.rdb.Get(ctx, key).Bytes()
		if err != nil {
			return nil, false
		}
		return raw, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.mem[key]
	if !ok || time.Now().After(item.expires) {
		delete(s.mem, key)
		return nil, false
	}
	return item.value, true
}

func (s *Store) set(ctx context.Context, key string, value []byte, ttl time.Duration) {
	if s == nil {
		return
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if s.rdb != nil {
		_ = s.rdb.Set(ctx, key, value, ttl).Err()
		return
	}
	s.mu.Lock()
	s.mem[key] = memEntry{value: value, expires: time.Now().Add(ttl)}
	s.mu.Unlock()
}
