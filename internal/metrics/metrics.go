package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// Gauge to track non-compliant VMs per namespace
	NonSyncableVMs = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "inventory_sync_non_compliant_vms_total",
			Help: "Number of VMs missing the sre.org.io/inventorysync=true label",
		},
		[]string{"namespace"},
	)
)

func init() {
	// Register custom metrics with the global prometheus registry
	metrics.Registry.MustRegister(NonSyncableVMs)
}
