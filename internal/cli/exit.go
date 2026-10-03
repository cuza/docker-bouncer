package cli

import (
	"fmt"

	dockercli "github.com/docker/cli/cli"
)

// Exit wraps err with the process exit code (1 bounce failed, 2 config
// error); plugin.Run exits with StatusError.StatusCode.
func Exit(code int, err error) error {
	if err == nil {
		return nil
	}
	return dockercli.StatusError{StatusCode: code, Status: err.Error(), Cause: err}
}

func configErr(format string, a ...any) error { return Exit(2, fmt.Errorf(format, a...)) }
