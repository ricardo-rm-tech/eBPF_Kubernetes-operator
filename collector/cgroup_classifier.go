package main

import "strings"

type CgroupKind string

const (
	CgroupKindHost       CgroupKind = "host"
	CgroupKindSystemd    CgroupKind = "systemd"
	CgroupKindContainer  CgroupKind = "container"
	CgroupKindKubernetes CgroupKind = "kubernetes"
	CgroupKindUnknown    CgroupKind = "unknown"
)

func ClassifyCgroupPath(path string) CgroupKind {
	switch {
	case path == "" || path == "<desconocido>":
		return CgroupKindUnknown

	case path == "/sys/fs/cgroup":
		return CgroupKindHost

	case strings.Contains(path, "kubepods"):
		return CgroupKindKubernetes

	case strings.Contains(path, "containerd"),
		strings.Contains(path, "docker"),
		strings.Contains(path, "cri-containerd"):
		return CgroupKindContainer

	case strings.Contains(path, ".slice"),
		strings.Contains(path, "system.slice"),
		strings.Contains(path, "user.slice"),
		strings.Contains(path, "init.scope"):
		return CgroupKindSystemd

	default:
		return CgroupKindUnknown
	}
}
