package piruntime

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
)

func storeFile(path string) bool {
	if len(path) > 4096 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	for _, r := range path {
		if unicode.IsControl(r) {
			return false
		}
	}
	rel, err := filepath.Rel("/nix/store", path)
	if err != nil {
		return false
	}
	first, _, nested := strings.Cut(rel, "/")
	return nested && storePattern.MatchString("/nix/store/"+first)
}

func DecodeWorkerMetadata(data []byte) (workerMetadata, error) {
	var metadata workerMetadata
	if err := strictDecode(data, &metadata, 4096); err != nil {
		return metadata, err
	}
	if metadata.Schema != 1 || metadata.Protocol != 1 || !storeFile(metadata.Node) || !storeFile(metadata.Entrypoint) || !storePattern.MatchString(metadata.PiPackage) {
		return metadata, ErrScope
	}
	return metadata, nil
}

func validateWorkerPaths(metadata workerMetadata) error {
	for _, path := range []string{metadata.Node, metadata.Entrypoint} {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return ErrScope
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !storeFile(resolved) {
			return ErrScope
		}
		if err := syscall.Access(path, 4); err != nil {
			return err
		}
	}
	if err := syscall.Access(metadata.Node, 1); err != nil {
		return err
	}
	info, err := os.Stat(metadata.PiPackage)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrScope
	}
	return nil
}

func readWorkerMetadata(app string) (workerMetadata, error) {
	var metadata workerMetadata
	if !storePattern.MatchString(app) {
		return metadata, ErrScope
	}
	file, err := os.Open(filepath.Join(app, "share/dev-workspace/pi-worker.json"))
	if err != nil {
		return metadata, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return metadata, err
	}
	metadata, err = DecodeWorkerMetadata(data)
	if err != nil {
		return metadata, err
	}
	return metadata, validateWorkerPaths(metadata)
}
