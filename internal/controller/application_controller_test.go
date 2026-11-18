package controller

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

const (
	testNS        = "kargo-demo"
	testStage     = "qa"
	testAppNS     = "argocd"
	testAppName   = "qa-app"
	testRepo      = "registry.example.com/acme/web"
	deployedTag   = "1.2.3"
	currentTag    = "1.2.2"
	warehouseName = "demo-warehouse"
	freightName   = "fr-2c9f4a7b"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	for _, gvk := range []schema.GroupVersionKind{applicationGVK, stageGVK, freightGVK, promotionGVK, warehouseGVK} {
		sch.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		sch.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return sch
}

func testApp(annotations map[string]string, images []string) *unstructured.Unstructured {
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(applicationGVK)
	app.SetNamespace(testAppNS)
	app.SetName(testAppName)
	app.SetAnnotations(annotations)
	if len(images) > 0 {
		_ = unstructured.SetNestedStringSlice(app.Object, images, "status", "summary", "images")
	}
	return app
}

func testStageObj(currentImageTag string, promotionInProgress bool) *unstructured.Unstructured {
	stage := &unstructured.Unstructured{}
	stage.SetGroupVersionKind(stageGVK)
	stage.SetNamespace(testNS)
	stage.SetName(testStage)
	_ = unstructured.SetNestedSlice(stage.Object, []any{
		map[string]any{
			"origin": map[string]any{"kind": "Warehouse", "name": warehouseName},
		},
	}, "spec", "requestedFreight")
	_ = unstructured.SetNestedSlice(stage.Object, []any{
		map[string]any{
			"items": map[string]any{
				"Warehouse/" + warehouseName: map[string]any{
					"name": "fr-previous",
					"images": []any{
						map[string]any{"repoURL": testRepo, "tag": currentImageTag},
					},
				},
			},
		},
	}, "status", "freightHistory")
	_ = unstructured.SetNestedSlice(stage.Object, []any{
		map[string]any{"name": "env", "value": "test"},
	}, "spec", "vars")
	_ = unstructured.SetNestedMap(stage.Object, map[string]any{
		"spec": map[string]any{
			"vars": []any{
				map[string]any{"name": "flag", "value": "true"},
			},
			"steps": []any{
				map[string]any{"task": map[string]any{"name": "sync-argo"}},
			},
		},
	}, "spec", "promotionTemplate")
	if promotionInProgress {
		_ = unstructured.SetNestedMap(stage.Object,
			map[string]any{"name": "someone-elses-promotion"}, "status", "currentPromotion")
	}
	return stage
}

func testWarehouse() *unstructured.Unstructured {
	warehouse := &unstructured.Unstructured{}
	warehouse.SetGroupVersionKind(warehouseGVK)
	warehouse.SetNamespace(testNS)
	warehouse.SetName(warehouseName)
	_ = unstructured.SetNestedSlice(warehouse.Object, []any{
		map[string]any{"image": map[string]any{"repoURL": testRepo}},
	}, "spec", "subscriptions")
	return warehouse
}

func testFreight(name, tag string) *unstructured.Unstructured {
	freight := &unstructured.Unstructured{}
	freight.SetGroupVersionKind(freightGVK)
	freight.SetNamespace(testNS)
	freight.SetName(name)
	_ = unstructured.SetNestedMap(freight.Object,
		map[string]any{"kind": "Warehouse", "name": warehouseName}, "origin")
	_ = unstructured.SetNestedSlice(freight.Object, []any{
		map[string]any{"repoURL": testRepo, "tag": tag},
	}, "images")
	return freight
}

func testPromotion(name, freight, phase string) *unstructured.Unstructured {
	promotion := &unstructured.Unstructured{}
	promotion.SetGroupVersionKind(promotionGVK)
	promotion.SetNamespace(testNS)
	promotion.SetName(name)
	promotion.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-time.Hour)))
	promotion.SetLabels(map[string]string{
		managedByLabel: managedByValue,
		stageLabel:     testStage,
		freightLabel:   freight,
	})
	_ = unstructured.SetNestedField(promotion.Object, testStage, "spec", "stage")
	_ = unstructured.SetNestedField(promotion.Object, freight, "spec", "freight")
	if phase != "" {
		_ = unstructured.SetNestedField(promotion.Object, phase, "status", "phase")
	}
	return promotion
}

func listObserverPromotions(t *testing.T, cl client.Client) []unstructured.Unstructured {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(promotionListGVK)
	if err := cl.List(context.Background(), list,
		client.InNamespace(testNS),
		client.MatchingLabels{managedByLabel: managedByValue},
	); err != nil {
		t.Fatalf("listing promotions: %v", err)
	}
	return list.Items
}

func drainEvents(recorder *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case e := <-recorder.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

func eventsContain(events []string, substr string) bool {
	for _, e := range events {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

func float64Ptr(v float64) *float64 { return &v }

func TestStagePredicate(t *testing.T) {
	base := testStageObj(currentTag, false)

	tests := []struct {
		name   string
		mutate func(stage *unstructured.Unstructured)
		want   bool
	}{
		{
			name: "status noise (health) is filtered",
			mutate: func(s *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(s.Object, "Degraded", "status", "health")
			},
			want: false,
		},
		{
			name: "freight history change fans out",
			mutate: func(s *unstructured.Unstructured) {
				_ = unstructured.SetNestedSlice(s.Object, []any{
					map[string]any{"items": map[string]any{}},
				}, "status", "freightHistory")
			},
			want: true,
		},
		{
			name: "current promotion change fans out",
			mutate: func(s *unstructured.Unstructured) {
				_ = unstructured.SetNestedMap(s.Object, map[string]any{"name": "p"}, "status", "currentPromotion")
			},
			want: true,
		},
		{
			name:   "spec change (generation bump) fans out",
			mutate: func(s *unstructured.Unstructured) { s.SetGeneration(base.GetGeneration() + 1) },
			want:   true,
		},
	}
	pred := stagePredicate()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			newStage := base.DeepCopy()
			tc.mutate(newStage)
			got := pred.Update(event.UpdateEvent{ObjectOld: base.DeepCopy(), ObjectNew: newStage})
			if got != tc.want {
				t.Errorf("stagePredicate update = %v, want %v", got, tc.want)
			}
		})
	}

	if !pred.Create(event.CreateEvent{Object: base.DeepCopy()}) {
		t.Error("stagePredicate must admit create events")
	}
	if pred.Delete(event.DeleteEvent{Object: base.DeepCopy()}) {
		t.Error("stagePredicate must filter delete events")
	}
}

func TestReconcile(t *testing.T) {
	authorizedAnns := map[string]string{
		authorizedStageAnnotation: testNS + ":" + testStage,
	}
	deployedImage := testRepo + ":" + deployedTag

	tests := []struct {
		name             string
		app              *unstructured.Unstructured
		objs             []client.Object
		dryRun           bool
		observeMode      string
		wantPromotions   int
		wantEvents       []string
		wantNoEvents     bool
		wantRequeueAfter time.Duration
		wantGauge        *float64
		verify           func(t *testing.T, promotions []unstructured.Unstructured)
	}{
		{
			name: "drift with matching freight creates promotion",
			app:  testApp(authorizedAnns, []string{deployedImage}),
			objs: []client.Object{
				testStageObj(currentTag, false),
				testWarehouse(),
				testFreight(freightName, deployedTag),
			},
			wantPromotions:   1,
			wantEvents:       []string{"PromotionCreated"},
			wantRequeueAfter: time.Minute,
			wantGauge:        float64Ptr(0),
			verify: func(t *testing.T, promotions []unstructured.Unstructured) {
				p := promotions[0]
				// Kargo-style <stage>.<ulid>.<freight> name — the stage
				// controller ignores names that don't sort after
				// status.lastPromotion.name.
				if !strings.HasPrefix(p.GetName(), testStage+".") {
					t.Errorf("promotion name %q missing Kargo-style %q prefix", p.GetName(), testStage+".")
				}
				gotStage, _, _ := unstructured.NestedString(p.Object, "spec", "stage")
				gotFreight, _, _ := unstructured.NestedString(p.Object, "spec", "freight")
				if gotStage != testStage {
					t.Errorf("spec.stage = %q, want %q", gotStage, testStage)
				}
				if gotFreight != freightName {
					t.Errorf("spec.freight = %q, want %q", gotFreight, freightName)
				}
				labels := p.GetLabels()
				if labels[managedByLabel] != managedByValue {
					t.Errorf("label %s = %q, want %q", managedByLabel, labels[managedByLabel], managedByValue)
				}
				if labels[stageLabel] != testStage {
					t.Errorf("label %s = %q, want %q", stageLabel, labels[stageLabel], testStage)
				}
				if labels[freightLabel] != freightName {
					t.Errorf("label %s = %q, want %q", freightLabel, labels[freightLabel], freightName)
				}
				steps, _, _ := unstructured.NestedSlice(p.Object, "spec", "steps")
				if len(steps) != 1 {
					t.Fatalf("spec.steps len = %d, want 1 (copied from stage promotion template)", len(steps))
				}
				step, _ := steps[0].(map[string]any)
				task, _, _ := unstructured.NestedString(step, "task", "name")
				if task != "sync-argo" {
					t.Errorf("spec.steps[0].task.name = %q, want %q", task, "sync-argo")
				}
				vars, _, _ := unstructured.NestedSlice(p.Object, "spec", "vars")
				if len(vars) != 2 {
					t.Fatalf("spec.vars len = %d, want 2 (stage vars then template vars)", len(vars))
				}
				firstVar, _ := vars[0].(map[string]any)
				if firstVar["name"] != "env" {
					t.Errorf("spec.vars[0].name = %v, want env (stage vars first)", firstVar["name"])
				}
				secondVar, _ := vars[1].(map[string]any)
				if secondVar["name"] != "flag" {
					t.Errorf("spec.vars[1].name = %v, want flag (template vars second)", secondVar["name"])
				}
			},
		},
		{
			name: "already in sync",
			app:  testApp(authorizedAnns, []string{testRepo + ":" + currentTag}),
			objs: []client.Object{
				testStageObj(currentTag, false),
				testWarehouse(),
				testFreight(freightName, deployedTag),
			},
			wantPromotions: 0,
			wantNoEvents:   true,
			wantGauge:      float64Ptr(0),
		},
		{
			name: "drift without matching freight",
			app:  testApp(authorizedAnns, []string{deployedImage}),
			objs: []client.Object{
				testStageObj(currentTag, false),
				testWarehouse(),
				testFreight("fr-old", currentTag),
			},
			wantPromotions:   0,
			wantEvents:       []string{"FreightMissing"},
			wantRequeueAfter: 5 * time.Minute,
			wantGauge:        float64Ptr(1),
		},
		{
			name: "observer promotion in flight",
			app:  testApp(authorizedAnns, []string{deployedImage}),
			objs: []client.Object{
				testStageObj(currentTag, false),
				testWarehouse(),
				testFreight(freightName, deployedTag),
				testPromotion("qa-observer-aaaaa", freightName, "Running"),
			},
			wantPromotions:   1,
			wantNoEvents:     true,
			wantRequeueAfter: 30 * time.Second,
		},
		{
			name: "same freight already succeeded waits for stage to record",
			app:  testApp(authorizedAnns, []string{deployedImage}),
			objs: []client.Object{
				testStageObj(currentTag, false),
				testWarehouse(),
				testFreight(freightName, deployedTag),
				testPromotion("qa-observer-succeeded", freightName, "Succeeded"),
			},
			wantPromotions:   1,
			wantNoEvents:     true,
			wantRequeueAfter: 30 * time.Second,
		},
		{
			name: "stage promotion in progress",
			app:  testApp(authorizedAnns, []string{deployedImage}),
			objs: []client.Object{
				testStageObj(currentTag, true),
				testWarehouse(),
				testFreight(freightName, deployedTag),
			},
			wantPromotions:   0,
			wantNoEvents:     true,
			wantRequeueAfter: 30 * time.Second,
		},
		{
			name: "dry-run emits event without promotion",
			app:  testApp(authorizedAnns, []string{deployedImage}),
			objs: []client.Object{
				testStageObj(currentTag, false),
				testWarehouse(),
				testFreight(freightName, deployedTag),
			},
			dryRun:         true,
			wantPromotions: 0,
			wantEvents:     []string{"DryRunPromotionSkipped"},
		},
		{
			name: "stage without promotion steps emits event and skips",
			app:  testApp(authorizedAnns, []string{deployedImage}),
			objs: []client.Object{
				func() client.Object {
					stage := testStageObj(currentTag, false)
					unstructured.RemoveNestedField(stage.Object, "spec", "promotionTemplate")
					return stage
				}(),
				testWarehouse(),
				testFreight(freightName, deployedTag),
			},
			wantPromotions: 0,
			wantEvents:     []string{"StageHasNoPromotionSteps"},
		},
		{
			name: "target-aware stage emits event and skips",
			app:  testApp(authorizedAnns, []string{deployedImage}),
			objs: []client.Object{
				func() client.Object {
					stage := testStageObj(currentTag, false)
					_ = unstructured.SetNestedField(stage.Object, map[string]any{"selectors": []any{}}, "spec", "targets")
					return stage
				}(),
				testWarehouse(),
				testFreight(freightName, deployedTag),
			},
			wantPromotions: 0,
			wantEvents:     []string{"StageIsTargetAware"},
		},
		{
			// Kargo doesn't require steps on a target-aware Stage; report the
			// real cause rather than suggesting a promotionTemplate.
			name: "target-aware stage without promotion steps reports target-aware",
			app:  testApp(authorizedAnns, []string{deployedImage}),
			objs: []client.Object{
				func() client.Object {
					stage := testStageObj(currentTag, false)
					unstructured.RemoveNestedField(stage.Object, "spec", "promotionTemplate")
					_ = unstructured.SetNestedField(stage.Object, map[string]any{"selectors": []any{}}, "spec", "targets")
					return stage
				}(),
				testWarehouse(),
				testFreight(freightName, deployedTag),
			},
			wantPromotions: 0,
			wantEvents:     []string{"StageIsTargetAware"},
		},
		{
			name: "ignore annotation",
			app: testApp(map[string]string{
				authorizedStageAnnotation: testNS + ":" + testStage,
				ignoreAnnotation:          "true",
			}, []string{deployedImage}),
			objs: []client.Object{
				testStageObj(currentTag, false),
				testWarehouse(),
				testFreight(freightName, deployedTag),
			},
			wantPromotions: 0,
			wantNoEvents:   true,
		},
		{
			name: "malformed authorized-stage annotation",
			app: testApp(map[string]string{
				authorizedStageAnnotation: "no-colon-here",
			}, []string{deployedImage}),
			wantPromotions: 0,
			wantNoEvents:   true,
		},
		{
			name: "previously failed promotion for same freight",
			app:  testApp(authorizedAnns, []string{deployedImage}),
			objs: []client.Object{
				testStageObj(currentTag, false),
				testWarehouse(),
				testFreight(freightName, deployedTag),
				testPromotion("qa-observer-bbbbb", freightName, phaseFailed),
			},
			wantPromotions: 1,
			wantEvents:     []string{"PromotionPreviouslyFailed"},
			wantGauge:      float64Ptr(0),
		},
		{
			name: "opt-in mode skips unannotated application",
			app:  testApp(authorizedAnns, []string{deployedImage}),
			objs: []client.Object{
				testStageObj(currentTag, false),
				testWarehouse(),
				testFreight(freightName, deployedTag),
			},
			observeMode:    ObserveModeOptIn,
			wantPromotions: 0,
			wantNoEvents:   true,
		},
		{
			name: "opt-in mode promotes annotated application",
			app: testApp(map[string]string{
				authorizedStageAnnotation: testNS + ":" + testStage,
				observeAnnotation:         "true",
			}, []string{deployedImage}),
			objs: []client.Object{
				testStageObj(currentTag, false),
				testWarehouse(),
				testFreight(freightName, deployedTag),
			},
			observeMode:      ObserveModeOptIn,
			wantPromotions:   1,
			wantEvents:       []string{"PromotionCreated"},
			wantRequeueAfter: time.Minute,
			wantGauge:        float64Ptr(0),
		},
		{
			name: "opt-in ignore wins over observe",
			app: testApp(map[string]string{
				authorizedStageAnnotation: testNS + ":" + testStage,
				observeAnnotation:         "true",
				ignoreAnnotation:          "true",
			}, []string{deployedImage}),
			objs: []client.Object{
				testStageObj(currentTag, false),
				testWarehouse(),
				testFreight(freightName, deployedTag),
			},
			observeMode:    ObserveModeOptIn,
			wantPromotions: 0,
			wantNoEvents:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sch := newTestScheme(t)
			objs := append([]client.Object{tc.app}, tc.objs...)
			cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
			recorder := record.NewFakeRecorder(20)
			r := &ApplicationReconciler{Client: cl, Scheme: sch, Recorder: recorder, DryRun: tc.dryRun, ObserveMode: tc.observeMode}

			res, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Namespace: testAppNS, Name: testAppName},
			})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if res.RequeueAfter != tc.wantRequeueAfter {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, tc.wantRequeueAfter)
			}

			promotions := listObserverPromotions(t, cl)
			if len(promotions) != tc.wantPromotions {
				t.Fatalf("got %d observer promotions, want %d", len(promotions), tc.wantPromotions)
			}

			events := drainEvents(recorder)
			if tc.wantNoEvents && len(events) > 0 {
				t.Errorf("unexpected events: %v", events)
			}
			for _, want := range tc.wantEvents {
				if !eventsContain(events, want) {
					t.Errorf("events %v missing %q", events, want)
				}
			}

			if tc.wantGauge != nil {
				got := testutil.ToFloat64(freightMissing.WithLabelValues(testNS, testStage))
				if got != *tc.wantGauge {
					t.Errorf("kargo_observer_freight_missing = %v, want %v", got, *tc.wantGauge)
				}
			}

			if tc.verify != nil {
				tc.verify(t, promotions)
			}
		})
	}
}

func TestReconcileApplicationGone(t *testing.T) {
	sch := newTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &ApplicationReconciler{Client: cl, Scheme: sch, Recorder: record.NewFakeRecorder(20)}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testAppNS, Name: "does-not-exist"},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("Reconcile() result = %+v, want zero", res)
	}
}

func TestReconcileStageGone(t *testing.T) {
	sch := newTestScheme(t)
	app := testApp(map[string]string{
		authorizedStageAnnotation: testNS + ":" + testStage,
	}, []string{testRepo + ":" + deployedTag})
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(app).Build()
	r := &ApplicationReconciler{Client: cl, Scheme: sch, Recorder: record.NewFakeRecorder(20)}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testAppNS, Name: testAppName},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("Reconcile() result = %+v, want zero", res)
	}
}

func TestShouldObserve(t *testing.T) {
	tests := []struct {
		name        string
		observeMode string
		annotations map[string]string
		want        bool
	}{
		{name: "opt-out: no annotations", observeMode: ObserveModeOptOut, annotations: nil, want: true},
		{name: "opt-out: authorized only", observeMode: ObserveModeOptOut, annotations: map[string]string{authorizedStageAnnotation: testNS + ":" + testStage}, want: true},
		{name: "opt-out: observe true", observeMode: ObserveModeOptOut, annotations: map[string]string{observeAnnotation: "true"}, want: true},
		{name: "opt-out: observe false", observeMode: ObserveModeOptOut, annotations: map[string]string{observeAnnotation: "false"}, want: true},
		{name: "opt-out: ignore true", observeMode: ObserveModeOptOut, annotations: map[string]string{ignoreAnnotation: "true"}, want: false},
		{name: "opt-out: ignore true and observe true", observeMode: ObserveModeOptOut, annotations: map[string]string{ignoreAnnotation: "true", observeAnnotation: "true"}, want: false},

		{name: "opt-in: no annotations", observeMode: ObserveModeOptIn, annotations: nil, want: false},
		{name: "opt-in: authorized only", observeMode: ObserveModeOptIn, annotations: map[string]string{authorizedStageAnnotation: testNS + ":" + testStage}, want: false},
		{name: "opt-in: observe true", observeMode: ObserveModeOptIn, annotations: map[string]string{observeAnnotation: "true"}, want: true},
		{name: "opt-in: observe false", observeMode: ObserveModeOptIn, annotations: map[string]string{observeAnnotation: "false"}, want: false},
		{name: "opt-in: ignore true", observeMode: ObserveModeOptIn, annotations: map[string]string{ignoreAnnotation: "true"}, want: false},
		{name: "opt-in: ignore true and observe true", observeMode: ObserveModeOptIn, annotations: map[string]string{ignoreAnnotation: "true", observeAnnotation: "true"}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &ApplicationReconciler{ObserveMode: tc.observeMode}
			if got := r.shouldObserve(tc.annotations); got != tc.want {
				t.Errorf("shouldObserve() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseObserveMode(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: ObserveModeOptOut},
		{in: "opt-out", want: ObserveModeOptOut},
		{in: "opt-in", want: ObserveModeOptIn},
		{in: "bogus", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseObserveMode(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseObserveMode(%q) error = nil, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseObserveMode(%q) error = %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseObserveMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestApplicationPredicate(t *testing.T) {
	authorizedAnns := map[string]string{authorizedStageAnnotation: testNS + ":" + testStage}

	t.Run("create", func(t *testing.T) {
		r := &ApplicationReconciler{}
		pred := r.applicationPredicate()

		tests := []struct {
			name string
			app  *unstructured.Unstructured
			want bool
		}{
			{name: "annotated admitted", app: testApp(authorizedAnns, nil), want: true},
			{name: "unannotated rejected", app: testApp(nil, nil), want: false},
			{
				name: "ignored rejected",
				app: testApp(map[string]string{
					authorizedStageAnnotation: testNS + ":" + testStage,
					ignoreAnnotation:          "true",
				}, nil),
				want: false,
			},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if got := pred.Create(event.CreateEvent{Object: tc.app}); got != tc.want {
					t.Errorf("Create() = %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("update", func(t *testing.T) {
		r := &ApplicationReconciler{}
		pred := r.applicationPredicate()

		oldApp := testApp(authorizedAnns, []string{testRepo + ":" + currentTag})

		imageChanged := testApp(authorizedAnns, []string{testRepo + ":" + deployedTag})

		annotationChanged := testApp(map[string]string{
			authorizedStageAnnotation: testNS + ":" + testStage,
			"example.com/other":       "value",
		}, []string{testRepo + ":" + currentTag})

		statusOnlyNoise := testApp(authorizedAnns, []string{testRepo + ":" + currentTag})
		_ = unstructured.SetNestedField(statusOnlyNoise.Object, "Synced", "status", "sync", "status")

		tests := []struct {
			name string
			old  *unstructured.Unstructured
			new  *unstructured.Unstructured
			want bool
		}{
			{name: "image change admitted", old: oldApp, new: imageChanged, want: true},
			{name: "annotation change admitted", old: oldApp, new: annotationChanged, want: true},
			{name: "status-only noise rejected", old: oldApp, new: statusOnlyNoise, want: false},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if got := pred.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new}); got != tc.want {
					t.Errorf("Update() = %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("delete of an observed application is admitted", func(t *testing.T) {
		r := &ApplicationReconciler{}
		pred := r.applicationPredicate()
		if got := pred.Delete(event.DeleteEvent{Object: testApp(authorizedAnns, nil)}); !got {
			t.Errorf("Delete() = false, want true")
		}
	})

	t.Run("opt-in mode", func(t *testing.T) {
		r := &ApplicationReconciler{ObserveMode: ObserveModeOptIn}
		pred := r.applicationPredicate()

		unannotatedWithObserve := testApp(map[string]string{observeAnnotation: "true"}, nil)
		optedIn := testApp(map[string]string{
			authorizedStageAnnotation: testNS + ":" + testStage,
			observeAnnotation:         "true",
		}, nil)

		if got := pred.Create(event.CreateEvent{Object: unannotatedWithObserve}); got {
			t.Errorf("Create(unannotated-with-observe) = true, want false")
		}
		if got := pred.Create(event.CreateEvent{Object: optedIn}); !got {
			t.Errorf("Create(opted-in) = false, want true")
		}
	})
}

func TestApplicationsForStage(t *testing.T) {
	want := testNS + ":" + testStage

	optedInApp := testApp(map[string]string{
		authorizedStageAnnotation: want,
		observeAnnotation:         "true",
	}, nil)
	optedInApp.SetName("opted-in-app")

	notOptedInApp := testApp(map[string]string{authorizedStageAnnotation: want}, nil)
	notOptedInApp.SetName("not-opted-in-app")

	ignoredApp := testApp(map[string]string{
		authorizedStageAnnotation: want,
		ignoreAnnotation:          "true",
	}, nil)
	ignoredApp.SetName("ignored-app")

	unrelatedApp := testApp(map[string]string{authorizedStageAnnotation: "other-ns:other-stage"}, nil)
	unrelatedApp.SetName("unrelated-app")

	sch := newTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(optedInApp, notOptedInApp, ignoredApp, unrelatedApp).
		Build()
	stage := testStageObj(currentTag, false)

	tests := []struct {
		name        string
		observeMode string
		wantNames   []string
	}{
		{name: "opt-out mode enqueues all but ignored", observeMode: ObserveModeOptOut, wantNames: []string{"not-opted-in-app", "opted-in-app"}},
		{name: "opt-in mode enqueues only opted-in", observeMode: ObserveModeOptIn, wantNames: []string{"opted-in-app"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &ApplicationReconciler{Client: cl, Scheme: sch, ObserveMode: tc.observeMode}
			requests := r.applicationsForStage(context.Background(), stage)

			var gotNames []string
			for _, req := range requests {
				if req.Namespace != testAppNS {
					t.Errorf("request namespace = %q, want %q", req.Namespace, testAppNS)
				}
				gotNames = append(gotNames, req.Name)
			}
			sort.Strings(gotNames)
			wantNames := append([]string(nil), tc.wantNames...)
			sort.Strings(wantNames)
			if !reflect.DeepEqual(gotNames, wantNames) {
				t.Errorf("enqueued names = %v, want %v", gotNames, wantNames)
			}
		})
	}
}

// ── Scenario fixtures ─────────────────────────────────────────────────────────

const (
	sidecarRepo     = "registry.example.com/acme/sidecar"
	secondWarehouse = "second-warehouse"
)

func newTestReconciler(t *testing.T, objs ...client.Object) (*ApplicationReconciler, client.Client, *record.FakeRecorder) {
	t.Helper()
	sch := newTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
	recorder := record.NewFakeRecorder(20)
	return &ApplicationReconciler{Client: cl, Scheme: sch, Recorder: recorder}, cl, recorder
}

// reconcileApp runs one Reconcile for the default test Application.
func reconcileApp(t *testing.T, r *ApplicationReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testAppNS, Name: testAppName},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	return res
}

// appDeploying is an observed Application deploying repo:tag for each entry.
func appDeploying(images map[string]string) *unstructured.Unstructured {
	refs := make([]string, 0, len(images))
	for repo, tag := range images {
		refs = append(refs, repo+":"+tag)
	}
	sort.Strings(refs)
	return testApp(map[string]string{authorizedStageAnnotation: testNS + ":" + testStage}, refs)
}

func driftedApp() *unstructured.Unstructured {
	return appDeploying(map[string]string{testRepo: deployedTag})
}

func imageList(images map[string]string) []any {
	list := make([]any, 0, len(images))
	for repo, tag := range images {
		list = append(list, map[string]any{"repoURL": repo, "tag": tag})
	}
	return list
}

// stageWithOrigins is testStageObj whose requested Freight and current
// Freight are replaced by the given warehouse -> repo -> tag layout.
func stageWithOrigins(origins map[string]map[string]string) *unstructured.Unstructured {
	stage := testStageObj(currentTag, false)
	var requested []any
	items := map[string]any{}
	for warehouse, images := range origins {
		requested = append(requested, map[string]any{
			"origin": map[string]any{"kind": "Warehouse", "name": warehouse},
		})
		items["Warehouse/"+warehouse] = map[string]any{"name": "fr-current-" + warehouse, "images": imageList(images)}
	}
	_ = unstructured.SetNestedSlice(stage.Object, requested, "spec", "requestedFreight")
	_ = unstructured.SetNestedSlice(stage.Object, []any{map[string]any{"items": items}}, "status", "freightHistory")
	return stage
}

func warehouseSubscribing(name string, repos ...string) *unstructured.Unstructured {
	warehouse := testWarehouse()
	warehouse.SetName(name)
	subs := make([]any, 0, len(repos))
	for _, repo := range repos {
		subs = append(subs, map[string]any{"image": map[string]any{"repoURL": repo}})
	}
	_ = unstructured.SetNestedSlice(warehouse.Object, subs, "spec", "subscriptions")
	return warehouse
}

func freightCarrying(name, warehouse string, created time.Time, images map[string]string) *unstructured.Unstructured {
	freight := testFreight(name, "")
	freight.SetCreationTimestamp(metav1.NewTime(created))
	_ = unstructured.SetNestedField(freight.Object, warehouse, "origin", "name")
	_ = unstructured.SetNestedSlice(freight.Object, imageList(images), "images")
	return freight
}

// withLastPromotion records a finished promotion on the Stage.
func withLastPromotion(stage *unstructured.Unstructured, name string, finished time.Time) *unstructured.Unstructured {
	_ = unstructured.SetNestedMap(stage.Object, map[string]any{
		"name":       name,
		"finishedAt": finished.UTC().Format(time.RFC3339),
	}, "status", "lastPromotion")
	return stage
}

func promotedFreight(t *testing.T, cl client.Client) []string {
	t.Helper()
	var names []string
	for _, p := range listObserverPromotions(t, cl) {
		freight, _, _ := unstructured.NestedString(p.Object, "spec", "freight")
		names = append(names, freight)
	}
	sort.Strings(names)
	return names
}

func freightMissingValue() float64 {
	return testutil.ToFloat64(freightMissing.WithLabelValues(testNS, testStage))
}

// ── Scenarios ─────────────────────────────────────────────────────────────────

func TestApplicationPredicateResyncAndCleanup(t *testing.T) {
	authorizedAnns := map[string]string{authorizedStageAnnotation: testNS + ":" + testStage}
	ignoredAnns := map[string]string{authorizedStageAnnotation: testNS + ":" + testStage, ignoreAnnotation: "true"}
	pred := (&ApplicationReconciler{}).applicationPredicate()

	// An informer resync delivers the same object as old and new; it is the
	// --sync-period backstop and must reach Reconcile.
	resynced := testApp(authorizedAnns, []string{testRepo + ":" + currentTag})
	resynced.SetResourceVersion("42")
	if !pred.Update(event.UpdateEvent{ObjectOld: resynced, ObjectNew: resynced.DeepCopy()}) {
		t.Error("Update(resync of observed application) = false, want true")
	}
	unobserved := testApp(nil, nil)
	unobserved.SetResourceVersion("42")
	if pred.Update(event.UpdateEvent{ObjectOld: unobserved, ObjectNew: unobserved.DeepCopy()}) {
		t.Error("Update(resync of unobserved application) = true, want false")
	}

	// An Application that stops being observed gets one last reconcile so its
	// metric contribution is dropped.
	if !pred.Update(event.UpdateEvent{ObjectOld: testApp(authorizedAnns, nil), ObjectNew: testApp(ignoredAnns, nil)}) {
		t.Error("Update(observed -> ignored) = false, want true")
	}
	if pred.Update(event.UpdateEvent{ObjectOld: testApp(ignoredAnns, nil), ObjectNew: testApp(ignoredAnns, nil)}) {
		t.Error("Update(ignored -> ignored) = true, want false")
	}
	if pred.Delete(event.DeleteEvent{Object: testApp(nil, nil)}) {
		t.Error("Delete(unobserved application) = true, want false")
	}
}

func TestReconcileSupersededObserverPromotion(t *testing.T) {
	// A -> B -> A: the observer promoted freightName two hours ago, Kargo has
	// since completed a later promotion, and the deployed tag is back at
	// freightName's. The old verdict must not block a new Promotion.
	now := time.Now()
	old := promotionName(testStage, freightName, now.Add(-2*time.Hour))
	later := promotionName(testStage, "fr-other", now.Add(-30*time.Minute))

	for _, phase := range []string{phaseSucceeded, phaseFailed} {
		t.Run("superseded "+phase+" promotion is retried", func(t *testing.T) {
			stage := withLastPromotion(testStageObj(currentTag, false), later, now.Add(-30*time.Minute))
			r, cl, _ := newTestReconciler(t, driftedApp(), stage, testWarehouse(),
				testFreight(freightName, deployedTag), testPromotion(old, freightName, phase))
			reconcileApp(t, r)
			if got := len(listObserverPromotions(t, cl)); got != 2 {
				t.Errorf("got %d observer promotions, want 2 (the old one plus a new one)", got)
			}
		})
	}

	t.Run("a later finish alone does not supersede", func(t *testing.T) {
		// An earlier-named promotion that finished after the observer's was
		// created is not newer: Kargo runs them in name order.
		earlier := promotionName(testStage, "fr-other", now.Add(-3*time.Hour))
		stage := withLastPromotion(testStageObj(currentTag, false), earlier, now.Add(-30*time.Minute))
		r, cl, _ := newTestReconciler(t, driftedApp(), stage, testWarehouse(),
			testFreight(freightName, deployedTag), testPromotion(old, freightName, phaseSucceeded))
		res := reconcileApp(t, r)
		if got := len(listObserverPromotions(t, cl)); got != 1 {
			t.Errorf("got %d observer promotions, want 1", got)
		}
		if res.RequeueAfter != 30*time.Second {
			t.Errorf("RequeueAfter = %v, want 30s", res.RequeueAfter)
		}
	})

	t.Run("a later name alone does not supersede", func(t *testing.T) {
		// The Stage's last promotion finished before the observer's was
		// created: its name only sorts later because the observer's clock
		// lags. Recreating here would loop.
		stage := withLastPromotion(testStageObj(currentTag, false), later, now.Add(-3*time.Hour))
		r, cl, _ := newTestReconciler(t, driftedApp(), stage, testWarehouse(),
			testFreight(freightName, deployedTag), testPromotion(old, freightName, phaseSucceeded))
		res := reconcileApp(t, r)
		if got := len(listObserverPromotions(t, cl)); got != 1 {
			t.Errorf("got %d observer promotions, want 1", got)
		}
		if res.RequeueAfter != 30*time.Second {
			t.Errorf("RequeueAfter = %v, want 30s", res.RequeueAfter)
		}
	})
}

func TestReconcileAbortedPromotionIsNotRecreated(t *testing.T) {
	r, cl, recorder := newTestReconciler(t, driftedApp(), testStageObj(currentTag, false), testWarehouse(),
		testFreight(freightName, deployedTag), testPromotion("qa.aborted", freightName, phaseAborted))
	reconcileApp(t, r)
	if got := len(listObserverPromotions(t, cl)); got != 1 {
		t.Errorf("got %d observer promotions, want 1 (an aborted promotion must not be recreated)", got)
	}
	if events := drainEvents(recorder); !eventsContain(events, "PromotionPreviouslyFailed") {
		t.Errorf("events %v missing PromotionPreviouslyFailed", events)
	}
}

func TestReconcileWaitsForStageToSettle(t *testing.T) {
	// ArgoCD can still report the previous tag right after a promotion
	// finished; acting on that would promote the Stage straight back.
	t.Run("recently finished promotion", func(t *testing.T) {
		stage := withLastPromotion(testStageObj(currentTag, false), "qa.native", time.Now().Add(-time.Minute))
		r, cl, recorder := newTestReconciler(t, driftedApp(), stage, testWarehouse(), testFreight(freightName, deployedTag))
		res := reconcileApp(t, r)
		if got := len(listObserverPromotions(t, cl)); got != 0 {
			t.Errorf("got %d observer promotions, want 0", got)
		}
		if res.RequeueAfter <= 0 || res.RequeueAfter > settlePeriod {
			t.Errorf("RequeueAfter = %v, want within (0, %v]", res.RequeueAfter, settlePeriod)
		}
		if events := drainEvents(recorder); len(events) > 0 {
			t.Errorf("unexpected events: %v", events)
		}
	})
	t.Run("settled", func(t *testing.T) {
		stage := withLastPromotion(testStageObj(currentTag, false), "qa.native", time.Now().Add(-settlePeriod-time.Minute))
		r, cl, _ := newTestReconciler(t, driftedApp(), stage, testWarehouse(), testFreight(freightName, deployedTag))
		reconcileApp(t, r)
		if got := len(listObserverPromotions(t, cl)); got != 1 {
			t.Errorf("got %d observer promotions, want 1", got)
		}
	})
}

func withSyncStatus(app *unstructured.Unstructured, status string) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(app.Object, status, "status", "sync", "status")
	return app
}

func TestReconcileRequiresSyncedApplication(t *testing.T) {
	// OutOfSync means the cluster is not running what git holds — typically
	// a Kargo promotion ArgoCD has not applied yet (sync window, manual
	// sync). Promoting to the images still running would undo it.
	for _, tc := range []struct {
		status         string
		wantPromotions int
	}{
		{status: "OutOfSync", wantPromotions: 0},
		{status: "Unknown", wantPromotions: 0},
		{status: "Synced", wantPromotions: 1},
	} {
		t.Run(tc.status, func(t *testing.T) {
			r, cl, recorder := newTestReconciler(t, withSyncStatus(driftedApp(), tc.status), testStageObj(currentTag, false),
				testWarehouse(), testFreight(freightName, deployedTag))
			res := reconcileApp(t, r)
			if got := len(listObserverPromotions(t, cl)); got != tc.wantPromotions {
				t.Errorf("got %d observer promotions, want %d", got, tc.wantPromotions)
			}
			if tc.wantPromotions == 0 {
				if res != (ctrl.Result{}) {
					t.Errorf("Reconcile() result = %+v, want zero", res)
				}
				if events := drainEvents(recorder); len(events) > 0 {
					t.Errorf("unexpected events: %v", events)
				}
			}
		})
	}
}

func TestApplicationPredicateSyncTransition(t *testing.T) {
	pred := (&ApplicationReconciler{}).applicationPredicate()
	outOfSync := withSyncStatus(driftedApp(), "OutOfSync")
	synced := withSyncStatus(driftedApp(), "Synced")
	// Becoming Synced lifts the gate in Reconcile, so it must be delivered.
	if !pred.Update(event.UpdateEvent{ObjectOld: outOfSync, ObjectNew: synced}) {
		t.Error("Update(OutOfSync -> Synced) = false, want true")
	}
	if pred.Update(event.UpdateEvent{ObjectOld: synced, ObjectNew: outOfSync}) {
		t.Error("Update(Synced -> OutOfSync) = true, want false")
	}
}

func TestReconcileIgnoresRepoDeployedWithTwoTags(t *testing.T) {
	// ArgoCD lists the images of live Pods, so a rollout shows the old and
	// the new tag at once. "1.9.0" sorts after "1.10.0": last-wins would read
	// the outgoing tag as deployed and promote the Stage back to it.
	app := testApp(map[string]string{authorizedStageAnnotation: testNS + ":" + testStage},
		[]string{testRepo + ":1.10.0", testRepo + ":1.9.0"})
	r, cl, recorder := newTestReconciler(t, app, testStageObj("1.10.0", false), testWarehouse(),
		testFreight("fr-old", "1.9.0"), testFreight("fr-new", "1.10.0"))
	res := reconcileApp(t, r)
	if got := len(listObserverPromotions(t, cl)); got != 0 {
		t.Errorf("got %d observer promotions, want 0", got)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("Reconcile() result = %+v, want zero", res)
	}
	if events := drainEvents(recorder); len(events) > 0 {
		t.Errorf("unexpected events: %v", events)
	}
}

func TestReconcileFreightSelection(t *testing.T) {
	now := time.Now()

	t.Run("freight whose other images differ is not promoted", func(t *testing.T) {
		// Hand-bump of web only. fr-a also carries the bumped web tag but an
		// older sidecar; promoting it would roll the sidecar back.
		stage := stageWithOrigins(map[string]map[string]string{warehouseName: {testRepo: currentTag, sidecarRepo: "5"}})
		app := appDeploying(map[string]string{testRepo: deployedTag, sidecarRepo: "5"})
		r, cl, _ := newTestReconciler(t, app, stage, warehouseSubscribing(warehouseName, testRepo, sidecarRepo),
			freightCarrying("fr-a", warehouseName, now, map[string]string{testRepo: deployedTag, sidecarRepo: "4"}),
			freightCarrying("fr-b", warehouseName, now.Add(-time.Hour), map[string]string{testRepo: deployedTag, sidecarRepo: "5"}))
		reconcileApp(t, r)
		if got := promotedFreight(t, cl); !reflect.DeepEqual(got, []string{"fr-b"}) {
			t.Errorf("promoted freight = %v, want [fr-b]", got)
		}
	})

	t.Run("no exact freight reports FreightMissing", func(t *testing.T) {
		stage := stageWithOrigins(map[string]map[string]string{warehouseName: {testRepo: currentTag, sidecarRepo: "5"}})
		app := appDeploying(map[string]string{testRepo: deployedTag, sidecarRepo: "5"})
		r, cl, recorder := newTestReconciler(t, app, stage, warehouseSubscribing(warehouseName, testRepo, sidecarRepo),
			freightCarrying("fr-a", warehouseName, now, map[string]string{testRepo: deployedTag, sidecarRepo: "4"}))
		reconcileApp(t, r)
		if got := promotedFreight(t, cl); len(got) != 0 {
			t.Errorf("promoted freight = %v, want none", got)
		}
		if events := drainEvents(recorder); !eventsContain(events, "FreightMissing") {
			t.Errorf("events %v missing FreightMissing", events)
		}
	})

	t.Run("an image only another application deploys must stay at the stage's current tag", func(t *testing.T) {
		stage := stageWithOrigins(map[string]map[string]string{warehouseName: {testRepo: currentTag, sidecarRepo: "5"}})
		r, cl, _ := newTestReconciler(t, driftedApp(), stage, warehouseSubscribing(warehouseName, testRepo, sidecarRepo),
			freightCarrying("fr-a", warehouseName, now, map[string]string{testRepo: deployedTag, sidecarRepo: "4"}),
			freightCarrying("fr-b", warehouseName, now.Add(-time.Hour), map[string]string{testRepo: deployedTag, sidecarRepo: "5"}))
		reconcileApp(t, r)
		if got := promotedFreight(t, cl); !reflect.DeepEqual(got, []string{"fr-b"}) {
			t.Errorf("promoted freight = %v, want [fr-b]", got)
		}
	})

	t.Run("a tag held from another origin does not make an image untouched", func(t *testing.T) {
		// Both Warehouses carry the sidecar. Rolling web back to 4 must not
		// drag the main line's sidecar from 3 to 1 just because the stable
		// line happens to hold 1.
		stage := stageWithOrigins(map[string]map[string]string{
			warehouseName:   {testRepo: "5", sidecarRepo: "3"},
			secondWarehouse: {sidecarRepo: "1"},
		})
		r, cl, recorder := newTestReconciler(t, appDeploying(map[string]string{testRepo: "4"}), stage,
			warehouseSubscribing(warehouseName, testRepo, sidecarRepo), warehouseSubscribing(secondWarehouse, sidecarRepo),
			freightCarrying("fr-web4-sidecar1", warehouseName, now, map[string]string{testRepo: "4", sidecarRepo: "1"}))
		reconcileApp(t, r)
		if got := promotedFreight(t, cl); len(got) != 0 {
			t.Errorf("promoted freight = %v, want none", got)
		}
		if events := drainEvents(recorder); !eventsContain(events, "FreightMissing") {
			t.Errorf("events %v missing FreightMissing", events)
		}
	})

	t.Run("an origin the stage holds nothing from is still bound by the other origins", func(t *testing.T) {
		// The stable-line Warehouse was never promoted to the Stage. Its
		// Freight must not bring a sidecar tag the Stage does not run.
		stage := stageWithOrigins(map[string]map[string]string{
			warehouseName:   {testRepo: "5", sidecarRepo: "3"},
			secondWarehouse: {},
		})
		r, cl, recorder := newTestReconciler(t, appDeploying(map[string]string{testRepo: "4"}), stage,
			warehouseSubscribing(warehouseName, testRepo, sidecarRepo), warehouseSubscribing(secondWarehouse, testRepo, sidecarRepo),
			freightCarrying("fr-stable-web4-sidecar1", secondWarehouse, now, map[string]string{testRepo: "4", sidecarRepo: "1"}))
		reconcileApp(t, r)
		if got := promotedFreight(t, cl); len(got) != 0 {
			t.Errorf("promoted freight = %v, want none", got)
		}
		if events := drainEvents(recorder); !eventsContain(events, "FreightMissing") {
			t.Errorf("events %v missing FreightMissing", events)
		}
	})

	t.Run("an image the stage holds no tag for is unconstrained", func(t *testing.T) {
		// The sidecar subscription is new: the Stage's current Freight
		// predates it, so there is nothing to keep it at.
		r, cl, _ := newTestReconciler(t, driftedApp(), testStageObj(currentTag, false),
			warehouseSubscribing(warehouseName, testRepo, sidecarRepo),
			freightCarrying("fr-a", warehouseName, now, map[string]string{testRepo: deployedTag, sidecarRepo: "9"}))
		reconcileApp(t, r)
		if got := promotedFreight(t, cl); !reflect.DeepEqual(got, []string{"fr-a"}) {
			t.Errorf("promoted freight = %v, want [fr-a]", got)
		}
	})

	t.Run("newest of several matching freight wins", func(t *testing.T) {
		r, cl, _ := newTestReconciler(t, driftedApp(), testStageObj(currentTag, false), testWarehouse(),
			freightCarrying("fr-a-older", warehouseName, now.Add(-time.Hour), map[string]string{testRepo: deployedTag}),
			freightCarrying("fr-z-newer", warehouseName, now, map[string]string{testRepo: deployedTag}),
			freightCarrying("fr-m-oldest", warehouseName, now.Add(-2*time.Hour), map[string]string{testRepo: deployedTag}))
		reconcileApp(t, r)
		if got := promotedFreight(t, cl); !reflect.DeepEqual(got, []string{"fr-z-newer"}) {
			t.Errorf("promoted freight = %v, want [fr-z-newer]", got)
		}
	})

	t.Run("drift across two warehouses promotes one origin at a time", func(t *testing.T) {
		// Each Freight comes from one Warehouse, so no single Freight can
		// carry both drifted repos.
		stage := stageWithOrigins(map[string]map[string]string{
			warehouseName:   {testRepo: currentTag},
			secondWarehouse: {sidecarRepo: "4"},
		})
		app := appDeploying(map[string]string{testRepo: deployedTag, sidecarRepo: "5"})
		r, cl, recorder := newTestReconciler(t, app, stage,
			warehouseSubscribing(warehouseName, testRepo), warehouseSubscribing(secondWarehouse, sidecarRepo),
			freightCarrying("fr-web", warehouseName, now, map[string]string{testRepo: deployedTag}),
			freightCarrying("fr-sidecar", secondWarehouse, now, map[string]string{sidecarRepo: "5"}))
		reconcileApp(t, r)
		if got := len(listObserverPromotions(t, cl)); got != 1 {
			t.Errorf("got %d observer promotions, want 1", got)
		}
		if events := drainEvents(recorder); eventsContain(events, "FreightMissing") {
			t.Errorf("unexpected FreightMissing: %v", events)
		}
	})

	t.Run("a repo carried by two origins is in sync when either matches", func(t *testing.T) {
		// A stable-line Warehouse next to the main one: both subscribe to the
		// same repo and hold different tags.
		stage := stageWithOrigins(map[string]map[string]string{
			warehouseName:   {testRepo: deployedTag},
			secondWarehouse: {testRepo: currentTag},
		})
		for range 20 { // map iteration order must not matter
			r, cl, recorder := newTestReconciler(t, driftedApp(), stage.DeepCopy(),
				warehouseSubscribing(warehouseName, testRepo), warehouseSubscribing(secondWarehouse, testRepo),
				freightCarrying("fr-web", warehouseName, now, map[string]string{testRepo: deployedTag}))
			reconcileApp(t, r)
			if got := len(listObserverPromotions(t, cl)); got != 0 {
				t.Fatalf("got %d observer promotions, want 0", got)
			}
			if events := drainEvents(recorder); len(events) > 0 {
				t.Fatalf("unexpected events: %v", events)
			}
		}
	})
}

func TestReconcileApplicationsDisagree(t *testing.T) {
	other := appDeploying(map[string]string{testRepo: currentTag})
	other.SetName("other-app")

	t.Run("another application of the stage deploys a different tag", func(t *testing.T) {
		// Promoting for one would be undone by the reconcile of the other.
		r, cl, recorder := newTestReconciler(t, driftedApp(), other.DeepCopy(), testStageObj(currentTag, false),
			testWarehouse(), testFreight(freightName, deployedTag))
		res := reconcileApp(t, r)
		if got := len(listObserverPromotions(t, cl)); got != 0 {
			t.Errorf("got %d observer promotions, want 0", got)
		}
		if res != (ctrl.Result{}) {
			t.Errorf("Reconcile() result = %+v, want zero", res)
		}
		if events := drainEvents(recorder); !eventsContain(events, "ApplicationsDisagree") {
			t.Errorf("events %v missing ApplicationsDisagree", events)
		}
	})

	t.Run("an application that does not deploy the repository does not count", func(t *testing.T) {
		unrelated := appDeploying(map[string]string{sidecarRepo: "1"})
		unrelated.SetName("other-app")
		r, cl, _ := newTestReconciler(t, driftedApp(), unrelated, testStageObj(currentTag, false),
			testWarehouse(), testFreight(freightName, deployedTag))
		reconcileApp(t, r)
		if got := len(listObserverPromotions(t, cl)); got != 1 {
			t.Errorf("got %d observer promotions, want 1", got)
		}
	})

	t.Run("an application the stage does not list does not count", func(t *testing.T) {
		// Otherwise annotating any Application would block the Stage.
		stage := testStageObj(currentTag, false)
		stage.SetAnnotations(map[string]string{applicationsAnnotation: testAppNS + "/" + testAppName})
		r, cl, _ := newTestReconciler(t, driftedApp(), other.DeepCopy(), stage,
			testWarehouse(), testFreight(freightName, deployedTag))
		r.RequireStageConsent = true
		reconcileApp(t, r)
		if got := len(listObserverPromotions(t, cl)); got != 1 {
			t.Errorf("got %d observer promotions, want 1", got)
		}
	})

	t.Run("an ignored application does not count", func(t *testing.T) {
		ignored := other.DeepCopy()
		ignored.SetAnnotations(map[string]string{authorizedStageAnnotation: testNS + ":" + testStage, ignoreAnnotation: "true"})
		r, cl, _ := newTestReconciler(t, driftedApp(), ignored, testStageObj(currentTag, false),
			testWarehouse(), testFreight(freightName, deployedTag))
		reconcileApp(t, r)
		if got := len(listObserverPromotions(t, cl)); got != 1 {
			t.Errorf("got %d observer promotions, want 1", got)
		}
	})
}

func TestReconcileWaitsForForeignPromotion(t *testing.T) {
	// A Promotion someone else created that Kargo has not picked up yet is
	// not in status.currentPromotion; queueing behind it would undo it.
	pending := testPromotion("qa.native-pending", "fr-other", "")
	pending.SetLabels(nil)
	r, cl, _ := newTestReconciler(t, driftedApp(), testStageObj(currentTag, false), testWarehouse(),
		testFreight(freightName, deployedTag), pending)
	res := reconcileApp(t, r)
	if got := len(listObserverPromotions(t, cl)); got != 0 {
		t.Errorf("got %d observer promotions, want 0", got)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("RequeueAfter = %v, want 30s", res.RequeueAfter)
	}
}

func TestReconcileStageConsent(t *testing.T) {
	// With --require-stage-consent the Application's annotation alone is not
	// enough: the Stage has to name the Application.
	for _, tc := range []struct {
		name           string
		listed         string
		wantPromotions int
	}{
		{name: "stage does not list the application", wantPromotions: 0},
		{name: "stage lists another application", listed: "argocd/other-app", wantPromotions: 0},
		{name: "stage lists the application", listed: "argocd/other-app, " + testAppNS + "/" + testAppName, wantPromotions: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage := testStageObj(currentTag, false)
			if tc.listed != "" {
				stage.SetAnnotations(map[string]string{applicationsAnnotation: tc.listed})
			}
			r, cl, recorder := newTestReconciler(t, driftedApp(), stage, testWarehouse(), testFreight(freightName, deployedTag))
			r.RequireStageConsent = true
			reconcileApp(t, r)
			if got := len(listObserverPromotions(t, cl)); got != tc.wantPromotions {
				t.Errorf("got %d observer promotions, want %d", got, tc.wantPromotions)
			}
			if events := drainEvents(recorder); tc.wantPromotions == 0 && len(events) > 0 {
				t.Errorf("unexpected events: %v", events)
			}
		})
	}
}

func TestReconcilePromotionCreateFailed(t *testing.T) {
	sch := newTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(driftedApp(), testStageObj(currentTag, false), testWarehouse(), testFreight(freightName, deployedTag)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return errors.New("admission webhook denied the request")
			},
		}).Build()
	recorder := record.NewFakeRecorder(20)
	r := &ApplicationReconciler{Client: cl, Scheme: sch, Recorder: recorder}
	before := testutil.ToFloat64(promotionCreateErrorsTotal.WithLabelValues(testNS, testStage))

	// The error is swallowed (an admission rejection would hot-loop the
	// workqueue) and retried on a slow timer instead.
	res := reconcileApp(t, r)
	if res.RequeueAfter != 5*time.Minute {
		t.Errorf("RequeueAfter = %v, want 5m", res.RequeueAfter)
	}
	if events := drainEvents(recorder); !eventsContain(events, "PromotionCreateFailed") {
		t.Errorf("events %v missing PromotionCreateFailed", events)
	}
	if got := testutil.ToFloat64(promotionCreateErrorsTotal.WithLabelValues(testNS, testStage)); got != before+1 {
		t.Errorf("kargo_observer_promotion_create_errors_total = %v, want %v", got, before+1)
	}
}

func TestFreightMissingGaugeLifecycle(t *testing.T) {
	ctx := context.Background()
	// missingFreight sets up a drifted Application with no Freight to match
	// and reconciles it once, leaving the gauge at 1.
	missingFreight := func(t *testing.T, extra ...client.Object) (*ApplicationReconciler, client.Client) {
		t.Helper()
		objs := append([]client.Object{driftedApp(), testStageObj(currentTag, false), testWarehouse()}, extra...)
		r, cl, _ := newTestReconciler(t, objs...)
		reconcileApp(t, r)
		if got := freightMissingValue(); got != 1 {
			t.Fatalf("kargo_observer_freight_missing = %v, want 1", got)
		}
		return r, cl
	}
	get := func(t *testing.T, cl client.Client, gvk schema.GroupVersionKind, namespace, name string) *unstructured.Unstructured {
		t.Helper()
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := cl.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
			t.Fatal(err)
		}
		return obj
	}

	t.Run("dry-run clears it once the freight appears", func(t *testing.T) {
		r, cl := missingFreight(t)
		r.DryRun = true
		if err := cl.Create(ctx, testFreight(freightName, deployedTag)); err != nil {
			t.Fatal(err)
		}
		reconcileApp(t, r)
		if got := freightMissingValue(); got != 0 {
			t.Errorf("kargo_observer_freight_missing = %v, want 0", got)
		}
	})

	t.Run("one application missing freight is not hidden by another that is fine", func(t *testing.T) {
		fine := appDeploying(map[string]string{sidecarRepo: "1"})
		fine.SetName("other-app")
		r, _ := missingFreight(t, fine)
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testAppNS, Name: "other-app"}}); err != nil {
			t.Fatal(err)
		}
		if got := freightMissingValue(); got != 1 {
			t.Errorf("kargo_observer_freight_missing = %v, want 1", got)
		}
	})

	t.Run("waiting on a promotion keeps the last verdict", func(t *testing.T) {
		r, cl := missingFreight(t)
		stage := get(t, cl, stageGVK, testNS, testStage)
		_ = unstructured.SetNestedMap(stage.Object, map[string]any{"name": "someone-elses-promotion"}, "status", "currentPromotion")
		if err := cl.Update(ctx, stage); err != nil {
			t.Fatal(err)
		}
		if res := reconcileApp(t, r); res.RequeueAfter != 30*time.Second {
			t.Fatalf("RequeueAfter = %v, want 30s", res.RequeueAfter)
		}
		if got := freightMissingValue(); got != 1 {
			t.Errorf("kargo_observer_freight_missing = %v, want 1", got)
		}
	})

	t.Run("an out-of-sync application keeps the last verdict", func(t *testing.T) {
		r, cl := missingFreight(t)
		if err := cl.Update(ctx, withSyncStatus(get(t, cl, applicationGVK, testAppNS, testAppName), "OutOfSync")); err != nil {
			t.Fatal(err)
		}
		reconcileApp(t, r)
		if got := freightMissingValue(); got != 1 {
			t.Errorf("kargo_observer_freight_missing = %v, want 1", got)
		}
	})

	t.Run("a disagreement keeps the last verdict", func(t *testing.T) {
		// No Freight lookup happens on that path, so it must not read as
		// "freight found".
		r, cl := missingFreight(t)
		other := appDeploying(map[string]string{testRepo: currentTag})
		other.SetName("other-app")
		if err := cl.Create(ctx, other); err != nil {
			t.Fatal(err)
		}
		reconcileApp(t, r)
		if got := freightMissingValue(); got != 1 {
			t.Errorf("kargo_observer_freight_missing = %v, want 1", got)
		}
	})

	// Once nothing reports on the Stage the series must go away rather than
	// read 0 (or a stale 1) forever.
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, r *ApplicationReconciler, cl client.Client)
	}{
		{name: "application deleted", change: func(t *testing.T, _ *ApplicationReconciler, cl client.Client) {
			if err := cl.Delete(ctx, driftedApp()); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "application ignored", change: func(t *testing.T, _ *ApplicationReconciler, cl client.Client) {
			app := get(t, cl, applicationGVK, testAppNS, testAppName)
			app.SetAnnotations(map[string]string{authorizedStageAnnotation: testNS + ":" + testStage, ignoreAnnotation: "true"})
			if err := cl.Update(ctx, app); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "annotation malformed", change: func(t *testing.T, _ *ApplicationReconciler, cl client.Client) {
			app := get(t, cl, applicationGVK, testAppNS, testAppName)
			app.SetAnnotations(map[string]string{authorizedStageAnnotation: "no-colon-here"})
			if err := cl.Update(ctx, app); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "application moved to another stage", change: func(t *testing.T, _ *ApplicationReconciler, cl client.Client) {
			prod := testStageObj(currentTag, false)
			prod.SetName("prod")
			if err := cl.Create(ctx, prod); err != nil {
				t.Fatal(err)
			}
			app := get(t, cl, applicationGVK, testAppNS, testAppName)
			app.SetAnnotations(map[string]string{authorizedStageAnnotation: testNS + ":prod"})
			if err := cl.Update(ctx, app); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "stage deleted", change: func(t *testing.T, _ *ApplicationReconciler, cl client.Client) {
			if err := cl.Delete(ctx, testStageObj(currentTag, false)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "stage consent missing", change: func(_ *testing.T, r *ApplicationReconciler, _ client.Client) {
			r.RequireStageConsent = true
		}},
	} {
		t.Run("series removed: "+tc.name, func(t *testing.T) {
			r, cl := missingFreight(t)
			tc.change(t, r, cl)
			reconcileApp(t, r)
			if freightMissing.DeleteLabelValues(testNS, testStage) {
				t.Errorf("kargo_observer_freight_missing{%s,%s} still exported", testNS, testStage)
			}
			freightMissing.DeleteLabelValues(testNS, "prod")
		})
	}
}

func TestStagePredicateConsentAnnotation(t *testing.T) {
	// Annotations don't bump generation; granting consent must not wait for
	// the next resync.
	granted := testStageObj(currentTag, false)
	granted.SetAnnotations(map[string]string{applicationsAnnotation: testAppNS + "/" + testAppName})
	if !stagePredicate().Update(event.UpdateEvent{ObjectOld: testStageObj(currentTag, false), ObjectNew: granted}) {
		t.Error("stagePredicate must admit a change of the applications annotation")
	}
	other := testStageObj(currentTag, false)
	other.SetAnnotations(map[string]string{"kargo.akuity.io/refresh": "now"})
	if stagePredicate().Update(event.UpdateEvent{ObjectOld: testStageObj(currentTag, false), ObjectNew: other}) {
		t.Error("stagePredicate must filter unrelated annotation changes")
	}
}
