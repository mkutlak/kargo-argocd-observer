package controller

import (
	"crypto/rand"
	"math/big"
	"slices"
	"sort"
	"strings"
	"time"

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

// deployedImageTags extracts repo->distinct tags from an ArgoCD Application's
// .status.summary.images.
func deployedImageTags(app *unstructured.Unstructured) map[string][]string {
	refs, _, _ := unstructured.NestedStringSlice(app.Object, "status", "summary", "images")
	tags := map[string][]string{}
	for _, ref := range refs {
		repo, tag, ok := parseImageRef(ref)
		if !ok {
			continue
		}
		repo = normalizeRepoURL(repo)
		if !slices.Contains(tags[repo], tag) {
			tags[repo] = append(tags[repo], tag)
		}
	}
	return tags
}

// deployedImages extracts repo->tag from an ArgoCD Application's
// .status.summary.images. ArgoCD lists the images of live Pods, so a rollout
// shows the outgoing and the incoming tag side by side; a repository seen
// with more than one tag is left out until it settles on one.
func deployedImages(app *unstructured.Unstructured) map[string]string {
	images := map[string]string{}
	for repo, tags := range deployedImageTags(app) {
		if len(tags) == 1 {
			images[repo] = tags[0]
		}
	}
	return images
}

// stageCurrentFreight returns the Stage's current Freight,
// .status.freightHistory[0].items: a map of origin ID ("Warehouse/<name>")
// to FreightReference, each carrying denormalized .images.
func stageCurrentFreight(stage *unstructured.Unstructured) map[string]any {
	history, _, _ := unstructured.NestedSlice(stage.Object, "status", "freightHistory")
	if len(history) == 0 {
		return nil
	}
	entry, ok := history[0].(map[string]any)
	if !ok {
		return nil
	}
	items, _, _ := unstructured.NestedMap(entry, "items")
	return items
}

// stageCurrentImages extracts repo->tags from the Stage's current Freight.
// A repository has one tag per origin that carries it.
func stageCurrentImages(stage *unstructured.Unstructured) map[string][]string {
	images := map[string][]string{}
	for _, item := range stageCurrentFreight(stage) {
		ref, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for repo, tag := range imagesFromList(ref["images"]) {
			images[repo] = append(images[repo], tag)
		}
	}
	return images
}

// stageHeldImages extracts the repo->tags that promoting a Freight of the
// given Warehouse must leave alone: the tag the Stage holds from that
// Warehouse — the one the Freight would replace — or, for a repository that
// Warehouse holds no tag for, the tags held from the other origins.
func stageHeldImages(stage *unstructured.Unstructured, warehouse string) map[string][]string {
	held := stageCurrentImages(stage)
	ref, _ := stageCurrentFreight(stage)["Warehouse/"+warehouse].(map[string]any)
	for repo, tag := range imagesFromList(ref["images"]) {
		held[repo] = []string{tag}
	}
	return held
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

// freightOrigin returns the name of the Warehouse the Freight came from.
func freightOrigin(freight *unstructured.Unstructured) string {
	origin, _, _ := unstructured.NestedString(freight.Object, "origin", "name")
	if origin == "" {
		// Older Kargo versions record only a top-level warehouse name.
		origin, _, _ = unstructured.NestedString(freight.Object, "warehouse")
	}
	return origin
}

// freightMatches reports whether promoting the Freight would align the Stage
// with the Application without moving any other image: it originates from
// one of the given Warehouses, every image it carries is at the deployed tag
// (or, for a repository the Application does not deploy, at a tag in held —
// see stageHeldImages), and at least one is a drifted repo:tag. Commits and
// charts the Freight carries are not compared.
func freightMatches(freight *unstructured.Unstructured, warehouses map[string]struct{}, deployed, drifted map[string]string, held map[string][]string) bool {
	if _, ok := warehouses[freightOrigin(freight)]; !ok {
		return false
	}
	fixesDrift := false
	for repo, tag := range imagesFromList(freight.Object["images"]) {
		if want, ok := deployed[repo]; ok {
			if tag != want {
				return false
			}
		} else if tags := held[repo]; len(tags) > 0 && !slices.Contains(tags, tag) {
			return false
		}
		fixesDrift = fixesDrift || drifted[repo] == tag
	}
	return fixesDrift
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

// stageIsTargetAware reports whether the Stage selects Targets (spec.targets,
// Kargo v1.12+). Kargo's webhook rejects Promotions created directly against
// such a Stage; it is promoted only through a PromotionRequest.
func stageIsTargetAware(stage *unstructured.Unstructured) bool {
	targets, found, _ := unstructured.NestedFieldNoCopy(stage.Object, "spec", "targets")
	return found && targets != nil
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
	p.SetName(promotionName(stage, freight, time.Now()))
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

// Kargo promotion-name layout, mirrored from Kargo's own promotion builder.
// The stage controller records a finished Promotion into freight history only
// when its name sorts lexically AFTER status.lastPromotion.name — an ordering
// Kargo guarantees via the time-ordered ULID embedded in generated names. Any
// other scheme (e.g. generateName with a random suffix) makes Kargo silently
// ignore succeeded Promotions, and the observer would keep re-creating them.
const (
	promotionNameSeparator = "."
	ulidLength             = 26
	freightHashLength      = 7
	maxStageNameLength     = 253 - 2*len(promotionNameSeparator) - ulidLength - freightHashLength
)

// promotionName renders <stage>.<ulid>.<freight-prefix> in lowercase, capped
// at the 253-char Kubernetes resource name limit.
func promotionName(stage, freight string, now time.Time) string {
	if len(freight) > freightHashLength {
		freight = freight[:freightHashLength]
	}
	if len(stage) > maxStageNameLength {
		stage = stage[:maxStageNameLength]
	}
	return strings.ToLower(stage + promotionNameSeparator + newULID(now) + promotionNameSeparator + freight)
}

// newULID renders a ULID (48-bit millisecond timestamp + 80 random bits) in
// Crockford base32: 26 chars whose lexical order equals creation-time order.
func newULID(now time.Time) string {
	const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	var b [16]byte
	ms := uint64(now.UnixMilli()) //nolint:gosec // non-negative for any realistic clock
	b[0], b[1], b[2] = byte(ms>>40), byte(ms>>32), byte(ms>>24)
	b[3], b[4], b[5] = byte(ms>>16), byte(ms>>8), byte(ms)
	// crypto/rand.Read is documented never to fail.
	_, _ = rand.Read(b[6:])
	n := new(big.Int).SetBytes(b[:])
	base := big.NewInt(int64(len(alphabet)))
	mod := new(big.Int)
	out := make([]byte, ulidLength)
	for i := ulidLength - 1; i >= 0; i-- {
		n.DivMod(n, base, mod)
		out[i] = alphabet[mod.Int64()]
	}
	return string(out)
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
