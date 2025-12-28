package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
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

	// 1. Database Credentials Logic (Vault vs local Env)
	var user, pass string
	if os.Getenv("DEV_MODE") == "true" {
		setupLog.Info("Running in DEV_MODE, using environment variables")
		user = os.Getenv("DB_USER")
		pass = os.Getenv("DB_PASS")
	} else {
		u, errUser := os.ReadFile("/vault/secrets/db-user")
		p, errPass := os.ReadFile("/vault/secrets/db-pass")
		if errUser != nil || errPass != nil {
			setupLog.Error(nil, "Unable to read Vault secrets at /vault/secrets/")
			os.Exit(1)
		}
		user = strings.TrimSpace(string(u))
		pass = strings.TrimSpace(string(p))
	}

	dbHost := os.Getenv("DB_HOST")
	dbName := os.Getenv("DB_NAME")
	clusterName := os.Getenv("CLUSTER_NAME")
	if clusterName == "" { clusterName = "unnamed-cluster" }

	// 2. Initialize DB Connection Pool
	dsn := fmt.Sprintf("host=%s user=%s password=%s dbname=%s sslmode=disable", dbHost, user, pass, dbName)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		setupLog.Error(err, "Unable to open database")
		os.Exit(1)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	// 3. Cache Filtering and Resync Configuration
	labelReq, _ := labels.NewRequirement("sre.org.io/inventorysync", selection.Equals, []string{"true"})
	selector := labels.NewSelector().Add(*labelReq)
	resyncPeriod := 10 * time.Hour

	// 4. Initialize Manager (Cluster-wide by default)
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "vm-inventory-sync-lock.sre.io",
		Cache: cache.Options{
			SyncPeriod: &resyncPeriod,
			ByObject: map[client.Object]cache.ByObject{
				&kubevirtv1.VirtualMachine{}: {Label: selector},
				&kubevirtv1.VirtualMachineInstance{}: {}, 
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "Unable to start manager")
		os.Exit(1)
	}

	// 5. Register Reconciler
	if err = (&controller.InventoryReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		DB:          db,
		ClusterName: clusterName,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller")
		os.Exit(1)
	}

	// 6. Health Checks
	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)

	setupLog.Info("Starting VM Inventory Manager", "cluster", clusterName)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Problem running manager")
		os.Exit(1)
	}
}
