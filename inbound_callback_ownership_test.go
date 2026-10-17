package quickfix

import (
	"bytes"
	"fmt"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/datadictionary"
	"github.com/stretchr/testify/require"
)

type inboundOwnershipApplication struct {
	*inboundRecordingApplication
	fromAdmin func(*Message) MessageRejectError
}

func (a *inboundOwnershipApplication) FromAdmin(message *Message, id SessionID) MessageRejectError {
	if reject := a.inboundRecordingApplication.FromAdmin(message, id); reject != nil {
		return reject
	}
	if a.fromAdmin != nil {
		return a.fromAdmin(message)
	}
	return nil
}

func TestInboundLegacyCallbacksRetainMessageAndBytes(t *testing.T) {
	for _, admin := range []bool{false, true} {
		name := "FromApp"
		if admin {
			name = "FromAdmin"
		}
		t.Run(name, func(t *testing.T) {
			s, recorder, _, store := newInboundIntegrationSession(t, 0, 1)
			var retained *Message
			var shallow Message
			var section FieldMap
			var getter, wire []byte
			capture := func(message *Message) MessageRejectError {
				if retained == nil {
					retained, shallow, section = message, *message, message.Body.FieldMap
					var reject MessageRejectError
					getter, reject = message.Body.GetBytes(11)
					require.Nil(t, reject)
					wire = message.Bytes()
				} else {
					require.NotSame(t, retained, message, "ordinary callbacks must not recycle a delivered message")
				}
				return nil
			}
			application := &inboundOwnershipApplication{inboundRecordingApplication: recorder}
			s.application = application
			if admin {
				application.fromAdmin = capture
			} else {
				recorder.fromApp = capture
			}
			first := inboundIntegrationOrder(1, "retained-order")
			if admin {
				first.Header.SetString(tagMsgType, "0")
			}
			wantWire := bytes.Clone(first.build())
			received := time.Now().Add(-time.Second)
			inboundIntegrationDeliverAt(t, s, first, received)
			require.NotNil(t, retained)
			count := len(s.boundedIn.ring.slots)*3 + 1
			for sequence := 2; sequence <= count; sequence++ {
				message := inboundIntegrationOrder(sequence, "next-"+strconv.Itoa(sequence))
				if admin {
					message.Header.SetString(tagMsgType, "0")
				}
				inboundIntegrationDeliver(t, s, message)
			}
			require.Equal(t, count+1, store.NextTargetMsgSeqNum())
			assertInboundRingVacant(t, s.boundedIn.ring)
			s.Disconnected(s)
			s.State = inSession{}
			s.messageOut = make(chan []byte, 64)
			afterReconnect := inboundIntegrationOrder(count+1, "after-reconnect")
			if admin {
				afterReconnect.Header.SetString(tagMsgType, "0")
			}
			inboundIntegrationDeliver(t, s, afterReconnect)
			require.Equal(t, count+2, store.NextTargetMsgSeqNum())
			assertInboundRingVacant(t, s.boundedIn.ring)
			for _, fields := range []*FieldMap{&retained.Body.FieldMap, &shallow.Body.FieldMap, &section} {
				id, reject := fields.GetString(11)
				require.Nil(t, reject)
				require.Equal(t, "retained-order", id)
			}
			require.Equal(t, "retained-order", string(getter))
			require.Equal(t, wantWire, wire)
			require.Equal(t, wantWire, retained.Bytes())
			require.Equal(t, wantWire, shallow.Bytes())
			require.Equal(t, received, retained.ReceiveTime)
		})
	}
}

func TestInboundLegacyCallbackRetainsNestedGroups(t *testing.T) {
	s, application, _, store := newInboundIntegrationSession(t, 0, 1)
	dictionary, err := datadictionary.Parse("spec/FIX44.xml")
	require.NoError(t, err)
	s.sessionID.BeginString = BeginStringFIX44
	s.appDataDictionary, s.transportDataDictionary = dictionary, dictionary
	s.Validator = NewValidator(ValidatorSettings{}, dictionary, dictionary)
	require.NoError(t, s.configureBoundedInbound(inboundIntegrationSettings(0, 1)))
	var retained *Message
	var retainedGroups *RepeatingGroup
	template := GroupTemplate{
		GroupElement(79),
		NewRepeatingGroup(539, GroupTemplate{GroupElement(524), NewRepeatingGroup(804, GroupTemplate{GroupElement(545)})}),
	}
	application.fromApp = func(message *Message) MessageRejectError {
		if retained == nil {
			retained = message
			retainedGroups = NewRepeatingGroup(78, template)
			require.Nil(t, message.Body.GetGroup(retainedGroups))
		}
		return nil
	}
	count := len(s.boundedIn.ring.slots)*3 + 1
	for sequence := 1; sequence <= count; sequence++ {
		message := inboundIntegrationNestedOrder()
		message.Header.SetInt(tagMsgSeqNum, sequence)
		message.Body.SetString(11, "group-"+strconv.Itoa(sequence))
		subIDs := NewRepeatingGroup(804, GroupTemplate{GroupElement(545)})
		subIDs.Add().SetString(545, "sub-"+strconv.Itoa(sequence))
		parties := NewRepeatingGroup(539, GroupTemplate{GroupElement(524), subIDs})
		party := parties.Add()
		party.SetString(524, "nested-party-"+strconv.Itoa(sequence))
		party.SetGroup(subIDs)
		allocations := NewRepeatingGroup(78, template)
		allocation := allocations.Add()
		allocation.SetString(79, "allocation-"+strconv.Itoa(sequence))
		allocation.SetGroup(parties)
		message.Body.SetGroup(allocations)
		message.Body.SetString(55, "TEST")
		message.Body.SetString(54, "1")
		message.Body.SetInt(38, 1)
		message.Body.SetString(40, "1")
		message.Body.SetField(60, FIXUTCTimestamp{Time: time.Now()})
		inboundIntegrationDeliver(t, s, message)
	}
	require.NotNil(t, retained, "the valid dictionary-backed order must reach FromApp")
	require.Equal(t, count+1, store.NextTargetMsgSeqNum())
	for _, group := range []*RepeatingGroup{retainedGroups, NewRepeatingGroup(78, template)} {
		if group != retainedGroups {
			require.Nil(t, retained.Body.GetGroup(group))
		}
		require.Equal(t, 1, group.Len())
		account, reject := group.Get(0).GetString(79)
		require.Nil(t, reject)
		require.Equal(t, "allocation-1", account)
		parties := NewRepeatingGroup(539, GroupTemplate{GroupElement(524), NewRepeatingGroup(804, GroupTemplate{GroupElement(545)})})
		require.Nil(t, group.Get(0).GetGroup(parties))
		require.Equal(t, 1, parties.Len())
		party, reject := parties.Get(0).GetString(524)
		require.Nil(t, reject)
		require.Equal(t, "nested-party-1", party)
		subIDs := NewRepeatingGroup(804, GroupTemplate{GroupElement(545)})
		require.Nil(t, parties.Get(0).GetGroup(subIDs))
		require.Equal(t, 1, subIDs.Len())
		value, reject := subIDs.Get(0).GetString(545)
		require.Nil(t, reject)
		require.Equal(t, "sub-1", value)
	}
	assertInboundRingVacant(t, s.boundedIn.ring)
}

func TestInboundLegacyCallbackPreservesParsedRawAliases(t *testing.T) {
	s, application, _, _ := newInboundIntegrationSession(t, 0, 1)
	var retained *Message
	var raw, originalGetter []byte
	application.fromApp = func(message *Message) MessageRejectError {
		if retained != nil {
			return nil
		}
		retained = message
		var reject MessageRejectError
		originalGetter, reject = message.Body.GetBytes(11)
		require.Nil(t, reject)
		raw = message.Bytes()
		originalGetter[0] = 'R'
		require.Contains(t, string(raw), "\x0111=Retained\x01", "parsed getters alias their own raw input")
		message.Body.SetString(11, "replacement")
		require.Contains(t, string(message.Bytes()), "\x0111=Retained\x01", "a setter must not replace the original raw frame")
		return nil
	}
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(1, "retained"))
	for sequence := 2; sequence <= len(s.boundedIn.ring.slots)*3+1; sequence++ {
		inboundIntegrationDeliver(t, s, inboundIntegrationOrder(sequence, "overwrite"))
	}
	require.NotNil(t, retained)
	id, reject := retained.Body.GetString(11)
	require.Nil(t, reject)
	require.Equal(t, "replacement", id)
	require.Equal(t, "Retained", string(originalGetter))
	require.Contains(t, string(raw), "\x0111=Retained\x01")
	require.Equal(t, raw, retained.Bytes())
}

func TestInboundLegacyFromAdminMutationChangesHeartbeat(t *testing.T) {
	s, recorder, _, store := newInboundIntegrationSession(t, 0, 1)
	output := make(chan []byte, 4)
	s.messageOut = output
	var retained *Message
	s.application = &inboundOwnershipApplication{
		inboundRecordingApplication: recorder,
		fromAdmin: func(message *Message) MessageRejectError {
			retained = message
			message.Body.SetString(tagTestReqID, "callback-replacement")
			return nil
		},
	}
	factory := MessageFactory{}
	request := factory.buildMessage("1")
	request.Body.SetString(tagTestReqID, "original-request")
	inboundIntegrationDeliver(t, s, request)
	select {
	case wire := <-output:
		heartbeat := NewMessage()
		require.NoError(t, ParseMessage(heartbeat, bytes.NewBuffer(wire)))
		messageType, reject := heartbeat.MsgType()
		require.Nil(t, reject)
		require.Equal(t, "0", messageType)
		value, reject := heartbeat.Body.GetString(tagTestReqID)
		require.Nil(t, reject)
		require.Equal(t, "callback-replacement", value)
	default:
		t.Fatal("TestRequest did not produce a heartbeat")
	}
	require.NotNil(t, retained)
	value, reject := retained.Body.GetString(tagTestReqID)
	require.Nil(t, reject)
	require.Equal(t, "callback-replacement", value)
	require.Equal(t, 2, store.NextTargetMsgSeqNum())
	assertInboundRingVacant(t, s.boundedIn.ring)
}

func TestInboundLegacyRetainedMessageConcurrentWithSlotReuse(t *testing.T) {
	s, application, _, _ := newInboundIntegrationSession(t, 0, 1)
	var retained *Message
	var getter, wire []byte
	application.fromApp = func(message *Message) MessageRejectError {
		if retained == nil {
			retained = message
			var reject MessageRejectError
			getter, reject = message.Body.GetBytes(11)
			require.Nil(t, reject)
			wire = message.Bytes()
		}
		return nil
	}
	first := inboundIntegrationOrder(1, "retained-concurrent")
	wantWire := bytes.Clone(first.build())
	inboundIntegrationDeliver(t, s, first)
	require.NotNil(t, retained)
	ready, stop := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(ready)
		for {
			id, reject := retained.Body.GetString(11)
			if reject != nil || id != "retained-concurrent" || string(getter) != "retained-concurrent" || !bytes.Equal(wire, wantWire) || !bytes.Equal(retained.Bytes(), wantWire) {
				done <- fmt.Errorf("retained callback data changed during slot reuse: id=%q reject=%v", id, reject)
				return
			}
			select {
			case <-stop:
				done <- nil
				return
			default:
				runtime.Gosched()
			}
		}
	}()
	<-ready
	defer func() {
		close(stop)
		require.NoError(t, <-done)
	}()
	for sequence := 2; sequence <= 100; sequence++ {
		inboundIntegrationDeliver(t, s, inboundIntegrationOrder(sequence, "later-"+strconv.Itoa(sequence)))
		runtime.Gosched()
	}
	assertInboundRingVacant(t, s.boundedIn.ring)
}

func TestInboundOwnedMaterializationMatchesParsedRepresentation(t *testing.T) {
	dictionary, err := datadictionary.Parse("spec/FIX44.xml")
	require.NoError(t, err)
	trailerDictionary := *dictionary
	trailerDictionary.Trailer = datadictionary.NewMessageDef("Trailer", "", []datadictionary.MessagePart{
		datadictionary.NewGroupFieldDef(datadictionary.NewFieldType("TrailerGroup", 9000, "NUMINGROUP"), false,
			[]datadictionary.MessagePart{datadictionary.NewFieldDef(datadictionary.NewFieldType("TrailerID", 9001, "STRING"), true)}),
	})
	for _, fixture := range []struct {
		name        string
		frame       []byte
		transport   *datadictionary.DataDictionary
		application *datadictionary.DataDictionary
		section     string
	}{
		{"plain", buildFIXT11Wire("35=D\x01", "11=order\x0155=SYM\x01"), nil, nil, ""},
		{"body_groups", []byte(allocationBaselineRepeatingGroupMessage), dictionary, dictionary, ""},
		{"nested_sibling_groups", buildFIXT11Wire("35=D\x0134=2\x0149=S\x0156=T\x0152=20231231-20:19:41\x01", "78=2\x0179=A\x01539=1\x01524=N\x01804=1\x01545=X\x0180=10\x0179=B\x01539=0\x0180=20\x01453=1\x01448=P\x01447=D\x01452=1\x01528=I\x01"), dictionary, dictionary, ""},
		{"header_group_preserves_fixed_parser_group", buildFIXT11Wire("35=D\x01627=2\x01628=A\x01628=B\x01", "11=order\x01"), dictionary, dictionary, "header"},
		{"trailer_group_preserves_fixed_parser_group", buildFIXT11Wire("35=D\x01", "11=order\x019000=2\x019001=T\x019001=U\x01"), &trailerDictionary, dictionary, "trailer"},
		{"xml_embedded_soh", buildFIXT11Wire("35=D\x01212=5\x01213=a\x01b\x01c\x01", ""), dictionary, dictionary, ""},
		{"duplicate_scalar_fields", buildFIXT11Wire("35=D\x0149=first\x0149=second\x01", "11=first\x0111=second\x0155=SYM\x01"), dictionary, dictionary, ""},
		{"duplicate_group_count", buildFIXT11Wire("35=D\x01", "453=1\x01448=A\x01453=1\x01448=B\x01"), dictionary, dictionary, ""},
		{"nonnumeric_group_count", buildFIXT11Wire("35=D\x01", "453=abc\x01448=A\x01"), dictionary, dictionary, ""},
		{"header_after_body", buildFIXT11Wire("35=D\x0149=S\x01", "11=order\x0156=T\x0155=SYM\x01"), dictionary, dictionary, ""},
		{"body_after_trailer", buildFIXT11Wire("35=D\x01", "11=order\x0189=signature\x0155=late-body\x01"), dictionary, dictionary, ""},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			frame := bytes.Clone(fixture.frame)
			slot, err := NewMessageSlot(fixture.transport, fixture.application, DefaultParseLimits())
			require.NoError(t, err)
			require.NoError(t, slot.Parse(frame), "fixture must be accepted by fixed parsing")
			owned := materializeInboundMessage(slot.Message())
			assertInboundOwnedRepresentation(t, slot.Message(), owned)
			require.Nil(t, owned.parseSlot)
			require.Nil(t, owned.inbound)
			legacy := NewMessage()
			require.NoError(t, ParseMessageWithDataDictionary(legacy, bytes.NewBuffer(bytes.Clone(fixture.frame)), fixture.transport, fixture.application))
			require.Equal(t, legacy.Bytes(), owned.Bytes())
			// The convenience parser sizes fields by SOH count, leaving empty
			// trailing entries for XMLData that contains SOH. Compare real fields.
			legacyFields := legacy.fields
			for len(legacyFields) != 0 && len(legacyFields[len(legacyFields)-1].bytes) == 0 {
				legacyFields = legacyFields[:len(legacyFields)-1]
			}
			require.Equal(t, legacyFields, owned.fields)
			if fixture.section == "" {
				assertInboundOwnedRepresentation(t, legacy, owned)
			} else {
				// Ordinary parsing omits header/trailer group ranges. The owned
				// adapter must keep the fixed parser's existing richer grouping;
				// that parser difference is not an ownership conversion failure.
				assertInboundEquivalentScalarFields(t, legacy, owned)
			}
			for index := range frame {
				frame[index] = 'X'
			}
			slot.Reset()
			require.NoError(t, slot.Parse(buildFIXT11Wire("35=0\x01", "")))
			fresh, err := NewMessageSlot(fixture.transport, fixture.application, DefaultParseLimits())
			require.NoError(t, err)
			require.NoError(t, fresh.Parse(bytes.Clone(fixture.frame)))
			assertInboundOwnedRepresentation(t, fresh.Message(), owned)
			// Mutating each retained getter must affect its own raw frame,
			// body slice and field/group indexes exactly as parsed input does.
			for index := range owned.fields {
				if len(owned.fields[index].value) == 0 {
					continue
				}
				previous := owned.fields[index].value[0]
				owned.fields[index].value[0] = 'X'
				fresh.Message().fields[index].value[0] = 'X'
				assertInboundOwnedRepresentation(t, fresh.Message(), owned)
				owned.fields[index].value[0] = previous
				fresh.Message().fields[index].value[0] = previous
			}
			if fixture.section != "" {
				fields, tag, member, expected := &owned.Header.FieldMap, Tag(627), Tag(628), []string{"A", "B"}
				if fixture.section == "trailer" {
					fields, tag, member, expected = &owned.Trailer.FieldMap, 9000, 9001, []string{"T", "U"}
				}
				group := NewRepeatingGroup(tag, GroupTemplate{GroupElement(member)})
				require.Nil(t, fields.GetGroup(group))
				require.Equal(t, len(expected), group.Len())
				for index, want := range expected {
					value, reject := group.Get(index).GetString(member)
					require.Nil(t, reject)
					require.Equal(t, want, value)
				}
			}
		})
	}
}

func assertInboundOwnedRepresentation(t *testing.T, expected, actual *Message) {
	t.Helper()
	require.Equal(t, expected.Bytes(), actual.Bytes())
	require.Equal(t, expected.bodyBytes, actual.bodyBytes)
	fields := expected.fields
	for len(fields) != 0 && len(fields[len(fields)-1].bytes) == 0 {
		fields = fields[:len(fields)-1]
	}
	require.Equal(t, fields, actual.fields)
	for _, section := range []struct{ expected, actual *FieldMap }{
		{&expected.Header.FieldMap, &actual.Header.FieldMap},
		{&expected.Body.FieldMap, &actual.Body.FieldMap},
		{&expected.Trailer.FieldMap, &actual.Trailer.FieldMap},
	} {
		require.ElementsMatch(t, section.expected.Tags(), section.actual.Tags())
		for _, tag := range section.expected.Tags() {
			want, _ := section.expected.lookup(tag)
			got, ok := section.actual.lookup(tag)
			require.True(t, ok)
			require.Equal(t, want, got, "field/group tag %d", tag)
			for index := range want {
				for fieldIndex := range expected.fields {
					if &want[index] == &expected.fields[fieldIndex] {
						require.Same(t, &actual.fields[fieldIndex], &got[index], "field/group index must address the owned field slab")
					}
				}
			}
		}
	}
	wantWire, err := expected.AppendTo(make([]byte, 0, len(expected.Bytes())+128))
	require.NoError(t, err)
	gotWire, err := actual.AppendTo(make([]byte, 0, len(actual.Bytes())+128))
	require.NoError(t, err)
	require.Equal(t, wantWire, gotWire, "field order must survive materialization")
}

func assertInboundEquivalentScalarFields(t *testing.T, expected, actual *Message) {
	t.Helper()
	for _, section := range []struct{ expected, actual *FieldMap }{
		{&expected.Header.FieldMap, &actual.Header.FieldMap},
		{&expected.Body.FieldMap, &actual.Body.FieldMap},
		{&expected.Trailer.FieldMap, &actual.Trailer.FieldMap},
	} {
		for _, tag := range section.actual.Tags() {
			got, _ := section.actual.lookup(tag)
			want, present := section.expected.lookup(tag)
			if len(got) == 1 && present && len(want) == 1 {
				require.Equal(t, want, got, "scalar tag %d", tag)
			}
		}
	}
}
