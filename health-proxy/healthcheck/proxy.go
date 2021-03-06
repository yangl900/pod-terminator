package healthcheck

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"

	"github.com/yangl900/pod-terminator/health-proxy/iptables"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"k8s.io/client-go/tools/record"
	"k8s.io/klog"
)

// listener allows for testing of ServiceHealthServer and ProxierHealthServer.
type listener interface {
	// Listen is very much like net.Listen, except the first arg (network) is
	// fixed to be "tcp".
	Listen(addr string) (net.Listener, error)
}

// httpServerFactory allows for testing of ServiceHealthServer and ProxierHealthServer.
type httpServerFactory interface {
	// New creates an instance of a type satisfying HTTPServer.  This is
	// designed to include http.Server.
	New(addr string, handler http.Handler) httpServer
}

// httpServer allows for testing of ServiceHealthServer and ProxierHealthServer.
// It is designed so that http.Server satisfies this interface,
type httpServer interface {
	Serve(listener net.Listener) error
}

// Implement listener in terms of net.Listen.
type stdNetListener struct{}

func (stdNetListener) Listen(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

var _ listener = stdNetListener{}

// Implement httpServerFactory in terms of http.Server.
type stdHTTPServerFactory struct{}

func (stdHTTPServerFactory) New(addr string, handler http.Handler) httpServer {
	return &http.Server{
		Addr:    addr,
		Handler: handler,
	}
}

type ServiceHealthServer interface {
	// Make the new set of services be active.  Services that were open before
	// will be closed.  Services that are new will be opened.  Service that
	// existed and are in the new set will be left alone.  The value of the map
	// is the healthcheck-port to listen on.
	SyncServices(newServices map[types.NamespacedName]uint16) error
	FailService(nsn types.NamespacedName) error
	ResetService(nsn types.NamespacedName) error
	Stop()
}

func newServiceHealthServer(hostname, hostIP string, recorder record.EventRecorder, listener listener, factory httpServerFactory) ServiceHealthServer {
	return &server{
		hostname:    hostname,
		HostIP:      hostIP,
		recorder:    recorder,
		listener:    listener,
		httpFactory: factory,
		services:    map[types.NamespacedName]*hcInstance{},
	}
}

// NewServiceHealthServer allocates a new service healthcheck server manager
func NewServiceHealthServer(hostname, hostIP string, recorder record.EventRecorder) ServiceHealthServer {
	return newServiceHealthServer(hostname, hostIP, recorder, stdNetListener{}, stdHTTPServerFactory{})
}

var _ httpServerFactory = stdHTTPServerFactory{}

type server struct {
	hostname    string
	HostIP      string
	recorder    record.EventRecorder // can be nil
	listener    listener
	httpFactory httpServerFactory

	lock     sync.RWMutex
	stopped  bool
	services map[types.NamespacedName]*hcInstance
}

func (hcs *server) Stop() {
	hcs.lock.Lock()
	defer hcs.lock.Unlock()
	hcs.stopped = true
	for nsn, svc := range hcs.services {
		klog.V(2).Infof("Removing iptable rules for %q on port %d \n", nsn.String(), svc.proxyPort)
		if err := iptables.DeleteCustomChain(strconv.Itoa(int(svc.healthcheckPort))); err != nil {
			klog.Errorf("Failed to cleanup iptable rules for service %s", nsn)
		}

		klog.V(2).Infof("Closing healthcheck %q on port %d \n", nsn.String(), svc.proxyPort)
		if err := svc.listener.Close(); err != nil {
			klog.Errorf("Close(%v): %v", svc.listener.Addr(), err)
		}
		delete(hcs.services, nsn)
	}
}

func (hcs *server) FailService(nsn types.NamespacedName) error {
	hcs.lock.Lock()
	defer hcs.lock.Unlock()

	svc, ok := hcs.services[nsn]
	if !ok {
		return fmt.Errorf("service not found: %s/%s", nsn.Namespace, nsn.Name)
	}

	klog.V(2).Infof("Setting service %s to fail.", nsn)
	svc.terminating = true
	return nil
}

func (hcs *server) ResetService(nsn types.NamespacedName) error {
	hcs.lock.Lock()
	defer hcs.lock.Unlock()

	klog.V(2).Infof("Ressting service %s", nsn)

	svc, ok := hcs.services[nsn]
	if !ok {
		klog.V(2).Infof("Skip reset because service not found: %s", nsn)
		return nil
	}

	svc.terminating = false
	return nil
}

func (hcs *server) SyncServices(newServices map[types.NamespacedName]uint16) error {
	hcs.lock.Lock()
	defer hcs.lock.Unlock()

	if hcs.stopped {
		klog.Errorf("Health proxy server has been stopped. Skip syncing services.")
		return nil
	}

	// Remove any that are not needed any more.
	for nsn, svc := range hcs.services {
		if port, found := newServices[nsn]; !found || port != svc.healthcheckPort {
			klog.V(2).Infof("Closing healthcheck %q on port %d \n", nsn.String(), svc.healthcheckPort)
			if err := svc.listener.Close(); err != nil {
				klog.Errorf("Close(%v): %v", svc.listener.Addr(), err)
			}

			iptables.DeleteCustomChain(strconv.Itoa(int(svc.healthcheckPort)))
			delete(hcs.services, nsn)
		}
	}

	// Add any that are needed.
	for nsn, port := range newServices {
		if hci := hcs.services[nsn]; hci != nil {
			klog.V(3).Infof("Existing healthcheck %q on port %d", nsn.String(), port)

			if err := iptables.AddCustomChain(hcs.HostIP, strconv.Itoa(int(hci.healthcheckPort)), "127.0.0.1", strconv.Itoa(int(hci.proxyPort))); err != nil {
				klog.Errorf("Failed to ensure iptable rules for svc %s healthcheck port %d: %s", nsn, hci.healthcheckPort, err)
			}

			continue
		}

		// Default from 30000 to 32767, so add 10000
		svc := &hcInstance{healthcheckPort: port, proxyPort: port + 10000}
		addr := fmt.Sprintf(":%d", svc.proxyPort)
		svc.server = hcs.httpFactory.New(addr, hcHandler{name: nsn, hcs: hcs})

		klog.V(2).Infof("Opening healthcheck %q on port %d", nsn.String(), svc.proxyPort)
		var err error
		svc.listener, err = hcs.listener.Listen(addr)
		if err != nil {
			msg := fmt.Sprintf("node %s failed to start healthcheck proxy %q on port %d: %v", hcs.hostname, nsn.String(), port, err)

			if hcs.recorder != nil {
				hcs.recorder.Eventf(
					&v1.ObjectReference{
						Kind:      "Service",
						Namespace: nsn.Namespace,
						Name:      nsn.Name,
						UID:       types.UID(nsn.String()),
					}, "Warning", "FailedToStartServiceHealthcheckProxy", msg)
			}
			klog.Error(msg)
			continue
		}
		hcs.services[nsn] = svc

		go func(nsn types.NamespacedName, svc *hcInstance) {
			// Serve() will exit when the listener is closed.
			klog.V(3).Infof("Starting goroutine for healthcheck %q on port %d", nsn.String(), svc.proxyPort)
			if err := svc.server.Serve(svc.listener); err != nil {
				klog.V(3).Infof("Healthcheck %q closed: %v", nsn.String(), err)
				return
			}
			klog.V(3).Infof("Healthcheck %q closed", nsn.String())
		}(nsn, svc)

		if err := iptables.AddCustomChain(hcs.HostIP, strconv.Itoa(int(svc.healthcheckPort)), "127.0.0.1", strconv.Itoa(int(svc.proxyPort))); err != nil {
			klog.Errorf("Failed to add iptable rules for svc %s healthcheck port %d: %s", nsn, svc.healthcheckPort, err)
		}
	}
	return nil
}

type hcInstance struct {
	proxyPort       uint16
	healthcheckPort uint16
	listener        net.Listener
	server          httpServer
	terminating     bool
}

type hcHandler struct {
	name types.NamespacedName
	hcs  *server
}

var _ http.Handler = hcHandler{}

func (h hcHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	h.hcs.lock.RLock()
	svc, ok := h.hcs.services[h.name]
	if !ok || svc == nil {
		h.hcs.lock.RUnlock()
		resp.WriteHeader(http.StatusInternalServerError)
		klog.Errorf("Received request for closed healthcheck %q", h.name.String())
		return
	}
	h.hcs.lock.RUnlock()

	resp.Header().Set("Content-Type", "application/json")
	resp.Header().Set("X-Content-Type-Options", "nosniff")
	resp.Header().Set("Server", "health-proxy")
	if svc.terminating {
		resp.WriteHeader(http.StatusServiceUnavailable)
	} else {
		r, err := http.Get(fmt.Sprintf("http://localhost:%d", svc.healthcheckPort))
		if err != nil {
			klog.Errorf("Failed to send health check request on port %d", svc.healthcheckPort)
			resp.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer r.Body.Close()
		resp.Header().Set("Content-Length", r.Header.Get("Content-Length"))
		resp.WriteHeader(r.StatusCode)
		io.Copy(resp, r.Body)
	}
}
