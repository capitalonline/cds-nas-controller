package main

import "k8s.io/client-go/kubernetes"

type NfsServer struct {
	Address string
	Path    string
}

type nasProvisioner struct {
	client kubernetes.Interface
}
