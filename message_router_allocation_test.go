// Copyright (c) quickfixengine.org All rights reserved.
// See LICENSE for licensing information.

package quickfix

import "testing"

type routingAllocationCase struct {
	name           string
	beginString    string
	msgType        string
	applVerID      string
	defaultVersion bool
	routeVersion   string
}

var routingAllocationCases = []routingAllocationCase{
	{name: "fix44", beginString: BeginStringFIX44, msgType: "D", routeVersion: BeginStringFIX44},
	{name: "fixt_explicit", beginString: BeginStringFIXT11, msgType: "D", applVerID: ApplVerIDFIX50SP2, routeVersion: ApplVerIDFIX50SP2},
	{name: "fixt_default", beginString: BeginStringFIXT11, msgType: "D", applVerID: ApplVerIDFIX50SP2, defaultVersion: true, routeVersion: ApplVerIDFIX50SP2},
	{name: "fix44_multibyte_type", beginString: BeginStringFIX44, msgType: "AE", routeVersion: BeginStringFIX44},
	{name: "fixt_custom_long_keys", beginString: BeginStringFIXT11, msgType: "CUSTOM_MESSAGE_TYPE_LONGER_THAN_32_BYTES", applVerID: "CUSTOM_APPLICATION_VERSION_LONGER_THAN_32_BYTES", routeVersion: "CUSTOM_APPLICATION_VERSION_LONGER_THAN_32_BYTES"},
	{name: "fixt_default_custom", beginString: BeginStringFIXT11, msgType: "AE", applVerID: "CUSTOM_APPLICATION_VERSION_LONGER_THAN_32_BYTES", defaultVersion: true, routeVersion: "CUSTOM_APPLICATION_VERSION_LONGER_THAN_32_BYTES"},
}

func routingAllocationMessage(test testing.TB, fixture routingAllocationCase) (*Message, SessionID) {
	test.Helper()
	message := NewMessage()
	message.Header.SetString(tagBeginString, fixture.beginString)
	message.Header.SetString(tagMsgType, fixture.msgType)
	sessionID := SessionID{BeginString: fixture.beginString, SenderCompID: "routing-allocation", TargetCompID: test.Name()}
	if fixture.defaultVersion {
		if err := registerSession(&session{sessionID: sessionID, targetDefaultApplVerID: fixture.applVerID}); err != nil {
			test.Fatal(err)
		}
		test.Cleanup(func() {
			if err := UnregisterSession(sessionID); err != nil {
				test.Error(err)
			}
		})
	} else if fixture.beginString == BeginStringFIXT11 {
		message.Header.SetString(tagApplVerID, fixture.applVerID)
	}
	return message, sessionID
}

func TestMessageRouterZeroAllocations(test *testing.T) {
	for _, fixture := range routingAllocationCases {
		test.Run(fixture.name, func(test *testing.T) {
			message, sessionID := routingAllocationMessage(test, fixture)
			router := NewMessageRouter()
			calls := 0
			router.AddRoute(fixture.routeVersion, fixture.msgType, func(actual *Message, actualID SessionID) MessageRejectError {
				if actual != message || actualID != sessionID {
					test.Fatal("route changed callback arguments")
				}
				calls++
				return nil
			})
			allocations := testing.AllocsPerRun(100, func() {
				if reject := router.Route(message, sessionID); reject != nil {
					test.Fatal(reject)
				}
			})
			if allocations != 0 || calls != 101 {
				test.Fatalf("allocations = %v, calls = %d; want 0 allocations and 101 calls", allocations, calls)
			}
		})
	}
}

// BenchmarkMessageRouterAllocation measures only the existing Route API. All
// messages, routes and default-version sessions are constructed before timing.
func BenchmarkMessageRouterAllocation(benchmark *testing.B) {
	for _, fixture := range routingAllocationCases {
		benchmark.Run(fixture.name, func(benchmark *testing.B) {
			message, sessionID := routingAllocationMessage(benchmark, fixture)
			router := NewMessageRouter()
			calls := 0
			router.AddRoute(fixture.routeVersion, fixture.msgType, func(actual *Message, actualID SessionID) MessageRejectError {
				if actual != message || actualID != sessionID {
					benchmark.Fatal("route changed callback arguments")
				}
				calls++
				return nil
			})
			if reject := router.Route(message, sessionID); reject != nil {
				benchmark.Fatal(reject)
			}
			calls = 0
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for iteration := 0; iteration < benchmark.N; iteration++ {
				if reject := router.Route(message, sessionID); reject != nil {
					benchmark.Fatal(reject)
				}
			}
			benchmark.StopTimer()
			if calls != benchmark.N {
				benchmark.Fatalf("route calls = %d, want %d", calls, benchmark.N)
			}
		})
	}
}
