package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"
	"sigs.k8s.io/sig-storage-lib-external-provisioner/controller"
)

const (
	provisionerNameKey = "PROVISIONER_NAME"
	defaultProvisioner = "cds/nas"
	driverName = "cds/nas"
)

type nasProvisioner struct {
	client kubernetes.Interface
}

const (
	mountPath = "/persistentvolumes"
)

var _ controller.Provisioner = &nasProvisioner{}

func (p *nasProvisioner) Provision(options controller.ProvisionOptions) (*v1.PersistentVolume, error) {
	if options.PVC.Spec.Selector != nil {
		return nil, fmt.Errorf("claim Selector is not supported")
	}
	klog.Infof("nas provisioner: VolumeOptions %+v", options)

	pvcNamespace := options.PVC.Namespace
	pvcName := options.PVC.Name
	pvName := strings.Join([]string{pvcNamespace, pvcName, options.PVName}, "-")
	fullPath := filepath.Join(mountPath, pvName)
	klog.Infof("creating path %s", fullPath)
	if err := os.MkdirAll(fullPath, 0777); err != nil {
		return nil, errors.New("unable to create directory to provision new pv: " + err.Error())
	}
	if err := os.Chmod(fullPath, 0777); err != nil {
		klog.Errorf("Failed to change mode of directory %s to 0777", fullPath)
	}

	pvs := v1.PersistentVolumeSource{}
	nasServer, ok := options.StorageClass.Parameters["server"]
	if !ok {
		return nil, errors.New("server must be provided in the storage class parameters")
	}
	nasServerPath, ok := options.StorageClass.Parameters["path"]
	if !ok {
		nasServerPath = "/nfsshare"
	}

	flexNasVers, ok := options.StorageClass.Parameters["vers"]
	if !ok {
		flexNasVers = "4.0"
	}
	flexNasOptions, ok := options.StorageClass.Parameters["options"]
	if !ok {
		flexNasOptions = "noresvport"
	}
	pvs.FlexVolume = &v1.FlexPersistentVolumeSource{
		Driver:   driverName,
		ReadOnly: false,
		Options: map[string]string{
			"server":  nasServer,
			"path":    filepath.Join(nasServerPath, pvName),
			"vers":    flexNasVers,
			"mode":    options.StorageClass.Parameters["mode"],
			"options": flexNasOptions,
		},
	}

	// create PersistentVolume object
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: options.PVName,
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: *options.StorageClass.ReclaimPolicy,
			AccessModes:                   options.PVC.Spec.AccessModes,
			MountOptions:                  options.StorageClass.MountOptions,
			Capacity: v1.ResourceList{
				v1.ResourceStorage: options.PVC.Spec.Resources.Requests[v1.ResourceStorage],
			},
			PersistentVolumeSource: pvs,
		},
	}
	return pv, nil
}

func (p *nasProvisioner) Delete(volume *v1.PersistentVolume) error {
	path := volume.Spec.PersistentVolumeSource.FlexVolume.Options["path"]
	pvName := filepath.Base(path)
	oldPath := filepath.Join(mountPath, pvName)
	klog.Infof("deleting pv %s on path %s(flexvolume path parameter: %s)", pvName, oldPath, path)
	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		klog.Warningf("path %s does not exist, deletion skipped", oldPath)
		return nil
	}
	// Get the storage class for this volume.
	storageClass, err := p.getClassForVolume(volume)
	if err != nil {
		klog.Errorf("failed to get storage class from volume %s: %s", volume.Name, err)
		return err
	}
	// Determine if the "archiveOnDelete" parameter exists.
	// If it exists and has a false value, delete the directory.
	// Otherwise, archive it.
	archiveOnDelete, exists := storageClass.Parameters["archiveOnDelete"]
	if exists {
		archiveBool, err := strconv.ParseBool(archiveOnDelete)
		if err != nil {
			return err
		}
		if !archiveBool {
			return os.RemoveAll(oldPath)
		}
	}

	archivePath := filepath.Join(mountPath, "archived-"+pvName)
	klog.Infof("archiving path %s to %s", oldPath, archivePath)
	return os.Rename(oldPath, archivePath)
}

// getClassForVolume returns StorageClass
func (p *nasProvisioner) getClassForVolume(pv *v1.PersistentVolume) (*storage.StorageClass, error) {
	if p.client == nil {
		return nil, fmt.Errorf("cannot get kube client")
	}
	className := GetPersistentVolumeClass(pv)
	if className == "" {
		return nil, fmt.Errorf("volume has no storage class")
	}
	class, err := p.client.StorageV1().StorageClasses().Get(className, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return class, nil
}

// GetPersistentVolumeClass returns StorageClassName.
func GetPersistentVolumeClass(volume *v1.PersistentVolume) string {
	// Use beta annotation first
	if class, found := volume.Annotations[v1.BetaStorageClassAnnotation]; found {
		return class
	}

	return volume.Spec.StorageClassName
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

	clientNasProvisioner := &nasProvisioner{
		client: clientset,
	}
	// Start the provision controller which will dynamically provision Nas PVs
	pc := controller.NewProvisionController(clientset, provisionerName, clientNasProvisioner, serverVersion.GitVersion)
	pc.Run(wait.NeverStop)
}
