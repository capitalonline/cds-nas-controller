package main

import (
	"fmt"
	"math/rand"
	"os/exec"
	"path/filepath"
	"strings"

	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func SelectServer(servers []*NfsServer, sc string, strategy string) *NfsServer {
	switch strategy {
	case "roundrobin":
		return SelectServerRoundRobin(servers, sc)
	case "random":
		return SelectServerRandom(servers)
	default:
		return servers[0]
	}
}

func SelectServerRoundRobin(servers []*NfsServer, scName string) *NfsServer {
	RRLock.Lock()
	count := RR[scName]
	RR[scName] = count + 1
	RRLock.Unlock()
	length := uint(len(servers))
	if length == 0 {
		return nil
	}
	return servers[count%length]
}

func SelectServerRandom(servers []*NfsServer) *NfsServer {
	length := len(servers)
	if length == 0 {
		return nil
	}
	return servers[rand.Intn(length)]
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
		return "", fmt.Errorf("Failed to run cmd: " + cmd + ", Output: " + string(out) + ", with error: " + err.Error())
	}
	return string(out), nil
}

func ParseServerList(serverList []string) []*NfsServer {
	servers := make([]*NfsServer, 0)
	for _, server := range serverList {

		addrPath := strings.SplitN(strings.TrimSpace(server), "/", 2)
		if len(addrPath) < 2 {
			continue
		}
		if addrPath[0] == "" {
			continue
		}
		addr := strings.TrimSpace(addrPath[0])
		path := strings.TrimSpace(addrPath[1])
		if path == "" {
			path = defaultNfsPath
		}
		servers = append(servers, &NfsServer{Address: addr, Path: filepath.Join("/", path)})
	}
	return servers
}

func getNormalizedMountPath(mountRoot, sc, server, path string ) string{
	sc = strings.ReplaceAll(sc, ".", "_")
	server = strings.ReplaceAll(server, ".", "_")
	return filepath.Join(mountRoot, sc, server, path)
}