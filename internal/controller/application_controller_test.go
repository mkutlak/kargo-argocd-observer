package controller

import (
	"context"
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
				if !strings.HasPrefix(p.GetName(), testStage+"-observer-") {
					t.Errorf("promotion name %q missing %q prefix", p.GetName(), testStage+"-observer-")
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sch := newTestScheme(t)
			objs := append([]client.Object{tc.app}, tc.objs...)
			cl := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
			recorder := record.NewFakeRecorder(20)
			r := &ApplicationReconciler{Client: cl, Scheme: sch, Recorder: recorder, DryRun: tc.dryRun}

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
