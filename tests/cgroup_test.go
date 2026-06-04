package tests

import (
	"regexp"
	"strings"
	"testing"
)

// ── Tipos y lógica copiados de collector (package main no es importable) ──

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

// ── Tests ──

func TestClassifyCgroupPath(t *testing.T) {
	cases := []struct {
		path string
		want CgroupKind
	}{
		{"/sys/fs/cgroup", CgroupKindHost},
		{"/sys/fs/cgroup/kubepods/burstable/podabc123", CgroupKindKubernetes},
		{"/sys/fs/cgroup/kubepods/besteffort/pod11111111-2222-3333-4444-555555555555/cri-containerd-aabbcc", CgroupKindKubernetes},
		{"/sys/fs/cgroup/system.slice/docker-abc123.scope", CgroupKindContainer},
		{"/sys/fs/cgroup/system.slice/kubelet.service", CgroupKindSystemd},
		{"/sys/fs/cgroup/user.slice/user-1000.slice", CgroupKindSystemd},
		{"/sys/fs/cgroup/init.scope", CgroupKindSystemd},
		{"/sys/fs/cgroup/containerd/abc123", CgroupKindContainer},
		{"/sys/fs/cgroup/docker/abc123", CgroupKindContainer},
		{"", CgroupKindUnknown},
		{"<desconocido>", CgroupKindUnknown},
		{"/sys/fs/cgroup/misc/something", CgroupKindUnknown},
	}

	for _, tc := range cases {
		got := ClassifyCgroupPath(tc.path)
		if got != tc.want {
			t.Errorf("ClassifyCgroupPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestParseKubePath_PodUID(t *testing.T) {
	uid := "11111111-2222-3333-4444-555555555555"
	path := "/sys/fs/cgroup/kubepods/burstable/pod" + uid + "/cri-containerd-aabbcc"
	info := ParseKubePath(path)

	if !info.IsKubernetes {
		t.Error("expected IsKubernetes=true")
	}
	if info.PodUID != uid {
		t.Errorf("PodUID = %q, want %q", info.PodUID, uid)
	}
	if info.Runtime != "containerd" {
		t.Errorf("Runtime = %q, want containerd", info.Runtime)
	}
}

func TestParseKubePath_UnderscoredUID(t *testing.T) {
	// Algunos runtimes usan guiones bajos en lugar de guiones en el cgroup path
	uidUnder := "11111111_2222_3333_4444_555555555555"
	uidDash := "11111111-2222-3333-4444-555555555555"
	path := "/sys/fs/cgroup/kubepods/pod" + uidUnder
	info := ParseKubePath(path)

	if info.PodUID != uidDash {
		t.Errorf("PodUID = %q, want %q (underscore→dash conversion)", info.PodUID, uidDash)
	}
}

func TestParseKubePath_ContainerID(t *testing.T) {
	containerID := strings.Repeat("a", 64)
	path := "/sys/fs/cgroup/kubepods/burstable/pod11111111-2222-3333-4444-555555555555/" + containerID
	info := ParseKubePath(path)

	if info.ContainerID != containerID {
		t.Errorf("ContainerID = %q, want %q", info.ContainerID, containerID)
	}
}

func TestParseKubePath_NonKube(t *testing.T) {
	info := ParseKubePath("/sys/fs/cgroup/system.slice/containerd.service")
	if info.IsKubernetes {
		t.Error("expected IsKubernetes=false for system.slice path")
	}
	// Runtime sí puede detectarse aunque no sea kubepods
	if info.Runtime != "containerd" {
		t.Errorf("Runtime = %q, want containerd", info.Runtime)
	}
}

func TestParseKubePath_Empty(t *testing.T) {
	info := ParseKubePath("")
	if info.IsKubernetes || info.PodUID != "" || info.Runtime != "" {
		t.Errorf("empty path should return zero KubePathInfo, got %+v", info)
	}
}
