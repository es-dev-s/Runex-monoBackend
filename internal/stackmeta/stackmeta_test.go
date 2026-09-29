package stackmeta

import "testing"

func TestNormalizeMergesTsx(t *testing.T) {
	if Normalize("tsx") != "TypeScript" {
		t.Fatal(Normalize("tsx"))
	}
	if Normalize("jsx") != "JavaScript" {
		t.Fatal(Normalize("jsx"))
	}
}

func TestKeepDropsNoise(t *testing.T) {
	for _, name := range []string{"Ignore List", "Dockerfile", "Dotenv", "Makefile"} {
		if Keep(name) {
			t.Fatalf("should drop %q", name)
		}
	}
	if !Keep("Go") {
		t.Fatal("should keep Go")
	}
}

func TestLabelBothForms(t *testing.T) {
	if Label("nextjs") != "Next.js" {
		t.Fatal(Label("nextjs"))
	}
	if Label("nodejs") != "Node.js" {
		t.Fatal(Label("nodejs"))
	}
}
