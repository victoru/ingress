package controller

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/pflag"

	"k8s.io/kubernetes/pkg/api"
	client "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	"k8s.io/kubernetes/pkg/client/unversioned/clientcmd"
	clientcmdapi "k8s.io/kubernetes/pkg/client/unversioned/clientcmd/api"
	"k8s.io/kubernetes/pkg/healthz"

	"k8s.io/ingress/core/pkg/ingress"
	"k8s.io/ingress/core/pkg/k8s"
)

// NewIngressController returns a configured Ingress controller
func NewIngressController(backend ingress.Controller) *GenericController {
	var (
		flags = pflag.NewFlagSet("", pflag.ExitOnError)

		apiserverHost = flags.String("apiserver-host", "", "The address of the Kubernetes Apiserver "+
			"to connect to in the format of protocol://address:port, e.g., "+
			"http://localhost:8080. If not specified, the assumption is that the binary runs inside a "+
			"Kubernetes cluster and local discovery is attempted.")
		kubeConfigFile = flags.String("kubeconfig", "", "Path to kubeconfig file with authorization and master location information.")

		defaultSvc = flags.String("default-backend-service", "",
			`Service used to serve a 404 page for the default backend. Takes the form
    	namespace/name. The controller uses the first node port of this Service for
    	the default backend.`)

		ingressClass = flags.String("ingress-class", "",
			`Name of the ingress class to route through this controller.`)

		configMap = flags.String("configmap", "",
			`Name of the ConfigMap that contains the custom configuration to use`)

		publishSvc = flags.String("publish-service", "",
			`Service fronting the ingress controllers. Takes the form
 		namespace/name. The controller will set the endpoint records on the
 		ingress objects to reflect those on the service.`)

		tcpConfigMapName = flags.String("tcp-services-configmap", "",
			`Name of the ConfigMap that contains the definition of the TCP services to expose.
		The key in the map indicates the external port to be used. The value is the name of the
		service with the format namespace/serviceName and the port of the service could be a 
		number of the name of the port.
		The ports 80 and 443 are not allowed as external ports. This ports are reserved for the backend`)

		udpConfigMapName = flags.String("udp-services-configmap", "",
			`Name of the ConfigMap that contains the definition of the UDP services to expose.
		The key in the map indicates the external port to be used. The value is the name of the
		service with the format namespace/serviceName and the port of the service could be a 
		number of the name of the port.`)

		resyncPeriod = flags.Duration("sync-period", 60*time.Second,
			`Relist and confirm cloud resources this often.`)

		watchNamespace = flags.String("watch-namespace", api.NamespaceAll,
			`Namespace to watch for Ingress. Default is to watch all namespaces`)

		healthzPort = flags.Int("healthz-port", 10254, "port for healthz endpoint.")

		profiling = flags.Bool("profiling", true, `Enable profiling via web interface host:port/debug/pprof/`)

		defSSLCertificate = flags.String("default-ssl-certificate", "", `Name of the secret 
		that contains a SSL certificate to be used as default for a HTTPS catch-all server`)

		defHealthzURL = flags.String("health-check-path", "/healthz", `Defines 
		the URL to be used as health check inside in the default server in NGINX.`)

		updateStatus = flags.Bool("update-status", true, `Indicates if the 
		ingress controller should update the Ingress status IP/hostname. Default is true`)

		electionID = flags.String("election-id", "ingress-controller-leader", `Election id to use for status update.`)
	)

	backend.OverrideFlags(flags)

	flags.AddGoFlagSet(flag.CommandLine)
	flags.Parse(os.Args)

	flag.Set("logtostderr", "true")

	glog.V(1).Info(backend.Info())

	if *ingressClass != "" {
		glog.Infof("Watching for ingress class: %s", *ingressClass)
	}

	if *defaultSvc == "" {
		glog.Fatalf("Please specify --default-backend-service")
	}

	kubeClient, err := createApiserverClient(*apiserverHost, *kubeConfigFile)
	if err != nil {
		handleFatalInitError(err)
	}

	_, err = k8s.IsValidService(kubeClient, *defaultSvc)
	if err != nil {
		glog.Fatalf("no service with name %v found: %v", *defaultSvc, err)
	}
	glog.V(1).Infof("validated %v as the default backend", *defaultSvc)

	if *publishSvc != "" {
		svc, err := k8s.IsValidService(kubeClient, *publishSvc)
		if err != nil {
			glog.Fatalf("no service with name %v found: %v", *publishSvc, err)
		}

		if len(svc.Status.LoadBalancer.Ingress) == 0 {
			// We could poll here, but we instead just exit and rely on k8s to restart us
			glog.Fatalf("service %s does not (yet) have ingress points", *publishSvc)
		}

		glog.V(1).Infof("service %v validated as source of Ingress status", *publishSvc)
	}

	for _, configMap := range []string{*configMap, *tcpConfigMapName, *udpConfigMapName} {

		if configMap == "" {
			continue
		}

		_, err = k8s.IsValidConfigMap(kubeClient, configMap)

		if err != nil {
			glog.Fatalf("%v", err)
		}
	}

	if *watchNamespace != "" {

		_, err = k8s.IsValidNamespace(kubeClient, *watchNamespace)

		if err != nil {
			glog.Fatalf("no watchNamespace with name %v found: %v", *watchNamespace, err)
		}
	}

	err = os.MkdirAll(ingress.DefaultSSLDirectory, 0655)
	if err != nil {
		glog.Errorf("Failed to mkdir SSL directory: %v", err)
	}

	config := &Configuration{
		UpdateStatus:          *updateStatus,
		ElectionID:            *electionID,
		Client:                kubeClient,
		ResyncPeriod:          *resyncPeriod,
		DefaultService:        *defaultSvc,
		IngressClass:          *ingressClass,
		DefaultIngressClass:   backend.DefaultIngressClass(),
		Namespace:             *watchNamespace,
		ConfigMapName:         *configMap,
		TCPConfigMapName:      *tcpConfigMapName,
		UDPConfigMapName:      *udpConfigMapName,
		DefaultSSLCertificate: *defSSLCertificate,
		DefaultHealthzURL:     *defHealthzURL,
		PublishService:        *publishSvc,
		Backend:               backend,
	}

	ic := newIngressController(config)
	go registerHandlers(*profiling, *healthzPort, ic)
	return ic
}

func registerHandlers(enableProfiling bool, port int, ic *GenericController) {
	mux := http.NewServeMux()
	// expose health check endpoint (/healthz)
	healthz.InstallHandler(mux,
		healthz.PingHealthz,
		ic.cfg.Backend,
	)

	mux.Handle("/metrics", promhttp.Handler())

	mux.HandleFunc("/build", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		b, _ := json.Marshal(ic.Info())
		w.Write(b)
	})

	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	})

	if enableProfiling {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	server := &http.Server{
		Addr:    fmt.Sprintf(":%v", port),
		Handler: mux,
	}
	glog.Fatal(server.ListenAndServe())
}

const (
	// High enough QPS to fit all expected use cases. QPS=0 is not set here, because
	// client code is overriding it.
	defaultQPS = 1e6
	// High enough Burst to fit all expected use cases. Burst=0 is not set here, because
	// client code is overriding it.
	defaultBurst = 1e6
)

// createApiserverClient creates new Kubernetes Apiserver client. When kubeconfig or apiserverHost param is empty
// the function assumes that it is running inside a Kubernetes cluster and attempts to
// discover the Apiserver. Otherwise, it connects to the Apiserver specified.
//
// apiserverHost param is in the format of protocol://address:port/pathPrefix, e.g.http://localhost:8001.
// kubeConfig location of kubeconfig file
func createApiserverClient(apiserverHost string, kubeConfig string) (*client.Clientset, error) {

	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeConfig},
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: apiserverHost}})

	cfg, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, err
	}

	cfg.QPS = defaultQPS
	cfg.Burst = defaultBurst
	cfg.ContentType = "application/vnd.kubernetes.protobuf"

	glog.V(1).Infof("Creating API server client for %s", cfg.Host)

	client, err := client.NewForConfig(cfg)

	if err != nil {
		return nil, err
	}
	return client, nil
}

/**
 * Handles fatal init error that prevents server from doing any work. Prints verbose error
 * message and quits the server.
 */
func handleFatalInitError(err error) {
	glog.Fatalf("Error while initializing connection to Kubernetes apiserver. "+
		"This most likely means that the cluster is misconfigured (e.g., it has "+
		"invalid apiserver certificates or service accounts configuration). Reason: %s\n"+
		"Refer to the troubleshooting guide for more information: "+
		"https://github.com/kubernetes/ingress/blob/master/docs/troubleshooting.md", err)
}
