.PHONY: test run tidy db redis stores stores-down stores-status

# Control-plane stores. These hold users, projects, nodes, deployments and the
# GitHub cache, so they run on this machine next to the control plane: every
# API call makes several queries, and a managed database in another region
# charged ~265ms of round trip to each one.
#
# Bound to loopback so they can never collide with a per-project sidecar and
# are not reachable from outside the host.
CONTROL_PG_NAME  ?= runex-control-pg
CONTROL_PG_PORT  ?= 5433
CONTROL_PG_USER  ?= runex
CONTROL_PG_DB    ?= runex
CONTROL_PG_VOL   ?= runex-control-pg
CONTROL_RD_NAME  ?= runex-control-redis
CONTROL_RD_PORT  ?= 6380
CONTROL_RD_VOL   ?= runex-control-redis

# Password for the local control-plane Postgres. Overridable so a real
# deployment supplies its own instead of using this development value.
CONTROL_PG_PASSWORD ?= runex_local_dev

db:
	@docker start $(CONTROL_PG_NAME) >/dev/null 2>&1 || docker run -d \
	  --name $(CONTROL_PG_NAME) \
	  --restart unless-stopped \
	  --memory 1g --memory-swap 1g --cpus 2.0 \
	  --shm-size 256m \
	  --health-cmd "pg_isready -U $(CONTROL_PG_USER) -d $(CONTROL_PG_DB)" \
	  --health-interval 10s --health-timeout 3s --health-retries 5 \
	  -e POSTGRES_USER=$(CONTROL_PG_USER) \
	  -e POSTGRES_PASSWORD=$(CONTROL_PG_PASSWORD) \
	  -e POSTGRES_DB=$(CONTROL_PG_DB) \
	  -v $(CONTROL_PG_VOL):/var/lib/postgresql/data \
	  -p 127.0.0.1:$(CONTROL_PG_PORT):5432 \
	  postgres:16-alpine \
	  postgres \
	    -c shared_buffers=256MB \
	    -c effective_cache_size=768MB \
	    -c maintenance_work_mem=64MB \
	    -c work_mem=8MB \
	    -c max_connections=100 \
	    -c random_page_cost=1.1 \
	    -c effective_io_concurrency=200 \
	    -c checkpoint_completion_target=0.9 \
	    -c wal_compression=on \
	    -c max_wal_size=1GB \
	    -c min_wal_size=128MB \
	    -c jit=off \
	    -c track_io_timing=on \
	    -c shared_preload_libraries=pg_stat_statements \
	    -c pg_stat_statements.track=all
	@echo "$(CONTROL_PG_NAME) on 127.0.0.1:$(CONTROL_PG_PORT)"

# The cache also holds GitHub webhook delivery claims, which are what stop a
# redelivered event from deploying twice. noeviction means a full cache refuses
# writes instead of silently dropping a claim, and the control plane already
# falls back to an in-process guard when a write fails. The append-only file
# keeps those claims across a restart.
redis:
	@docker start $(CONTROL_RD_NAME) >/dev/null 2>&1 || docker run -d \
	  --name $(CONTROL_RD_NAME) \
	  --restart unless-stopped \
	  --memory 320m --memory-swap 320m --cpus 1.0 \
	  --health-cmd "redis-cli ping" \
	  --health-interval 10s --health-timeout 3s --health-retries 5 \
	  -v $(CONTROL_RD_VOL):/data \
	  -p 127.0.0.1:$(CONTROL_RD_PORT):6379 \
	  redis:7-alpine \
	  redis-server \
	    --appendonly yes \
	    --appendfsync everysec \
	    --maxmemory 256mb \
	    --maxmemory-policy noeviction
	@echo "$(CONTROL_RD_NAME) on 127.0.0.1:$(CONTROL_RD_PORT)"

stores: db redis

stores-status:
	@docker ps --filter name=$(CONTROL_PG_NAME) --filter name=$(CONTROL_RD_NAME) \
	  --format '{{.Names}}\t{{.Status}}\t{{.Ports}}'

# Stops the stores without touching their volumes, so data survives.
stores-down:
	@docker stop $(CONTROL_PG_NAME) $(CONTROL_RD_NAME) >/dev/null 2>&1 || true

test:
	go test -count=1 ./...

tidy:
	go mod tidy

run: stores
	go run ./cmd/server
