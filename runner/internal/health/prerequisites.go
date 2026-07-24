package health

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type Dependencies struct {
	LookPath func(string) (string, error)
	MkdirAll func(string, os.FileMode) error
	Create   func(string) (*os.File, error)
	Remove   func(string) error
}

func Check(dataDirectory string, requireDocker bool, dependencies Dependencies) error {
	return CheckWithBackend(dataDirectory, requireDocker, "opencode", dependencies)
}

func CheckWithBackend(dataDirectory string, requireDocker bool, backendBinary string, dependencies Dependencies) error {
	if backendBinary == "" {
		return errors.New("runner agent backend binary is required")
	}
	if dependencies.LookPath == nil || dependencies.MkdirAll == nil || dependencies.Create == nil || dependencies.Remove == nil {
		return errors.New("runner health dependencies are required")
	}
	for _, binary := range requiredBinaries(requireDocker, backendBinary) {
		if _, err := dependencies.LookPath(binary); err != nil {
			return fmt.Errorf("required runner dependency %q is unavailable: %w", binary, err)
		}
	}
	if err := dependencies.MkdirAll(dataDirectory, 0700); err != nil {
		return fmt.Errorf("create runner data directory: %w", err)
	}
	probe := filepath.Join(dataDirectory, ".loop-write-probe")
	file, err := dependencies.Create(probe)
	if err != nil {
		return fmt.Errorf("runner data directory is not writable: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = dependencies.Remove(probe)
		return fmt.Errorf("close runner data directory probe: %w", err)
	}
	if err := dependencies.Remove(probe); err != nil {
		return fmt.Errorf("remove runner data directory probe: %w", err)
	}
	return nil
}

func DefaultDependencies() Dependencies {
	return Dependencies{
		LookPath: exec.LookPath,
		MkdirAll: os.MkdirAll,
		Create:   os.Create,
		Remove:   os.Remove,
	}
}

func requiredBinaries(requireDocker bool, backendBinary string) []string {
	binaries := []string{"git", backendBinary}
	if requireDocker {
		binaries = append(binaries, "docker")
	}
	return binaries
}
