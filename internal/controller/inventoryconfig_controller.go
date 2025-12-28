package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
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

func (r *InventoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	l.Info("Reconciling VirtualMachine", "name", req.Name, "namespace", req.Namespace)

	// 1. Fetch VM
	var vm kubevirtv1.VirtualMachine
	if err := r.Get(ctx, req.NamespacedName, &vm); err != nil {
		if apierrors.IsNotFound(err) {
			l.Info("VM not found, calling handleDeletion")
			return r.handleDeletion(ctx, req)
		}
		l.Error(err, "Failed to fetch VM")
		return ctrl.Result{}, err
	}

	// 2. Filter out VMs mid-deletion
	if !vm.DeletionTimestamp.IsZero() {
		l.Info("VM has deletion timestamp, skipping sync until NotFound")
		return ctrl.Result{}, nil
	}

	// 3. Fetch VMI (Runtime context)
	var vmi kubevirtv1.VirtualMachineInstance
	vmiFound := r.Get(ctx, req.NamespacedName, &vmi) == nil

	// 4. Extract Data
	status := string(vm.Status.PrintableStatus)
	if status == "" {
		status = "Provisioning"
	}

	nodeName, ipAddr := "", ""
	if vmiFound {
		nodeName = vmi.Status.NodeName
		if len(vmi.Status.Interfaces) > 0 {
			ipAddr = vmi.Status.Interfaces[0].IP
		}
	}

	cpu := 0
	mem := ""
	if vm.Spec.Template.Spec.Domain.Resources.Requests != nil {
		// Convert milli-cores to whole cores safely for DB
		cpu = int(vm.Spec.Template.Spec.Domain.Resources.Requests.Cpu().MilliValue()) / 1000
		mem = vm.Spec.Template.Spec.Domain.Resources.Requests.Memory().String()
	}
	mem := "unknown"

    if mem == "" || mem == "unknown" {
        if vm.Spec.Template.Spec.Domain.Resources.Limits != nil {
            mem = vm.Spec.Template.Spec.Domain.Resources.Limits.Memory().String()
        }
    }

    if (mem == "" || mem == "unknown") && vm.Spec.Template.Spec.Domain.Memory != nil {
        mem = vm.Spec.Template.Spec.Domain.Memory.Guest.String()
    }
	
	osDistro := vm.Labels["kubevirt.io/template"]
	annoData, _ := json.Marshal(vm.Annotations).

    //l.Info("Inspecting VM Labels", "vm", vm.Name)
    //for key, value := range vm.Labels {
    //   l.Info("Found Label", "key", key, "value", value)
    //}
	// 5. Database Transaction
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		l.Error(err, "Failed to begin transaction")
		return ctrl.Result{}, err
	}
	defer tx.Rollback()

	// 6. Update Inventory (Fixed: Uses all extracted variables)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO vm_inventory (cluster_name, vm_name, namespace, status, node_name, ip_address, cpu_cores, memory_gb, os_distro, annotations, last_seen)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
		ON CONFLICT (cluster_name, vm_name, namespace) 
		DO UPDATE SET status=$4, node_name=$5, ip_address=$6, cpu_cores=$7, memory_gb=$8, os_distro=$9, annotations=$10, last_seen=NOW()`,
		r.ClusterName, vm.Name, vm.Namespace, status, nodeName, ipAddr, cpu, mem, osDistro, annoData)
	if err != nil {
		l.Error(err, "Failed to upsert inventory table")
		return ctrl.Result{}, err
	}

	// 7. Sync Disks (Strict error checking added to prevent aborted transactions)
	_, err = tx.ExecContext(ctx, "DELETE FROM vm_disks WHERE cluster_name=$1 AND vm_name=$2 AND namespace=$3", r.ClusterName, vm.Name, vm.Namespace)
	if err != nil {
		l.Error(err, "Failed to clear old disks")
		return ctrl.Result{}, err
	}

	for _, vol := range vm.Spec.Template.Spec.Volumes {
		claimName, vType, sc, cap, phase := "", "unknown", "", "", ""
		if vol.PersistentVolumeClaim != nil {
			claimName = vol.PersistentVolumeClaim.ClaimName
			vType = "pvc"
			pvc := &corev1.PersistentVolumeClaim{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: claimName}, pvc); err == nil {
				phase = string(pvc.Status.Phase)
				if pvc.Spec.StorageClassName != nil {
					sc = *pvc.Spec.StorageClassName
				}
				if c, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
					cap = c.String()
				}
			}
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO vm_disks (cluster_name, vm_name, namespace, disk_name, claim_name, volume_type, storage_class, capacity_gb, volume_phase) 
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			r.ClusterName, vm.Name, vm.Namespace, vol.Name, claimName, vType, sc, cap, phase)
		if err != nil {
			l.Error(err, "Disk sync failed", "disk", vol.Name)
			return ctrl.Result{}, err
		}
	}

	// 8. Deduplicated History Insert (Fixed: Removed HAVING, added subquery check)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO vm_history (cluster_name, vm_name, namespace, status, node_name, action) 
		SELECT $1, $2, $3, $4, $5, 'SYNC'
		WHERE NOT EXISTS (
			SELECT 1 FROM (
				SELECT status, node_name FROM vm_history 
				WHERE cluster_name=$1 AND vm_name=$2 AND namespace=$3 
				ORDER BY changed_at DESC LIMIT 1
			) AS last_state 
			WHERE last_state.status = $4 AND last_state.node_name = $5
		)`,
		r.ClusterName, vm.Name, vm.Namespace, status, nodeName)
	if err != nil {
		l.Error(err, "Failed to insert history")
		return ctrl.Result{}, err
	}

	// 9. Final Commit
	if err := tx.Commit(); err != nil {
		l.Error(err, "Failed to commit transaction")
		return ctrl.Result{}, err
	}

	l.Info("Successfully synced VM", "name", vm.Name)
	return ctrl.Result{}, nil
}

func (r *InventoryReconciler) handleDeletion(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer tx.Rollback()

	// 1. Delete Inventory (Cascade delete should handle vm_disks if setup correctly in SQL)
	_, err = tx.ExecContext(ctx, "DELETE FROM vm_inventory WHERE cluster_name=$1 AND vm_name=$2 AND namespace=$3", r.ClusterName, req.Name, req.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 2. Log History
	_, err = tx.ExecContext(ctx, `
		INSERT INTO vm_history (cluster_name, vm_name, namespace, action, status) 
		SELECT $1, $2, $3, 'DELETE', 'Deleted'
		WHERE NOT EXISTS (
			SELECT 1 FROM vm_history 
			WHERE cluster_name=$1 AND vm_name=$2 AND namespace=$3 AND action='DELETE'
			AND changed_at > NOW() - INTERVAL '1 minute'
		)`, r.ClusterName, req.Name, req.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	l.Info("Completed deletion sync for VM", "name", req.Name)
	return ctrl.Result{}, tx.Commit()
}

func (r *InventoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubevirtv1.VirtualMachine{}).
		Watches(&kubevirtv1.VirtualMachineInstance{}, &handler.EnqueueRequestForObject{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool { return true },
			DeleteFunc: func(e event.DeleteEvent) bool { return true },
			UpdateFunc: func(e event.UpdateEvent) bool {
				if oldVM, ok := e.ObjectOld.(*kubevirtv1.VirtualMachine); ok {
					newVM := e.ObjectNew.(*kubevirtv1.VirtualMachine)
					if !newVM.DeletionTimestamp.IsZero() {
						return false
					}
					return !reflect.DeepEqual(oldVM.Annotations, newVM.Annotations) ||
						oldVM.Status.PrintableStatus != newVM.Status.PrintableStatus ||
						oldVM.Generation != newVM.Generation
				}
				return true
			},
		}).
		Complete(r)
}
