package quickfix

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var (
	errBoundedFullFixture = errors.New("quickfix test: unexpected full-session fixture or sequence")
	errBoundedFullWire    = errors.New("quickfix test: invalid full-session wire framing")
	boundedFullPartyWire  = []byte("453=1\x01448=party\x01447=D\x01452=1\x01802=1\x01523=sub-party\x01803=1\x01")
)

// A checkpoint is copied by the session owner after completed writes and explicit
// peer acknowledgement. The test never observes a concurrently mutated session.
type boundedFullCheckpoint struct {
	nextSender, nextTarget         int
	freeOutputs, retained          int
	orders, groupReads, queueWaits int
	err                            error
}

type boundedFullAdmission struct {
	accepted      int
	before, after boundedFullStoreFingerprint
	err           error
}

// A fixed-size digest keeps capacity assertions inside measured intervals free
// of snapshot allocation. It includes every retained byte and both counters.
type boundedFullStoreFingerprint struct {
	nextSender, nextTarget, floor, entries int
	reservedIndex, reservedSequence        int
	reservedReset                          bool
	creation                               time.Time
	digest                                 uint64
}

func fingerprintBoundedFullStore(store *BoundedStore) boundedFullStoreFingerprint {
	result := boundedFullStoreFingerprint{nextSender: store.NextSenderMsgSeqNum(), nextTarget: store.NextTargetMsgSeqNum(), floor: store.floor, reservedIndex: store.reservedIndex, reservedSequence: store.reservedSequence, reservedReset: store.reservedReset, creation: store.creationTime, digest: 14695981039346656037}
	for _, entry := range store.entries {
		if entry.sequence == 0 {
			continue
		}
		result.entries++
		result.digest = (result.digest ^ uint64(entry.sequence)) * 1099511628211
		wire, _ := store.message(entry.sequence)
		for _, value := range wire {
			result.digest = (result.digest ^ uint64(value)) * 1099511628211
		}
	}
	return result
}

type boundedFullReject struct{ err error }

func (e *boundedFullReject) Error() string             { return e.err.Error() }
func (*boundedFullReject) RejectReason() int           { return 0 }
func (*boundedFullReject) BusinessRejectRefID() string { return "" }
func (*boundedFullReject) RefTagID() *Tag              { return nil }
func (*boundedFullReject) IsBusinessReject() bool      { return false }

type boundedFullApplication struct {
	session                        *session
	response                       *Message
	groups                         bool
	checkpoints                    chan boundedFullCheckpoint
	logons, logouts                chan struct{}
	admissions                     chan boundedFullAdmission
	commands                       chan func(*boundedFullApplication)
	onOrder                        func(*Message) MessageRejectError
	orders, groupReads, queueWaits int
	reportsPerOrder                int
	measureAdmission               bool
	failure                        boundedFullReject
}

func (*boundedFullApplication) OnCreate(SessionID)              {}
func (a *boundedFullApplication) OnLogon(SessionID)             { a.logons <- struct{}{} }
func (a *boundedFullApplication) OnLogout(SessionID)            { a.logouts <- struct{}{} }
func (*boundedFullApplication) ToAdmin(*Message, SessionID)     {}
func (*boundedFullApplication) ToApp(*Message, SessionID) error { return nil }
func (a *boundedFullApplication) fail(err error) MessageRejectError {
	a.failure.err = err
	return &a.failure
}

func newBoundedFullResponse(groups bool) *Message {
	message := NewMessage()
	message.Header.SetString(tagMsgType, "8")
	for _, field := range []struct {
		tag   Tag
		value string
	}{
		{150, "0"}, {39, "0"}, {55, "SYMABC"}, {54, "1"}, {14, "0"}, {6, "0"},
	} {
		message.Body.SetString(field.tag, field.value)
	}
	if groups {
		subParties := NewRepeatingGroup(802, GroupTemplate{GroupElement(523), GroupElement(803)})
		subParty := subParties.Add()
		subParty.SetString(523, "sub-party")
		subParty.SetInt(803, 1)
		parties := NewRepeatingGroup(453, GroupTemplate{GroupElement(448), GroupElement(447), GroupElement(452), subParties})
		party := parties.Add()
		party.SetString(448, "party")
		party.SetString(447, "D")
		party.SetInt(452, 1)
		party.SetGroup(subParties)
		message.Body.SetGroup(parties)
	}
	return message
}

// The ordinary count accessor participates in the callback. Exact nested field
// bytes are checked independently, without a production-only Group API. Legacy
// GetGroup/SetGroup reconstruction is deliberately not claimed allocation-free.
func boundedFullCheckGroups(message *Message) error {
	count, reject := message.Body.GetInt(453)
	if reject != nil {
		return reject
	}
	if count != 1 || bytes.Count(message.bodyBytes, boundedFullPartyWire) != 1 {
		return errBoundedFullFixture
	}
	return nil
}

func (a *boundedFullApplication) FromApp(message *Message, _ SessionID) MessageRejectError {
	if a.onOrder != nil {
		return a.onOrder(message)
	}
	return a.handleOrder(message)
}

func (a *boundedFullApplication) handleOrder(message *Message) MessageRejectError {
	accepted := 0
	for range max(1, a.reportsPerOrder) {
		var before boundedFullStoreFingerprint
		if a.measureAdmission {
			before = fingerprintBoundedFullStore(a.session.store.(*BoundedStore))
		}
		if reject := a.handleReport(message); reject != nil {
			if a.measureAdmission && reject == &a.failure && errors.Is(a.failure.err, ErrOutboundCapacity) {
				a.queueWaits++
				a.admissions <- boundedFullAdmission{accepted: accepted, before: before, after: fingerprintBoundedFullStore(a.session.store.(*BoundedStore)), err: a.failure.err}
				// Application backpressure is handled by retrying after the peer
				// drains the accepted prefix; it must not become a FIX reject.
				return nil
			}
			return reject
		}
		accepted++
	}
	if a.measureAdmission {
		a.admissions <- boundedFullAdmission{accepted: accepted}
	}
	return nil
}

func (a *boundedFullApplication) handleReport(message *Message) MessageRejectError {
	kind, reject := message.Header.GetBytes(tagMsgType)
	if reject != nil {
		return reject
	}
	if string(kind) != "D" {
		return a.fail(errBoundedFullFixture)
	}
	id, reject := message.Body.GetBytes(11)
	if reject != nil {
		return reject
	}
	identifier, reject := message.Body.GetInt(11)
	if reject != nil {
		return reject
	}
	quantity, reject := message.Body.GetInt(38)
	if reject != nil {
		return reject
	}
	if a.groups {
		if err := boundedFullCheckGroups(message); err != nil {
			return a.fail(err)
		}
	}
	// This is the ordinary public API on one caller-owned message. Variable
	// fields change every request; only static group construction is untimed.
	body := &a.response.Body
	body.SetBytes(11, id)
	body.SetBytes(37, id)
	body.SetInt(17, identifier)
	body.SetInt(38, quantity)
	body.SetInt(151, quantity)
	if message.Body.Has(58) {
		value, _ := message.Body.GetBytes(58)
		body.SetBytes(58, value)
	} else {
		body.Remove(58)
	}
	if err := SendToTarget(a.response, a.session.sessionID); err != nil {
		return a.fail(err)
	}
	a.orders++
	if a.groups {
		a.groupReads++
	}
	return nil
}

func (a *boundedFullApplication) FromAdmin(message *Message, _ SessionID) MessageRejectError {
	kind, reject := message.Header.GetBytes(tagMsgType)
	if reject != nil {
		return reject
	}
	if string(kind) != "0" || !message.Header.Has(tagLastMsgSeqNumProcessed) {
		return nil
	}
	acknowledged, reject := message.Header.GetInt(tagLastMsgSeqNumProcessed)
	if reject != nil {
		return reject
	}
	s := a.session
	s.resendMutex.RLock()
	s.sendMutex.Lock()
	completionError := s.flushOutbound(true)
	var returned [33]*outboundSlot
	n := 0
	if completionError == nil {
		for n < len(s.outbound.slots) {
			select {
			case returned[n] = <-s.outbound.vacant:
				n++
			case <-s.writeConnection.done:
				completionError = s.writeConnection.failure()
			}
			if completionError != nil {
				break
			}
		}
	}
	for index := 0; index < n; index++ {
		s.outbound.vacant <- returned[index]
	}
	store := s.store.(*BoundedStore)
	if completionError == nil {
		completionError = store.RetireBefore(acknowledged + 1)
	}
	retained := 0
	for _, entry := range store.entries {
		if entry.sequence != 0 {
			retained++
		}
	}
	checkpoint := boundedFullCheckpoint{nextSender: store.NextSenderMsgSeqNum(), nextTarget: store.NextTargetMsgSeqNum(), freeOutputs: len(s.outbound.vacant), retained: retained, orders: a.orders, groupReads: a.groupReads, queueWaits: a.queueWaits, err: completionError}
	s.sendMutex.Unlock()
	s.resendMutex.RUnlock()
	select {
	case command := <-a.commands:
		command(a)
	default:
	}
	a.checkpoints <- checkpoint
	if completionError != nil {
		return a.fail(completionError)
	}
	return nil
}

func TestBoundedFullSession(t *testing.T) {
	for _, groups := range []bool{false, true} {
		name := "ordinary"
		if groups {
			name = "groups"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newBoundedFullSession(t, groups)
			require.NoError(t, fixture.warm())
			require.NoError(t, fixture.ordinaryBatch(32, false))
			require.NoError(t, fixture.outboundResend())
			require.NoError(t, fixture.inboundGap())
			require.NoError(t, fixture.heartbeatTestRequest())
			require.Greater(t, fixture.lastCheckpoint.orders, 1024)
			if groups {
				require.Equal(t, fixture.lastCheckpoint.orders, fixture.lastCheckpoint.groupReads)
			}
		})
	}
}

func TestBoundedFullSessionSlowConsumer(t *testing.T) {
	fixture := newBoundedFullSession(t, true)
	require.NoError(t, fixture.warm())
	require.NoError(t, fixture.prepareSlowConsumer())
	require.NoError(t, fixture.slowConsumer())
	require.Greater(t, fixture.lastCheckpoint.queueWaits, 0)
	require.NoError(t, fixture.slowConsumer())
}

func TestBoundedFullSessionWarmedPostReconnect(t *testing.T) {
	fixture := newBoundedFullSession(t, true)
	require.NoError(t, fixture.warm())
	before := fixture.lastCheckpoint
	require.NoError(t, fixture.reconnect())
	require.NoError(t, fixture.warm())
	require.NoError(t, fixture.ordinaryBatch(32, false))
	require.Greater(t, fixture.lastCheckpoint.nextSender, before.nextSender)
	require.Greater(t, fixture.lastCheckpoint.nextTarget, before.nextTarget)
}

// Real TCP, dictionary/groups, validation, ordinary callbacks and variable-field
// construction, persistence, peer validation, completed writes and slot return
// are timed. Setup and warm-up are excluded. This is not sub-microsecond engine
// latency: the measurement includes the independent peer and acknowledgement.
// Ordinary/groups/post-reconnect operations each process a batch of 32 requests.
func BenchmarkBoundedFullSession(b *testing.B) {
	for _, scenario := range []string{"ordinary", "groups", "outbound_resend", "inbound_gap", "slow_consumer", "warmed_post_reconnect", "heartbeat_test_request"} {
		b.Run(scenario, func(b *testing.B) {
			b.StopTimer()
			fixture := newBoundedFullSession(b, scenario != "ordinary")
			require.NoError(b, fixture.warm())
			if scenario == "warmed_post_reconnect" {
				require.NoError(b, fixture.reconnect())
				require.NoError(b, fixture.warm())
			}
			if scenario == "slow_consumer" {
				require.NoError(b, fixture.prepareSlowConsumer())
			}
			cycle := fixture.scenario(scenario)
			require.NoError(b, cycle())
			fixture.bytes = 0
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for range b.N {
				if err := cycle(); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.SetBytes(fixture.bytes / int64(b.N))
		})
	}
}

func (f *boundedFullSession) scenario(name string) func() error {
	switch name {
	case "outbound_resend":
		return f.outboundResend
	case "inbound_gap":
		return f.inboundGap
	case "slow_consumer":
		return f.slowConsumer
	case "heartbeat_test_request":
		return f.heartbeatTestRequest
	default:
		return func() error { return f.ordinaryBatch(32, false) }
	}
}
