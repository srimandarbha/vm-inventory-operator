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
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

    var user, pass string
    var err error
	
    if os.Getenv("DEV_MODE") == "true" {
        setupLog.Info("Running in DEV_MODE, using environment variables for DB credentials")
        user = os.Getenv("DB_USER")
        pass = os.Getenv("DB_PASS")
    } else {
        u, errUser := os.ReadFile("/vault/secrets/db-user")
        p, errPass := os.ReadFile("/vault/secrets/db-pass")
        
        if errUser != nil || errPass != nil {
            setupLog.Error(nil, "Failed to read Vault secrets. Check sidecar injection.")
            os.Exit(1)
        }
        
        user = strings.TrimSpace(string(u))
        pass = strings.TrimSpace(string(p))
    }
    
    dbHost := os.Getenv("DB_HOST") 
    dbName := os.Getenv("DB_NAME")

    // Validate connectivity details
    if user == "" || pass == "" || dbHost == "" || dbName == "" {
        setupLog.Error(nil, "Missing DB configuration. Check ENV vars (DB_HOST, DB_NAME, etc.)")
        os.Exit(1)
    }

	dsn := fmt.Sprintf("host=%s user=%s password=%s dbname=%s sslmode=disable", 
		dbHost, user, pass, dbName)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		setupLog.Error(err, "unable to connect to database")
		os.Exit(1)
	}

	// SRE Best Practice: Connection Pooling for Transactions
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	// 2. Setup Label Filter Selector (Only sync VMs with this label)
	labelReq, _ := labels.NewRequirement("sre.org.io/inventorysync", selection.Equals, []string{"true"})
	selector := labels.NewSelector().Add(*labelReq)

	// 3. Initialize Manager
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		WebhookServer: webhook.NewServer(webhook.Options{
			Port: 9443,
		}),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "inventory-sync-lock.my.sre.io",
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&kubevirtv1.VirtualMachine{}: {
					Label: selector,
				},
				// Note: We don't filter VMIs so we can always catch IP updates
				&kubevirtv1.VirtualMachineInstance{}: {}, 
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// 4. Get Cluster Name from Env (Helm values.yaml)
	clusterName := os.Getenv("CLUSTER_NAME")
	if clusterName == "" {
		clusterName = "unknown-cluster"
	}

	// 5. Register the Inventory Reconciler
	if err = (&controller.InventoryReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		DB:          db,
		ClusterName: clusterName,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Inventory")
		os.Exit(1)
	}

	// 6. Health Checks
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager", "cluster", clusterName, "db_host", dbHost)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
