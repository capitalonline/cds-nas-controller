package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"
	"sigs.k8s.io/sig-storage-lib-external-provisioner/controller"
)

func (p *nasProvisioner) Provision(options controller.ProvisionOptions) (*core.PersistentVolume, error) {
	if options.PVC.Spec.Selector != nil {
		return nil, fmt.Errorf("claim Selector is not supported")
	}
	klog.Infof("provision volume: ver %s, VolumeOptions %+v", version, options)

	pvcNamespace := options.PVC.Namespace
	pvcName := options.PVC.Name
	pvDirectoryName := strings.Join([]string{pvcNamespace, pvcName, options.PVName}, "-")

	serverList := strings.Split(options.StorageClass.Parameters["servers"], ",")
	serverList = append(serverList, strings.Join([]string{options.StorageClass.Parameters["server"], options.StorageClass.Parameters["path"]}, "/"))
	servers := ParseServerList(serverList)
	var nfsServer *NfsServer
	switch len(servers) {
	case 0:
		return nil, errors.New("provision volume: at least one valid server or servers must be provided in the storage class parameters")
	case 1:
		nfsServer = servers[0]
	default:
		strategy, ok := options.StorageClass.Parameters["strategy"]
		if !ok {
			strategy = "RoundRobin"
		}
		nfsServer = SelectServer(servers, options.StorageClass.Name, strings.ToLower(strategy))
		if nfsServer == nil {
			klog.Errorf("provision volume: failed to choose a server using strategy %s, use the first one instead", strategy)
			nfsServer = servers[0]
		}
	}

	flexNasVers, ok := options.StorageClass.Parameters["vers"]
	if !ok {
		flexNasVers = "4.0"
	} else if strings.HasPrefix(flexNasVers, "3") {
		// ony vers=3 is supported
		flexNasVers = "3"
	}

	flexNasOptions, ok := options.StorageClass.Parameters["options"]
	if !ok {
		if strings.HasPrefix(flexNasVers, "4") {
			flexNasOptions = defaultV4Opts
		} else {
			flexNasOptions = defaultV3Opts
		}
	}
	pvs := core.PersistentVolumeSource{}
	pvs.FlexVolume = &core.FlexPersistentVolumeSource{
		Driver:   driverName,
		ReadOnly: false,
		Options: map[string]string{
			"server":  nfsServer.Address,
			"path":    filepath.Join(nfsServer.Path, pvDirectoryName),
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
	klog.Infof("provision volume: succeeded, volume is created: %s", pv.ObjectMeta.Name)
	return pv, nil
}

func (p *nasProvisioner) Delete(pv *core.PersistentVolume) error {
	sc:=pv.Spec.StorageClassName
	nasServer := pv.Spec.PersistentVolumeSource.FlexVolume.Options["server"]
	nasVers := pv.Spec.PersistentVolumeSource.FlexVolume.Options["vers"]
	pvPath := pv.Spec.PersistentVolumeSource.FlexVolume.Options["path"]
	normalizedMountPath := getNormalizedMountPath(DeleteMountRoot, sc, nasServer, pvPath)
	if err := os.MkdirAll(normalizedMountPath, 0777); err!=nil{
		klog.Errorf("delete volume: cannot make dir: %s", normalizedMountPath)
	}

	if pvPath == "/" || pvPath == "" {
		return errors.New("delete volume: pvPath cannot be / or empty")
	}
	pvDirectoryName := filepath.Base(pvPath)
	nasPath := getNasPathFromPvPath(pvPath)
	oldPath := filepath.Join(normalizedMountPath, pvDirectoryName)

	mntCmd := fmt.Sprintf("mount -t nfs -o vers=%s %s:%s %s", nasVers, nasServer, nasPath, normalizedMountPath)
	if _, err := runCmd(mntCmd); err != nil {
		klog.Errorf("delete volume: mount nas directory failed: %s", err.Error())
		if _, err := runCmd("df -P | grep -iF " + normalizedMountPath); err != nil {
			klog.Error("delete volume: the directory is not mounted, while the mount failed")
			return fmt.Errorf("delete volume: mount directory failed: %s", err.Error())
		}
		klog.Warning("delete volume: the directory is somehow already mounted, skip the mount")
	}
	defer func() {
		if _, err := runCmd("umount " + normalizedMountPath); err != nil {
			klog.Errorf("delete volume: unmount directory failed: %s", err.Error())
			klog.Info("delete volume: trying to do a force unmount")
			if _, err := runCmd("umount -f " + normalizedMountPath); err != nil {
				klog.Errorf("delete volume: force unmount directory failed: %s", err.Error())
			}
		}
	}()

	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		klog.Warningf("delete volume: path %s does not exist, deletion skipped", oldPath)
		return nil
	}

	// Get the storage class for this volume.
	storageClass, err := p.getClassForVolume(pv)
	if err != nil {
		klog.Errorf("delete volume: failed to get storage class from volume %s: %s", pv.Name, err)
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
			klog.Infof("delete volume: succeeded, volume directory is removed: %s", oldPath)
			return os.RemoveAll(oldPath)
		}
	}

	archivePath := filepath.Join(normalizedMountPath, "archived-"+pvDirectoryName)
	klog.Infof("delete volume: succeeded, archiving directory %s to %s", oldPath, archivePath)
	return os.Rename(oldPath, archivePath)
}
