package qos

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/X1Kun/AgentBox/internal/cgroup"
)

const maxObservedAncestors = 8

type HierarchyBoundary struct{ Path, Reason string }

func observationSources(parent, protected string, current func() (string, error), stat func(string) (os.FileInfo, error)) ([]Source, HierarchyBoundary) {
	sources := []Source{{Name: "host", Path: "/proc/pressure"}, {Name: "sandboxes", Path: parent, Cgroup: true}, {Name: "protected", Path: protected, Cgroup: true}}
	manager, err := current()
	managerSource := Source{Name: "manager", Path: manager, Cgroup: true}
	if err != nil {
		managerSource.DiscoveryError = err.Error()
	}
	sources = append(sources, managerSource)
	path := filepath.Dir(parent)
	for index := 0; index < maxObservedAncestors; index++ {
		if path == "/sys/fs/cgroup" {
			// The real kernel root has no bandwidth-limit interface. A private
			// cgroup namespace root can have one, e.g. Docker's outer CPU cap.
			_, err := stat(filepath.Join(path, "cpu.max"))
			if os.IsNotExist(err) {
				return sources, HierarchyBoundary{path, "kernel_root_without_cpu_max"}
			}
		}
		sources = append(sources, Source{Name: fmt.Sprintf("ancestor_%d", index), Path: path, Cgroup: true})
		if path == "/sys/fs/cgroup" {
			return sources, HierarchyBoundary{path, "visible_namespace_root"}
		}
		next := filepath.Dir(path)
		if next == path || next != "/sys/fs/cgroup" && !isCgroupPath(next) {
			return sources, HierarchyBoundary{path, "path_boundary"}
		}
		path = next
	}
	return sources, HierarchyBoundary{path, "depth_limit"}
}

func isCgroupPath(path string) bool {
	rel, err := filepath.Rel("/sys/fs/cgroup", path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && len(rel) > 0 && (len(rel) < 3 || rel[:3] != "../")
}

func discoverSources(parent, protected string) ([]Source, HierarchyBoundary) {
	return observationSources(parent, protected, cgroup.CurrentPath, os.Stat)
}
