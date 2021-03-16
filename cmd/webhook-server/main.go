package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	tlsDir                  = `/run/secrets/tls`
	tlsCertFile             = `tls.crt`
	tlsKeyFile              = `tls.key`
	podTerminatorAnnotation = "pod-terminator"
)

var (
	nilNodeName      = "nil"
	podResource      = metav1.GroupVersionResource{Version: "v1", Resource: "pods"}
	endpointResource = metav1.GroupVersionResource{Version: "v1", Resource: "endpoints"}
	deletionCache    = map[string]time.Time{}
)

func prettyJSON(obj interface{}) string {
	buffer, _ := json.Marshal(obj)

	var prettyJSON string
	if len(buffer) > 0 {
		var jsonBuffer bytes.Buffer
		error := json.Indent(&jsonBuffer, buffer, "", "  ")
		if error != nil {
			return string(buffer)
		}
		prettyJSON = jsonBuffer.String()
	} else {
		prettyJSON = ""
	}

	return prettyJSON
}

func mutateResource(req *v1beta1.AdmissionRequest, clientSet *kubernetes.Clientset) (bool, string, []patchOperation, error) {
	if req.Resource == endpointResource {
		return mutateEndpoints(req, clientSet)
	}

	if req.Resource == podResource {
		return mutatePods(req, clientSet)
	}

	log.Printf("Unexpected resource type %s", req.Resource)
	return true, "", nil, nil
}

func mutatePods(req *v1beta1.AdmissionRequest, clientSet *kubernetes.Clientset) (bool, string, []patchOperation, error) {
	if req.Resource != podResource {
		log.Printf("expect resource to be %s", podResource)
		return true, "", nil, nil
	}

	raw := req.Object.Raw
	pod := corev1.Pod{}
	if _, _, err := universalDeserializer.Decode(raw, nil, &pod); err != nil {
		return false, "", nil, fmt.Errorf("could not deserialize pod object: %v", err)
	}

	log.Printf("Pod before mutate: %v \n", prettyJSON(pod))

	rerouter := corev1.Container{
		Name:            "rerouter",
		Image:           "yangl/rerouter",
		ImagePullPolicy: corev1.PullAlways,
		ReadinessProbe: &corev1.Probe{
			Handler: corev1.Handler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromInt(9527),
				},
			},
			InitialDelaySeconds: 30,
			TimeoutSeconds:      5,
			PeriodSeconds:       1,
			SuccessThreshold:    1,
			FailureThreshold:    3,
		},
	}

	patch := patchOperation{
		Op:    "add",
		Path:  "/spec/containers/-",
		Value: rerouter,
	}

	return true, "Mutated the pod", []patchOperation{patch}, nil
}

func mutateEndpoints(req *v1beta1.AdmissionRequest, clientSet *kubernetes.Clientset) (bool, string, []patchOperation, error) {
	if req.Resource != endpointResource {
		log.Printf("expect resource to be %s", endpointResource)
		return true, "", nil, nil
	}

	raw := req.Object.Raw
	ep := corev1.Endpoints{}
	if _, _, err := universalDeserializer.Decode(raw, nil, &ep); err != nil {
		return false, "", nil, fmt.Errorf("could not deserialize endpoint object: %v", err)
	}

	log.Printf("Endpoint before mutate: %v \n", prettyJSON(ep))

	var patches []patchOperation
	for i, ss := range ep.Subsets {
		if len(ss.Addresses) == 0 {
			patches = append(patches, patchOperation{
				Op:    "add",
				Path:  fmt.Sprintf("/subsets/%d/addresses", i),
				Value: []corev1.EndpointAddress{},
			})
		}
		for _, nra := range ss.NotReadyAddresses {
			patches = append(patches, patchOperation{
				Op:   "add",
				Path: fmt.Sprintf("/subsets/%d/addresses/-", i),
				Value: corev1.EndpointAddress{
					IP:        nra.IP,
					NodeName:  &nilNodeName,
					TargetRef: nra.TargetRef,
				},
			})
		}

		if len(patches) > 0 {
			patches = append(patches, patchOperation{
				Op:   "remove",
				Path: fmt.Sprintf("/subsets/%d/notReadyAddresses", i),
			})
		}
	}

	log.Printf("Patches: %v \n", prettyJSON(patches))

	return true, "Mutated the endpoints", patches, nil
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

	pod, err := clientSet.CoreV1().Pods(req.Namespace).Get(req.Name, metav1.GetOptions{})
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

	resp, err := http.DefaultClient.Post(fmt.Sprintf("http://%s:9527/fail", pod.Status.PodIP), "application/json", bytes.NewBufferString(""))
	if err != nil {
		return false, fmt.Sprintf("Failed to set pod to fail %s/%s: %v", req.Namespace, req.Name, err), nil, nil
	}
	defer resp.Body.Close()

	if c, ok := deletionCache[cacheID]; ok {
		if time.Now().After(c) {
			log.Printf("Pod %s passed pre-deletion-hook, allow deletion.", cacheID)
			delete(deletionCache, cacheID)
			return true, "", nil, nil
		}
	} else {
		deletionCache[cacheID] = time.Now().UTC().Add(time.Minute)
	}

	reason := fmt.Sprintf("Pod %s requires pre-deletion-hook, will allow deletion at %s.", cacheID, deletionCache[cacheID])
	log.Printf(reason)
	return false, reason, nil, nil
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
	mux.Handle("/mutate", admitFuncHandler(mutateResource, clientSet))
	mux.Handle("/validate", admitFuncHandler(validateDeletion, clientSet))
	server := &http.Server{
		Addr:    ":8443",
		Handler: mux,
	}
	log.Fatal(server.ListenAndServeTLS(certPath, keyPath))
}
