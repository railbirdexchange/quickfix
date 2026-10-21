// Copyright (c) quickfixengine.org All rights reserved.
// See LICENSE for licensing information.

package quickfix

import (
	"bytes"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

func TestMessageRouterVersionMappings(test *testing.T) {
	for _, version := range []struct{ application, route string }{
		{ApplVerIDFIX27, ApplVerIDFIX27},
		{ApplVerIDFIX30, ApplVerIDFIX30},
		{ApplVerIDFIX40, BeginStringFIX40},
		{ApplVerIDFIX41, BeginStringFIX41},
		{ApplVerIDFIX42, BeginStringFIX42},
		{ApplVerIDFIX43, BeginStringFIX43},
		{ApplVerIDFIX44, BeginStringFIX44},
		{ApplVerIDFIX50, ApplVerIDFIX50},
		{ApplVerIDFIX50SP1, ApplVerIDFIX50SP1},
		{ApplVerIDFIX50SP2, ApplVerIDFIX50SP2},
		{"", ""},
		{"CUSTOM_APPLICATION_VERSION_LONGER_THAN_32_BYTES", "CUSTOM_APPLICATION_VERSION_LONGER_THAN_32_BYTES"},
	} {
		for _, useDefault := range []bool{false, true} {
			name := version.application + "/explicit"
			if useDefault {
				name = version.application + "/default"
			}
			test.Run(name, func(test *testing.T) {
				fixture := routingAllocationCase{beginString: BeginStringFIXT11, msgType: "AE", applVerID: version.application, defaultVersion: useDefault}
				message, sessionID := routingAllocationMessage(test, fixture)
				router := NewMessageRouter()
				calls := 0
				router.AddRoute(version.route, fixture.msgType, func(actual *Message, actualID SessionID) MessageRejectError {
					if actual != message || actualID != sessionID {
						test.Fatal("route changed callback arguments")
					}
					calls++
					return nil
				})
				if reject := router.Route(message, sessionID); reject != nil || calls != 1 {
					test.Fatalf("reject = %v, calls = %d", reject, calls)
				}
			})
		}
	}
}

func TestMessageRouterMissingAndUnmatchedFields(test *testing.T) {
	for _, fixture := range []struct {
		name        string
		beginString string
		msgType     string
		applVerID   string
		omitBegin   bool
		omitType    bool
		omitVersion bool
		want        MessageRejectError
	}{
		{name: "no_begin_or_type", omitBegin: true, omitType: true, want: ConditionallyRequiredFieldMissing(tagBeginString)},
		{name: "no_begin", omitBegin: true, msgType: "D", want: ConditionallyRequiredFieldMissing(tagBeginString)},
		{name: "no_type", beginString: BeginStringFIX44, omitType: true, want: ConditionallyRequiredFieldMissing(tagMsgType)},
		{name: "unsupported", beginString: BeginStringFIX44, msgType: "D", want: UnsupportedMessageType()},
		{name: "empty_type", beginString: BeginStringFIX44, want: UnsupportedMessageType()},
		{name: "unknown_default_session", beginString: BeginStringFIXT11, msgType: "D", omitVersion: true, want: NewMessageRejectError(errUnknownSession.Error(), rejectReasonCompIDProblem, nil)},
		{name: "explicit_empty_without_session", beginString: BeginStringFIXT11, msgType: "D", want: UnsupportedMessageType()},
		{name: "fixt_j_still_needs_default_session", beginString: BeginStringFIXT11, msgType: "j", omitVersion: true, want: NewMessageRejectError(errUnknownSession.Error(), rejectReasonCompIDProblem, nil)},
		{name: "fixt_j_explicit", beginString: BeginStringFIXT11, msgType: "j", applVerID: ApplVerIDFIX50SP2},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			message := NewMessage()
			if !fixture.omitBegin {
				message.Header.SetString(tagBeginString, fixture.beginString)
			}
			if !fixture.omitType {
				message.Header.SetString(tagMsgType, fixture.msgType)
			}
			if !fixture.omitVersion {
				message.Header.SetString(tagApplVerID, fixture.applVerID)
			}
			reject := NewMessageRouter().Route(message, SessionID{TargetCompID: test.Name()})
			if !reflect.DeepEqual(reject, fixture.want) {
				test.Fatalf("reject = %#v, want %#v", reject, fixture.want)
			}
		})
	}
}

func TestMessageRouterAdminAndBusinessReject(test *testing.T) {
	for _, msgType := range []string{"0", "A", "1", "2", "3", "4", "5", "j"} {
		for _, beginString := range []string{BeginStringFIX44, BeginStringFIXT11} {
			test.Run(beginString+"/"+msgType, func(test *testing.T) {
				fixture := routingAllocationCase{beginString: beginString, msgType: msgType, applVerID: ApplVerIDFIX50SP2}
				message, sessionID := routingAllocationMessage(test, fixture)
				router := NewMessageRouter()
				if reject := router.Route(message, sessionID); reject != nil {
					test.Fatal(reject)
				}
				routeVersion := beginString
				if beginString == BeginStringFIXT11 && msgType == "j" {
					routeVersion = ApplVerIDFIX50SP2
				}
				wanted := NewMessageRejectError("callback reject", 5, nil)
				router.AddRoute(routeVersion, msgType, func(*Message, SessionID) MessageRejectError { return wanted })
				if reject := router.Route(message, sessionID); reject != wanted {
					test.Fatalf("reject = %v, want original callback reject %v", reject, wanted)
				}
			})
		}
	}
}

func TestMessageRouterExplicitEmptyOverridesDefault(test *testing.T) {
	fixture := routingAllocationCase{beginString: BeginStringFIXT11, msgType: "D", applVerID: ApplVerIDFIX50SP2, defaultVersion: true}
	message, sessionID := routingAllocationMessage(test, fixture)
	message.Header.SetString(tagApplVerID, "")
	router := NewMessageRouter()
	wanted := NewMessageRejectError("explicit empty version", 5, nil)
	router.AddRoute("", "D", func(*Message, SessionID) MessageRejectError { return wanted })
	router.AddRoute(ApplVerIDFIX50SP2, "D", func(*Message, SessionID) MessageRejectError {
		test.Fatal("explicit empty version fell back to session default")
		return nil
	})
	if reject := router.Route(message, sessionID); reject != wanted {
		test.Fatalf("reject = %v, want %v", reject, wanted)
	}
}

func TestMessageRouterOverrideAndRetention(test *testing.T) {
	fixture := routingAllocationCases[4]
	message, sessionID := routingAllocationMessage(test, fixture)
	router := NewMessageRouter()
	router.AddRoute(fixture.routeVersion, fixture.msgType, func(*Message, SessionID) MessageRejectError {
		test.Fatal("replaced route called")
		return nil
	})
	var retained *Message
	var retainedVersion []byte
	router.AddRoute(fixture.routeVersion, fixture.msgType, func(actual *Message, _ SessionID) MessageRejectError {
		retained = actual
		retainedVersion, _ = actual.Header.GetBytes(tagApplVerID)
		return nil
	})
	if reject := router.Route(message, sessionID); reject != nil {
		test.Fatal(reject)
	}
	other := NewMessage()
	other.Header.SetString(tagBeginString, BeginStringFIX44)
	other.Header.SetString(tagMsgType, "D")
	router.AddRoute(BeginStringFIX44, "D", func(*Message, SessionID) MessageRejectError { return nil })
	if reject := router.Route(other, sessionID); reject != nil {
		test.Fatal(reject)
	}
	if retained != message || !bytes.Equal(retainedVersion, []byte(fixture.applVerID)) {
		test.Fatal("routing another message changed retained callback data")
	}
	// Keys derived from a message must never become retained aliases of that
	// message's bytes. A later mutation cannot change the registered routes.
	for _, tag := range []Tag{tagApplVerID, tagMsgType} {
		value, reject := message.Header.GetBytes(tag)
		if reject != nil {
			test.Fatal(reject)
		}
		original := value[0]
		value[0] = 'X'
		reject = router.Route(message, sessionID)
		value[0] = original
		if !reflect.DeepEqual(reject, UnsupportedMessageType()) {
			test.Fatalf("modified tag %d reject = %v", tag, reject)
		}
		if reject := router.Route(message, sessionID); reject != nil {
			test.Fatalf("restored tag %d reject = %v", tag, reject)
		}
	}
}

func TestMessageRouterSharedImmutableMessages(test *testing.T) {
	router := NewMessageRouter()
	var calls atomic.Int64
	messages := make([]*Message, len(routingAllocationCases))
	sessionIDs := make([]SessionID, len(routingAllocationCases))
	for index, fixture := range routingAllocationCases {
		// The fixture helper uses the test name for registration; make each
		// default-version session unique without parallelizing global setup.
		sessionIDs[index] = SessionID{BeginString: fixture.beginString, SenderCompID: fixture.name, TargetCompID: test.Name()}
		message := NewMessage()
		message.Header.SetString(tagBeginString, fixture.beginString)
		message.Header.SetString(tagMsgType, fixture.msgType)
		if fixture.defaultVersion {
			sessionID := sessionIDs[index]
			if err := registerSession(&session{sessionID: sessionID, targetDefaultApplVerID: fixture.applVerID}); err != nil {
				test.Fatal(err)
			}
			test.Cleanup(func() { _ = UnregisterSession(sessionID) })
		} else if fixture.beginString == BeginStringFIXT11 {
			message.Header.SetString(tagApplVerID, fixture.applVerID)
		}
		messages[index] = message
		router.AddRoute(fixture.routeVersion, fixture.msgType, func(*Message, SessionID) MessageRejectError {
			calls.Add(1)
			return nil
		})
	}
	const workers, iterations = 8, 1000
	var workersDone sync.WaitGroup
	for range workers {
		workersDone.Add(1)
		go func() {
			defer workersDone.Done()
			for range iterations {
				for index, message := range messages {
					if reject := router.Route(message, sessionIDs[index]); reject != nil {
						test.Error(reject)
					}
				}
			}
		}()
	}
	workersDone.Wait()
	if got, want := calls.Load(), int64(workers*iterations*len(messages)); got != want {
		test.Fatalf("calls = %d, want %d", got, want)
	}
}
