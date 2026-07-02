package controller

import (
	"reflect"
	"strings"
	"testing"

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

func TestPromotionGenerateName(t *testing.T) {
	if got := promotionGenerateName(testStage); got != testStage+"-observer-" {
		t.Errorf("promotionGenerateName(%q) = %q, want %q", testStage, got, testStage+"-observer-")
	}

	long := strings.Repeat("a", 80)
	got := promotionGenerateName(long)
	if !strings.HasSuffix(got, "-observer-") {
		t.Errorf("promotionGenerateName(long) = %q, missing -observer- suffix", got)
	}
	// The generated name must stay a valid DNS label with the 5-char random
	// suffix the API server appends.
	if len(got)+5 > 63 {
		t.Errorf("promotionGenerateName(long) = %q (len %d), exceeds 63 chars with random suffix", got, len(got))
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
