package engine

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// downClient: the daemon is unreachable, Events fails at once.
type downClient struct{ client.APIClient }

func (downClient) Events(context.Context, client.EventsListOptions) client.EventsResult {
	errs := make(chan error, 1)
	errs <- errors.New("cannot connect to the Docker daemon")
	return client.EventsResult{Err: errs}
}

func TestWaitSleepsWhenEventsFail(t *testing.T) {
	start := time.Now()
	NewDocker(downClient{}).Wait(context.Background(), "proj", 100*time.Millisecond)
	if d := time.Since(start); d < 90*time.Millisecond {
		t.Fatalf("Wait returned after %v, want ≈100ms", d)
	}
}

// A TTY container's log stream is raw; any other is multiplexed.
func TestReadLogs(t *testing.T) {
	var mux bytes.Buffer
	for _, f := range []struct {
		stream byte
		data   string
	}{{1, "out\n"}, {2, "err\n"}} { // the multiplexed frame: stream, 3 zero bytes, big-endian size
		mux.Write([]byte{f.stream, 0, 0, 0, 0, 0, 0, byte(len(f.data))})
		mux.WriteString(f.data)
	}
	for _, tc := range []struct {
		in   []byte
		tty  bool
		want string
	}{
		{mux.Bytes(), false, "out\nerr\n"},
		{[]byte("migration failed\r\n"), true, "migration failed\r\n"},
	} {
		if got, err := readLogs(bytes.NewReader(tc.in), tc.tty); err != nil || got != tc.want {
			t.Errorf("tty %v: got %q %v, want %q", tc.tty, got, err, tc.want)
		}
	}
}
