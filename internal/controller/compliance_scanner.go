// internal/controller/scanner.go
package controller

import (
	"context"
	"time"

	"github.com/srimandarbha/vm-inventory-operator/internal/metrics"

	kubevirtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	//"github.com/prometheus/client_golang/prometheus"
)

func StartComplianceScanner(ctx context.Context, c client.Client) {
	ticker := time.NewTicker(10 * time.Minute)
	for {
		select {
		case <-ticker.C:
			var vmList kubevirtv1.VirtualMachineList
			// Use a direct client to bypass the filtered cache
			c.List(ctx, &vmList)

			counts := make(map[string]float64)
			for _, vm := range vmList.Items {
				if vm.Labels["sre.org.io/inventorysync"] != "true" {
					counts[vm.Namespace]++
				}
			}

			// Update Prometheus
			for ns, count := range counts {
				//metrics.NonSyncableVMs.WithLabelValues(ns).Set(count)
				metrics.NonSyncableVMs.WithLabelValues(ns).Set(count)
			}
		case <-ctx.Done():
			return
		}
	}
}
