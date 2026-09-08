//go:build !darwin

package common

import (
	"errors"
	"os"
)

func RemoveProgram(executable string) (string, error) {
	if err := os.Remove(executable); err != nil && !errors.Is(err, os.ErrNotExist) {
		return executable, err
	}
	return executable, nil
}
