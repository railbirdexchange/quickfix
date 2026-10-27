// Copyright (c) quickfixengine.org  All rights reserved.
//
// This file may be distributed under the terms of the quickfixengine.org
// license as defined by quickfixengine.org and appearing in the file
// LICENSE included in the packaging of this file.
//
// This file is provided AS IS with NO WARRANTY OF ANY KIND, INCLUDING
// THE WARRANTY OF DESIGN, MERCHANTABILITY AND FITNESS FOR A
// PARTICULAR PURPOSE.
//
// See http://www.quickfixengine.org/LICENSE for licensing information.
//
// Contact ask@quickfixengine.org if any conditions of this licensing
// are not clear to you.

package quickfix

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWriteLoop(t *testing.T) {
	writer := bytes.NewBufferString("")
	msgOut := make(chan []byte)

	go func() {
		msgOut <- []byte("test msg 1 ")
		msgOut <- []byte("test msg 2 ")
		msgOut <- []byte("test msg 3")
		close(msgOut)
	}()
	writeLoop(context.Background(), func() {}, writer, msgOut, nullLog{})

	expected := "test msg 1 test msg 2 test msg 3"

	if writer.String() != expected {
		t.Errorf("expected %v got %v", expected, writer.String())
	}
}

func TestReadLoop(t *testing.T) {
	msgIn := make(chan fixIn)
	stream := "hello8=FIX.4.09=5blah10=103garbage8=FIX.4.09=4foo10=103"

	parser := newParser(strings.NewReader(stream))
	go readLoop(context.Background(), func() {}, parser, msgIn, nullLog{})

	var tests = []struct {
		expectedMsg   string
		channelClosed bool
	}{
		{expectedMsg: "8=FIX.4.09=5blah10=103"},
		{expectedMsg: "8=FIX.4.09=4foo10=103"},
		{channelClosed: true},
	}

	for _, test := range tests {
		msg, ok := <-msgIn
		switch {
		case !ok && !test.channelClosed:
			t.Error("Channel unexpectedly closed")
			fallthrough
		case !ok && test.channelClosed:
			continue
		}

		if msg.bytes.String() != test.expectedMsg {
			t.Errorf("Expected %v got %v", test.expectedMsg, msg.bytes.String())
		}
	}
}

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
		{"unclassified", fmt.Errorf("unclassified writer error"), false},
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
func TestReaderExitCancelsConnection(t *testing.T) {
	for _, wire := range []string{"", "8=FIX.4.4\x019=invalid\x01"} {
		t.Run(fmt.Sprintf("wire_%q", wire), func(t *testing.T) {
			server, peer := net.Pipe()
			defer peer.Close()
			ctx, terminate := connectionLifetime(server)
			defer terminate()
			in := make(chan fixIn)
			readLoop(ctx, terminate, newParser(strings.NewReader(wire)), in, nullLog{})
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			_, ok := <-in
			require.False(t, ok)
			_, err := peer.Write([]byte("closed peer"))
			require.Error(t, err, "reader termination must close its socket")
			terminate() // Safe after reader cleanup.
		})
	}
}

func TestTerminalWriterReleasesBlockedReader(t *testing.T) {
	server, peer := net.Pipe()
	defer peer.Close()
	ctx, terminate := connectionLifetime(server)
	defer terminate()
	in := make(chan fixIn)
	readerDone := make(chan struct{})
	go func() { readLoop(ctx, terminate, newParser(bufio.NewReader(server)), in, nullLog{}); close(readerDone) }()
	_, err := peer.Write(replayWireMessage("0", 1).build())
	require.NoError(t, err)
	out := make(chan []byte, 1)
	out <- []byte("payload")
	writer := &replayErrorWriter{err: syscall.EPIPE, written: make(chan []byte, 1)}
	writeLoop(ctx, terminate, writer, out, nullLog{})
	awaitReplaySignal(t, readerDone)
	close(out) // Only the session closes out.
}
