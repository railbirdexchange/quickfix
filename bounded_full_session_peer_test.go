package quickfix

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/config"
	"github.com/stretchr/testify/require"
)

type boundedFullSession struct {
	tb                            testing.TB
	session                       *session
	application                   *boundedFullApplication
	peer, server                  *net.TCPConn
	reader                        *boundedReadConnection
	writer                        *boundedWriteConnection
	loopDone                      chan struct{}
	builder                       *OutboundMessage
	peerSlot                      *MessageSlot
	peerValidator                 *BoundedValidator
	peerParser                    *parser
	frame, encoded, savedBody     []byte
	savedTime                     [64]byte
	savedTimeLength               int
	sendSequence, receiveSequence int
	bytes                         int64
	closed                        bool
	payload                       []byte
	slowTimer                     *time.Timer
	lastCheckpoint                boundedFullCheckpoint
	wrapServer                    func(net.Conn) net.Conn
}

func newBoundedFullSession(tb testing.TB, groups bool) *boundedFullSession {
	tb.Helper()
	application := &boundedFullApplication{groups: groups, checkpoints: make(chan boundedFullCheckpoint, 1), logons: make(chan struct{}, 1), logouts: make(chan struct{}, 1), slowReady: make(chan struct{}, 1), commands: make(chan func(*boundedFullApplication), 1)}
	settings := NewSessionSettings()
	settings.Set(config.BoundedInbound, "Y")
	settings.Set(config.BoundedOutbound, "Y")
	settings.Set(config.DataDictionary, "spec/FIX44.xml")
	settings.Set(config.TimeStampPrecision, "MILLIS")
	s, err := (sessionFactory{}).newSession(SessionID{BeginString: BeginStringFIX44, SenderCompID: "SERVER", TargetCompID: "PEER"}, NewBoundedStoreFactory(1024, 16384), settings, NewNullLogFactory(), application)
	require.NoError(tb, err)
	application.session = s
	limits := DefaultParseLimits()
	builder, err := NewOutboundMessage(2*limits.MaxFields, 32768)
	require.NoError(tb, err)
	peerSlot, err := NewMessageSlot(nil, s.appDataDictionary, limits)
	require.NoError(tb, err)
	validator, err := NewBoundedValidator(defaultValidatorSettings, s.appDataDictionary, nil, limits.MaxFields)
	require.NoError(tb, err)
	f := &boundedFullSession{tb: tb, session: s, application: application, loopDone: make(chan struct{}), builder: builder, peerSlot: peerSlot, peerValidator: validator, frame: make([]byte, limits.MaxFrameBytes), encoded: make([]byte, 0, limits.MaxFrameBytes), savedBody: make([]byte, 0, limits.MaxFrameBytes), sendSequence: 1, receiveSequence: 1}
	go func() { s.run(); close(f.loopDone) }()
	tb.Cleanup(f.close)
	require.NoError(tb, f.connect())
	return f
}

func (f *boundedFullSession) connect() error {
	return f.connectWithAcknowledgement(true)
}

func (f *boundedFullSession) connectWithAcknowledgement(acknowledge bool) error {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return err
	}
	defer listener.Close()
	peer, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		return err
	}
	server, err := listener.AcceptTCP()
	if err != nil {
		peer.Close()
		return err
	}
	f.peer, f.server = peer, server
	if err := peer.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	peerParser, err := newBoundedParser(peer, DefaultParseLimits().MaxFrameBytes)
	if err != nil {
		return err
	}
	f.peerParser = peerParser
	var connection net.Conn = server
	if f.wrapServer != nil {
		connection = f.wrapServer(server)
	}
	s := f.session
	messageIn := make(chan fixIn, s.InChanCapacity)
	reader, err := startBoundedRead(connection, newParser(connection), s.boundedIn, messageIn, nil, time.Time{})
	if err != nil {
		return err
	}
	writer := newBoundedWriteConnection(connection, s.boundedOut)
	f.reader, f.writer = reader, writer
	go func() {
		writer.run()
		connection.Close()
	}()
	if err := s.connectWithIO(messageIn, nil, reader, writer); err != nil {
		return err
	}
	f.prepare("A", f.sendSequence)
	f.builder.Body().SetString(tagEncryptMethod, "0")
	f.builder.Body().SetInt(tagHeartBtInt, 60)
	if err := f.writePrepared(); err != nil {
		return err
	}
	f.sendSequence++
	message, err := f.readMessage()
	if err != nil {
		return err
	}
	kind, reject := message.Header.GetBytes(tagMsgType)
	if reject != nil {
		return reject
	}
	if string(kind) != "A" {
		return errBoundedFullFixture
	}
	<-f.application.logons
	if !acknowledge {
		return nil
	}
	_, err = f.acknowledge(true)
	return err
}

func (f *boundedFullSession) close() {
	if f.closed {
		return
	}
	f.closed = true
	if f.slowTimer != nil {
		f.slowTimer.Stop()
	}
	if f.peer != nil {
		f.peer.Close()
	}
	if f.server != nil {
		f.server.Close()
	}
	f.session.stop()
	select {
	case <-f.loopDone:
	case <-time.After(10 * time.Second):
		f.tb.Error("full session did not stop after socket closure")
		return
	}
	assertBoundedOutboundSlotsReturned(f.tb, f.session.boundedOut)
	if got, want := len(f.session.boundedIn.ring.vacant), len(f.session.boundedIn.ring.slots); got != want {
		f.tb.Errorf("returned inbound slots = %d, want %d", got, want)
	}
}

func (f *boundedFullSession) prepare(kind string, sequence int) {
	f.builder.Reset()
	header := f.builder.Header()
	header.SetString(tagBeginString, BeginStringFIX44)
	header.SetString(tagMsgType, kind)
	header.SetInt(tagMsgSeqNum, sequence)
	header.SetString(tagSenderCompID, "PEER")
	header.SetString(tagTargetCompID, "SERVER")
	header.SetTime(tagSendingTime, FIXUTCTimestamp{Time: time.Now().UTC(), Precision: Millis})
}

func (f *boundedFullSession) writePrepared() error {
	if err := f.builder.Err(); err != nil {
		return err
	}
	wire, err := f.builder.Message().AppendTo(f.encoded[:0])
	if err != nil {
		return err
	}
	f.bytes += int64(len(wire))
	for len(wire) != 0 {
		written, err := f.peer.Write(wire)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrNoProgress
		}
		wire = wire[written:]
	}
	return nil
}

func (f *boundedFullSession) sendOrder(sequence int) error {
	f.prepare("D", sequence)
	body := f.builder.Body()
	body.SetInt(11, sequence)
	body.SetString(21, "1")
	body.SetString(55, "SYMABC")
	body.SetString(54, "1")
	body.SetString(38, "10")
	body.SetString(40, "2")
	body.SetString(44, "12.50")
	body.SetTime(60, FIXUTCTimestamp{Time: time.Now().UTC(), Precision: Millis})
	if f.application.groups {
		body.SetGroup(453, boundedFullParties[:])
	}
	if f.payload != nil {
		body.SetBytes(58, f.payload)
	}
	if err := f.writePrepared(); err != nil {
		return err
	}
	if sequence == f.sendSequence {
		f.sendSequence++
	}
	return nil
}

func (f *boundedFullSession) readMessage() (*Message, error) {
	wire, err := f.peerParser.ReadMessageInto(f.frame[:0])
	if err != nil {
		return nil, err
	}
	f.bytes += int64(len(wire))
	if err := f.peerSlot.Parse(wire); err != nil {
		return nil, err
	}
	message := f.peerSlot.Message()
	if reject, err := f.peerValidator.Validate(message); err != nil {
		return nil, err
	} else if reject != nil {
		return nil, reject
	}
	checksum, reject := message.Trailer.GetInt(tagCheckSum)
	if reject != nil {
		return nil, reject
	}
	var total byte
	for _, value := range wire[:len(wire)-7] {
		total += value
	}
	if int(total) != checksum {
		return nil, errBoundedFullWire
	}
	sequence, reject := message.Header.GetInt(tagMsgSeqNum)
	if reject != nil {
		return nil, reject
	}
	var duplicate []byte
	if message.Header.Has(tagPossDupFlag) {
		duplicate, _ = message.Header.GetBytes(tagPossDupFlag)
	}
	if string(duplicate) != "Y" {
		if sequence != f.receiveSequence {
			return nil, errBoundedFullFixture
		}
		f.receiveSequence++
	} else if sequence >= f.receiveSequence {
		return nil, errBoundedFullFixture
	}
	return message, nil
}

func (f *boundedFullSession) readReport() (*Message, error) {
	message, err := f.readMessage()
	if err != nil {
		return nil, err
	}
	kind, reject := message.Header.GetBytes(tagMsgType)
	if reject != nil {
		return nil, reject
	}
	if string(kind) != "8" {
		return nil, errBoundedFullFixture
	}
	if f.application.groups {
		if err := boundedFullCheckGroups(f.peerSlot); err != nil {
			return nil, err
		}
	}
	return message, nil
}

func (f *boundedFullSession) acknowledge(rotate bool) (boundedFullCheckpoint, error) {
	f.prepare("0", f.sendSequence)
	f.builder.Header().SetInt(tagLastMsgSeqNumProcessed, f.receiveSequence-1)
	if rotate {
		f.builder.Body().SetString(tagTestReqID, "warm")
	}
	if err := f.writePrepared(); err != nil {
		return boundedFullCheckpoint{}, err
	}
	f.sendSequence++
	checkpoint := <-f.application.checkpoints
	f.lastCheckpoint = checkpoint
	if checkpoint.err != nil {
		return checkpoint, checkpoint.err
	}
	if checkpoint.nextSender != f.receiveSequence || checkpoint.nextTarget != f.sendSequence-1 || checkpoint.freeOutputs != 33 || checkpoint.retained != 0 {
		return checkpoint, errBoundedFullFixture
	}
	return checkpoint, nil
}

func (f *boundedFullSession) configure(command func(*boundedFullApplication)) error {
	f.application.commands <- command
	_, err := f.acknowledge(false)
	return err
}

func (f *boundedFullSession) ordinaryBatch(count int, rotate bool) error {
	if err := f.unacknowledgedBatch(count); err != nil {
		return err
	}
	_, err := f.acknowledge(rotate)
	return err
}

func (f *boundedFullSession) unacknowledgedBatch(count int) error {
	for range count {
		if err := f.sendOrder(f.sendSequence); err != nil {
			return err
		}
	}
	for range count {
		if _, err := f.readReport(); err != nil {
			return err
		}
	}
	return nil
}

func (f *boundedFullSession) outboundResend() error {
	if err := f.sendOrder(f.sendSequence); err != nil {
		return err
	}
	message, err := f.readReport()
	if err != nil {
		return err
	}
	sequence, reject := message.Header.GetInt(tagMsgSeqNum)
	if reject != nil {
		return reject
	}
	f.savedBody = append(f.savedBody[:0], message.bodyBytes...)
	original, reject := message.Header.GetBytes(tagSendingTime)
	if reject != nil {
		return reject
	}
	f.savedTimeLength = copy(f.savedTime[:], original)
	f.prepare("2", f.sendSequence)
	f.builder.Body().SetInt(tagBeginSeqNo, sequence)
	f.builder.Body().SetInt(tagEndSeqNo, sequence)
	if err := f.writePrepared(); err != nil {
		return err
	}
	f.sendSequence++
	message, err = f.readReport()
	if err != nil {
		return err
	}
	resentSequence, reject := message.Header.GetInt(tagMsgSeqNum)
	if reject != nil {
		return reject
	}
	original, reject = message.Header.GetBytes(tagOrigSendingTime)
	if reject != nil {
		return reject
	}
	duplicate, _ := message.Header.GetBytes(tagPossDupFlag)
	if resentSequence != sequence || string(duplicate) != "Y" || !bytes.Equal(original, f.savedTime[:f.savedTimeLength]) || !bytes.Equal(message.bodyBytes, f.savedBody) {
		return errBoundedFullFixture
	}
	_, err = f.acknowledge(false)
	return err
}

func (f *boundedFullSession) inboundGap() error {
	missing := f.sendSequence
	if err := f.sendOrder(missing + 1); err != nil {
		return err
	}
	request, err := f.readMessage()
	if err != nil {
		return err
	}
	kind, reject := request.Header.GetBytes(tagMsgType)
	if reject != nil {
		return reject
	}
	begin, reject := request.Body.GetInt(tagBeginSeqNo)
	if reject != nil {
		return reject
	}
	end, reject := request.Body.GetInt(tagEndSeqNo)
	if reject != nil {
		return reject
	}
	if string(kind) != "2" || begin != missing || end != 0 {
		return errBoundedFullFixture
	}
	if err := f.sendOrder(missing); err != nil {
		return err
	}
	f.sendSequence++ // The future frame has already transferred to the session.
	for expected := missing; expected <= missing+1; expected++ {
		report, err := f.readReport()
		if err != nil {
			return err
		}
		identifier, reject := report.Body.GetInt(11)
		if reject != nil {
			return reject
		}
		if identifier != expected {
			return errBoundedFullFixture
		}
	}
	_, err = f.acknowledge(false)
	return err
}

func (f *boundedFullSession) prepareSlowConsumer() error {
	f.payload = bytes.Repeat([]byte("x"), 8192)
	f.slowTimer = time.NewTimer(10 * time.Second)
	f.slowTimer.Stop()
	if err := f.server.SetWriteBuffer(8192); err != nil {
		return err
	}
	if err := f.peer.SetReadBuffer(16384); err != nil {
		return err
	}
	return f.configure(func(a *boundedFullApplication) { a.reportsPerOrder = 64 })
}

func (f *boundedFullSession) slowConsumer() error {
	f.slowTimer.Reset(10 * time.Second)
	if err := f.sendOrder(f.sendSequence); err != nil {
		return err
	}
	// The peer deliberately waits until the session has observed its fixed
	// output queue full. It then consumes the entire persisted burst before ACK.
	select {
	case <-f.application.slowReady:
		f.slowTimer.Stop()
	case <-f.slowTimer.C:
		return errBoundedFullFixture
	}
	for range 64 {
		report, err := f.readReport()
		if err != nil {
			return err
		}
		text, reject := report.Body.GetBytes(58)
		if reject != nil {
			return reject
		}
		if !bytes.Equal(text, f.payload) {
			return errBoundedFullFixture
		}
	}
	_, err := f.acknowledge(false)
	return err
}

func (f *boundedFullSession) warm() error {
	// Fill every journal slot with real, peer-validated messages before the
	// peer explicitly acknowledges the complete range. No history is reset.
	for range 32 {
		if err := f.unacknowledgedBatch(32); err != nil {
			return err
		}
	}
	if _, err := f.acknowledge(true); err != nil {
		return err
	}
	// Rotate every output slot; the FIFO input ring has already wrapped. Both
	// sequence counters now exceed the small-integer boxing cache.
	for range 33 {
		if err := f.ordinaryBatch(1, true); err != nil {
			return err
		}
	}
	return f.outboundResend()
}
