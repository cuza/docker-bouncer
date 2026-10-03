package cli

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// Merging persistent flags panics on a shorthand clash (e.g. -f).
func TestNoFlagClashes(t *testing.T) {
	root := NewRoot(nil)
	for _, c := range root.Commands() {
		if _, _, err := root.Find([]string{c.Name(), "x"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInterruptCancelsContext(t *testing.T) {
	root := NewRoot(nil)
	var cancelled bool
	root.AddCommand(&cobra.Command{Use: "probe", RunE: func(cmd *cobra.Command, _ []string) error {
		syscall.Kill(os.Getpid(), syscall.SIGINT)
		select {
		case <-cmd.Context().Done():
			cancelled = true
		case <-time.After(5 * time.Second):
		}
		return nil
	}})
	root.SetArgs([]string{"probe"})
	if err := root.ExecuteContext(context.Background()); err != nil || !cancelled {
		t.Fatalf("SIGINT must cancel the command's context: %v", err)
	}
}
