package quickfix

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type replayErrorWriter struct {
	err     error
	written chan []byte
}

func (w *replayErrorWriter) Write(p []byte) (int, error) { w.written <- p; return 1, w.err }
func TestWriterSignalsTerminalFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		terminal bool
	}{
		{"broken_pipe", fmt.Errorf("wrapped: %w", syscall.EPIPE), true},
		{"reset", &net.OpError{Op: "write", Err: syscall.ECONNRESET}, true},
		{"closed", fmt.Errorf("wrapped: %w", net.ErrClosed), true},
		{"timeout", os.ErrDeadlineExceeded, false},
		{"short_write", io.ErrShortWrite, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := make(chan []byte, 2)
			out <- []byte("first")
			out <- []byte("second")
			writer := &replayErrorWriter{err: tc.err, written: make(chan []byte, 2)}
			done := make(chan struct{})
			go func() { writeLoop(ctx, cancel, writer, out, nullLog{}); close(done) }()
			<-writer.written
			if tc.terminal {
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("terminal writer failure did not stop")
				}
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				require.Empty(t, writer.written, "writer continued after terminal failure")
			} else {
				select {
				case <-writer.written:
				case <-time.After(time.Second):
					t.Fatal("recoverable error stopped writer")
				}
				require.NoError(t, ctx.Err())
				close(out)
				<-done
			}
		})
	}
}
func TestReaderCancellationReleasesInboundHandoff(t *testing.T) {
	server, peer := net.Pipe()
	defer peer.Close()
	ctx, terminate := connectionLifetime(server)
	defer terminate()
	in := make(chan fixIn)
	done := make(chan struct{})
	go func() {
		readLoop(ctx, terminate, newParser(bufio.NewReader(server)), in, nullLog{})
		close(done)
	}()
	_, err := peer.Write(replayWireMessage("0", 1).build())
	require.NoError(t, err)
	terminate()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reader remained blocked delivering inbound message")
	}
}
