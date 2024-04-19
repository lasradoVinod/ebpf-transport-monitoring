// main package contains all the functionality required to keep track of pods
// that need to be traced with lightfoot.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

type podInfo struct {
	nodeName    string
	containerid []string
}

// controllerInfo holds the information about the lightfoot controller.
type controllerInfo struct {
	// map of node name to lightfoot pod ip on that node
	nodes map[string]string
	// map of pod names to container_id
	// These are all pods which have lightfoot:enable
	container map[string]podInfo
	// List of containers lightfoot is not yet updated about
	pending map[string]bool
	// Channel to trigger new pending
	wake chan struct{}
	// lock for adding to pending state
	mu sync.Mutex
}

// PodEvent is the type of pod event.
type PodEvent int

// Enum for Pod events
const (
	Add    PodEvent = iota
	Update          = iota
	Delete          = iota
)

var cInfo controllerInfo

func (c *controllerInfo) sendUpdate(name string) bool {
	var ip string
	var ok bool
	if ip, ok = c.nodes[c.container[name].nodeName]; !ok {
		glog.Info("Lightfoot instance not found for node ", c.container[name].nodeName)
		return false
	}
	var buffer bytes.Buffer
	for _, str := range c.container[name].containerid {
		buffer.WriteString(str)
		buffer.WriteString("\n") // Add a newline if you want separation
	}
	req, err := http.NewRequest("POST", "http://"+ip+":12000/crio-id", bytes.NewReader(buffer.Bytes()))
	if err != nil {
		glog.Error("Error sending request:", err)
		return false
	}
	req.Header.Set("Content-Type", "text/plain")
	// Create an HTTP client and perform the request
	glog.Info("Sending ", c.container[name].nodeName, ip, " pod ", name, buffer.String())
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		glog.Error("Error sending request:", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 300 {
		glog.Error("Request failed with status:", buffer.String(), resp.Status)
		return false
	}
	return true
}

func (c *controllerInfo) managePending() {
	c.mu.Lock()
	for k := range c.pending {
		if c.sendUpdate(k) {
			delete(c.pending, k)
		}
	}
	c.mu.Unlock()
}

func (c *controllerInfo) contactLightfoot() {
	timer := time.NewTicker(10 * time.Second)
	for {
		select {
		case _ = <-timer.C:
			c.managePending()
		case <-c.wake:
			c.managePending()
		}
	}
}

func checkPodLabels(labels map[string]string) bool {
	for k, v := range labels {
		if k == "lightfoot" && v == "enable" {
			return true
		}
	}
	return false
}

func addPod(name string, p podInfo) {
	cInfo.container[name] = p
	cInfo.mu.Lock()
	cInfo.pending[name] = true
	cInfo.mu.Unlock()
	cInfo.wake <- struct{}{}
}

func handlePodEvent(e PodEvent, pod *v1.Pod) {
	switch e {
	case Add:
		// Add the ip to the lightfoot container corresponding to nodename
		name := pod.ObjectMeta.GetName()
		glog.Info("Adding ", name)
		if strings.HasPrefix(name, "lightfoot-daemon") {
			cInfo.nodes[pod.Spec.NodeName] = pod.Status.PodIP
			break
		}
		if !checkPodLabels(pod.ObjectMeta.GetLabels()) {
			break
		}
		if _, ok := cInfo.container[name]; ok {
			break
		}
		var p podInfo
		p.nodeName = pod.Spec.NodeName
		for _, containerStatus := range pod.Status.ContainerStatuses {
			p.containerid = append(p.containerid, strings.TrimPrefix(containerStatus.ContainerID, "cri-o://"))
		}
		addPod(name, p)
	case Delete:
		name := pod.ObjectMeta.GetName()
		glog.Info("Deleting ", name)
		if strings.HasPrefix(name, "lightfoot-daemon") {
			// Delete lightfoot-ip
			nodeName := pod.Spec.NodeName
			delete(cInfo.nodes, nodeName)
			// Add all pods on this node to pending
			cInfo.mu.Lock()
			for k, v := range cInfo.container {
				fmt.Println(v.nodeName, nodeName)
				if v.nodeName == nodeName {
					cInfo.pending[k] = true
				}
			}
			cInfo.mu.Unlock()
			break
		}
		if checkPodLabels(pod.ObjectMeta.GetLabels()) {
			cInfo.mu.Lock()
			delete(cInfo.container, name)
			delete(cInfo.pending, name)
			cInfo.mu.Unlock()
		}
	}
}

func main() {
	flag.Parse()
	fmt.Print("Started")
	// Create a Kubernetes client using the provided kubeconfig.
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	cInfo.container = make(map[string]podInfo)
	cInfo.pending = make(map[string]bool)
	cInfo.nodes = make(map[string]string)
	cInfo.wake = make(chan struct{})

	factory := informers.NewSharedInformerFactory(clientset, 2*time.Second)
	podInformer := factory.Core().V1().Pods()

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			pod := obj.(*v1.Pod)
			handlePodEvent(Add, pod)
		},
		UpdateFunc: func(oldObj, newObj any) {
			pod := oldObj.(*v1.Pod)
			handlePodEvent(Add, pod)
		},
		DeleteFunc: func(obj any) {
			pod := obj.(*v1.Pod)
			handlePodEvent(Delete, pod)
		},
	})

	go cInfo.contactLightfoot()
	stopCh := make(chan struct{})
	defer close(stopCh)

	factory.Start(stopCh)
	// Keep the program running.
	select {}
}
