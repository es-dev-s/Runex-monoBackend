package sidecarquery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	// poolIdleTTL drops a database's pool once nobody has browsed it for a
	// while, so an account with many databases does not hold a connection to
	// every one of them forever.
	poolIdleTTL = 10 * time.Minute
	// poolMaxConns caps concurrent queries against a single project database.
	// These are small containers shared with the customer's own app, so the
	// explorer must never be the reason they run out of connections.
	poolMaxConns = 4
)

// Pools keeps one warm connection pool per database node. Opening a connection
// costs a TCP round trip plus a SCRAM exchange, which is deliberately slow to
// compute; paying that on every table and every page of rows is what made
// browsing feel sluggish even against a database on this machine.
//
// Entries are keyed by node and fingerprinted by address and credentials, so a
// recreated container with a new address or password is never reused.
type Pools struct {
	mu    sync.Mutex
	pg    map[string]*pgEntry
	redis map[string]*redisEntry
}

type pgEntry struct {
	pool *pgxpool.Pool
	key  string
	used time.Time
}

type redisEntry struct {
	client *redis.Client
	key    string
	used   time.Time
}

func NewPools() *Pools {
	return &Pools{pg: map[string]*pgEntry{}, redis: map[string]*redisEntry{}}
}

// Postgres hands back a pooled connection. The caller must release it.
func (p *Pools) Postgres(ctx context.Context, nodeID, ip string, creds Creds) (*pgxpool.Conn, error) {
	if p == nil {
		return nil, context.Canceled
	}
	key := fingerprint(ip, creds.User, creds.Password, creds.Database)
	pool, err := p.postgresPool(ctx, nodeID, ip, creds, key)
	if err != nil {
		return nil, err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		// A pool that cannot serve a connection is usually pointing at a
		// container that has gone away. Drop it so the next read rebuilds it
		// instead of failing for as long as the entry lives.
		p.Forget(nodeID)
		return nil, err
	}
	return conn, nil
}

func (p *Pools) postgresPool(ctx context.Context, nodeID, ip string, creds Creds, key string) (*pgxpool.Pool, error) {
	p.mu.Lock()
	if entry, ok := p.pg[nodeID]; ok {
		if entry.key == key {
			entry.used = time.Now()
			pool := entry.pool
			p.sweepLocked()
			p.mu.Unlock()
			return pool, nil
		}
		delete(p.pg, nodeID)
		go entry.pool.Close()
	}
	p.sweepLocked()
	p.mu.Unlock()

	pool, err := newPostgresPool(ctx, ip, creds)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// Two readers can race to build the first pool for a node. Keep whichever
	// landed first so the node never ends up with two live pools.
	if entry, ok := p.pg[nodeID]; ok && entry.key == key {
		entry.used = time.Now()
		go pool.Close()
		return entry.pool, nil
	}
	if entry, ok := p.pg[nodeID]; ok {
		go entry.pool.Close()
	}
	p.pg[nodeID] = &pgEntry{pool: pool, key: key, used: time.Now()}
	return pool, nil
}

func newPostgresPool(ctx context.Context, ip string, creds Creds) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(creds.PostgresURL(ip, 5432) + "?sslmode=disable")
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = poolMaxConns
	cfg.MinConns = 1
	cfg.MaxConnIdleTime = poolIdleTTL
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnLifetimeJitter = 5 * time.Minute
	cfg.HealthCheckPeriod = time.Minute
	cfg.ConnConfig.ConnectTimeout = 3 * time.Second
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	// Sent in the startup packet, so the read-only guard and the statement
	// timeout cost nothing and cannot be missed on a pooled connection.
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "8000"
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.ConnConfig.RuntimeParams["application_name"] = "runex-studio"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// Redis hands back a shared client. The caller must not close it.
func (p *Pools) Redis(ctx context.Context, nodeID, ip, password string) (*redis.Client, error) {
	if p == nil {
		return nil, context.Canceled
	}
	key := fingerprint(ip, password)

	p.mu.Lock()
	if entry, ok := p.redis[nodeID]; ok {
		if entry.key == key {
			entry.used = time.Now()
			client := entry.client
			p.sweepLocked()
			p.mu.Unlock()
			return client, nil
		}
		delete(p.redis, nodeID)
		go entry.client.Close()
	}
	p.sweepLocked()
	p.mu.Unlock()

	client := redis.NewClient(&redis.Options{
		Addr:         net.JoinHostPort(ip, "6379"),
		Password:     password,
		DB:           0,
		PoolSize:     poolMaxConns,
		MinIdleConns: 1,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  QueryTimeout,
		WriteTimeout: 3 * time.Second,
	})
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if entry, ok := p.redis[nodeID]; ok && entry.key == key {
		entry.used = time.Now()
		go client.Close()
		return entry.client, nil
	}
	if entry, ok := p.redis[nodeID]; ok {
		go entry.client.Close()
	}
	p.redis[nodeID] = &redisEntry{client: client, key: key, used: time.Now()}
	return client, nil
}

// Forget drops a node's pools. Call it whenever the container behind the node
// is stopped or replaced, so a read never reaches a dead address.
func (p *Pools) Forget(nodeID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	pgEntry, hasPG := p.pg[nodeID]
	redisEntry, hasRedis := p.redis[nodeID]
	delete(p.pg, nodeID)
	delete(p.redis, nodeID)
	p.mu.Unlock()

	if hasPG {
		go pgEntry.pool.Close()
	}
	if hasRedis {
		go redisEntry.client.Close()
	}
}

// Close releases every pool. Used on shutdown.
func (p *Pools) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	pgPools := make([]*pgxpool.Pool, 0, len(p.pg))
	for id, entry := range p.pg {
		pgPools = append(pgPools, entry.pool)
		delete(p.pg, id)
	}
	clients := make([]*redis.Client, 0, len(p.redis))
	for id, entry := range p.redis {
		clients = append(clients, entry.client)
		delete(p.redis, id)
	}
	p.mu.Unlock()

	for _, pool := range pgPools {
		pool.Close()
	}
	for _, client := range clients {
		_ = client.Close()
	}
}

// sweepLocked retires pools nobody has used lately. Called on access so there
// is no background goroutine to own.
func (p *Pools) sweepLocked() {
	cutoff := time.Now().Add(-poolIdleTTL)
	for id, entry := range p.pg {
		if entry.used.Before(cutoff) {
			delete(p.pg, id)
			go entry.pool.Close()
		}
	}
	for id, entry := range p.redis {
		if entry.used.Before(cutoff) {
			delete(p.redis, id)
			go entry.client.Close()
		}
	}
}

func fingerprint(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
