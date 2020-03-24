package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"
	"sigs.k8s.io/sig-storage-lib-external-provisioner/controller"
)

const (
	provisionerNameKey = "PROVISIONER_NAME"
	defaultProvisioner = "cds/nas"
	driverName         = "cds/nas"
	mountPath          = "/persistentvolumes"
	version            = "v1.0.0"
	defaultNfsPath     = "/nfsshare"
	defaultV3Opts      = "noresvport,nolock,tcp"
	defaultV4Opts      = "noresvport"
)

type nasProvisioner struct {
	client kubernetes.Interface
}

var _ controller.Provisioner = &nasProvisioner{}

func (p *nasProvisioner) Provision(options controller.ProvisionOptions) (*core.PersistentVolume, error) {
	if options.PVC.Spec.Selector != nil {
		return nil, fmt.Errorf("claim Selector is not supported")
	}
	klog.Infof("nas provisioner ver:%s, VolumeOptions %+v", version, options)

	pvcNamespace := options.PVC.Namespace
	pvcName := options.PVC.Name
	pvDirectoryName := strings.Join([]string{pvcNamespace, pvcName, options.PVName}, "-")

	pvs := core.PersistentVolumeSource{}
	nasServer, ok := options.StorageClass.Parameters["server"]
	if !ok {
		return nil, errors.New("server must be provided in the storage class parameters")
	}

	flexNasVers, ok := options.StorageClass.Parameters["vers"]
	if !ok {
		flexNasVers = "4.0"
	} else if strings.HasPrefix(flexNasVers, "3") {
		// ony vers=3 is supported
		flexNasVers = "3"
	}

	nfsV4 := false
	if strings.HasPrefix(flexNasVers, "4") {
		nfsV4 = true
	}
	nasServerPath, ok := options.StorageClass.Parameters["path"]
	if !ok {
		nasServerPath = defaultNfsPath
	}

	flexNasOptions, ok := options.StorageClass.Parameters["options"]
	if !ok {
		if nfsV4 {
			flexNasOptions = defaultV4Opts
		} else {
			flexNasOptions = defaultV3Opts
		}
	}
	pvs.FlexVolume = &core.FlexPersistentVolumeSource{
		Driver:   driverName,
		ReadOnly: false,
		Options: map[string]string{
			"server":  nasServer,
			"path":    filepath.Join(nasServerPath, pvDirectoryName),
			"vers":    flexNasVers,
			"mode":    options.StorageClass.Parameters["mode"],
			"options": flexNasOptions,
		},
	}

	// create PersistentVolume object
	pv := &core.PersistentVolume{
		ObjectMeta: meta.ObjectMeta{
			Name: options.PVName,
		},
		Spec: core.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: *options.StorageClass.ReclaimPolicy,
			AccessModes:                   options.PVC.Spec.AccessModes,
			MountOptions:                  options.StorageClass.MountOptions,
			Capacity: core.ResourceList{
				core.ResourceStorage: options.PVC.Spec.Resources.Requests[core.ResourceStorage],
			},
			PersistentVolumeSource: pvs,
		},
	}
	return pv, nil
}

func (p *nasProvisioner) Delete(pv *core.PersistentVolume) error {
	nasServer := pv.Spec.PersistentVolumeSource.FlexVolume.Options["server"]
	nasVers := pv.Spec.PersistentVolumeSource.FlexVolume.Options["vers"]
	pvPath := pv.Spec.PersistentVolumeSource.FlexVolume.Options["path"]
	if pvPath == "/" || pvPath == "" {
		klog.Errorf("deleteVolume: pvPath cannot be / or empty")
		return errors.New("pvPath cannot be / or empty")
	}
	pvDirectoryName := filepath.Base(pvPath)
	nasPath := getNasPathFromPvPath(pvPath)
	oldPath := filepath.Join(mountPath, pvDirectoryName)

	mntCmd := fmt.Sprintf("mount -t nfs -o vers=%s %s:%s %s", nasVers, nasServer, nasPath, mountPath)
	if _, err := runCmd(mntCmd); err != nil {
		klog.Errorf("mount nas directory failed: %s", err.Error())
		if _, err := runCmd("df -P | grep -iF " + mountPath); err != nil {
			klog.Error("the directory is not mounted, while the mount failed")
			return fmt.Errorf("mount directory failed: %s", err.Error())
		}
		klog.Warning("The directory is somehow already mounted, skip the mount")
	}
	defer func() {
		if _, err := runCmd("umount " + mountPath); err != nil {
			klog.Errorf("unmount directory failed: %s", err.Error())
			klog.Info("trying to do a force unmount")
			if _, err := runCmd("umount -f " + mountPath); err != nil {
				klog.Errorf("force unmount directory failed: %s", err.Error())
			}
		}
	}()

	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		klog.Warningf("path %s does not exist, deletion skipped", oldPath)
		return nil
	}

	// Get the storage class for this volume.
	storageClass, err := p.getClassForVolume(pv)
	if err != nil {
		klog.Errorf("failed to get storage class from volume %s: %s", pv.Name, err)
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

	archivePath := filepath.Join(mountPath, "archived-"+pvDirectoryName)
	klog.Infof("archiving path %s to %s", oldPath, archivePath)
	return os.Rename(oldPath, archivePath)
}

// getClassForVolume returns StorageClass
func (p *nasProvisioner) getClassForVolume(pv *core.PersistentVolume) (*storage.StorageClass, error) {
	if p.client == nil {
		return nil, fmt.Errorf("cannot get kube client")
	}
	//className := GetPersistentVolumeClass(pv)

	className := pv.Spec.StorageClassName
	// Use beta annotation first
	if classNameFromAnnotation, found := pv.Annotations[core.BetaStorageClassAnnotation]; found {
		className = classNameFromAnnotation
	}
	if className == "" {
		return nil, fmt.Errorf("volume has no storage class")
	}
	class, err := p.client.StorageV1().StorageClasses().Get(className, meta.GetOptions{})
	if err != nil {
		return nil, err
	}
	return class, nil
}

func getNasPathFromPvPath(pvPath string) (nasPath string) {
	tmpPath := pvPath
	if strings.HasSuffix(pvPath, "/") {
		tmpPath = pvPath[0 : len(pvPath)-1]
	}
	pos := strings.LastIndex(tmpPath, "/")
	nasPath = pvPath[0:pos]
	if nasPath == "" {
		nasPath = "/"
	}
	return
}

func runCmd(cmd string) (string, error) {
	out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to run cmd:%s; Output: %s; Error: %s", cmd, string(out), err.Error())
	}
	return string(out), nil
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

	// trying to mkdir of mountPath if there isn't any
	if err := os.MkdirAll(mountPath, 0777); err != nil {
		klog.Fatalf("unable to create default mountPath directory: " + err.Error())
	}
	clientNasProvisioner := &nasProvisioner{
		client: clientset,
	}
	// Start the provision controller which will dynamically provision Nas PVs
	pc := controller.NewProvisionController(clientset, provisionerName, clientNasProvisioner, serverVersion.GitVersion)
	pc.Run(wait.NeverStop)
}
