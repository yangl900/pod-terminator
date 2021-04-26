package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k8s.io/api/admission/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	tlsDir                        = `/run/secrets/tls`
	tlsCertFile                   = `tls.crt`
	tlsKeyFile                    = `tls.key`
	podTerminatorAnnotation       = "pod-terminator"
	podTerminationDelayAnnotation = "pod-terminator-delay"
	defaultDelay                  = time.Second * 150
)

var (
	podResource   = metav1.GroupVersionResource{Version: "v1", Resource: "pods"}
	deletionCache = map[string]time.Time{}
)

type ResourceIDRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func validateDeletion(req *v1beta1.AdmissionRequest, clientSet *kubernetes.Clientset) (bool, string, []patchOperation, error) {
	if req.Resource != podResource {
		log.Printf("expect resource to be %s", podResource)
		return true, "", nil, nil
	}

	if req.Operation != v1beta1.Delete {
		log.Printf("Allow non-deletion operation %v", req.Operation)
		return true, "", nil, nil
	}

	cacheID := fmt.Sprintf("%s/%s", req.Namespace, req.Name)

	log.Printf("Reviewing pod deletion operation: %s", cacheID)

	pod, err := clientSet.CoreV1().Pods(req.Namespace).Get(context.Background(), req.Name, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Sprintf("Failed to read pod %s/%s: %v", req.Namespace, req.Name, err), nil, nil
	}

	if pod.DeletionTimestamp != nil {
		log.Printf("Pod %s in terminating, allow deletion.", cacheID)
		return true, "Pod in terminating, allow deletion.", nil, nil
	}

	if val, ok := pod.Annotations[podTerminatorAnnotation]; !ok || strings.EqualFold(val, "false") {
		log.Printf("Pod %s does not have annotation, allow deletion.", cacheID)
		return true, "Pod does not have annotation, allow deletion.", nil, nil
	}

	rrs, err := findService(clientSet, req.Namespace, req.Name, pod.Status.PodIP)
	if err != nil {
		return false, fmt.Sprintf("Failed to locate service for pod %s/%s: %s", req.Namespace, req.Name, err), nil, nil
	}

	for _, rr := range rrs {
		reqBody, err := json.Marshal(rr)
		if err != nil {
			return false, fmt.Sprintf("Failed to marshal service name for pod %s/%s: %s", req.Namespace, req.Name, err), nil, nil
		}

		if c, ok := deletionCache[cacheID]; ok {
			// TODO: delete the pod in timer
			if time.Now().After(c) {
				log.Printf("Pod %s passed pre-deletion-hook, allow deletion.", cacheID)

				resp, err := http.DefaultClient.Post(fmt.Sprintf("http://%s:10257/reset", pod.Status.HostIP), "application/json", bytes.NewBuffer(reqBody))
				if err != nil {
					return false, fmt.Sprintf("Failed to reset endpoint %s/%s: %v", req.Namespace, req.Name, err), nil, nil
				}
				if resp.StatusCode != http.StatusOK {
					return false, fmt.Sprintf("Failed to reset endpoint %s/%s, status code: %d", req.Namespace, req.Name, resp.StatusCode), nil, nil
				}
				defer resp.Body.Close()

				delete(deletionCache, cacheID)
				return true, "", nil, nil
			}
		} else {
			// TODO: fail if no other healthy pods on the same node (by getting node name and loop endpoint sets)
			resp, err := http.DefaultClient.Post(fmt.Sprintf("http://%s:10257/fail", pod.Status.HostIP), "application/json", bytes.NewBuffer(reqBody))
			if err != nil {
				return false, fmt.Sprintf("Failed to set pod to fail %s/%s: %v", req.Namespace, req.Name, err), nil, nil
			}
			if resp.StatusCode != http.StatusOK {
				return false, fmt.Sprintf("Failed to set pod to fail %s/%s, status code: %d", req.Namespace, req.Name, resp.StatusCode), nil, nil
			}
			defer resp.Body.Close()

			delayDuration := defaultDelay
			if val, ok := pod.Annotations[podTerminationDelayAnnotation]; ok {
				if sec, success := strconv.Atoi(val); success != nil {
					delayDuration = time.Second * time.Duration(sec)
				}
			}
			deletionCache[cacheID] = time.Now().UTC().Add(delayDuration)
		}
	}

	reason := fmt.Sprintf("Pod %s requires pre-deletion-hook, will allow deletion at %s.", cacheID, deletionCache[cacheID])
	log.Println(reason)
	return false, reason, nil, nil
}

func findService(clientSet *kubernetes.Clientset, namespace, name, podIP string) ([]ResourceIDRequest, error) {
	eps, err := clientSet.CoreV1().Endpoints(namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		log.Printf("Failed to list endpoints: %s", err)
		return []ResourceIDRequest{}, fmt.Errorf("failed to list endpoints: %s", err)
	}

	rr := make([]ResourceIDRequest, 0)
	for _, ep := range eps.Items {
		for _, ss := range ep.Subsets {
			for _, addr := range ss.Addresses {
				if addr.IP == podIP {
					nsn := ResourceIDRequest{
						Namespace: ep.Namespace,
						Name:      ep.Name,
					}

					rr = append(rr, nsn)
				}
			}

			for _, addr := range ss.NotReadyAddresses {
				if addr.IP == podIP {
					nsn := ResourceIDRequest{
						Namespace: ep.Namespace,
						Name:      ep.Name,
					}

					rr = append(rr, nsn)
				}
			}
		}
	}

	return rr, nil
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

func main() {
	certPath := filepath.Join(tlsDir, tlsCertFile)
	keyPath := filepath.Join(tlsDir, tlsKeyFile)

	clientSet, err := kubeClientSet(true)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/validate", admitFuncHandler(validateDeletion, clientSet))
	server := &http.Server{
		Addr:    ":8443",
		Handler: mux,
	}
	log.Fatal(server.ListenAndServeTLS(certPath, keyPath))
}
