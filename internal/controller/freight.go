package controller

import (
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Promotion phases as reported by Kargo in .status.phase.
const (
	phaseSucceeded = "Succeeded"
	phaseFailed    = "Failed"
	phaseErrored   = "Errored"
	phaseAborted   = "Aborted"
)

// terminalPromotionPhases are the phases after which Kargo never resumes a
// Promotion.
var terminalPromotionPhases = map[string]struct{}{
	phaseSucceeded: {},
	phaseFailed:    {},
	phaseErrored:   {},
	phaseAborted:   {},
}

// parseImageRef splits an image reference into repository and tag. Digest
// references ("@") and refs without a tag are rejected. The trailing ":" only
// counts as a tag separator when it appears after the last "/", so registry
// ports ("registry:5000/repo") are not mistaken for tags.
func parseImageRef(ref string) (repo, tag string, ok bool) {
	if strings.Contains(ref, "@") {
		return "", "", false
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.LastIndex(ref, "/") > i {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

// normalizeRepoURL canonicalizes Docker Hub references so "nginx",
// "library/nginx", "docker.io/nginx", and the "index.docker.io" /
// "registry-1.docker.io" hostname aliases all compare equal as
// "docker.io/library/nginx". Non-Hub registries (first path component
// containing "." or ":", or "localhost") pass through unchanged.
// Lowercasing and digest handling are out of scope.
func normalizeRepoURL(repo string) string {
	if repo == "" {
		return ""
	}
	i := strings.Index(repo, "/")
	if i < 0 {
		return "docker.io/library/" + repo
	}
	first, rest := repo[:i], repo[i+1:]
	if first == "index.docker.io" || first == "registry-1.docker.io" {
		first = "docker.io"
	}
	if first == "docker.io" {
		if !strings.Contains(rest, "/") {
			return "docker.io/library/" + rest
		}
		return "docker.io/" + rest
	}
	if !strings.Contains(first, ".") && !strings.Contains(first, ":") && first != "localhost" {
		return "docker.io/" + repo
	}
	return repo
}

// deployedImages extracts repo->tag from an ArgoCD Application's
// .status.summary.images.
func deployedImages(app *unstructured.Unstructured) map[string]string {
	refs, _, _ := unstructured.NestedStringSlice(app.Object, "status", "summary", "images")
	images := map[string]string{}
	for _, ref := range refs {
		if repo, tag, ok := parseImageRef(ref); ok {
			images[normalizeRepoURL(repo)] = tag
		}
	}
	return images
}

// stageCurrentImages extracts repo->tag from the Stage's current Freight,
// i.e. .status.freightHistory[0].items (a map of origin ID to
// FreightReference, each carrying denormalized .images).
func stageCurrentImages(stage *unstructured.Unstructured) map[string]string {
	history, _, _ := unstructured.NestedSlice(stage.Object, "status", "freightHistory")
	if len(history) == 0 {
		return nil
	}
	entry, ok := history[0].(map[string]any)
	if !ok {
		return nil
	}
	items, _, _ := unstructured.NestedMap(entry, "items")
	images := map[string]string{}
	for _, item := range items {
		ref, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for repo, tag := range imagesFromList(ref["images"]) {
			images[repo] = tag
		}
	}
	return images
}

// imagesFromList extracts repoURL->tag from a Kargo image list
// ([]{repoURL, tag, ...}).
func imagesFromList(v any) map[string]string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	images := map[string]string{}
	for _, e := range list {
		img, ok := e.(map[string]any)
		if !ok {
			continue
		}
		repo, _ := img["repoURL"].(string)
		tag, _ := img["tag"].(string)
		if repo != "" && tag != "" {
			images[normalizeRepoURL(repo)] = tag
		}
	}
	return images
}

// stageWarehouseNames returns the Warehouse names the Stage requests Freight
// from (.spec.requestedFreight[].origin with kind Warehouse).
func stageWarehouseNames(stage *unstructured.Unstructured) []string {
	requested, _, _ := unstructured.NestedSlice(stage.Object, "spec", "requestedFreight")
	var names []string
	for _, r := range requested {
		req, ok := r.(map[string]any)
		if !ok {
			continue
		}
		kind, _, _ := unstructured.NestedString(req, "origin", "kind")
		name, _, _ := unstructured.NestedString(req, "origin", "name")
		if kind == "Warehouse" && name != "" {
			names = append(names, name)
		}
	}
	return names
}

// warehouseImageRepos returns the image repositories a Warehouse subscribes
// to (.spec.subscriptions[].image.repoURL).
func warehouseImageRepos(warehouse *unstructured.Unstructured) []string {
	subs, _, _ := unstructured.NestedSlice(warehouse.Object, "spec", "subscriptions")
	var repos []string
	for _, s := range subs {
		sub, ok := s.(map[string]any)
		if !ok {
			continue
		}
		repo, _, _ := unstructured.NestedString(sub, "image", "repoURL")
		if repo != "" {
			repos = append(repos, normalizeRepoURL(repo))
		}
	}
	return repos
}

// freightMatches reports whether the Freight originates from one of the
// given Warehouses and carries repo:tag for every drifted repository.
func freightMatches(freight *unstructured.Unstructured, warehouses map[string]struct{}, drifted map[string]string) bool {
	origin, _, _ := unstructured.NestedString(freight.Object, "origin", "name")
	if origin == "" {
		// Older Kargo versions record only a top-level warehouse name.
		origin, _, _ = unstructured.NestedString(freight.Object, "warehouse")
	}
	if _, ok := warehouses[origin]; !ok {
		return false
	}
	if len(drifted) == 0 {
		return false
	}
	images := imagesFromList(freight.Object["images"])
	for repo, tag := range drifted {
		if images[repo] != tag {
			return false
		}
	}
	return true
}

// stagePromotionSteps returns the Stage's promotion template steps
// (.spec.promotionTemplate.spec.steps). Kargo's admission webhook rejects
// Promotions whose own spec.steps is empty and does not default them from
// the Stage — copying them is the creator's job, exactly as Kargo's API
// server does for UI/CLI-created Promotions.
func stagePromotionSteps(stage *unstructured.Unstructured) []any {
	steps, _, _ := unstructured.NestedSlice(stage.Object, "spec", "promotionTemplate", "spec", "steps")
	return steps
}

// stagePromotionVars returns the Stage's variables followed by its promotion
// template's variables — the same order Kargo's own promotion builder uses,
// so task steps resolve their inputs identically.
func stagePromotionVars(stage *unstructured.Unstructured) []any {
	vars, _, _ := unstructured.NestedSlice(stage.Object, "spec", "vars")
	templateVars, _, _ := unstructured.NestedSlice(stage.Object, "spec", "promotionTemplate", "spec", "vars")
	return append(vars, templateVars...)
}

// buildPromotion assembles the Promotion the observer creates to align
// Kargo's view with what is actually deployed. steps and vars are copied
// from the Stage (see stagePromotionSteps/stagePromotionVars); the webhook
// inflates task references and stamps the Stage's shard and owner reference.
func buildPromotion(namespace, stage, freight string, steps, vars []any) *unstructured.Unstructured {
	p := &unstructured.Unstructured{}
	p.SetGroupVersionKind(promotionGVK)
	p.SetNamespace(namespace)
	p.SetGenerateName(promotionGenerateName(stage))
	p.SetLabels(map[string]string{
		managedByLabel: managedByValue,
		stageLabel:     stage,
		freightLabel:   freight,
	})
	_ = unstructured.SetNestedField(p.Object, stage, "spec", "stage")
	_ = unstructured.SetNestedField(p.Object, freight, "spec", "freight")
	_ = unstructured.SetNestedSlice(p.Object, steps, "spec", "steps")
	if len(vars) > 0 {
		_ = unstructured.SetNestedSlice(p.Object, vars, "spec", "vars")
	}
	return p
}

// promotionGenerateName truncates the stage name so the generated Promotion
// name (generateName + 5-char random suffix) stays within the 63-character
// DNS label limit.
func promotionGenerateName(stage string) string {
	const suffix = "-observer-"
	const randomSuffixLen = 5
	if maxStage := 63 - len(suffix) - randomSuffixLen; len(stage) > maxStage {
		stage = stage[:maxStage]
	}
	return stage + suffix
}

func promotionPhase(promotion *unstructured.Unstructured) string {
	phase, _, _ := unstructured.NestedString(promotion.Object, "status", "phase")
	return phase
}

func promotionPhaseTerminal(phase string) bool {
	_, ok := terminalPromotionPhases[phase]
	return ok
}

// formatDrift renders a drift map as a stable, sorted "repo:tag" list for
// events and logs.
func formatDrift(drifted map[string]string) string {
	refs := make([]string, 0, len(drifted))
	for repo, tag := range drifted {
		refs = append(refs, repo+":"+tag)
	}
	sort.Strings(refs)
	return strings.Join(refs, ", ")
}
