package controller

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestParseImageRef(t *testing.T) {
	tests := []struct {
		ref      string
		wantRepo string
		wantTag  string
		wantOK   bool
	}{
		{"nginx:1.27", "nginx", "1.27", true},
		{"registry.example.com/acme/web:1.2.3", "registry.example.com/acme/web", "1.2.3", true},
		{"registry:5000/app:v1", "registry:5000/app", "v1", true},
		{"registry:5000/app", "", "", false},
		{"app@sha256:deadbeef", "", "", false},
		{"app", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range tests {
		repo, tag, ok := parseImageRef(tc.ref)
		if repo != tc.wantRepo || tag != tc.wantTag || ok != tc.wantOK {
			t.Errorf("parseImageRef(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.ref, repo, tag, ok, tc.wantRepo, tc.wantTag, tc.wantOK)
		}
	}
}

func TestPromotionName(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	name := promotionName(testStage, freightName, now)

	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(testStage) + `\.[0-9a-z]{26}\.` + regexp.QuoteMeta(freightName[:7]) + `$`)
	if !pattern.MatchString(name) {
		t.Errorf("promotionName = %q, want match for %s", name, pattern)
	}

	// Kargo's stage controller only records promotions whose names sort
	// lexically AFTER status.lastPromotion.name (its names embed a
	// time-ordered ULID) — a name generated now must sort after a real
	// Kargo-generated name from the past.
	past := "qa-ox.01ktpewnchktjpafjxx7g5f152.15fa47a"
	if got := promotionName("qa-ox", "15fa47acb13935bf9d820f8dc17ece41fc0ec053", now); got <= past {
		t.Errorf("promotionName %q does not sort after past Kargo name %q", got, past)
	}

	// Later timestamps must sort after earlier ones.
	if later := promotionName(testStage, freightName, now.Add(time.Second)); later <= name {
		t.Errorf("later name %q does not sort after earlier %q", later, name)
	}

	// Long stage names are truncated to keep the whole name within the
	// 253-char Kubernetes resource name limit, mirroring Kargo's builder.
	if long := promotionName(strings.Repeat("a", 300), freightName, now); len(long) != 253 {
		t.Errorf("promotionName(long stage) length = %d, want 253", len(long))
	}

	// Freight names shorter than the hash length are used as-is.
	if short := promotionName(testStage, "abc", now); !strings.HasSuffix(short, ".abc") {
		t.Errorf("promotionName with short freight = %q, want .abc suffix", short)
	}
}

func TestStagePromotionStepsAndVars(t *testing.T) {
	stage := testStageObj(currentTag, false)
	steps := stagePromotionSteps(stage)
	if len(steps) != 1 {
		t.Fatalf("stagePromotionSteps len = %d, want 1", len(steps))
	}
	vars := stagePromotionVars(stage)
	if len(vars) != 2 {
		t.Fatalf("stagePromotionVars len = %d, want 2", len(vars))
	}
	first, _ := vars[0].(map[string]any)
	if first["name"] != "env" {
		t.Errorf("vars[0].name = %v, want env (stage vars before template vars)", first["name"])
	}

	unstructured.RemoveNestedField(stage.Object, "spec", "promotionTemplate")
	unstructured.RemoveNestedField(stage.Object, "spec", "vars")
	if got := stagePromotionSteps(stage); len(got) != 0 {
		t.Errorf("stagePromotionSteps on bare stage = %v, want empty", got)
	}
	if got := stagePromotionVars(stage); len(got) != 0 {
		t.Errorf("stagePromotionVars on bare stage = %v, want empty", got)
	}
}

func TestBuildPromotion(t *testing.T) {
	steps := []any{map[string]any{"uses": "fake-step"}}
	vars := []any{map[string]any{"name": "env", "value": "qa"}}

	p := buildPromotion("ns", "stg", "fr-1", steps, vars)
	gotSteps, _, _ := unstructured.NestedSlice(p.Object, "spec", "steps")
	if len(gotSteps) != 1 {
		t.Fatalf("spec.steps len = %d, want 1", len(gotSteps))
	}
	gotVars, _, _ := unstructured.NestedSlice(p.Object, "spec", "vars")
	if len(gotVars) != 1 {
		t.Fatalf("spec.vars len = %d, want 1", len(gotVars))
	}
	if p.GetName() == "" || p.GetGenerateName() != "" {
		t.Errorf("promotion must carry an explicit Kargo-style name, got name=%q generateName=%q",
			p.GetName(), p.GetGenerateName())
	}

	p = buildPromotion("ns", "stg", "fr-1", steps, nil)
	if _, found, _ := unstructured.NestedSlice(p.Object, "spec", "vars"); found {
		t.Error("spec.vars should be absent when the stage defines no vars")
	}
}

func TestNormalizeRepoURL(t *testing.T) {
	tests := []struct {
		repo string
		want string
	}{
		{"nginx", "docker.io/library/nginx"},
		{"library/nginx", "docker.io/library/nginx"},
		{"docker.io/nginx", "docker.io/library/nginx"},
		{"docker.io/library/nginx", "docker.io/library/nginx"},
		{"index.docker.io/nginx", "docker.io/library/nginx"},
		{"registry-1.docker.io/nginx", "docker.io/library/nginx"},
		{"registry-1.docker.io/acme/web", "docker.io/acme/web"},
		{"acme/web", "docker.io/acme/web"},
		{"docker.io/acme/web", "docker.io/acme/web"},
		{"registry.example.com/app", "registry.example.com/app"},
		{"registry.example.com/acme/web", "registry.example.com/acme/web"},
		{"localhost/app", "localhost/app"},
		{"localhost:5000/app", "localhost:5000/app"},
		{"registry:5000/app", "registry:5000/app"},
		{"ghcr.io/org/app", "ghcr.io/org/app"},
		{"quay.io/org/sub/app", "quay.io/org/sub/app"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := normalizeRepoURL(tc.repo); got != tc.want {
			t.Errorf("normalizeRepoURL(%q) = %q, want %q", tc.repo, got, tc.want)
		}
	}
}

func TestDeployedImages(t *testing.T) {
	app := testApp(nil, []string{
		testRepo + ":" + deployedTag,
		"nginx:1.27",
		"registry:5000/no-tag",
		"pinned@sha256:deadbeef",
	})
	got := deployedImages(app)
	want := map[string]string{
		testRepo:                  deployedTag,
		"docker.io/library/nginx": "1.27",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deployedImages() = %v, want %v", got, want)
	}
}

func TestStageCurrentImages(t *testing.T) {
	got := stageCurrentImages(testStageObj(currentTag, false))
	want := map[string]string{testRepo: currentTag}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stageCurrentImages() = %v, want %v", got, want)
	}

	empty := &unstructured.Unstructured{}
	empty.SetGroupVersionKind(stageGVK)
	if got := stageCurrentImages(empty); len(got) != 0 {
		t.Errorf("stageCurrentImages(empty) = %v, want empty", got)
	}
}

func TestStageWarehouseNames(t *testing.T) {
	got := stageWarehouseNames(testStageObj(currentTag, false))
	want := []string{warehouseName}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stageWarehouseNames() = %v, want %v", got, want)
	}
}

func TestWarehouseImageRepos(t *testing.T) {
	got := warehouseImageRepos(testWarehouse())
	want := []string{testRepo}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("warehouseImageRepos() = %v, want %v", got, want)
	}
}

func TestFreightMatches(t *testing.T) {
	warehouses := map[string]struct{}{warehouseName: {}}
	drift := map[string]string{testRepo: deployedTag}

	if !freightMatches(testFreight("f1", deployedTag), warehouses, drift) {
		t.Error("freightMatches() = false for matching origin and images, want true")
	}
	if freightMatches(testFreight("f2", currentTag), warehouses, drift) {
		t.Error("freightMatches() = true for wrong tag, want false")
	}

	legacy := testFreight("f3", deployedTag)
	unstructured.RemoveNestedField(legacy.Object, "origin")
	_ = unstructured.SetNestedField(legacy.Object, warehouseName, "warehouse")
	if !freightMatches(legacy, warehouses, drift) {
		t.Error("freightMatches() = false for legacy warehouse field, want true")
	}

	foreign := testFreight("f4", deployedTag)
	_ = unstructured.SetNestedField(foreign.Object, "other-warehouse", "origin", "name")
	if freightMatches(foreign, warehouses, drift) {
		t.Error("freightMatches() = true for foreign warehouse, want false")
	}

	if freightMatches(testFreight("f5", deployedTag), warehouses, nil) {
		t.Error("freightMatches() = true for empty drift, want false")
	}

	hubFreight := &unstructured.Unstructured{}
	hubFreight.SetGroupVersionKind(freightGVK)
	hubFreight.SetNamespace(testNS)
	hubFreight.SetName("f-hub")
	_ = unstructured.SetNestedMap(hubFreight.Object,
		map[string]any{"kind": "Warehouse", "name": warehouseName}, "origin")
	_ = unstructured.SetNestedSlice(hubFreight.Object, []any{
		map[string]any{"repoURL": "docker.io/nginx", "tag": "1.27"},
	}, "images")
	hubDrift := map[string]string{"docker.io/library/nginx": "1.27"}
	if !freightMatches(hubFreight, warehouses, hubDrift) {
		t.Error("freightMatches() = false for cross-spelled Docker Hub repo, want true")
	}
}

func TestFormatDrift(t *testing.T) {
	got := formatDrift(map[string]string{
		"b.example.com/app": "2",
		"a.example.com/app": "1",
	})
	want := "a.example.com/app:1, b.example.com/app:2"
	if got != want {
		t.Errorf("formatDrift() = %q, want %q", got, want)
	}
}

func TestParseAuthorizedStage(t *testing.T) {
	tests := []struct {
		value  string
		wantNS string
		wantN  string
		wantOK bool
	}{
		{"kargo-demo:qa", "kargo-demo", "qa", true},
		{"no-colon", "", "", false},
		{":stage-only", "", "", false},
		{"ns-only:", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range tests {
		ns, name, ok := parseAuthorizedStage(tc.value)
		if ns != tc.wantNS || name != tc.wantN || ok != tc.wantOK {
			t.Errorf("parseAuthorizedStage(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.value, ns, name, ok, tc.wantNS, tc.wantN, tc.wantOK)
		}
	}
}
