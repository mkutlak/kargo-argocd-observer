// Package controller reconciles ArgoCD Applications against Kargo Stages:
// when the deployed image tags diverge from the Stage's current Freight
// (e.g. after a direct git commit), it finds the Freight matching the
// deployed tags and creates a Promotion so Kargo's view matches reality.
package controller

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// authorizedStageAnnotation is stamped by Kargo on Applications it manages;
	// its value is "<namespace>:<stage>".
	authorizedStageAnnotation = "kargo.akuity.io/authorized-stage"
	// ignoreAnnotation opts an Application out of observation when set to "true".
	ignoreAnnotation = "kargo-observer.kutlak.cc/ignore"
	// observeAnnotation opts an Application into observation in opt-in mode
	// when set to "true"; see ObserveModeOptIn.
	observeAnnotation = "kargo-observer.kutlak.cc/observe"
	// applicationsAnnotation, on a Stage, lists the Applications allowed to
	// drive it as comma-separated "<namespace>/<name>"; only consulted with
	// RequireStageConsent.
	applicationsAnnotation = "kargo-observer.kutlak.cc/applications"

	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "kargo-argocd-observer"
	stageLabel     = "kargo-observer.kutlak.cc/stage"
	freightLabel   = "kargo-observer.kutlak.cc/freight"
)

// Observe mode values for ApplicationReconciler.ObserveMode / --observe-mode.
const (
	// ObserveModeOptOut observes every Application carrying the
	// authorized-stage annotation unless it is ignored (the default).
	ObserveModeOptOut = "opt-out"
	// ObserveModeOptIn additionally requires the observe annotation to be
	// "true" before an Application is observed.
	ObserveModeOptIn = "opt-in"
)

// ParseObserveMode validates and normalizes an --observe-mode flag value.
// The empty string maps to ObserveModeOptOut so the ApplicationReconciler{}
// zero value keeps existing behavior.
func ParseObserveMode(s string) (string, error) {
	switch s {
	case "", ObserveModeOptOut:
		return ObserveModeOptOut, nil
	case ObserveModeOptIn:
		return ObserveModeOptIn, nil
	default:
		return "", fmt.Errorf("invalid observe mode %q: must be %q or %q", s, ObserveModeOptOut, ObserveModeOptIn)
	}
}

var (
	applicationGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}

	applicationListGVK = applicationGVK.GroupVersion().WithKind("ApplicationList")

	stageGVK         = schema.GroupVersionKind{Group: "kargo.akuity.io", Version: "v1alpha1", Kind: "Stage"}
	freightGVK       = stageGVK.GroupVersion().WithKind("Freight")
	freightListGVK   = stageGVK.GroupVersion().WithKind("FreightList")
	promotionGVK     = stageGVK.GroupVersion().WithKind("Promotion")
	promotionListGVK = stageGVK.GroupVersion().WithKind("PromotionList")
	warehouseGVK     = stageGVK.GroupVersion().WithKind("Warehouse")
)

// ApplicationReconciler watches ArgoCD Applications and creates Kargo
// Promotions when the deployed images drift from the Stage's current Freight.
type ApplicationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	DryRun   bool
	// ObserveMode is one of ObserveModeOptOut (default) or ObserveModeOptIn;
	// see shouldObserve.
	ObserveMode string
	// RequireStageConsent makes the Application's authorized-stage annotation
	// insufficient on its own: the Stage must also name the Application in
	// applicationsAnnotation.
	RequireStageConsent bool

	// appReader lists Applications from the informer cache; unset (tests) it
	// falls back to the live client.
	appReader client.Reader
	missing   freightMissingTracker
}

// freightMissingTracker folds the per-Application FreightMissing verdicts
// into the per-Stage kargo_observer_freight_missing gauge, so Applications
// sharing a Stage don't overwrite each other and a Stage nobody reports on
// any more loses its series.
type freightMissingTracker struct {
	mu     sync.Mutex
	stages map[types.NamespacedName]map[types.NamespacedName]bool
}

// set records app's verdict for stage and drops it from any other Stage.
func (t *freightMissingTracker) set(app, stage types.NamespacedName, missing bool) {
	t.update(app, &stage, missing)
}

// forget drops an Application that is gone or no longer observed.
func (t *freightMissingTracker) forget(app types.NamespacedName) {
	t.update(app, nil, false)
}

func (t *freightMissingTracker) update(app types.NamespacedName, stage *types.NamespacedName, missing bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if stage != nil {
		if t.stages == nil {
			t.stages = map[types.NamespacedName]map[types.NamespacedName]bool{}
		}
		if t.stages[*stage] == nil {
			t.stages[*stage] = map[types.NamespacedName]bool{}
		}
		t.stages[*stage][app] = missing
	}
	for key, apps := range t.stages {
		if stage == nil || key != *stage {
			if _, reported := apps[app]; !reported {
				continue
			}
			delete(apps, app)
		}
		if len(apps) == 0 {
			delete(t.stages, key)
			freightMissing.DeleteLabelValues(key.Namespace, key.Name)
			continue
		}
		value := 0.0
		for _, m := range apps {
			if m {
				value = 1
			}
		}
		freightMissing.WithLabelValues(key.Namespace, key.Name).Set(value)
	}
}

// settlePeriod is how long after a Stage's last promotion finished the
// observer waits before acting on drift: ArgoCD may still report the images
// of the previous revision, and promoting on that would undo the promotion.
const settlePeriod = 5 * time.Minute

// shouldObserve reports whether an Application with these annotations is
// observed: ignore always wins; opt-in mode also requires the observe
// annotation to be "true".
func (r *ApplicationReconciler) shouldObserve(annotations map[string]string) bool {
	if annotations[ignoreAnnotation] == "true" {
		return false
	}
	if r.ObserveMode == ObserveModeOptIn {
		return annotations[observeAnnotation] == "true"
	}
	return true
}

// Reconcile compares the Application's deployed images with its Kargo Stage's
// current Freight and creates a Promotion when they diverge.
func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, err error) {
	log := logf.FromContext(ctx)

	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(applicationGVK)
	if err := r.Get(ctx, req.NamespacedName, app); err != nil {
		if apierrors.IsNotFound(err) {
			r.missing.forget(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	annotations := app.GetAnnotations()
	if !r.shouldObserve(annotations) {
		r.missing.forget(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	stageNS, stageName, ok := parseAuthorizedStage(annotations[authorizedStageAnnotation])
	if !ok {
		log.V(1).Info("missing or malformed authorized-stage annotation",
			"annotation", annotations[authorizedStageAnnotation])
		r.missing.forget(req.NamespacedName)
		return ctrl.Result{}, nil
	}
	log = log.WithValues("stageNamespace", stageNS, "stage", stageName)

	stage := &unstructured.Unstructured{}
	stage.SetGroupVersionKind(stageGVK)
	if err := r.Get(ctx, types.NamespacedName{Namespace: stageNS, Name: stageName}, stage); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("stage not found, skipping application")
			r.missing.forget(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !r.stageConsents(stage, req.NamespacedName) {
		log.Info("stage does not list this application in its " + applicationsAnnotation + " annotation, skipping")
		r.missing.forget(req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// Every return below that reaches a verdict publishes it to the
	// freight-missing gauge; waits and errors leave the last verdict standing.
	missing, waiting := false, false
	defer func() {
		if err == nil && !waiting {
			r.missing.set(req.NamespacedName, types.NamespacedName{Namespace: stageNS, Name: stageName}, missing)
		}
	}()

	repos, err := r.stageImageRepos(ctx, stage)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Expected during a rollout; logged because a Pod that never goes away
	// (a finished Job, an evicted Pod) keeps the repository ignored for good.
	for repo, tags := range deployedImageTags(app) {
		if len(tags) > 1 && slices.Contains(repos, repo) {
			log.Info("repository is deployed with several tags, ignoring it until one remains", "repo", repo, "tags", tags)
		}
	}
	deployed := deployedImages(app)

	current := stageCurrentImages(stage)
	drifted := map[string]string{}
	for _, repo := range repos {
		if tag := deployed[repo]; tag != "" && !slices.Contains(current[repo], tag) {
			drifted[repo] = tag
		}
	}
	if len(drifted) == 0 {
		return ctrl.Result{}, nil
	}
	// Drift alone is V(1): the same divergence is re-detected on every
	// reconcile until the Stage records the promotion. Action paths below
	// (create / dry-run / events) carry the drift at Info level.
	log.V(1).Info("deployed images diverge from stage's current freight", "drift", formatDrift(drifted))

	// Unless the Application is Synced, the cluster is not running what git
	// holds — typically a Kargo promotion ArgoCD has not applied yet (sync
	// window, manual or failed sync). Promoting to the images still running
	// would undo that promotion.
	if sync := applicationSyncStatus(app); sync != "" && sync != syncStatusSynced {
		log.Info("application is not Synced, its images may not be what git holds; skipping",
			"syncStatus", sync, "drift", formatDrift(drifted))
		waiting = true
		return ctrl.Result{}, nil
	}

	// Kargo v1.12+ rejects direct Promotions against a target-aware Stage;
	// only a PromotionRequest may promote it. Checked before steps: Kargo
	// doesn't require a promotionTemplate on such a Stage.
	if stageIsTargetAware(stage) {
		r.Recorder.Eventf(stage, corev1.EventTypeWarning, "StageIsTargetAware",
			"stage selects targets (spec.targets) and Kargo accepts only PromotionRequest-driven Promotions for it; cannot align it with deployed images %s",
			formatDrift(drifted))
		log.Info("stage is target-aware, skipping")
		return ctrl.Result{}, nil
	}

	// Kargo cannot promote to a Stage without promotion template steps; the
	// admission webhook would reject every Promotion the observer creates.
	steps := stagePromotionSteps(stage)
	if len(steps) == 0 {
		r.Recorder.Eventf(stage, corev1.EventTypeWarning, "StageHasNoPromotionSteps",
			"stage defines no promotion template steps; cannot create a Promotion to align it with deployed images %s",
			formatDrift(drifted))
		log.Info("stage has no promotion template steps, skipping")
		return ctrl.Result{}, nil
	}

	// Never race an in-flight promotion, whoever created it.
	if running, found, _ := unstructured.NestedMap(stage.Object, "status", "currentPromotion"); found && len(running) > 0 {
		log.V(1).Info("stage has a promotion in progress, requeueing")
		waiting = true
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if wait := settlePeriod - time.Since(lastPromotionFinishedAt(stage)); wait > 0 {
		log.V(1).Info("stage finished a promotion recently, waiting for the application to settle")
		waiting = true
		return ctrl.Result{RequeueAfter: min(wait, settlePeriod)}, nil
	}
	latest, inFlight, err := r.stagePromotions(ctx, stageNS, stageName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if inFlight != "" {
		log.V(1).Info("promotion still in flight, requeueing", "promotion", inFlight)
		waiting = true
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Promoting for one Application while another deploys a different tag
	// would have that one's reconcile promote the Stage straight back.
	disagreement, err := r.disagreement(ctx, stage, req.NamespacedName, drifted)
	if err != nil {
		return ctrl.Result{}, err
	}
	if disagreement != "" {
		r.Recorder.Eventf(stage, corev1.EventTypeWarning, "ApplicationsDisagree",
			"application %s deploys %s but %s; not promoting until the stage's applications agree",
			req.NamespacedName, formatDrift(drifted), disagreement)
		log.Info("applications of the stage deploy different tags, skipping", "drift", formatDrift(drifted), "other", disagreement)
		waiting = true // no Freight lookup happened
		return ctrl.Result{}, nil
	}

	target, err := r.findFreight(ctx, stage, deployed, drifted)
	if err != nil {
		return ctrl.Result{}, err
	}
	if target == nil {
		r.Recorder.Eventf(stage, corev1.EventTypeWarning, "FreightMissing",
			"no Freight matches deployed images %s", formatDrift(drifted))
		missing = true
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	if latest != nil && latest.GetLabels()[freightLabel] == target.GetName() && !promotionSuperseded(stage, latest) {
		switch phase := promotionPhase(latest); phase {
		case phaseSucceeded:
			// Kargo already accepted this exact Freight; the Stage's freight
			// history just hasn't recorded the result yet. Creating another
			// Promotion would duplicate it — hold off and let the Stage
			// catch up.
			log.V(1).Info("promotion for this freight already succeeded, waiting for the stage to record it",
				"promotion", latest.GetName())
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		case phaseFailed, phaseErrored, phaseAborted:
			// A failed or aborted promotion for the same freight is a human
			// problem: recreating it would just end the same way. It must be
			// deleted to retry.
			r.Recorder.Eventf(stage, corev1.EventTypeWarning, "PromotionPreviouslyFailed",
				"promotion %s for freight %s ended in phase %s; delete it to let the observer retry",
				latest.GetName(), target.GetName(), phase)
			return ctrl.Result{}, nil
		}
	}

	if r.DryRun {
		r.Recorder.Eventf(stage, corev1.EventTypeNormal, "DryRunPromotionSkipped",
			"dry-run: would promote freight %s (%s)", target.GetName(), formatDrift(drifted))
		log.Info("dry-run: would create promotion", "freight", target.GetName(), "drift", formatDrift(drifted))
		return ctrl.Result{}, nil
	}

	promotion := buildPromotion(stageNS, stageName, target.GetName(), steps, stagePromotionVars(stage))
	if err := r.Create(ctx, promotion); err != nil {
		r.Recorder.Eventf(stage, corev1.EventTypeWarning, "PromotionCreateFailed",
			"creating promotion for freight %s failed: %v", target.GetName(), err)
		promotionCreateErrorsTotal.WithLabelValues(stageNS, stageName).Inc()
		log.Error(err, "failed to create promotion", "freight", target.GetName())
		// Swallow the error: admission rejections (e.g. freight not available
		// to the stage) would otherwise hot-loop the workqueue.
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}
	r.Recorder.Eventf(stage, corev1.EventTypeNormal, "PromotionCreated",
		"created promotion %s for freight %s (%s)", promotion.GetName(), target.GetName(), formatDrift(drifted))
	promotionsCreatedTotal.WithLabelValues(stageNS, stageName).Inc()
	log.Info("created promotion", "promotion", promotion.GetName(), "freight", target.GetName(), "drift", formatDrift(drifted))
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// syncStatusSynced is ArgoCD's .status.sync.status for an Application whose
// live state matches git.
const syncStatusSynced = "Synced"

func applicationSyncStatus(app *unstructured.Unstructured) string {
	status, _, _ := unstructured.NestedString(app.Object, "status", "sync", "status")
	return status
}

// stageConsents reports whether the Stage lets the Application drive it:
// always, unless RequireStageConsent demands the Stage name it.
func (r *ApplicationReconciler) stageConsents(stage *unstructured.Unstructured, app types.NamespacedName) bool {
	if !r.RequireStageConsent {
		return true
	}
	for entry := range strings.SplitSeq(stage.GetAnnotations()[applicationsAnnotation], ",") {
		if strings.TrimSpace(entry) == app.Namespace+"/"+app.Name {
			return true
		}
	}
	return false
}

// stageImageRepos resolves the image repositories the Stage's Warehouses
// subscribe to; drift detection is limited to those repositories.
func (r *ApplicationReconciler) stageImageRepos(ctx context.Context, stage *unstructured.Unstructured) ([]string, error) {
	log := logf.FromContext(ctx)
	var repos []string
	for _, name := range stageWarehouseNames(stage) {
		warehouse := &unstructured.Unstructured{}
		warehouse.SetGroupVersionKind(warehouseGVK)
		if err := r.Get(ctx, types.NamespacedName{Namespace: stage.GetNamespace(), Name: name}, warehouse); err != nil {
			if apierrors.IsNotFound(err) {
				log.V(1).Info("warehouse not found", "warehouse", name)
				continue
			}
			return nil, err
		}
		repos = append(repos, warehouseImageRepos(warehouse)...)
	}
	return repos, nil
}

// lastPromotionFinishedAt returns when the Stage's last promotion finished
// (.status.lastPromotion.finishedAt), or the zero time.
func lastPromotionFinishedAt(stage *unstructured.Unstructured) time.Time {
	value, _, _ := unstructured.NestedString(stage.Object, "status", "lastPromotion", "finishedAt")
	finished, _ := time.Parse(time.RFC3339, value)
	return finished
}

// promotionSuperseded reports whether the Stage has finished a newer
// promotion since the given one was created, which makes that one's outcome
// history rather than a verdict on the current drift. Kargo orders Promotions
// by name; the finish time is checked as well so that a name sorting later
// only because of clock skew cannot make the observer recreate in a loop.
func promotionSuperseded(stage, promotion *unstructured.Unstructured) bool {
	last, _, _ := unstructured.NestedString(stage.Object, "status", "lastPromotion", "name")
	return last > promotion.GetName() &&
		lastPromotionFinishedAt(stage).After(promotion.GetCreationTimestamp().Time)
}

// stagePromotions inspects every Promotion targeting the Stage, whoever
// created it: inFlight names one Kargo has not finished yet (it may not have
// reached .status.currentPromotion), latest is the observer's newest by name
// (Kargo's own ordering).
func (r *ApplicationReconciler) stagePromotions(ctx context.Context, namespace, stage string) (latest *unstructured.Unstructured, inFlight string, err error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(promotionListGVK)
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, "", err
	}
	for i := range list.Items {
		p := &list.Items[i]
		if target, _, _ := unstructured.NestedString(p.Object, "spec", "stage"); target != stage {
			continue
		}
		if !promotionPhaseTerminal(promotionPhase(p)) {
			return nil, p.GetName(), nil
		}
		if p.GetLabels()[managedByLabel] == managedByValue && (latest == nil || p.GetName() > latest.GetName()) {
			latest = p
		}
	}
	return latest, "", nil
}

// disagreement describes another observed Application of the Stage that
// deploys a different tag for a drifted repository, or returns "".
func (r *ApplicationReconciler) disagreement(ctx context.Context, stage *unstructured.Unstructured, app types.NamespacedName, drifted map[string]string) (string, error) {
	others, err := r.stageApplications(ctx, stage)
	if err != nil {
		return "", err
	}
	for i := range others {
		other := types.NamespacedName{Namespace: others[i].GetNamespace(), Name: others[i].GetName()}
		if other == app || !r.stageConsents(stage, other) {
			continue
		}
		deployed := deployedImages(&others[i])
		for _, repo := range slices.Sorted(maps.Keys(drifted)) {
			if tag, ok := deployed[repo]; ok && tag != drifted[repo] {
				return fmt.Sprintf("%s deploys %s:%s", other, repo, tag), nil
			}
		}
	}
	return "", nil
}

// findFreight returns the newest Freight in the Stage's namespace that
// freightMatches accepts, or nil.
func (r *ApplicationReconciler) findFreight(ctx context.Context, stage *unstructured.Unstructured, deployed, drifted map[string]string) (*unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(freightListGVK)
	if err := r.List(ctx, list, client.InNamespace(stage.GetNamespace())); err != nil {
		return nil, err
	}
	warehouses := map[string]struct{}{}
	for _, name := range stageWarehouseNames(stage) {
		warehouses[name] = struct{}{}
	}
	var newest *unstructured.Unstructured
	for i := range list.Items {
		f := &list.Items[i]
		if !freightMatches(f, warehouses, deployed, drifted, stageHeldImages(stage, freightOrigin(f))) {
			continue
		}
		if newest == nil {
			newest = f
			continue
		}
		// Ties (same second) go to the name so the pick is stable.
		created, best := f.GetCreationTimestamp(), newest.GetCreationTimestamp()
		if created.After(best.Time) || (created.Equal(&best) && f.GetName() > newest.GetName()) {
			newest = f
		}
	}
	return newest, nil
}

// parseAuthorizedStage splits the "<namespace>:<stage>" annotation value.
func parseAuthorizedStage(value string) (namespace, name string, ok bool) {
	namespace, name, found := strings.Cut(value, ":")
	if !found || namespace == "" || name == "" {
		return "", "", false
	}
	return namespace, name, true
}

// SetupWithManager registers the controller: it reconciles annotated
// Applications and re-enqueues them when their Stage changes.
func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(applicationGVK)
	stage := &unstructured.Unstructured{}
	stage.SetGroupVersionKind(stageGVK)
	// The Application informer this controller starts already holds every
	// Application; the manager's client would LIST them live each time.
	r.appReader = mgr.GetCache()

	return ctrl.NewControllerManagedBy(mgr).
		Named("kargo-argocd-observer").
		For(app, builder.WithPredicates(r.applicationPredicate())).
		Watches(stage,
			handler.EnqueueRequestsFromMapFunc(r.applicationsForStage),
			builder.WithPredicates(stagePredicate())).
		Complete(r)
}

// stagePredicate reduces Stage fan-out to the changes the observer acts on:
// the spec (via generation), the consent annotation, the recorded freight
// history, and the in-flight promotion pointer. Kargo touches Stage status constantly (health checks,
// verification bookkeeping); without this filter every touch would
// re-reconcile every Application attached to the Stage.
func stagePredicate() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
				e.ObjectOld.GetAnnotations()[applicationsAnnotation] != e.ObjectNew.GetAnnotations()[applicationsAnnotation] {
				return true
			}
			oldStage, okOld := e.ObjectOld.(*unstructured.Unstructured)
			newStage, okNew := e.ObjectNew.(*unstructured.Unstructured)
			if !okOld || !okNew {
				return true
			}
			oldHistory, _, _ := unstructured.NestedSlice(oldStage.Object, "status", "freightHistory")
			newHistory, _, _ := unstructured.NestedSlice(newStage.Object, "status", "freightHistory")
			if !reflect.DeepEqual(oldHistory, newHistory) {
				return true
			}
			oldPromo, _, _ := unstructured.NestedMap(oldStage.Object, "status", "currentPromotion")
			newPromo, _, _ := unstructured.NestedMap(newStage.Object, "status", "currentPromotion")
			return !reflect.DeepEqual(oldPromo, newPromo)
		},
	}
}

// applicationPredicate admits Applications carrying the authorized-stage
// annotation and observed per shouldObserve, and filters updates down to
// annotation or deployed-image changes, the Application becoming Synced, and
// the periodic informer resync.
// An Application that is deleted or stops being observed gets one last
// reconcile, which drops it from the freight-missing gauge.
func (r *ApplicationReconciler) applicationPredicate() predicate.Funcs {
	relevant := func(o client.Object) bool {
		annotations := o.GetAnnotations()
		return annotations[authorizedStageAnnotation] != "" && r.shouldObserve(annotations)
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return relevant(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return relevant(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return relevant(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !relevant(e.ObjectNew) {
				return relevant(e.ObjectOld)
			}
			// A resync (--sync-period) redelivers the object unchanged; it is
			// the backstop for everything the observer does not watch, e.g.
			// new Freight or a deleted Promotion.
			if rv := e.ObjectNew.GetResourceVersion(); rv != "" && rv == e.ObjectOld.GetResourceVersion() {
				return true
			}
			if !reflect.DeepEqual(e.ObjectOld.GetAnnotations(), e.ObjectNew.GetAnnotations()) {
				return true
			}
			oldApp, okOld := e.ObjectOld.(*unstructured.Unstructured)
			newApp, okNew := e.ObjectNew.(*unstructured.Unstructured)
			if !okOld || !okNew {
				return true
			}
			// Becoming Synced lifts the gate in Reconcile.
			if was, is := applicationSyncStatus(oldApp), applicationSyncStatus(newApp); was != "" && was != is && is == syncStatusSynced {
				return true
			}
			oldImages, _, _ := unstructured.NestedStringSlice(oldApp.Object, "status", "summary", "images")
			newImages, _, _ := unstructured.NestedStringSlice(newApp.Object, "status", "summary", "images")
			return !reflect.DeepEqual(oldImages, newImages)
		},
	}
}

// applicationsForStage enqueues every Application whose authorized-stage
// annotation points at the changed Stage.
func (r *ApplicationReconciler) applicationsForStage(ctx context.Context, obj client.Object) []reconcile.Request {
	apps, err := r.stageApplications(ctx, obj)
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to list applications for stage",
			"stage", obj.GetNamespace()+":"+obj.GetName())
		return nil
	}
	requests := make([]reconcile.Request, 0, len(apps))
	for i := range apps {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: apps[i].GetNamespace(), Name: apps[i].GetName()},
		})
	}
	return requests
}

// stageApplications lists the observed Applications whose authorized-stage
// annotation points at the Stage.
func (r *ApplicationReconciler) stageApplications(ctx context.Context, stage client.Object) ([]unstructured.Unstructured, error) {
	reader := r.appReader
	if reader == nil {
		reader = r.Client
	}
	want := stage.GetNamespace() + ":" + stage.GetName()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(applicationListGVK)
	if err := reader.List(ctx, list); err != nil {
		return nil, err
	}
	var apps []unstructured.Unstructured
	for i := range list.Items {
		annotations := list.Items[i].GetAnnotations()
		if annotations[authorizedStageAnnotation] == want && r.shouldObserve(annotations) {
			apps = append(apps, list.Items[i])
		}
	}
	return apps, nil
}
