package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	"context"

	"github.com/yangl900/pod-terminator/health-proxy/healthcheck"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"
)

type ResourceIDRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func kubeClientSet(inCluster bool) (*kubernetes.Clientset, error) {
	var config *rest.Config

	if inCluster {
		c, err := rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
		config = c
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return clientset, nil
}

func createRecorder(kubeClient *kubernetes.Clientset, userAgent string) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	return eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: userAgent})
}

func main() {
	klog.InitFlags(nil)
	flag.Set("v", "9")
	flag.Parse()

	clientSet, err := kubeClientSet(true)
	if err != nil {
		klog.Errorf("Failed to create kubeclient: %s \n", err.Error())
		return
	}
	recorder := createRecorder(clientSet, "pod-terminator")
	server := healthcheck.NewServiceHealthServer("localhost", recorder)

	go func() {
		for {
			svcs, err := clientSet.CoreV1().Services("").List(context.Background(), metav1.ListOptions{})
			if err != nil {
				time.After(time.Second * 10)
				continue
			}

			svcPorts := map[types.NamespacedName]uint16{}
			for _, svc := range svcs.Items {
				if svc.Spec.ExternalTrafficPolicy != v1.ServiceExternalTrafficPolicyTypeLocal {
					continue
				}

				klog.V(4).Infof("Found svc with local traffic policy: %s/%s port: %d\n", svc.Namespace, svc.Name, svc.Spec.HealthCheckNodePort)
				nsn := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
				svcPorts[nsn] = uint16(svc.Spec.HealthCheckNodePort)
			}

			if err := server.SyncServices(svcPorts); err != nil {
				klog.Errorf("Failed to sync service ports.")
			}

			<-time.After(time.Second * 30)
		}
	}()

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(200)
	})

	mux.HandleFunc("/fail", func(rw http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			rw.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		body, err := ioutil.ReadAll(req.Body)
		if err != nil {
			klog.Errorf("Failed to read request body: %s", err.Error())
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}

		resID := ResourceIDRequest{}
		if err := json.Unmarshal(body, &resID); err != nil {
			klog.Errorf("Failed to read request body: %s", err.Error())
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}

		nsn := types.NamespacedName{
			Namespace: resID.Namespace,
			Name:      resID.Name,
		}

		if err := server.FailService(nsn); err != nil {
			klog.Errorf("Unable to set service to fail: %s", err.Error())
			rw.WriteHeader(http.StatusBadRequest)
			return
		}

		klog.V(2).Infof("Successfully set service to fail: %s", nsn)
		rw.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/reset", func(rw http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			rw.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		body, err := ioutil.ReadAll(req.Body)
		if err != nil {
			klog.Errorf("Failed to read request body: %s", err.Error())
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}

		resID := ResourceIDRequest{}
		if err := json.Unmarshal(body, &resID); err != nil {
			klog.Errorf("Failed to read request body: %s", err.Error())
			rw.WriteHeader(http.StatusInternalServerError)
			return
		}

		nsn := types.NamespacedName{
			Namespace: resID.Namespace,
			Name:      resID.Name,
		}

		if err := server.ResetService(nsn); err != nil {
			klog.Errorf("Unable to set service to success: %s", err.Error())
			rw.WriteHeader(http.StatusBadRequest)
			return
		}

		klog.V(2).Infof("Successfully set service to success: %s", nsn)
		rw.WriteHeader(http.StatusOK)
	})

	healthProxyServer := &http.Server{
		Addr:    ":10257",
		Handler: mux,
	}
	log.Fatal(healthProxyServer.ListenAndServe())
}
