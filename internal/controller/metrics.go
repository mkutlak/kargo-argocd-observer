package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	promotionsCreatedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kargo_observer_promotions_created_total",
			Help: "Number of Kargo Promotions created by the observer.",
		},
		[]string{"namespace", "stage"},
	)

	freightMissing = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kargo_observer_freight_missing",
			Help: "1 when the deployed image tag has no matching Freight, 0 otherwise.",
		},
		[]string{"namespace", "stage"},
	)

	promotionCreateErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kargo_observer_promotion_create_errors_total",
			Help: "Number of failed attempts to create a Kargo Promotion.",
		},
		[]string{"namespace", "stage"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		promotionsCreatedTotal,
		freightMissing,
		promotionCreateErrorsTotal,
	)
}
