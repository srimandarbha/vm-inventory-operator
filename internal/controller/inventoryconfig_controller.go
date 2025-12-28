package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1" // Added for PVC/PV lookups
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/controller"
)

type InventoryReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	DB          *sql.DB
	ClusterName string
}

func (r *InventoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	// 1. Fetch VM
	var vm kubevirtv1.VirtualMachine
	if err := r.Get(ctx, req.NamespacedName, &vm); err != nil {
		if apierrors.IsNotFound(err) {
			return r.handleDeletion(ctx, req)
		}
		return ctrl.Result{}, err
	}

	// FIX: Skip reconciliation if VM is currently being deleted to prevent duplicate history
	if !vm.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// 2. Fetch VMI
	var vmi kubevirtv1.VirtualMachineInstance
	vmiFound := r.Get(ctx, req.NamespacedName, &vmi) == nil

	// 3. Extract Data (with defaults for newly created VMs)
	status := string(vm.Status.PrintableStatus)
	if status == "" {
		status = "Provisioning" // Default for new VMs
	}

	nodeName, ipAddr := "", ""
	if vmiFound {
		nodeName = vmi.Status.NodeName
		if len(vmi.Status.Interfaces) > 0 {
			ipAddr = vmi.Status.Interfaces[0].IP
		}
	}

	cpu := 0
	if vm.Spec.Template.Spec.Domain.Resources.Requests != nil {
		cpu = int(vm.Spec.Template.Spec.Domain.Resources.Requests.Cpu().Value())
	}
	mem := ""
	if vm.Spec.Template.Spec.Domain.Resources.Requests != nil {
		mem = vm.Spec.Template.Spec.Domain.Resources.Requests.Memory().String()
	}
	
	osDistro := vm.Labels["kubevirt.io/os"]

	// 4. Database Transaction
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil { return ctrl.Result{RequeueAfter: 5 * time.Second}, err }
	defer tx.Rollback()

	annoData, _ := json.Marshal(vm.Annotations)

	// 5. Update Inventory (Upsert)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO vm_inventory (cluster_name, vm_name, namespace, status, node_name, ip_address, cpu_cores, memory_gb, os_distro, annotations, last_seen)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
		ON CONFLICT (cluster_name, vm_name, namespace) 
		DO UPDATE SET status=$4, node_name=$5, ip_address=$6, cpu_cores=$7, memory_gb=$8, os_distro=$9, annotations=$10, last_seen=NOW()`,
		r.ClusterName, vm.Name, vm.Namespace, status, nodeName, ipAddr, cpu, mem, osDistro, annoData)
	if err != nil { return ctrl.Result{}, err }

	// 6. Sync Disks + PVC/PV Details
	_, err = tx.ExecContext(ctx, "DELETE FROM vm_disks WHERE cluster_name=$1 AND vm_name=$2 AND namespace=$3", r.ClusterName, vm.Name, vm.Namespace)
	if err != nil { return ctrl.Result{}, err }

	for _, vol := range vm.Spec.Template.Spec.Volumes {
		claimName, vType := "", "unknown"
		sc, cap, phase := "", "", ""

		if vol.PersistentVolumeClaim != nil {
			claimName = vol.PersistentVolumeClaim.ClaimName
			vType = "pvc"
			
			// Fetch PVC for deep details
			pvc := &corev1.PersistentVolumeClaim{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: vm.Namespace, Name: claimName}, pvc); err == nil {
				phase = string(pvc.Status.Phase)
				if pvc.Spec.StorageClassName != nil { sc = *pvc.Spec.StorageClassName }
				if c, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok { cap = c.String() }
			}
		} else if vol.DataVolume != nil {
			claimName = vol.DataVolume.Name
			vType = "dataVolume"
		} else if vol.ContainerDisk != nil {
			vType = "containerDisk"
		}

		_, err = tx.ExecContext(ctx, `
            INSERT INTO vm_disks (cluster_name, vm_name, namespace, disk_name, claim_name, volume_type, storage_class, capacity_gb, volume_phase) 
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			r.ClusterName, vm.Name, vm.Namespace, vol.Name, claimName, vType, sc, cap, phase)
		if err != nil { return ctrl.Result{}, err }
	}

	// 7. Update History (Only if status actually changed to prevent spam)
	_, err = tx.ExecContext(ctx, `
        INSERT INTO vm_history (cluster_name, vm_name, namespace, status, node_name, action) 
        SELECT $1, $2, $3, $4, $5, 'SYNC'
        WHERE NOT EXISTS (
            SELECT 1 FROM vm_history 
            WHERE cluster_name=$1 AND vm_name=$2 AND namespace=$3 
            ORDER BY changed_at DESC LIMIT 1 
            HAVING status=$4 AND node_name=$5
        )`,
		r.ClusterName, vm.Name, vm.Namespace, status, nodeName)
	if err != nil { return ctrl.Result{}, err }

	if err := tx.Commit(); err != nil { return ctrl.Result{RequeueAfter: 5 * time.Second}, err }

	l.Info("Successfully synced VM", "vm", vm.Name, "status", status)
	return ctrl.Result{}, nil
}

func (r *InventoryReconciler) handleDeletion(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil { return ctrl.Result{}, err }
	defer tx.Rollback()

	// Final check: did we already log a delete for this VM recently?
	_, err = tx.ExecContext(ctx, `
        INSERT INTO vm_history (cluster_name, vm_name, namespace, action, status) 
        SELECT $1, $2, $3, 'DELETE', 'Deleted'
        WHERE NOT EXISTS (
            SELECT 1 FROM vm_history 
            WHERE cluster_name=$1 AND vm_name=$2 AND namespace=$3 AND action='DELETE'
            AND changed_at > NOW() - INTERVAL '1 minute'
        )`,
		r.ClusterName, req.Name, req.Namespace)
	if err != nil { return ctrl.Result{}, err }

	_, err = tx.ExecContext(ctx, "DELETE FROM vm_inventory WHERE cluster_name=$1 AND vm_name=$2 AND namespace=$3", r.ClusterName, req.Name, req.Namespace)
	if err != nil { return ctrl.Result{}, err }

	return ctrl.Result{}, tx.Commit()
}

func (r *InventoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubevirtv1.VirtualMachine{}).
		Watches(&kubevirtv1.VirtualMachineInstance{}, &handler.EnqueueRequestForObject{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}).
		WithEventFilter(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				// Handle VM Updates
				if oldVM, ok := e.ObjectOld.(*kubevirtv1.VirtualMachine); ok {
					newVM := e.ObjectNew.(*kubevirtv1.VirtualMachine)
					// Ignore updates if the VM is being deleted
					if !newVM.DeletionTimestamp.IsZero() { return false }
					
					return !reflect.DeepEqual(oldVM.Annotations, newVM.Annotations) ||
						oldVM.Status.PrintableStatus != newVM.Status.PrintableStatus ||
						oldVM.Generation != newVM.Generation
				}
				// Handle VMI Updates
				if oldVMI, ok := e.ObjectOld.(*kubevirtv1.VirtualMachineInstance); ok {
					newVMI := e.ObjectNew.(*kubevirtv1.VirtualMachineInstance)
					return oldVMI.Status.NodeName != newVMI.Status.NodeName || 
						!reflect.DeepEqual(oldVMI.Status.Interfaces, newVMI.Status.Interfaces)
				}
				return true
			},
		}).
		Complete(r)
}
