package alloc

import (
	"strconv"
	"strings"
	"testing"
)

func settings(args []string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(args)-1; i++ {
		if args[i] != "-c" {
			continue
		}
		if key, value, ok := strings.Cut(args[i+1], "="); ok {
			out[key] = value
		}
	}
	return out
}

func TestPostgresTuningScalesToMemory(t *testing.T) {
	if got := PostgresTuning(512)[0]; got != "postgres" {
		t.Fatalf("first argument must be the server binary, got %q", got)
	}

	small := settings(PostgresTuning(512))
	if small["shared_buffers"] != "128MB" {
		t.Fatalf("shared_buffers = %q, want a quarter of the budget", small["shared_buffers"])
	}
	if small["effective_cache_size"] != "384MB" {
		t.Fatalf("effective_cache_size = %q", small["effective_cache_size"])
	}
	// The stock image would leave these at machine-wide values, which is what
	// pushed the planner onto sequential scans inside a small container.
	if small["random_page_cost"] != "1.1" || small["jit"] != "off" {
		t.Fatalf("planner settings not applied: %v", small)
	}

	big := settings(PostgresTuning(4096))
	if big["shared_buffers"] != "1024MB" || big["effective_cache_size"] != "3072MB" {
		t.Fatalf("large node not scaled: %v", big)
	}

	// work_mem is charged per sort per connection, so it must stay small
	// enough that the connection ceiling cannot exhaust the container.
	for _, mb := range []int{64, 512, 4096} {
		got := settings(PostgresTuning(mb))
		work, err := strconv.Atoi(strings.TrimSuffix(got["work_mem"], "MB"))
		if err != nil {
			t.Fatalf("work_mem %q: %v", got["work_mem"], err)
		}
		conns, err := strconv.Atoi(got["max_connections"])
		if err != nil {
			t.Fatalf("max_connections %q: %v", got["max_connections"], err)
		}
		if work*conns > mb*8 {
			t.Fatalf("at %dMB, %d connections x %dMB work_mem is unsafe", mb, conns, work)
		}
	}
}

func TestPostgresTuningHandlesTinyNodes(t *testing.T) {
	got := settings(PostgresTuning(0))
	if got["shared_buffers"] != "32MB" {
		t.Fatalf("a zero budget must fall back to the minimum, got %q", got["shared_buffers"])
	}
}

func TestCPUsFavoursDatabases(t *testing.T) {
	if CPUs("service", 512) != "0.5" {
		t.Fatalf("service cpus = %q", CPUs("service", 512))
	}
	if CPUs("postgresql", 512) != "2.0" {
		t.Fatalf("postgres cpus = %q", CPUs("postgresql", 512))
	}
	if CPUs("postgresql", 64) != "1.0" {
		t.Fatalf("a small database still needs a whole core, got %q", CPUs("postgresql", 64))
	}
	if CPUs("postgresql", 99999) != "4.0" {
		t.Fatalf("postgres cpus must stay capped, got %q", CPUs("postgresql", 99999))
	}
	if CPUs("redis", 4096) != "2.0" {
		t.Fatalf("redis is single threaded, got %q", CPUs("redis", 4096))
	}
}

func TestDefaultsByKind(t *testing.T) {
	if DefaultMemoryMB("redis") != 128 {
		t.Fatalf("redis ram")
	}
	if DefaultDiskMB("postgresql") != 2048 {
		t.Fatalf("postgres disk")
	}
	if DefaultMemoryMB("service") != 512 {
		t.Fatalf("service ram")
	}
}

func TestResolveClampsAndFills(t *testing.T) {
	mem, disk := Resolve("redis", 0, 0, 4096, 20480)
	if mem != 128 || disk != 512 {
		t.Fatalf("got mem=%d disk=%d", mem, disk)
	}
	mem, disk = Resolve("service", 8, 9_999_999, 4096, 20480)
	if mem != MinMemoryMB {
		t.Fatalf("min ram %d", mem)
	}
	if disk != 20480 {
		t.Fatalf("max disk %d", disk)
	}
}

func TestDockerAndRedisFormats(t *testing.T) {
	if DockerMemory(512) != "512m" {
		t.Fatal(DockerMemory(512))
	}
	if RedisMaxMemory(128) != "115mb" {
		t.Fatal(RedisMaxMemory(128))
	}
	if StorageOpt(2048) != "2G" {
		t.Fatal(StorageOpt(2048))
	}
	if StorageOpt(1500) != "2G" {
		t.Fatal(StorageOpt(1500))
	}
}
