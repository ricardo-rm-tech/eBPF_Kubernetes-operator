package main

import (
	"regexp"
	"strings"
)

type KubePathInfo struct {
	IsKubernetes bool
	PodUID       string
	ContainerID  string
	Runtime      string
}

var (
	rePodUIDDashed     = regexp.MustCompile(`pod([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)
	rePodUIDUnderscore = regexp.MustCompile(`pod([0-9a-f]{8}_[0-9a-f]{4}_[0-9a-f]{4}_[0-9a-f]{4}_[0-9a-f]{12})`)
	reContainerID      = regexp.MustCompile(`([0-9a-f]{64})`)
)

func ParseKubePath(path string) KubePathInfo {
	info := KubePathInfo{}

	if strings.Contains(path, "kubepods") {
		info.IsKubernetes = true
	}

	if strings.Contains(path, "containerd") || strings.Contains(path, "cri-containerd") {
		info.Runtime = "containerd"
	} else if strings.Contains(path, "docker") {
		info.Runtime = "docker"
	}

	if m := rePodUIDDashed.FindStringSubmatch(path); len(m) > 1 {
		info.PodUID = m[1]
	} else if m := rePodUIDUnderscore.FindStringSubmatch(path); len(m) > 1 {
		info.PodUID = strings.ReplaceAll(m[1], "_", "-")
	}

	if m := reContainerID.FindStringSubmatch(path); len(m) > 1 {
		info.ContainerID = m[1]
	}

	return info
}
