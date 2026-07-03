package controller

import (
	"context"
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

	t.Run("delete always false", func(t *testing.T) {
		r := &ApplicationReconciler{}
		pred := r.applicationPredicate()
		if got := pred.Delete(event.DeleteEvent{Object: testApp(authorizedAnns, nil)}); got {
			t.Errorf("Delete() = true, want false")
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
