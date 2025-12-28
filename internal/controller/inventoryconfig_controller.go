package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

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

	// 1. Fetch the VM from the local cache
	var vm kubevirtv1.VirtualMachine
	err := r.Get(ctx, req.NamespacedName, &vm)

	// --- START TRANSACTION ---
	tx, errTx := r.DB.BeginTx(ctx, nil)
	if errTx != nil {
		l.Error(errTx, "Failed to start database transaction")
		return ctrl.Result{}, errTx
	}

	// Defer a rollback in case of any failure. 
	// If tx.Commit() is called first, Rollback() does nothing.
	defer tx.Rollback()

	// --- CASE 1: DELETION ---
	if err != nil {
		if apierrors.IsNotFound(err) {
			l.Info("EVENT: DELETION detected", "vm", req.Name)

			// Update Main Table
			_, err = tx.ExecContext(ctx, 
				"DELETE FROM vm_inventory WHERE vm_name = $1 AND namespace = $2 AND cluster_name = $3", 
				req.Name, req.Namespace, r.ClusterName)
			if err != nil { return ctrl.Result{}, err }

			// Update History Table
			_, err = tx.ExecContext(ctx, `
                INSERT INTO vm_history (vm_name, namespace, cluster_name, action)
                VALUES ($1, $2, $3, 'DELETE')`,
				req.Name, req.Namespace, r.ClusterName)
			if err != nil { return ctrl.Result{}, err }

			// Commit the deletion
			if err := tx.Commit(); err != nil { return ctrl.Result{}, err }
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// --- CASE 2: CREATE OR UPDATE ---
	action := "UPDATE"
	if vm.CreationTimestamp.Time.After(time.Now().Add(-30 * time.Second)) && vm.ResourceVersion == "1" {
		action = "CREATE"
	}

	annoData, _ := json.Marshal(vm.Annotations)

	// Update Main Table (UPSERT)
	_, err = tx.ExecContext(ctx, `
        INSERT INTO vm_inventory (cluster_name, vm_name, namespace, annotations, last_seen)
        VALUES ($1, $2, $3, $4, NOW())
        ON CONFLICT (cluster_name, vm_name, namespace) 
        DO UPDATE SET annotations = $4, last_seen = NOW()`,
		r.ClusterName, vm.Name, vm.Namespace, annoData)
	if err != nil { return ctrl.Result{}, err }

	// Update History Table
	_, err = tx.ExecContext(ctx, `
        INSERT INTO vm_history (cluster_name, vm_name, namespace, annotations, action)
        VALUES ($1, $2, $3, $4, $5)`,
		r.ClusterName, vm.Name, vm.Namespace, annoData, action)
	if err != nil { return ctrl.Result{}, err }

	// --- COMMIT TRANSACTION ---
	if err := tx.Commit(); err != nil {
		l.Error(err, "Failed to commit transaction")
		return ctrl.Result{}, err
	}

	l.Info("Successfully synced state and history", "vm", vm.Name, "action", action)
	return ctrl.Result{}, nil
}

func (r *InventoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubevirtv1.VirtualMachine{}).
		Complete(r)
}
