package runtime

import (
	"strings"
	"testing"
)

func flagValues(args []string, flag string) []string {
	var out []string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			out = append(out, args[i+1])
		}
	}
	return out
}

func has(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// --init puts tini at pid 1, and tini must forward SIGTERM to a child that has
// dropped to an unprivileged user. Signalling across uids needs CAP_KILL: with
// ALL dropped and KILL missing, tini aborted on every stop and the container
// died instead of shutting down, which left Postgres to run crash recovery.
func TestContainersCanForwardStopSignals(t *testing.T) {
	sidecar := sidecarArgs(SidecarOpts{
		Name: "db", Image: "postgres:16-alpine", Network: "net", Alias: "pg", Memory: "512m",
	})
	if !has(sidecar, "--init") {
		t.Fatal("expected sidecars to keep --init")
	}
	if !has(flagValues(sidecar, "--cap-add"), "KILL") {
		t.Fatal("a sidecar running tini cannot forward SIGTERM without CAP_KILL")
	}
	if !has(flagValues(sidecar, "--cap-drop"), "ALL") {
		t.Fatal("sidecars must still drop every other capability")
	}

	app := appArgs(RunOpts{
		Name: "app", Image: "img", Network: "net", Memory: "512m", CPUs: "0.5",
	}, false)
	if !has(flagValues(app, "--cap-add"), "KILL") {
		t.Fatal("an app running tini cannot forward SIGTERM without CAP_KILL")
	}
	if !has(flagValues(app, "--cap-drop"), "ALL") {
		t.Fatal("apps must still drop every other capability")
	}
}

// Blue/green needs each attempt to own a distinct container name so the new
// version can boot alongside the one that is still serving traffic.
func TestReleaseNameIsUniquePerDeploy(t *testing.T) {
	project, service := "prj_abc", "svc_api"
	first := ReleaseName(project, service, "dep_111")
	second := ReleaseName(project, service, "dep_222")
	if first == second {
		t.Fatal("two deploys of one service must not share a container name")
	}
	if !strings.HasPrefix(first, AppName(project, service)+"-") {
		t.Fatalf("release name %q must stay under the service prefix so label cleanup finds it", first)
	}
	if other := ReleaseName(project, "svc_web", "dep_111"); other == first {
		t.Fatal("two services must not collide on the same release id")
	}
}

// Docker rejects names above 128 chars; a long deployment id must not be able
// to produce an unrunnable container.
func TestReleaseNameBoundsLength(t *testing.T) {
	name := ReleaseName("prj_abc", "svc_api", strings.Repeat("x", 200))
	if len(name) > 128 {
		t.Fatalf("release name is %d chars", len(name))
	}
}

func TestReleaseNameFallsBackWhenReleaseEmpty(t *testing.T) {
	if got := ReleaseName("prj_abc", "svc_api", "   "); got != AppName("prj_abc", "svc_api") {
		t.Fatalf("empty release should reuse the canonical name, got %q", got)
	}
}

func TestServiceNamesStayIsolated(t *testing.T) {
	project := "prj_abc"
	first := "service:" + project
	second := "svc_other"
	if AppName(project, first) == AppName(project, second) {
		t.Fatal("two services must not share a container name")
	}
	if ImageName(project, first) == ImageName(project, second) {
		t.Fatal("two services must not share an image tag")
	}
	if NetworkAlias(project, first) == NetworkAlias(project, second) {
		t.Fatal("two services must not share a network alias")
	}
	if AppName(project, first) == "deploy-"+project+"-app" {
		t.Fatal("legacy shared -app name must not be the current name")
	}
}

func TestAppNamesIncludeLegacyPrimary(t *testing.T) {
	project := "prj_abc"
	names := AppNames(project, "service:"+project, nil)
	want := "deploy-" + project + "-app"
	for _, name := range names {
		if name == want {
			return
		}
	}
	t.Fatalf("expected legacy name %s in %v", want, names)
}

func TestProjectImagesStayScoped(t *testing.T) {
	id := "prj_abc"
	if !IsProjectImage("deploy-prj_abc-svc-1:latest", id) {
		t.Fatal("service image must be purged")
	}
	if IsProjectImage("postgres:16-alpine", id) {
		t.Fatal("shared postgres image must stay")
	}
	if IsProjectImage("deploy-prj_other-svc-1:latest", id) {
		t.Fatal("another project's image must stay")
	}
}

func TestSidecarNamesStayIsolated(t *testing.T) {
	if PostgresName("prj", "nod_a") == PostgresName("prj", "nod_b") {
		t.Fatal("postgres sidecars must not share a container name")
	}
	if RedisName("prj", "nod_a") == RedisName("prj", "nod_b") {
		t.Fatal("redis sidecars must not share a container name")
	}
	if SidecarAlias("postgresql", "nod_a", 0) != "postgres" {
		t.Fatal("first postgres keeps the postgres alias")
	}
	if SidecarAlias("postgresql", "nod_b", 1) == "postgres" {
		t.Fatal("second postgres must not steal the postgres alias")
	}
}
