package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
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

	// 2. Filter out VMs mid-deletion
	if !vm.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// 3. Fetch VMI (Runtime)
	var vmi kubevirtv1.VirtualMachineInstance
	vmiFound := r.Get(ctx, req.NamespacedName, &vmi) == nil

	// 4. Extract Data
	status := string(vm.Status.PrintableStatus)
	if status == "" { status = "Provisioning" }

	nodeName, ipAddr := "", ""
	if vmiFound {
		nodeName = vmi.Status.NodeName
		if len(vmi.Status.Interfaces) > 0 {
			ipAddr = vmi.Status.Interfaces[0].IP
		}
	}

	// Resources
	cpu := 0
	mem := ""
	if vm.Spec.Template.Spec.Domain.Resources.Requests != nil {
		cpu = int(vm.Spec.Template.Spec.Domain.Resources.Requests.Cpu().Value())
		mem = vm.Spec.Template.Spec.Domain.Resources.Requests.Memory().String()
	}

	// 5. DB Transaction
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil { return ctrl.Result{RequeueAfter: 5 * time.Second}, err }
	defer tx.Rollback()

	annoData, _ := json.Marshal(vm.Annotations)

	// Update Main Inventory
	_, err = tx.ExecContext(ctx, `
		INSERT INTO vm_inventory (cluster_name, vm_name, namespace, status, node_name, ip_address, cpu_cores, memory_gb, os_distro, annotations, last_seen)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
		ON CONFLICT (cluster_name, vm_name, namespace) 
		DO UPDATE SET status=$4, node_name=$5, ip_address=$6, cpu_cores=$7, memory_gb=$8, os_distro=$9, annotations=$10, last_seen=NOW()`,
		r.ClusterName, vm.Name, vm.Namespace, status, nodeName, ipAddr, cpu, mem, vm.Labels["kubevirt.io/os"], annoData)
	if err != nil { return ctrl.Result{}, err }

	// 6. Sync Disks + PVC Details
	_, _ = tx.ExecContext(ctx, "DELETE FROM vm_disks WHERE cluster_name=$1 AND vm_name=$2 AND namespace=$3", r.ClusterName, vm.Name, vm.Namespace)
	for _, vol := range vm.Spec.Template.Spec.Volumes {
		claimName, vType, sc, cap, phase := "", "unknown", "", "", ""
		
		if vol.PersistentVolumeClaim != nil {
			claimName = vol.PersistentVolumeClaim.ClaimName
			vType = "pvc"
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

		_, _ = tx.ExecContext(ctx, `
			INSERT INTO vm_disks (cluster_name, vm_name, namespace, disk_name, claim_name, volume_type, storage_class, capacity_gb, volume_phase) 
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			r.ClusterName, vm.Name, vm.Namespace, vol.Name, claimName, vType, sc, cap, phase)
	}

	// 7. Deduplicated History Insert
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

	if err := tx.Commit(); err != nil { return ctrl.Result{RequeueAfter: 2 * time.Second}, err }

	return ctrl.Result{}, nil
}

func (r *InventoryReconciler) handleDeletion(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil { return ctrl.Result{}, err }
	defer tx.Rollback()

	// Only log DELETE once per minute per VM (prevents duplicate history entries)
	_, _ = tx.ExecContext(ctx, `
		INSERT INTO vm_history (cluster_name, vm_name, namespace, action, status) 
		SELECT $1, $2, $3, 'DELETE', 'Deleted'
		WHERE NOT EXISTS (
			SELECT 1 FROM vm_history 
			WHERE cluster_name=$1 AND vm_name=$2 AND namespace=$3 AND action='DELETE'
			AND changed_at > NOW() - INTERVAL '1 minute'
		)`, r.ClusterName, req.Name, req.Namespace)

	_, err = tx.ExecContext(ctx, "DELETE FROM vm_inventory WHERE cluster_name=$1 AND vm_name=$2 AND namespace=$3", r.ClusterName, req.Name, req.Namespace)
	
	return ctrl.Result{}, tx.Commit()
}

func (r *InventoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubevirtv1.VirtualMachine{}).
		Watches(&kubevirtv1.VirtualMachineInstance{}, &handler.EnqueueRequestForObject{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}).
		WithEventFilter(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				if oldVM, ok := e.ObjectOld.(*kubevirtv1.VirtualMachine); ok {
					newVM := e.ObjectNew.(*kubevirtv1.VirtualMachine)
					if !newVM.DeletionTimestamp.IsZero() { return false }
					return !reflect.DeepEqual(oldVM.Annotations, newVM.Annotations) ||
						oldVM.Status.PrintableStatus != newVM.Status.PrintableStatus ||
						oldVM.Generation != newVM.Generation
				}
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
