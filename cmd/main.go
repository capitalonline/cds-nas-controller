package main

import (
	"flag"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"
	"math/rand"
	"os"
	"sigs.k8s.io/sig-storage-lib-external-provisioner/controller"
	"time"
)

func init() {
	rand.Seed(time.Now().Unix())
}

func main() {
	flag.Parse()
	provisionerName := os.Getenv(provisionerNameKey)
	if provisionerName == "" {
		klog.Infof("env %s is empty, use default value: %s", provisionerNameKey, defaultProvisioner)
		provisionerName = defaultProvisioner
	}
	klog.Infof("starting provisioner with name %s", provisionerName)

	// Create an InClusterConfig and use it to create a client for the controller
	// to use to communicate with Kubernetes
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("Failed to create config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create client: %v", err)
	}

	// The controller needs to know what the server version is because out-of-tree
	// provisioners aren't officially supported until 1.5
	serverVersion, err := clientset.Discovery().ServerVersion()
	if err != nil {
		klog.Fatalf("Error getting server version: %v", err)
	}

	// trying to mkdir of DeleteMountRoot if there isn't any
	if err := os.MkdirAll(DeleteMountRoot, 0777); err != nil {
		klog.Fatalf("unable to create default DeleteMountRoot directory: " + err.Error())
	}
	clientNasProvisioner := &nasProvisioner{
		client: clientset,
	}
	// Start the provision controller which will dynamically provision Nas PVs
	pc := controller.NewProvisionController(clientset, provisionerName, clientNasProvisioner, serverVersion.GitVersion)
	pc.Run(wait.NeverStop)
}
