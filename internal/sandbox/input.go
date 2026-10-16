package sandbox

import (
	"os"
	"path/filepath"

	"github.com/X1Kun/AgentBox/internal/api"
)

func prepareInputDirectory(bundle string) error {
	input := filepath.Join(bundle, "input")
	if err := os.Mkdir(input, 0755); err != nil {
		return err
	}
	// Creation modes are filtered by the daemon's umask (0077 in systemd).
	// Set guest access explicitly; the enclosing bundle stays private (0700).
	if err := os.Chmod(input, 0755); err != nil {
		return err
	}
	return nil
}

func prepareInput(bundle string, request api.Request) error {
	input := filepath.Join(bundle, "input")
	if _, err := os.Stat(input); os.IsNotExist(err) {
		if err := prepareInputDirectory(bundle); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	task := filepath.Join(input, "task.json")
	if err := writeJSON(task, request, 0444); err != nil {
		return err
	}
	if err := os.Chmod(task, 0444); err != nil {
		return err
	}
	stdin := filepath.Join(input, "stdin")
	if err := os.WriteFile(stdin, []byte(request.Stdin), 0600); err != nil {
		return err
	}
	// stdin is donated as an open descriptor, not a guest-readable file.
	return os.Chmod(stdin, 0600)
}
