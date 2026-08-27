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
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestWriteLoop(t *testing.T) {
	writer := bytes.NewBufferString("")
	msgOut := make(chan outboundMessage)

	go func() {
		msgOut <- outboundMessage{bytes: []byte("test msg 1 ")}
		msgOut <- outboundMessage{bytes: []byte("test msg 2 ")}
		msgOut <- outboundMessage{bytes: []byte("test msg 3")}
		close(msgOut)
	}()
	writeLoop(writer, msgOut, nullLog{}, SessionID{})

	expected := "test msg 1 test msg 2 test msg 3"

	if writer.String() != expected {
		t.Errorf("expected %v got %v", expected, writer.String())
	}
}

func TestWriteLoopReportsTrackedMessageCompletion(t *testing.T) {
	defer SetWriteCompletionObserver(nil)

	writer := bytes.NewBuffer(nil)
	msgOut := make(chan outboundMessage, 1)
	sessionID := SessionID{BeginString: BeginStringFIXT11, SenderCompID: "SENDER", TargetCompID: "TARGET"}
	admittedAt := time.Now().Add(-time.Millisecond).Truncate(time.Microsecond)
	message := []byte("8=FIXT.1.1\x019=12\x0135=X\x0110=000\x01")
	msgOut <- outboundMessage{bytes: message, writeToken: 42, admittedAtUnixNano: admittedAt.UnixNano()}
	close(msgOut)

	events := make(chan WriteCompletionEvent, 1)
	SetWriteCompletionObserver(func(event WriteCompletionEvent) { events <- event })
	writeLoop(writer, msgOut, nullLog{}, sessionID)

	event := <-events
	if event.SessionID != sessionID || event.MsgType != "X" || event.Token != 42 {
		t.Fatalf("unexpected identity: %#v", event)
	}
	if !event.AdmittedAt.Equal(admittedAt) {
		t.Fatalf("admitted at = %v, want %v", event.AdmittedAt, admittedAt)
	}
	if event.WriteStart.IsZero() || event.CompletedAt.Before(event.WriteStart) {
		t.Fatalf("invalid write timing: %#v", event)
	}
	if event.Bytes != len(message) || event.Err != nil {
		t.Fatalf("unexpected write result: %#v", event)
	}
	if !bytes.Equal(writer.Bytes(), message) {
		t.Fatalf("written bytes = %q, want %q", writer.Bytes(), message)
	}
}

func TestWriteLoopDoesNotObserveUntrackedMessage(t *testing.T) {
	defer SetWriteCompletionObserver(nil)

	called := false
	SetWriteCompletionObserver(func(WriteCompletionEvent) { called = true })
	msgOut := make(chan outboundMessage, 1)
	msgOut <- outboundMessage{bytes: []byte("untracked")}
	close(msgOut)
	writeLoop(io.Discard, msgOut, nullLog{}, SessionID{})

	if called {
		t.Fatal("write observer called for untracked message")
	}
}

type shortWriter struct{}

func (shortWriter) Write(message []byte) (int, error) {
	return len(message) - 1, nil
}

func TestWriteLoopReportsShortWrite(t *testing.T) {
	defer SetWriteCompletionObserver(nil)

	msgOut := make(chan outboundMessage, 1)
	msgOut <- outboundMessage{bytes: []byte("tracked"), writeToken: 99, admittedAtUnixNano: time.Now().UnixNano()}
	close(msgOut)
	events := make(chan WriteCompletionEvent, 1)
	SetWriteCompletionObserver(func(event WriteCompletionEvent) { events <- event })
	writeLoop(shortWriter{}, msgOut, nullLog{}, SessionID{})

	event := <-events
	if !errors.Is(event.Err, io.ErrShortWrite) {
		t.Fatalf("write error = %v, want %v", event.Err, io.ErrShortWrite)
	}
}

func TestReadLoop(t *testing.T) {
	msgIn := make(chan fixIn)
	stream := "hello8=FIX.4.09=5blah10=103garbage8=FIX.4.09=4foo10=103"

	parser := newParser(strings.NewReader(stream))
	go readLoop(parser, msgIn, nullLog{})

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
