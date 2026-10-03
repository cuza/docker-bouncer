package cli

import (
	"errors"
	"testing"

	dockercli "github.com/docker/cli/cli"
)

func TestExitCarriesStatusCode(t *testing.T) {
	cause := errors.New("bad x-bouncer")
	err := Exit(2, cause)
	var se dockercli.StatusError
	if !errors.As(err, &se) || se.StatusCode != 2 || !errors.Is(err, cause) {
		t.Fatalf("Exit(2) = %#v", err)
	}
	if Exit(1, nil) != nil {
		t.Fatal("Exit(1, nil) must be nil")
	}
}
