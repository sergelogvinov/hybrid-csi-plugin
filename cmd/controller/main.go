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
	"flag"
	"net"
	"net/http"
	"os"

	proto "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"

	"github.com/sergelogvinov/hybrid-csi-plugin/pkg/csi"
	"github.com/sergelogvinov/hybrid-csi-plugin/pkg/tools"

	clientkubernetes "k8s.io/client-go/kubernetes"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"
)

var (
	version string
	commit  string

	showVersion = flag.Bool("version", false, "Print the version and exit.")
	csiEndpoint = flag.String("csi-address", "unix:///csi/csi.sock", "CSI Endpoint")

	nodeID = flag.String("node-id", "", "Node name")

	metricsAddress = flag.String("metrics-address", "", "The TCP network address where the HTTP server for metrics, will listen (example: `:8080`). By default the server is disabled.")
	metricsPath    = flag.String("metrics-path", "/metrics", "The HTTP path where prometheus metrics will be exposed.")

	kubeconfig = flag.String("kubeconfig", "", "Absolute path to the kubeconfig file. Either this or master needs to be set if the provisioner is being run out of cluster.")
)

func main() {
	klog.InitFlags(nil)
	flag.Set("logtostderr", "true") //nolint: errcheck
	flag.Parse()

	klog.V(2).InfoS("Version", "version", csi.DriverVersion, "csiVersion", csi.DriverSpecVersion, "gitVersion", version, "gitCommit", commit)

	if *showVersion {
		klog.Infof("Driver version %v, GitVersion %s", csi.DriverVersion, version)
		os.Exit(0)
	}

	if *csiEndpoint == "" {
		klog.Error("csi-address must be provided")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	kconfig, _, err := tools.BuildConfig(*kubeconfig, "")
	if err != nil {
		klog.Error(err, "Failed to build a Kubernetes config")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	clientset, err := clientkubernetes.NewForConfig(kconfig)
	if err != nil {
		klog.Error(err, "Failed to create a Clientset")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	nodeName := *nodeID
	if nodeName == "" {
		nodeName = os.Getenv("NODE_NAME")

		if nodeName == "" {
			klog.Fatalln("node-id or NODE_NAME environment must be provided")
		}
	}

	scheme, addr, err := csi.ParseEndpoint(*csiEndpoint)
	if err != nil {
		klog.Error(err, "Failed to parse endpoint")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	ctx := context.Background()

	listenConfig := new(net.ListenConfig)

	listener, err := listenConfig.Listen(ctx, scheme, addr)
	if err != nil {
		klog.ErrorS(err, "Failed to listen", "address", *csiEndpoint)
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	logErr := func(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		resp, rpcerr := handler(ctx, req)
		if rpcerr != nil {
			klog.ErrorS(rpcerr, "GRPC error")
		}

		return resp, rpcerr
	}

	opts := []grpc.ServerOption{
		grpc.UnaryInterceptor(logErr),
	}

	// Prepare http endpoint for metrics
	mux := http.NewServeMux()
	if *metricsAddress != "" {
		mux.Handle("/metrics", legacyregistry.Handler())

		go func() {
			klog.V(2).InfoS("Metrics listening", "address", *metricsAddress, "metricsPath", *metricsPath)

			err := http.ListenAndServe(*metricsAddress, mux)
			if err != nil {
				klog.ErrorS(err, "Failed to start HTTP server at specified address and metrics path", "address", addr, "metricsPath", *metricsPath)
			}
		}()
	}

	srv := grpc.NewServer(opts...)

	identityService := csi.NewIdentityService()

	controllerService, err := csi.NewControllerService(clientset)
	if err != nil {
		klog.ErrorS(err, "Failed to create controller service")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	nodeService := csi.NewNodeService(nodeName, clientset)

	proto.RegisterIdentityServer(srv, identityService)
	proto.RegisterControllerServer(srv, controllerService)
	proto.RegisterNodeServer(srv, nodeService)

	klog.InfoS("Listening for connection on address", "address", listener.Addr())

	if err := srv.Serve(listener); err != nil {
		klog.ErrorS(err, "Failed to run driver")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
}
