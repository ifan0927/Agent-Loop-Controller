package application

import (
	"fmt"
	"os"
)

func MaterializeArtifacts(specs []ArtifactSpec) error {
	for _, spec := range specs {
		if !spec.CreateExclusive {
			return fmt.Errorf("artifact must require exclusive creation: %s", spec.Path)
		}
		file, err := os.OpenFile(spec.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(spec.Mode))
		if err != nil {
			return fmt.Errorf("create artifact %s: %w", spec.Path, err)
		}
		_, writeErr := file.WriteString(spec.Content)
		closeErr := file.Close()
		if writeErr != nil {
			return fmt.Errorf("write artifact %s: %w", spec.Path, writeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close artifact %s: %w", spec.Path, closeErr)
		}
	}
	return nil
}
