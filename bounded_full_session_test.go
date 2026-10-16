package quickfix

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	errBoundedFullFixture = errors.New("quickfix test: unexpected full-session fixture or sequence")
	errBoundedFullWire    = errors.New("quickfix test: invalid full-session checksum")
	boundedFullReport     = []byte("8")
	boundedFullParties    = [...]OutboundField{
		{Tag: 453, Value: []byte("1")}, {Tag: 448, Value: []byte("party")},
		{Tag: 447, Value: []byte("D")}, {Tag: 452, Value: []byte("1")},
		{Tag: 802, Value: []byte("1")}, {Tag: 523, Value: []byte("sub-party")},
		{Tag: 803, Value: []byte("1")},
	}
)

// The session owner copies a checkpoint only after every write has completed
// and acknowledged history has been retired. Observers never read live state.
type boundedFullCheckpoint struct {
	nextSender, nextTarget int
	freeOutputs, retained  int
	orders, groupReads     int
	queueWaits             int
	err                    error
}

type boundedFullApplication struct {
	session            *session
	groups             bool
	checkpoints        chan boundedFullCheckpoint
	logons, logouts    chan struct{}
	slowReady          chan struct{}
	commands           chan func(*boundedFullApplication)
	onOrder            func(*MessageSlot) MessageRejectError
	orders, groupReads int
	reportsPerOrder    int
	queueWaits         int
	failure            boundedOutboundReject
}

func (*boundedFullApplication) OnCreate(SessionID)              {}
func (a *boundedFullApplication) OnLogon(SessionID)             { a.logons <- struct{}{} }
func (a *boundedFullApplication) OnLogout(SessionID)            { a.logouts <- struct{}{} }
func (*boundedFullApplication) ToAdmin(*Message, SessionID)     {}
func (*boundedFullApplication) ToApp(*Message, SessionID) error { return errBoundedFullFixture }
func (*boundedFullApplication) FromAdmin(*Message, SessionID) MessageRejectError {
	return IncorrectDataFormatForValue(tagMsgType)
}
func (*boundedFullApplication) FromApp(*Message, SessionID) MessageRejectError {
	return IncorrectDataFormatForValue(tagMsgType)
}
func (*boundedFullApplication) ToAdminBorrowed(*OutboundMessage, SessionID) error { return nil }
func (*boundedFullApplication) ToAppBorrowed(*OutboundMessage, SessionID) error   { return nil }
func (a *boundedFullApplication) fail(err error) MessageRejectError {
	a.failure.err = err
	return &a.failure
}

func boundedFullCheckGroups(slot *MessageSlot) error {
	parties, err := slot.Group(453)
	if err != nil {
		return err
	}
	if parties.Len() != 1 {
		return errBoundedFullFixture
	}
	iterator := parties.Iter()
	party, ok := iterator.Next()
	if !ok {
		return errBoundedFullFixture
	}
	identifier, ok := party.GetBytes(448)
	if !ok || string(identifier) != "party" {
		return errBoundedFullFixture
	}
	subParties, err := party.Group(802)
	if err != nil {
		return err
	}
	if subParties.Len() != 1 {
		return errBoundedFullFixture
	}
	subIterator := subParties.Iter()
	subParty, ok := subIterator.Next()
	if !ok {
		return errBoundedFullFixture
	}
	identifier, ok = subParty.GetBytes(523)
	if !ok || string(identifier) != "sub-party" {
		return errBoundedFullFixture
	}
	if _, ok = iterator.Next(); ok {
		return errBoundedFullFixture
	}
	if _, ok = subIterator.Next(); ok {
		return errBoundedFullFixture
	}
	return nil
}

func (a *boundedFullApplication) FromAppBorrowed(slot *MessageSlot, _ SessionID) MessageRejectError {
	if a.onOrder != nil {
		return a.onOrder(slot)
	}
	return a.handleOrder(slot)
}

func (a *boundedFullApplication) handleOrder(slot *MessageSlot) MessageRejectError {
	notified := false
	for range max(1, a.reportsPerOrder) {
		if len(a.session.boundedOut.ready) == cap(a.session.boundedOut.ready) {
			a.queueWaits++
			if a.reportsPerOrder > 1 && !notified {
				a.slowReady <- struct{}{}
				notified = true
			}
		}
		if reject := a.handleReport(slot); reject != nil {
			return reject
		}
	}
	return nil
}

func (a *boundedFullApplication) handleReport(slot *MessageSlot) MessageRejectError {
	message := slot.Message()
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
	sequence, reject := message.Header.GetInt(tagMsgSeqNum)
	if reject != nil {
		return reject
	}
	if a.groups {
		if err := boundedFullCheckGroups(slot); err != nil {
			return a.fail(err)
		}
		a.groupReads++
	}
	builder, err := slot.BeginOutbound(boundedFullReport)
	if err != nil {
		return a.fail(err)
	}
	body := builder.Body()
	body.SetBytes(11, id)
	body.SetBytes(37, id)
	body.SetInt(17, sequence)
	body.SetString(150, "0")
	body.SetString(39, "0")
	body.SetString(55, "SYMABC")
	body.SetString(54, "1")
	body.SetString(38, "10")
	body.SetString(151, "10")
	body.SetString(14, "0")
	body.SetString(6, "0")
	if a.groups {
		body.SetGroup(453, boundedFullParties[:])
	}
	if message.Body.Has(58) {
		text, _ := message.Body.GetBytes(58)
		body.SetBytes(58, text)
	}
	if err := builder.Send(); err != nil {
		return a.fail(err)
	}
	a.orders++
	return nil
}

func (a *boundedFullApplication) FromAdminBorrowed(slot *MessageSlot, _ SessionID) MessageRejectError {
	message := slot.Message()
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
	out := a.session.boundedOut
	var completionError error
	// This benchmark-only barrier executes on the actual session owner and
	// uses the same reclaim operation as session.run. There is no competing
	// consumer of completed while the synchronous callback is running.
	for len(out.free) < cap(out.free) {
		returned := <-out.completed
		if err := out.reclaim(returned); err != nil && completionError == nil {
			completionError = err
		}
	}
	if completionError == nil {
		completionError = slot.RetireOutboundBefore(acknowledged + 1)
	}
	if message.Body.Has(tagTestReqID) {
		marker, _ := message.Body.GetBytes(tagTestReqID)
		if string(marker) != "warm" {
			return a.fail(errBoundedFullFixture)
		}
		// Rotate the LIFO free stack during untimed warm-up to visit every slot.
		last := len(out.free) - 1
		warmed := out.free[last]
		copy(out.free[1:], out.free[:last])
		out.free[0] = warmed
	}
	select {
	case command := <-a.commands:
		command(a)
	default:
	}
	retained := 0
	for _, entry := range out.store.entries {
		if entry.sequence != 0 {
			retained++
		}
	}
	a.checkpoints <- boundedFullCheckpoint{
		nextSender: out.store.NextSenderMsgSeqNum(), nextTarget: out.store.NextTargetMsgSeqNum(),
		freeOutputs: len(out.free), retained: retained, orders: a.orders, groupReads: a.groupReads, queueWaits: a.queueWaits, err: completionError,
	}
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

// Real TCP requests, dictionary/group parsing, validation, borrowed callbacks,
// persistence, peer validation and acknowledgement, completed writes and slot
// return are timed. Setup, all-slot warm-up and shutdown are untimed.
func BenchmarkBoundedFullSession(b *testing.B) {
	for _, scenario := range []struct {
		name                                 string
		groups, resend, gap, slow, reconnect bool
	}{
		{name: "ordinary"}, {name: "groups", groups: true}, {name: "outbound_resend", groups: true, resend: true},
		{name: "inbound_gap", groups: true, gap: true},
		{name: "slow_consumer", groups: true, slow: true},
		{name: "warmed_post_reconnect", groups: true, reconnect: true},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			b.StopTimer()
			fixture := newBoundedFullSession(b, scenario.groups)
			require.NoError(b, fixture.warm())
			if scenario.reconnect {
				require.NoError(b, fixture.reconnect())
				require.NoError(b, fixture.warm())
			}
			if scenario.slow {
				require.NoError(b, fixture.prepareSlowConsumer())
				require.NoError(b, fixture.slowConsumer())
			}
			if scenario.gap {
				require.NoError(b, fixture.inboundGap())
			}
			fixture.bytes = 0
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			if scenario.slow {
				for range b.N {
					if err := fixture.slowConsumer(); err != nil {
						b.Fatal(err)
					}
				}
			} else if scenario.gap {
				for range b.N {
					if err := fixture.inboundGap(); err != nil {
						b.Fatal(err)
					}
				}
			} else if scenario.resend {
				for range b.N {
					if err := fixture.outboundResend(); err != nil {
						b.Fatal(err)
					}
				}
			} else {
				for completed := 0; completed < b.N; {
					count := min(32, b.N-completed)
					if err := fixture.ordinaryBatch(count, false); err != nil {
						b.Fatal(err)
					}
					completed += count
				}
			}
			b.StopTimer()
			b.SetBytes(fixture.bytes / int64(b.N))
		})
	}
}
