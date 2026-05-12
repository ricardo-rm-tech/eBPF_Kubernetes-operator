package main

import (
	"encoding/binary"
	"fmt"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const cgroupRoot = "/sys/fs/cgroup"

// estructura formada por un mapa uint64 y string
// su funcion es guardar el cgroup_id y su nombre asociado
type CgroupResolver struct {
	ByID map[uint64]string
}

func BuildCgroupResolver(root string) (*CgroupResolver, error) {
	resolver := &CgroupResolver{
		ByID: make(map[uint64]string),
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// No abortamos todo el recorrido por un directorio concreto
			return nil
		}
		if !d.IsDir() {
			return nil
		}

		id, err := CgroupIDFromPath(path)
		if err != nil {
			// Hay directorios para los que puede fallar la resolución; los ignoramos
			return nil
		}

		resolver.ByID[id] = path
		return nil
	})
	if err != nil {
		return nil, err
	}

	return resolver, nil
}

func CgroupIDFromPath(path string) (uint64, error) {
	//ignoramos el valor 2 usando _
	handle, _, err := unix.NameToHandleAt(unix.AT_FDCWD, path, 0)
	if err != nil {
		return 0, err
	}

	raw := handle.Bytes()
	if len(raw) != 8 {
		return 0, fmt.Errorf("file handle inesperado para %s: %d bytes", path, len(raw))
	}

	// El kernel rellena f_handle en orden de bytes nativo: usar NativeEndian
	// es portable a arquitecturas big-endian sin asumir x86.
	id := binary.NativeEndian.Uint64(raw)
	return id, nil
}

func (r *CgroupResolver) Resolve(id uint64) string {
	if path, ok := r.ByID[id]; ok {
		return path
	}
	return "<desconocido>"
}
