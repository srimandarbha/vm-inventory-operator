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

	// 2. Fetch VMI (Runtime context)
	var vmi kubevirtv1.VirtualMachineInstance
	vmiFound := r.Get(ctx, req.NamespacedName, &vmi) == nil

	// 3. Extract Data
	status := string(vm.Status.PrintableStatus)
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
	mem := vm.Spec.Template.Spec.Domain.Resources.Requests.Memory().String()
	osDistro := vm.Labels["kubevirt.io/os"]

	// 4. Database Transaction with Retry Logic
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil { return ctrl.Result{RequeueAfter: 5 * time.Second}, err }
	defer tx.Rollback()

	annoData, _ := json.Marshal(vm.Annotations)

	// 5. Update Inventory
	_, err = tx.ExecContext(ctx, `
		INSERT INTO vm_inventory (cluster_name, vm_name, namespace, status, node_name, ip_address, cpu_cores, memory_gb, os_distro, annotations, last_seen)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
		ON CONFLICT (cluster_name, vm_name, namespace) 
		DO UPDATE SET status=$4, node_name=$5, ip_address=$6, cpu_cores=$7, memory_gb=$8, os_distro=$9, annotations=$10, last_seen=NOW()`,
		r.ClusterName, vm.Name, vm.Namespace, status, nodeName, ipAddr, cpu, mem, osDistro, annoData)
	if err != nil { return ctrl.Result{}, err }

	// 6. Sync Disks
	_, err = tx.ExecContext(ctx, "DELETE FROM vm_disks WHERE cluster_name=$1 AND vm_name=$2 AND namespace=$3", r.ClusterName, vm.Name, vm.Namespace)
	if err != nil { return ctrl.Result{}, err }

	for _, vol := range vm.Spec.Template.Spec.Volumes {
		claim, vType := "", "unknown"
		if vol.PersistentVolumeClaim != nil {
			claim, vType = vol.PersistentVolumeClaim.ClaimName, "pvc"
		} else if vol.DataVolume != nil {
			claim, vType = vol.DataVolume.Name, "dataVolume"
		} else if vol.ContainerDisk != nil {
			vType = "containerDisk"
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO vm_disks (cluster_name, vm_name, namespace, disk_name, claim_name, volume_type) VALUES ($1,$2,$3,$4,$5,$6)",
			r.ClusterName, vm.Name, vm.Namespace, vol.Name, claim, vType)
		if err != nil { return ctrl.Result{}, err }
	}

	// 7. Update History
	_, err = tx.ExecContext(ctx, "INSERT INTO vm_history (cluster_name, vm_name, namespace, status, node_name, action) VALUES ($1,$2,$3,$4,$5,'SYNC')",
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

	_, err = tx.ExecContext(ctx, "DELETE FROM vm_inventory WHERE cluster_name=$1 AND vm_name=$2 AND namespace=$3", r.ClusterName, req.Name, req.Namespace)
	if err != nil { return ctrl.Result{}, err }

	_, err = tx.ExecContext(ctx, "INSERT INTO vm_history (cluster_name, vm_name, namespace, action) VALUES ($1,$2,$3,'DELETE')", r.ClusterName, req.Name, req.Namespace)
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
				if oldVM, ok := e.ObjectOld.(*kubevirtv1.VirtualMachine); ok {
					newVM := e.ObjectNew.(*kubevirtv1.VirtualMachine)
					return !reflect.DeepEqual(oldVM.Annotations, newVM.Annotations) ||
						oldVM.Status.PrintableStatus != newVM.Status.PrintableStatus ||
						oldVM.Generation != newVM.Generation
				}
				return true // Always sync VMI updates (IP/Node)
			},
		}).
		Complete(r)
}
