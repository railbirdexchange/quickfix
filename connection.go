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

import "io"

func writeLoop(connection io.Writer, messageOut chan []byte, log Log) {
	writeLoopWithCompletion(connection, messageOut, log, nil)
}

// The completion notification is used by ownership benchmarks to observe the
// completed legacy write, not just an accepted channel send or entered Write.
func writeLoopWithCompletion(connection io.Writer, messageOut chan []byte, log Log, completed chan bool) {
	for {
		msg, ok := <-messageOut
		if !ok {
			return
		}

		if _, err := connection.Write(msg); err != nil {
			log.OnEvent(err.Error())
		}
		if completed != nil {
			completed <- true
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
		msgIn <- fixIn{bytes: msg, receiveTime: parser.lastRead}
	}
}
