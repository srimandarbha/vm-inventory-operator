package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"     
	"sigs.k8s.io/controller-runtime/pkg/handler" 
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate" 
)

type InventoryReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	DB          *sql.DB
	ClusterName string
}

// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines;virtualmachineinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines/status,verbs=get

func (r *InventoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var vm kubevirtv1.VirtualMachine
	err := r.Get(ctx, req.NamespacedName, &vm)

	tx, errTx := r.DB.BeginTx(ctx, nil)
	if errTx != nil {
		l.Error(errTx, "Failed to start database transaction")
		return ctrl.Result{}, errTx
	}
	defer tx.Rollback()

	if err != nil {
		if apierrors.IsNotFound(err) {
			l.Info("EVENT: DELETION detected", "vm", req.Name)
			_, err = tx.ExecContext(ctx, 
				"DELETE FROM vm_inventory WHERE vm_name = $1 AND namespace = $2 AND cluster_name = $3", 
				req.Name, req.Namespace, r.ClusterName)
			if err != nil { return ctrl.Result{}, err }

			_, err = tx.ExecContext(ctx, `
				INSERT INTO vm_history (vm_name, namespace, cluster_name, action)
				VALUES ($1, $2, $3, 'DELETE')`,
				req.Name, req.Namespace, r.ClusterName)
			if err != nil { return ctrl.Result{}, err }

			if err := tx.Commit(); err != nil { return ctrl.Result{}, err }
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	action := "UPDATE"
	if vm.CreationTimestamp.Time.After(time.Now().Add(-30 * time.Second)) && vm.ResourceVersion == "1" {
		action = "CREATE"
	}

	annoData, _ := json.Marshal(vm.Annotations)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO vm_inventory (cluster_name, vm_name, namespace, annotations, last_seen)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (cluster_name, vm_name, namespace) 
		DO UPDATE SET annotations = $4, last_seen = NOW()`,
		r.ClusterName, vm.Name, vm.Namespace, annoData)
	if err != nil { return ctrl.Result{}, err }

	_, err = tx.ExecContext(ctx, `
		INSERT INTO vm_history (cluster_name, vm_name, namespace, annotations, action)
		VALUES ($1, $2, $3, $4, $5)`,
		r.ClusterName, vm.Name, vm.Namespace, annoData, action)
	if err != nil { return ctrl.Result{}, err }

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
		Watches(&kubevirtv1.VirtualMachineInstance{}, &handler.EnqueueRequestForObject{}).
		WithEventFilter(predicate.Funcs{
        UpdateFunc: func(e event.UpdateEvent) bool {
                if e.ObjectOld == nil || e.ObjectNew == nil {
                    return false
                }
                annotationsChanged := !reflect.DeepEqual(e.ObjectOld.GetAnnotations(), e.ObjectNew.GetAnnotations())
                labelsChanged := !reflect.DeepEqual(e.ObjectOld.GetLabels(), e.ObjectNew.GetLabels())

                return annotationsChanged || labelsChanged
            },
        }).
		Complete(r)
}
