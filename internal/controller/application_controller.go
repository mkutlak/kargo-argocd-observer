// Package controller reconciles ArgoCD Applications against Kargo Stages:
// when the deployed image tags diverge from the Stage's current Freight
// (e.g. after a direct git commit), it finds the Freight matching the
// deployed tags and creates a Promotion so Kargo's view matches reality.
package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
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
}

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
func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(applicationGVK)
	if err := r.Get(ctx, req.NamespacedName, app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	annotations := app.GetAnnotations()
	if !r.shouldObserve(annotations) {
		return ctrl.Result{}, nil
	}
	stageNS, stageName, ok := parseAuthorizedStage(annotations[authorizedStageAnnotation])
	if !ok {
		log.V(1).Info("missing or malformed authorized-stage annotation",
			"annotation", annotations[authorizedStageAnnotation])
		return ctrl.Result{}, nil
	}
	log = log.WithValues("stageNamespace", stageNS, "stage", stageName)

	stage := &unstructured.Unstructured{}
	stage.SetGroupVersionKind(stageGVK)
	if err := r.Get(ctx, types.NamespacedName{Namespace: stageNS, Name: stageName}, stage); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("stage not found, skipping application")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	deployed := deployedImages(app)
	if len(deployed) == 0 {
		return ctrl.Result{}, nil
	}

	repos, err := r.stageImageRepos(ctx, stage)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(repos) == 0 {
		return ctrl.Result{}, nil
	}

	current := stageCurrentImages(stage)
	drifted := map[string]string{}
	for _, repo := range repos {
		if tag := deployed[repo]; tag != "" && current[repo] != tag {
			drifted[repo] = tag
		}
	}
	if len(drifted) == 0 {
		freightMissing.WithLabelValues(stageNS, stageName).Set(0)
		return ctrl.Result{}, nil
	}
	// Drift alone is V(1): the same divergence is re-detected on every
	// reconcile until the Stage records the promotion. Action paths below
	// (create / dry-run / events) carry the drift at Info level.
	log.V(1).Info("deployed images diverge from stage's current freight", "drift", formatDrift(drifted))

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
	if current, found, _ := unstructured.NestedMap(stage.Object, "status", "currentPromotion"); found && len(current) > 0 {
		log.V(1).Info("stage has a promotion in progress, requeueing")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	latest, err := r.latestObserverPromotion(ctx, stageNS, stageName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if latest != nil && !promotionPhaseTerminal(promotionPhase(latest)) {
		log.V(1).Info("observer promotion still in flight, requeueing", "promotion", latest.GetName())
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	target, err := r.findFreight(ctx, stage, drifted)
	if err != nil {
		return ctrl.Result{}, err
	}
	if target == nil {
		r.Recorder.Eventf(stage, corev1.EventTypeWarning, "FreightMissing",
			"no Freight matches deployed images %s", formatDrift(drifted))
		freightMissing.WithLabelValues(stageNS, stageName).Set(1)
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	if latest != nil && latest.GetLabels()[freightLabel] == target.GetName() {
		switch phase := promotionPhase(latest); phase {
		case phaseSucceeded:
			// Kargo already accepted this exact Freight; the Stage's freight
			// history just hasn't recorded the result yet. Creating another
			// Promotion would duplicate it — hold off and let the Stage
			// catch up.
			log.V(1).Info("promotion for this freight already succeeded, waiting for the stage to record it",
				"promotion", latest.GetName())
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		case phaseFailed, phaseErrored:
			// A failed promotion for the same freight is a human problem:
			// recreating it would just fail again. The failed Promotion must
			// be deleted to retry.
			r.Recorder.Eventf(stage, corev1.EventTypeWarning, "PromotionPreviouslyFailed",
				"promotion %s for freight %s ended in phase %s; delete it to let the observer retry",
				latest.GetName(), target.GetName(), phase)
			freightMissing.WithLabelValues(stageNS, stageName).Set(0)
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
	freightMissing.WithLabelValues(stageNS, stageName).Set(0)
	log.Info("created promotion", "promotion", promotion.GetName(), "freight", target.GetName(), "drift", formatDrift(drifted))
	return ctrl.Result{RequeueAfter: time.Minute}, nil
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

// latestObserverPromotion returns the most recently created Promotion this
// observer made for the given stage, or nil when there is none.
func (r *ApplicationReconciler) latestObserverPromotion(ctx context.Context, namespace, stage string) (*unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(promotionListGVK)
	if err := r.List(ctx, list,
		client.InNamespace(namespace),
		client.MatchingLabels{managedByLabel: managedByValue, stageLabel: stage},
	); err != nil {
		return nil, err
	}
	var latest *unstructured.Unstructured
	for i := range list.Items {
		p := &list.Items[i]
		if latest == nil || p.GetCreationTimestamp().After(latest.GetCreationTimestamp().Time) {
			latest = p
		}
	}
	return latest, nil
}

// findFreight returns the Freight in the Stage's namespace that originates
// from one of its Warehouses and matches every drifted repo:tag, or nil.
func (r *ApplicationReconciler) findFreight(ctx context.Context, stage *unstructured.Unstructured, drifted map[string]string) (*unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(freightListGVK)
	if err := r.List(ctx, list, client.InNamespace(stage.GetNamespace())); err != nil {
		return nil, err
	}
	warehouses := map[string]struct{}{}
	for _, name := range stageWarehouseNames(stage) {
		warehouses[name] = struct{}{}
	}
	for i := range list.Items {
		if freightMatches(&list.Items[i], warehouses, drifted) {
			return &list.Items[i], nil
		}
	}
	return nil, nil
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

	return ctrl.NewControllerManagedBy(mgr).
		Named("kargo-argocd-observer").
		For(app, builder.WithPredicates(r.applicationPredicate())).
		Watches(stage,
			handler.EnqueueRequestsFromMapFunc(r.applicationsForStage),
			builder.WithPredicates(stagePredicate())).
		Complete(r)
}

// stagePredicate reduces Stage fan-out to the changes the observer acts on:
// the spec (via generation), the recorded freight history, and the in-flight
// promotion pointer. Kargo touches Stage status constantly (health checks,
// verification bookkeeping); without this filter every touch would
// re-reconcile every Application attached to the Stage.
func stagePredicate() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() {
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
// annotation or deployed-image changes.
func (r *ApplicationReconciler) applicationPredicate() predicate.Funcs {
	relevant := func(o client.Object) bool {
		annotations := o.GetAnnotations()
		return annotations[authorizedStageAnnotation] != "" && r.shouldObserve(annotations)
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return relevant(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return relevant(e.Object) },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !relevant(e.ObjectNew) {
				return false
			}
			if !reflect.DeepEqual(e.ObjectOld.GetAnnotations(), e.ObjectNew.GetAnnotations()) {
				return true
			}
			oldApp, okOld := e.ObjectOld.(*unstructured.Unstructured)
			newApp, okNew := e.ObjectNew.(*unstructured.Unstructured)
			if !okOld || !okNew {
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
	want := obj.GetNamespace() + ":" + obj.GetName()
	apps := &unstructured.UnstructuredList{}
	apps.SetGroupVersionKind(applicationListGVK)
	if err := r.List(ctx, apps); err != nil {
		logf.FromContext(ctx).Error(err, "failed to list applications for stage", "stage", want)
		return nil
	}
	var requests []reconcile.Request
	for i := range apps.Items {
		app := &apps.Items[i]
		annotations := app.GetAnnotations()
		if annotations[authorizedStageAnnotation] == want && r.shouldObserve(annotations) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: app.GetNamespace(), Name: app.GetName()},
			})
		}
	}
	return requests
}
