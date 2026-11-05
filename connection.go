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
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
)

func connectionLifetime(conn net.Conn) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	return ctx, func() { once.Do(func() { cancel(); _ = conn.Close() }) }
}
func writeLoop(ctx context.Context, terminate context.CancelFunc, connection io.Writer, messageOut chan []byte, log Log) {
	defer terminate()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-messageOut:
			if !ok {
				return
			}
			if ctx.Err() != nil {
				return
			}
			if _, err := connection.Write(msg); err != nil {
				log.OnEvent(err.Error())
				if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, net.ErrClosed) {
					return
				}
			}
		}
	}
}
func readLoop(ctx context.Context, terminate context.CancelFunc, parser *parser, msgIn chan fixIn, log Log) {
	defer close(msgIn)
	defer terminate()
	for {
		msg, err := parser.ReadMessage()
		if err != nil {
			log.OnEvent(err.Error())
			return
		}
		select {
		case msgIn <- fixIn{msg, parser.lastRead}:
		case <-ctx.Done():
			return
		}
	}
}
