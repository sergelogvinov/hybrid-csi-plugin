/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Hybrid CSI Plugin Controller
package main

import (
	"context"
	goflag "flag"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kubernetes-csi/csi-lib-utils/leaderelection"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	flag "github.com/spf13/pflag"
	controller "sigs.k8s.io/sig-storage-lib-external-provisioner/v10/controller"
	libmetrics "sigs.k8s.io/sig-storage-lib-external-provisioner/v10/controller/metrics"

	"github.com/sergelogvinov/hybrid-csi-plugin/pkg/lifecycle"
	"github.com/sergelogvinov/hybrid-csi-plugin/pkg/provisioner"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"
)

var (
	version string
	commit  string

	// faultInject is set only in test builds, see faultinject.go.
	faultInject *string

	showVersion = flag.Bool("version", false, "Print the version and exit.")

	master       = flag.String("master", "", "Master URL to build a client config from. Either this or kubeconfig needs to be set if the provisioner is being run out of cluster.")
	kubeconfig   = flag.String("kubeconfig", "", "Absolute path to the kubeconfig file. Either this or master needs to be set if the provisioner is being run out of cluster.")
	kubeAPIQPS   = flag.Float32("kube-api-qps", 5, "QPS to use while communicating with the kubernetes apiserver. Defaults to 5.0.")
	kubeAPIBurst = flag.Int("kube-api-burst", 10, "Burst to use while communicating with the kubernetes apiserver. Defaults to 10.")

	httpEndpoint = flag.String("http-endpoint", "", "The TCP network address where the HTTP server for diagnostics, including pprof, metrics and leader election health check, will listen (example: `:8080`). The default is empty string, which means the server is disabled.")
	metricsPath  = flag.String("metrics-path", "/metrics", "The HTTP path where prometheus metrics will be exposed. Default is `/metrics`.")

	enableLeaderElection        = flag.Bool("leader-election", false, "Enables leader election. If leader election is enabled, additional RBAC rules are required. Please refer to the Kubernetes CSI documentation for instructions on setting up these RBAC rules.")
	leaderElectionNamespace     = flag.String("leader-election-namespace", "", "Namespace where the leader election resource lives. Defaults to the pod namespace if not set.")
	leaderElectionLeaseDuration = flag.Duration("leader-election-lease-duration", 15*time.Second, "Duration, in seconds, that non-leader candidates will wait to force acquire leadership. Defaults to 15 seconds.")
	leaderElectionRenewDeadline = flag.Duration("leader-election-renew-deadline", 10*time.Second, "Duration, in seconds, that the acting leader will retry refreshing leadership before giving up. Defaults to 10 seconds.")
	leaderElectionRetryPeriod   = flag.Duration("leader-election-retry-period", 5*time.Second, "Duration, in seconds, the LeaderElector clients should wait between tries of actions. Defaults to 5 seconds.")

	retryIntervalStart = flag.Duration("retry-interval-start", time.Second, "Initial retry interval of provisioning. It doubles with each pass while the backend is still provisioning, up to retry-interval-max.")
	retryIntervalMax   = flag.Duration("retry-interval-max", 5*time.Minute, "Maximum retry interval of provisioning.")

	helperTimeout        = flag.Duration("helper-timeout", provisioner.DefaultHelperTimeout, "How long a helper PVC may stay pending before the claim is rescheduled to another node.")
	helperMaxReschedules = flag.Int("helper-max-reschedules", 3, "How many times a claim may be rescheduled because of the helper timeout, after that only events are emitted.")

	fixReclaimPolicy = flag.Bool("fix-reclaim-policy", false, "Set the reclaim policy of volumes provisioned by v0.x to the one of their hybrid storage class. Without it the difference is only reported.")

	lifecycleWorkers = flag.Int("lifecycle-workers", 1, "Number of workers of the lifecycle controller.")
)

const (
	// DriverName is the name of the driver
	DriverName = "csi.hybrid.sinextra.dev"

	// ResyncPeriodOfCsiNodeInformer is the resync period of the informer for the CSINode objects
	ResyncPeriodOfCsiNodeInformer = 1 * time.Hour
)

func main() {
	var (
		config *rest.Config
		err    error
	)

	klog.InitFlags(nil)
	flag.CommandLine.AddGoFlagSet(goflag.CommandLine)
	flag.Set("logtostderr", "true") //nolint: errcheck
	flag.Parse()

	klog.V(2).InfoS("Version", "version", provisioner.DriverVersion, "gitVersion", version, "gitCommit", commit)

	ctx := context.Background()

	if *showVersion {
		klog.Infof("Driver name: %s, Driver version %v, GitVersion %s", DriverName, provisioner.DriverVersion, version)
		os.Exit(0)
	}

	// get the KUBECONFIG from env if specified (useful for local/debug cluster)
	kubeconfigEnv := os.Getenv("KUBECONFIG")

	if kubeconfigEnv != "" {
		klog.Infof("Found KUBECONFIG environment variable set, using that..")

		kubeconfig = &kubeconfigEnv
	}

	if *master != "" || *kubeconfig != "" {
		klog.Infof("Either master or kubeconfig specified. building kube config from that..")

		config, err = clientcmd.BuildConfigFromFlags(*master, *kubeconfig)
	} else {
		klog.Infof("Building kube configs for running in cluster...")

		config, err = rest.InClusterConfig()
	}

	if err != nil {
		klog.Fatalf("Failed to create config: %v", err)
	}

	config.QPS = *kubeAPIQPS
	config.Burst = *kubeAPIBurst

	coreConfig := rest.CopyConfig(config)
	coreConfig.ContentType = runtime.ContentTypeProtobuf

	clientset, err := kubernetes.NewForConfig(coreConfig)
	if err != nil {
		klog.ErrorS(err, "Failed to create a Clientset")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	// Generate a unique ID for this provisioner
	timeStamp := time.Now().UnixNano() / int64(time.Millisecond)
	identity := strconv.FormatInt(timeStamp, 10) + "-" + strconv.Itoa(rand.Intn(10000)) + "-" + DriverName

	factory := informers.NewSharedInformerFactory(clientset, ResyncPeriodOfCsiNodeInformer)

	listers := provisioner.NewListers(factory)

	// Provision returns ProvisioningInBackground while the backend is provisioning,
	// the rate limiter sets how often it is called again.
	rateLimiter := workqueue.NewTypedItemExponentialFailureRateLimiter[any](*retryIntervalStart, *retryIntervalMax)

	// Setup options
	provisionerOptions := []func(*controller.ProvisionController) error{
		controller.LeaderElection(false), // Always disable leader election in provisioner lib. Leader election should be done here in the CSI provisioner level instead.
		controller.FailedProvisionThreshold(0),
		controller.FailedDeleteThreshold(0),
		controller.RateLimiter(rateLimiter),
		// controller.ClaimsInformer(claimInformer),
		controller.NodesLister(listers.Nodes),
		// controller.VolumesInformer(volumeInformer),
	}

	broadcaster := record.NewBroadcaster(record.WithContext(ctx))
	broadcaster.StartStructuredLogging(0)
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: clientset.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: DriverName})

	csiProvisioner := provisioner.NewProvisioner(ctx, clientset, listers, provisioner.Options{
		HelperTimeout:        *helperTimeout,
		HelperMaxReschedules: *helperMaxReschedules,
		Recorder:             recorder,
	})

	if faultInject != nil && *faultInject != "" {
		klog.Warningf("Fault injection is enabled, the controller exits at %q", *faultInject)

		csiProvisioner.SetFaultHook(func(point string) {
			if point == *faultInject {
				klog.ErrorS(nil, "Fault injection: exiting", "point", point)
				klog.FlushAndExit(klog.ExitFlushTimeout, 1)
			}
		})
	}

	lifecycleController, err := lifecycle.New(clientset, csiProvisioner, factory, recorder, lifecycle.Options{
		FixReclaimPolicy: *fixReclaimPolicy,
	})
	if err != nil {
		klog.Fatalf("Failed to create lifecycle controller: %v", err)
	}

	// Prepare http endpoint for metrics + leader election healthz
	mux := http.NewServeMux()
	gatherers := prometheus.Gatherers{
		// For workqueue and leader election metrics, set up via the anonymous imports of:
		// https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/component-base/metrics/prometheus/workqueue/metrics.go
		// https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/component-base/metrics/prometheus/clientgo/leaderelection/metrics.go
		//
		// Also to happens to include Go runtime and process metrics:
		// https://github.com/kubernetes/kubernetes/blob/9780d88cb6a4b5b067256ecb4abf56892093ee87/staging/src/k8s.io/component-base/metrics/legacyregistry/registry.go#L46-L49
		legacyregistry.DefaultGatherer,
	}

	if *httpEndpoint != "" {
		m := libmetrics.New("controller")
		reg := prometheus.NewRegistry()
		reg.MustRegister([]prometheus.Collector{
			m.PersistentVolumeClaimProvisionTotal,
			m.PersistentVolumeClaimProvisionFailedTotal,
			m.PersistentVolumeClaimProvisionDurationSeconds,
			m.PersistentVolumeDeleteTotal,
			m.PersistentVolumeDeleteFailedTotal,
			m.PersistentVolumeDeleteDurationSeconds,
		}...)
		reg.MustRegister(csiProvisioner.Collectors()...)
		reg.MustRegister(lifecycleController.Collectors()...)

		provisionerOptions = append(provisionerOptions, controller.MetricsInstance(m))
		gatherers = append(gatherers, reg)

		mux.Handle(*metricsPath,
			promhttp.InstrumentMetricHandler(
				reg,
				promhttp.HandlerFor(gatherers, promhttp.HandlerOpts{})))

		go func() {
			klog.Infof("ServeMux listening at %q", *httpEndpoint)

			err := http.ListenAndServe(*httpEndpoint, mux)
			if err != nil {
				klog.Fatalf("Failed to start HTTP server at specified address (%q) and metrics path (%q): %s", *httpEndpoint, *metricsPath, err)
			}
		}()
	}

	provisionController := controller.NewProvisionController(
		klog.FromContext(ctx),
		clientset,
		DriverName,
		csiProvisioner,
		provisionerOptions...,
	)

	klog.InfoS("Starting the CSI Provisioner")

	run := func(ctx context.Context) {
		factory.Start(ctx.Done())

		cacheSyncResult := factory.WaitForCacheSync(ctx.Done())
		for _, v := range cacheSyncResult {
			if !v {
				klog.Fatalf("Failed to sync Informers!")
			}
		}

		go lifecycleController.Run(ctx, *lifecycleWorkers)

		provisionController.Run(ctx)
	}

	if !*enableLeaderElection {
		run(ctx)
	} else {
		// this lock name pattern is also copied from sigs.k8s.io/sig-storage-lib-external-provisioner/controller
		// to preserve backwards compatibility
		lockName := strings.ReplaceAll(DriverName, "/", "-")

		// create a new clientset for leader election
		leClientset, err := kubernetes.NewForConfig(coreConfig)
		if err != nil {
			klog.Fatalf("Failed to create leaderelection client: %v", err)
		}

		le := leaderelection.NewLeaderElection(leClientset, lockName, run)

		if *httpEndpoint != "" {
			le.PrepareHealthCheck(mux, leaderelection.DefaultHealthCheckTimeout)
		}

		if *leaderElectionNamespace != "" {
			le.WithNamespace(*leaderElectionNamespace)
		}

		le.WithLeaseDuration(*leaderElectionLeaseDuration)
		le.WithRenewDeadline(*leaderElectionRenewDeadline)
		le.WithRetryPeriod(*leaderElectionRetryPeriod)
		le.WithIdentity(identity)

		if err := le.Run(); err != nil {
			klog.Fatalf("failed to initialize leader election: %v", err)
		}
	}
}
