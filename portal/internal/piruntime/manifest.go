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
	if !validString(path) || len(path) > 4096 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
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
	if !validString(metadata.PiPackage) || metadata.Schema != 1 || metadata.Protocol != 1 || !storeFile(metadata.Node) || !storeFile(metadata.Entrypoint) || !storePattern.MatchString(metadata.PiPackage) {
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
	resolved, err := filepath.EvalSymlinks(metadata.PiPackage)
	if err != nil || !storePattern.MatchString(resolved) {
		return ErrScope
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

// ValidatePackage checks immutable static inputs without loading SDK/auth or
// consulting a helper, socket or provider. It is not installed launch proof.
func ValidatePackage(app string) error {
	if !storePattern.MatchString(app) || !validString(app) {
		return ErrScope
	}
	if _, err := readWorkerMetadata(app); err != nil {
		return err
	}
	file, err := os.Open(filepath.Join(app, "share/dev-workspace/pi-runtime.json"))
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return err
	}
	if err := validateSupervisorManifest(data); err != nil {
		return err
	}
	executable := filepath.Join(app, "bin/workspace-portal")
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() {
		return ErrUnavailable
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil || !storeFile(resolved) {
		return ErrScope
	}
	return syscall.Access(executable, 5)
}

func validateSupervisorManifest(data []byte) error {
	var manifest struct {
		Schema     int      `json:"schema"`
		Host       int      `json:"host_launcher_contract"`
		Launch     int      `json:"launch_request_schema"`
		Handshake  int      `json:"handshake_schema"`
		Entrypoint string   `json:"supervisor_entrypoint"`
		Command    []string `json:"supervisor_command"`
	}
	if strictDecode(data, &manifest, 4096) != nil || manifest.Schema != 1 || manifest.Host != 1 || manifest.Launch != 1 || manifest.Handshake != 1 || manifest.Entrypoint != "bin/workspace-portal" || len(manifest.Command) != 2 || manifest.Command[0] != "pi-runtime" || manifest.Command[1] != "supervise" {
		return ErrScope
	}
	return nil
}
