package quickfix

import (
	"bytes"
	"context"
)

// A replay worker only reads storage. The session loop owns FIX processing and
// output, so an incoming Logout can interrupt a blocked read or send.
type replayRequest struct {
	begin, end int
	reply      Message
}
type replayOperation struct {
	cancel    context.CancelFunc
	messages  chan []byte
	done      chan struct{}
	err       error // published by closing done
	request   replayRequest
	seq, next int
	output    [][]byte // at most a gap fill and one application message
	finishing bool
}

func (s *session) startReplay(request replayRequest) {
	s.sendMutex.Lock()
	queued := len(s.toSend) > 0
	s.sendMutex.Unlock()
	if s.replay != nil || queued {
		if s.replay == nil {
			s.notifyMessageOut()
		}
		// Coalesce queued requests into a covering range, bounding backlog without
		// dropping a requested sequence. The active range completes first.
		if s.nextReplay == nil {
			s.nextReplay = &request
		} else {
			s.nextReplay.begin = min(s.nextReplay.begin, request.begin)
			s.nextReplay.end = max(s.nextReplay.end, request.end)
		}
		return
	}
	ctx, cancel := context.WithCancel(s.connectionContext)
	r := &replayOperation{cancel: cancel, messages: make(chan []byte), done: make(chan struct{}), request: request, seq: request.begin, next: request.begin}
	s.replay = r
	if s.DisableMessagePersist {
		r.next = request.end + 1
		close(r.done)
		return
	}
	store := s.store.(ContextMessageStore)
	go func() {
		defer close(r.done)
		r.err = store.IterateMessagesContext(ctx, request.begin, request.end, func(raw []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			owned := bytes.Clone(raw)
			select {
			case r.messages <- owned:
				return ctx.Err()
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
}

func (s *session) stopReplay() {
	s.nextReplay = nil
	if r := s.replay; r != nil {
		r.cancel()
		<-r.done
		s.replay = nil
	}
}

func (s *session) replayChannels() (<-chan []byte, <-chan struct{}, chan<- []byte, []byte) {
	r := s.replay
	if r == nil {
		if next := s.nextReplay; next != nil {
			s.sendMutex.Lock()
			empty := len(s.toSend) == 0
			s.sendMutex.Unlock()
			if empty {
				s.nextReplay = nil
				s.startReplay(*next)
				return s.replayChannels()
			}
		}
		return nil, nil, nil, nil
	}
	if len(r.output) > 0 {
		return nil, nil, s.messageOut, r.output[0]
	}
	if r.finishing {
		r.cancel()
		s.replay = nil
		// Original numbered output must reach the writer before another request
		// can replay it. The ordinary queue drain stays nonblocking.
		s.notifyMessageOut()
		return s.replayChannels()
	}
	return r.messages, r.done, nil, nil
}

func (s *session) replayMessage(raw []byte) {
	r := s.replay
	msg := NewMessage()
	if err := ParseMessageWithDataDictionary(msg, bytes.NewBuffer(raw), s.transportDataDictionary, s.appDataDictionary); err != nil {
		s.failReplay(err)
		return
	}
	kind, _ := msg.Header.GetBytes(tagMsgType)
	seq, _ := msg.Header.GetInt(tagMsgSeqNum)
	if isAdminMessageType(kind) || !s.resend(msg) {
		r.next = seq + 1
		return
	}
	if r.seq != seq {
		r.output = append(r.output, s.replayGapFill(r.seq, seq, r.request.reply))
	}
	data := msg.buildWithBodyBytes(msg.bodyBytes)
	s.notifyMessageBuilt(msg, data)
	r.output = append(r.output, data)
	r.seq = seq + 1
	r.next = r.seq
}

func (s *session) replayComplete() {
	r := s.replay
	if r.err != nil {
		s.failReplay(r.err)
		return
	}
	if r.seq != r.next {
		r.output = append(r.output, s.replayGapFill(r.seq, r.next, r.request.reply))
	}
	r.finishing = true
}

func (s *session) failReplay(err error) {
	s.logError(err)
	s.stopReplay()
	s.Disconnected(s)
}

func (s *session) replayGapFill(begin, end int, reply Message) []byte {
	msg := NewMessage()
	s.fillDefaultHeader(msg, &reply)
	msg.Header.SetString(tagMsgType, "4")
	msg.Header.SetInt(tagMsgSeqNum, begin)
	msg.Header.SetField(tagPossDupFlag, FIXBoolean(true))
	msg.Body.SetInt(tagNewSeqNo, end)
	msg.Body.SetField(tagGapFillFlag, FIXBoolean(true))
	if stamp, err := msg.Header.GetString(tagSendingTime); err == nil {
		msg.Header.SetString(tagOrigSendingTime, stamp)
	}
	s.application.ToAdmin(msg, s.sessionID)
	return msg.build()
}
