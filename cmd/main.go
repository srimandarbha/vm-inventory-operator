package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	// Import the postgres driver
	_ "github.com/lib/pq"

	// Kubernetes and Controller-Runtime imports
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	// Local project imports
	"github.com/srimandarbha/vm-inventory-operator/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kubevirtv1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for manager.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// 1. Database Credentials Logic
	var user, pass string
	if os.Getenv("DEV_MODE") == "true" {
		setupLog.Info("Running in DEV_MODE, pulling credentials from Environment Variables")
		user = os.Getenv("DB_USER")
		pass = os.Getenv("DB_PASS")
	} else {
		setupLog.Info("Running in PRODUCTION mode, pulling credentials from Vault injected files")
		u, errUser := os.ReadFile("/vault/secrets/db-user")
		p, errPass := os.ReadFile("/vault/secrets/db-pass")
		if errUser != nil || errPass != nil {
			setupLog.Error(nil, "Unable to read Vault secrets at /vault/secrets/. Check sidecar injection.")
			os.Exit(1)
		}
		user = strings.TrimSpace(string(u))
		pass = strings.TrimSpace(string(p))
	}

	dbHost := os.Getenv("DB_HOST")
	dbName := os.Getenv("DB_NAME")
	if user == "" || pass == "" || dbHost == "" || dbName == "" {
		setupLog.Error(nil, "Missing DB configuration (User/Pass/Host/Name).")
		os.Exit(1)
	}

	// 2. Database Connection Pool Setup
	dsn := fmt.Sprintf("host=%s user=%s password=%s dbname=%s sslmode=disable", dbHost, user, pass, dbName)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		setupLog.Error(err, "Could not open SQL connection")
		os.Exit(1)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	// 3. Cache and Filter Configuration
	// Label Selector: Only sync VMs with this label
	labelReq, _ := labels.NewRequirement("sre.org.io/inventorysync", selection.Equals, []string{"true"})
	selector := labels.NewSelector().Add(*labelReq)
	
	// Resync Period: Forced full-sync every 10 hours to prevent DB drift
	resyncPeriod := 10 * time.Hour

	// 4. Initialize Manager with Cluster-wide Cache Options
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "vm-inventory-sync-lock.sre.io",
		Cache: cache.Options{
			SyncPeriod: &resyncPeriod,
			// Since DefaultNamespaces is not set, we operate CLUSTER-WIDE
			ByObject: map[client.Object]cache.ByObject{
				&kubevirtv1.VirtualMachine{}: {
					Label: selector,
				},
				// Watch all VMIs to catch IP/Node/Status changes for active VMs
				&kubevirtv1.VirtualMachineInstance{}: {}, 
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "Unable to start manager")
		os.Exit(1)
	}

	// 5. Register the Inventory Reconciler
	clusterName := os.Getenv("CLUSTER_NAME")
	if clusterName == "" {
		clusterName = "unnamed-cluster"
	}

	if err = (&controller.InventoryReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		DB:          db,
		ClusterName: clusterName,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "Inventory")
		os.Exit(1)
	}

	// 6. Kubernetes Health & Ready Probes
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up ready check")
		os.Exit(1)
	}

	// 7. Start the Manager
	setupLog.Info("Starting VM Inventory Manager", 
		"cluster", clusterName, 
		"resync_period", resyncPeriod.String(),
		"db_host", dbHost)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Problem running manager")
		os.Exit(1)
	}
}
