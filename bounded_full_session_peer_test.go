package quickfix

import (
	"bytes"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/config"
	"github.com/stretchr/testify/require"
)

// This peer encoder is test-owned and independent of Message.AppendTo and the
// session's mutation arena. Every append is bounded before it runs. Thus peer
// request construction cannot conceal engine allocations in whole-process counts.
type boundedFullPeerFields struct {
	wire []byte
	err  error
}

func (fields *boundedFullPeerFields) reserve(tag Tag, length int) bool {
	if fields.err != nil {
		return false
	}
	if length+24 > cap(fields.wire)-len(fields.wire) {
		fields.err = io.ErrShortBuffer
		return false
	}
	fields.wire = strconv.AppendInt(fields.wire, int64(tag), 10)
	fields.wire = append(fields.wire, '=')
	return true
}
func (fields *boundedFullPeerFields) SetBytes(tag Tag, value []byte) {
	if fields.reserve(tag, len(value)) {
		fields.wire = append(fields.wire, value...)
		fields.wire = append(fields.wire, 1)
	}
}
func (fields *boundedFullPeerFields) SetString(tag Tag, value string) {
	if fields.reserve(tag, len(value)) {
		fields.wire = append(fields.wire, value...)
		fields.wire = append(fields.wire, 1)
	}
}
func (fields *boundedFullPeerFields) SetInt(tag Tag, value int) {
	if fields.reserve(tag, 20) {
		fields.wire = strconv.AppendInt(fields.wire, int64(value), 10)
		fields.wire = append(fields.wire, 1)
	}
}
func (fields *boundedFullPeerFields) SetTime(tag Tag, value time.Time) {
	if fields.reserve(tag, 21) {
		fields.wire = value.AppendFormat(fields.wire, "20060102-15:04:05.000")
		fields.wire = append(fields.wire, 1)
	}
}
func (fields *boundedFullPeerFields) addParties() {
	if len(boundedFullPartyWire) > cap(fields.wire)-len(fields.wire) {
		fields.err = io.ErrShortBuffer
		return
	}
	fields.wire = append(fields.wire, boundedFullPartyWire...)
}

type boundedFullPeerBuilder struct{ header, body boundedFullPeerFields }

func (builder *boundedFullPeerBuilder) Header() *boundedFullPeerFields { return &builder.header }
func (builder *boundedFullPeerBuilder) Body() *boundedFullPeerFields   { return &builder.body }
func (builder *boundedFullPeerBuilder) Reset() {
	builder.header.wire = builder.header.wire[:0]
	builder.body.wire = builder.body.wire[:0]
	builder.header.err = nil
	builder.body.err = nil
}
func (builder *boundedFullPeerBuilder) appendTo(dst []byte) ([]byte, error) {
	if builder.header.err != nil {
		return dst, builder.header.err
	}
	if builder.body.err != nil {
		return dst, builder.body.err
	}
	length := len(builder.header.wire) + len(builder.body.wire)
	if length+48 > cap(dst)-len(dst) {
		return dst, io.ErrShortBuffer
	}
	dst = append(dst, "8=FIX.4.4\x019="...)
	dst = strconv.AppendInt(dst, int64(length), 10)
	dst = append(dst, 1)
	dst = append(dst, builder.header.wire...)
	dst = append(dst, builder.body.wire...)
	var checksum byte
	for _, value := range dst {
		checksum += value
	}
	dst = append(dst, '1', '0', '=', '0'+checksum/100, '0'+checksum/10%10, '0'+checksum%10, 1)
	return dst, nil
}

type boundedFullSavedReport struct {
	sequence, timeLength int
	timestamp            [64]byte
	body                 []byte
}
type boundedFullSession struct {
	tb                            testing.TB
	session                       *session
	application                   *boundedFullApplication
	peer, server                  *net.TCPConn
	reader                        *boundedReadConnection
	writer                        *outboundWriter
	loopDone                      chan struct{}
	builder                       boundedFullPeerBuilder
	peerSlot                      *MessageSlot
	peerValidator                 *BoundedValidator
	peerParser                    *parser
	frame, encoded, savedBody     []byte
	savedTime                     [64]byte
	savedTimeLength               int
	savedReports                  [40]boundedFullSavedReport
	sendSequence, receiveSequence int
	bytes                         int64
	closed                        bool
	payload                       []byte
	slowTimer                     *time.Timer
	slowGate                      *boundedLifecycleWriteGate
	lastCheckpoint                boundedFullCheckpoint
	wrapServer                    func(net.Conn) net.Conn
	socketOutboundBufferSize      int
}

func newBoundedFullSession(tb testing.TB, groups bool) *boundedFullSession {
	tb.Helper()
	application := &boundedFullApplication{groups: groups, response: newBoundedFullResponse(groups), checkpoints: make(chan boundedFullCheckpoint, 1), logons: make(chan struct{}, 1), logouts: make(chan struct{}, 1), admissions: make(chan boundedFullAdmission, 1), commands: make(chan func(*boundedFullApplication), 1)}
	settings := NewSessionSettings()
	settings.Set(config.DataDictionary, "spec/FIX44.xml")
	settings.Set(config.TimeStampPrecision, "MILLIS")
	// Existing capacities only. No bounded-mode or alternate Application switch.
	settings.Set(config.InChanCapacity, "32")
	settings.Set(config.SocketOutboundBufferSize, "32")
	settings.Set(config.OutboundFrameQueueCapacity, "32")
	socketOutboundBufferSize, err := nonNegativeIntSetting(settings, config.SocketOutboundBufferSize)
	require.NoError(tb, err)
	s, err := (sessionFactory{}).newSession(SessionID{BeginString: BeginStringFIX44, SenderCompID: "SERVER", TargetCompID: "PEER", Qualifier: "ordinary-full-session"}, NewBoundedStoreFactory(1024, 16384), settings, NewNullLogFactory(), application)
	require.NoError(tb, err)
	require.NoError(tb, registerSession(s))
	application.session = s
	limits := DefaultParseLimits()
	peerSlot, err := NewMessageSlot(nil, s.appDataDictionary, limits)
	require.NoError(tb, err)
	validator, err := NewBoundedValidator(defaultValidatorSettings, s.appDataDictionary, nil, limits.MaxFields)
	require.NoError(tb, err)
	f := &boundedFullSession{tb: tb, session: s, application: application, loopDone: make(chan struct{}), peerSlot: peerSlot, peerValidator: validator, frame: make([]byte, limits.MaxFrameBytes), encoded: make([]byte, 0, limits.MaxFrameBytes), savedBody: make([]byte, 0, limits.MaxFrameBytes), sendSequence: 1, receiveSequence: 1}
	f.socketOutboundBufferSize = socketOutboundBufferSize
	f.builder.header.wire = make([]byte, 0, 1024)
	f.builder.body.wire = make([]byte, 0, limits.MaxFrameBytes)
	for index := range f.savedReports {
		f.savedReports[index].body = make([]byte, 0, limits.MaxFrameBytes)
	}
	go func() { s.run(); close(f.loopDone) }()
	tb.Cleanup(f.close)
	require.NoError(tb, f.connect())
	return f
}

func (f *boundedFullSession) connect() error { return f.connectWithAcknowledgement(true) }
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
	if err := peer.SetDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		return err
	}
	f.peerParser, err = newBoundedParser(peer, DefaultParseLimits().MaxFrameBytes)
	if err != nil {
		return err
	}
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
	writer := newOutboundWriter(connection, min(f.socketOutboundBufferSize, len(s.outbound.pending)), s.outbound.writeTimeout, s.messageEvent)
	f.reader, f.writer = reader, writer
	go writer.run()
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
	if f.slowGate != nil {
		close(f.slowGate.cancel)
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
	_ = UnregisterSession(f.session.sessionID)
	if got, want := len(f.session.outbound.vacant), len(f.session.outbound.slots); got != want {
		f.tb.Errorf("returned outbound slots = %d, want %d", got, want)
	}
	if got, want := len(f.session.boundedIn.ring.vacant), len(f.session.boundedIn.ring.slots); got != want {
		f.tb.Errorf("returned inbound slots = %d, want %d", got, want)
	}
}

func (f *boundedFullSession) prepare(kind string, sequence int) {
	f.builder.Reset()
	header := f.builder.Header()
	header.SetString(tagMsgType, kind)
	header.SetInt(tagMsgSeqNum, sequence)
	header.SetString(tagSenderCompID, "PEER")
	header.SetString(tagTargetCompID, "SERVER")
	header.SetTime(tagSendingTime, time.Now().UTC())
}

func (f *boundedFullSession) writePrepared() error {
	wire, err := f.builder.appendTo(f.encoded[:0])
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
	return f.sendOrderWithID(sequence, sequence)
}

func (f *boundedFullSession) sendOrderWithID(sequence, identifier int) error {
	f.prepare("D", sequence)
	body := f.builder.Body()
	body.SetInt(11, identifier)
	body.SetString(21, "1")
	body.SetString(55, "SYMABC")
	body.SetString(54, "1")
	body.SetInt(38, 1+identifier%97)
	body.SetString(40, "2")
	body.SetString(44, "12.50")
	body.SetTime(60, time.Now().UTC())
	if f.application.groups {
		body.addParties()
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
	first := bytes.IndexByte(wire, 1)
	second := first + 1 + bytes.IndexByte(wire[first+1:], 1)
	bodyLength, reject := message.Header.GetInt(tagBodyLength)
	if reject != nil {
		return nil, reject
	}
	if int(total) != checksum || bodyLength != len(wire)-7-second-1 {
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
	id, reject := message.Body.GetInt(11)
	if reject != nil {
		return nil, reject
	}
	orderID, reject := message.Body.GetInt(37)
	if reject != nil {
		return nil, reject
	}
	executionID, reject := message.Body.GetInt(17)
	if reject != nil {
		return nil, reject
	}
	quantity, reject := message.Body.GetInt(38)
	if reject != nil {
		return nil, reject
	}
	leaves, reject := message.Body.GetInt(151)
	if reject != nil {
		return nil, reject
	}
	if id != orderID || id != executionID || quantity != 1+id%97 || leaves != quantity {
		return nil, errBoundedFullFixture
	}
	if f.application.groups {
		if err := boundedFullCheckGroups(message); err != nil {
			return nil, err
		}
	}
	return message, nil
}

func (f *boundedFullSession) acknowledge(_ bool) (boundedFullCheckpoint, error) {
	f.prepare("0", f.sendSequence)
	f.builder.Header().SetInt(tagLastMsgSeqNumProcessed, f.receiveSequence-1)
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
	first := f.sendSequence
	for range count {
		if err := f.sendOrder(f.sendSequence); err != nil {
			return err
		}
	}
	for index := 0; index < count; index++ {
		message, err := f.readReport()
		if err != nil {
			return err
		}
		identifier, reject := message.Body.GetInt(11)
		if reject != nil {
			return reject
		}
		if identifier != first+index {
			return errBoundedFullFixture
		}
	}
	return nil
}

// Forty retained reports exceed all 33 output slots. Replay must make progress
// through completed writes without the blocked session owner reclaiming slots.
func (f *boundedFullSession) outboundResend() error {
	for index := range f.savedReports {
		if err := f.sendOrder(f.sendSequence); err != nil {
			return err
		}
		message, err := f.readReport()
		if err != nil {
			return err
		}
		saved := &f.savedReports[index]
		saved.sequence, _ = message.Header.GetInt(tagMsgSeqNum)
		original, _ := message.Header.GetBytes(tagSendingTime)
		saved.timeLength = copy(saved.timestamp[:], original)
		saved.body = append(saved.body[:0], message.bodyBytes...)
	}
	f.prepare("2", f.sendSequence)
	f.builder.Body().SetInt(tagBeginSeqNo, f.savedReports[0].sequence)
	f.builder.Body().SetInt(tagEndSeqNo, f.savedReports[len(f.savedReports)-1].sequence)
	if err := f.writePrepared(); err != nil {
		return err
	}
	f.sendSequence++
	for index := range f.savedReports {
		message, err := f.readReport()
		if err != nil {
			return err
		}
		saved := &f.savedReports[index]
		sequence, _ := message.Header.GetInt(tagMsgSeqNum)
		original, _ := message.Header.GetBytes(tagOrigSendingTime)
		duplicate, _ := message.Header.GetBytes(tagPossDupFlag)
		if sequence != saved.sequence || string(duplicate) != "Y" || !bytes.Equal(original, saved.timestamp[:saved.timeLength]) || !bytes.Equal(message.bodyBytes, saved.body) {
			return errBoundedFullFixture
		}
	}
	_, err := f.acknowledge(false)
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
	kind, _ := request.Header.GetBytes(tagMsgType)
	begin, _ := request.Body.GetInt(tagBeginSeqNo)
	end, _ := request.Body.GetInt(tagEndSeqNo)
	if string(kind) != "2" || begin != missing || end != 0 {
		return errBoundedFullFixture
	}
	if err := f.sendOrder(missing); err != nil {
		return err
	}
	f.sendSequence++
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

func (f *boundedFullSession) heartbeatTestRequest() error {
	f.prepare("1", f.sendSequence)
	f.builder.Body().SetInt(tagTestReqID, f.sendSequence)
	requested := f.sendSequence
	if err := f.writePrepared(); err != nil {
		return err
	}
	f.sendSequence++
	message, err := f.readMessage()
	if err != nil {
		return err
	}
	kind, _ := message.Header.GetBytes(tagMsgType)
	id, reject := message.Body.GetInt(tagTestReqID)
	if reject != nil {
		return reject
	}
	if string(kind) != "0" || id != requested {
		return errBoundedFullFixture
	}
	_, err = f.acknowledge(false)
	return err
}

func (f *boundedFullSession) prepareSlowConsumer() error {
	f.payload = bytes.Repeat([]byte("x"), 8192)
	f.slowTimer = time.NewTimer(10 * time.Second)
	f.slowTimer.Stop()
	f.slowGate = newBoundedLifecycleWriteGate(f.server)
	return f.configure(func(a *boundedFullApplication) {
		a.measureAdmission = true
		a.session.writeConnection.connection = f.slowGate
	})
}

// One response per ordinary request. A test-owned gate delays one real TCP
// Write while the session keeps processing requests and fills all output slots.
// This simulates a stalled consumer deterministically, without changing send's
// admission behavior or counting a queued response as a completed write.
func (f *boundedFullSession) slowConsumer() error {
	f.slowGate.armed <- struct{}{}
	f.slowTimer.Reset(10 * time.Second)
	first := f.sendSequence
	accepted := 0
	for attempt := 0; attempt < 34; attempt++ {
		if err := f.sendOrder(f.sendSequence); err != nil {
			return err
		}
		var admission boundedFullAdmission
		select {
		case admission = <-f.application.admissions:
		case <-f.slowTimer.C:
			return errBoundedFullFixture
		}
		if attempt == 0 {
			select {
			case <-f.slowGate.entered:
			case <-f.slowTimer.C:
				return errBoundedFullFixture
			}
		}
		if admission.err != nil {
			if admission.err != ErrOutboundCapacity || admission.before != admission.after || admission.accepted != 0 || accepted != 33 {
				return errBoundedFullFixture
			}
			break
		}
		if admission.accepted != 1 {
			return errBoundedFullFixture
		}
		accepted++
	}
	if accepted != 33 {
		return errBoundedFullFixture
	}
	f.slowGate.decision <- nil
	for index := 0; index < accepted; index++ {
		report, err := f.readReport()
		if err != nil {
			return err
		}
		id, _ := report.Body.GetInt(11)
		value, reject := report.Body.GetBytes(58)
		if reject != nil {
			return reject
		}
		if id != first+index || !bytes.Equal(value, f.payload) {
			return errBoundedFullFixture
		}
	}
	if _, err := f.acknowledge(false); err != nil {
		return err
	}
	// Retry the rejected business identifier under a fresh FIX sequence from an
	// ordinary callback after completed writes returned capacity.
	if err := f.sendOrderWithID(f.sendSequence, first+33); err != nil {
		return err
	}
	admission := <-f.application.admissions
	if admission.err != nil || admission.accepted != 1 {
		return errBoundedFullFixture
	}
	report, err := f.readReport()
	if err != nil {
		return err
	}
	identifier, reject := report.Body.GetInt(11)
	if reject != nil {
		return reject
	}
	if identifier != first+33 {
		return errBoundedFullFixture
	}
	f.slowTimer.Stop()
	_, err = f.acknowledge(false)
	return err
}

func (f *boundedFullSession) warm() error {
	// Fill all 1024 journal cells with real validated responses before retirement.
	for range 32 {
		if err := f.unacknowledgedBatch(32); err != nil {
			return err
		}
	}
	if _, err := f.acknowledge(true); err != nil {
		return err
	}
	// FIFO ownership transfer visits every input/output slot and pushes both
	// counters beyond integer boxing caches before measurement starts.
	for range 66 {
		if err := f.ordinaryBatch(1, true); err != nil {
			return err
		}
	}
	if err := f.outboundResend(); err != nil {
		return err
	}
	return f.heartbeatTestRequest()
}
