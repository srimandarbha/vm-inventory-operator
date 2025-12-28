package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// InventoryReconciler reconciles a VirtualMachine object
type InventoryReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	DB          *sql.DB
	ClusterName string
}

// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines/status,verbs=get

func (r *InventoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	// Fetch the VM from the local cache
	var vm kubevirtv1.VirtualMachine
	if err := r.Get(ctx, req.NamespacedName, &vm); err != nil {
      if apierrors.IsNotFound(err) {
          // VM is gone! Run your SQL DELETE here
          _, err := r.DB.Exec("DELETE FROM vm_inventory WHERE vm_name = $1 AND namespace = $2", req.Name, req.Namespace)
          return ctrl.Result{}, err
      }
      return ctrl.Result{}, err
    }
	
    l.Info("Reconciling VirtualMachine", 
        "namespace", vm.Namespace, 
        "name", vm.Name,
        "cluster", r.ClusterName)

    l.Info("Fetching details", "database", "postgres", "interval", "5m")
	 // Logic to sync to Postgres
	annoData, _ := json.Marshal(vm.Annotations)
	_, err := r.DB.Exec(`
		INSERT INTO vm_inventory (cluster_name, vm_name, namespace, annotations, last_seen)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (cluster_name, vm_name, namespace) 
		DO UPDATE SET annotations = $4, last_seen = NOW()`,
		r.ClusterName, vm.Name, vm.Namespace, annoData)

	if err != nil {
		l.Error(err, "Database sync failed")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager is the missing piece you need!
func (r *InventoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubevirtv1.VirtualMachine{}). // Watch VirtualMachines
		Complete(r)
}
