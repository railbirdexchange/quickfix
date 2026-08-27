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
	"io"
	"time"
)

func writeLoop(connection io.Writer, messageOut chan outboundMessage, log Log, sessionID SessionID) {
	for {
		msg, ok := <-messageOut
		if !ok {
			return
		}

		var writeStartedAt time.Time
		if msg.writeToken != 0 {
			writeStartedAt = time.Now()
		}
		written, err := connection.Write(msg.bytes)
		if err == nil && written != len(msg.bytes) {
			err = io.ErrShortWrite
		}
		if msg.writeToken != 0 {
			observeWriteCompletion(WriteCompletionEvent{
				SessionID:   sessionID,
				MsgType:     fixMsgTypeFromRaw(msg.bytes),
				Token:       msg.writeToken,
				AdmittedAt:  time.Unix(0, msg.admittedAtUnixNano),
				WriteStart:  writeStartedAt,
				CompletedAt: time.Now(),
				Bytes:       written,
				Err:         err,
			})
		}
		if err != nil {
			log.OnEvent(err.Error())
		}
	}
}

func readLoop(parser *parser, msgIn chan fixIn, log Log) {
	defer close(msgIn)

	for {
		msg, err := parser.ReadMessage()
		if err != nil {
			log.OnEvent(err.Error())
			return
		}
		msgIn <- fixIn{msg, parser.lastRead}
	}
}
