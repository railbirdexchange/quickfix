package quickfix

import (
	"sync"
	"testing"
)

func TestSessionRequiredRejectRetentionAndFallback(t *testing.T) {
	fixture := newRequiredValidationFixture(t, true)
	defer fixture.close()
	validator := fixture.session.Validator.(*sessionValidator)
	metadata := validator.bounded.requiredApp
	reject := validator.Validate(fixture.message)
	assertBoundedRejectEqual(t, reject, RequiredTagMissing(54))
	if metadata.rejects[54] == nil {
		t.Fatal("required Side was not prepared")
	}
	count := len(metadata.rejects)
	assertBoundedRejectEqual(t, metadata.missing(100001), RequiredTagMissing(100001))
	assertBoundedRejectEqual(t, (*validationRequiredMetadata)(nil).missing(54), RequiredTagMissing(54))
	if len(metadata.rejects) != count {
		t.Fatal("unknown tag grew the prepared table")
	}

	// A later rejection and reparsing reuse both validation scratch and the slot.
	fixture.message.Body.Remove(11)
	if next := validator.Validate(fixture.message); next == nil {
		t.Fatal("missing fields accepted")
	}
	wire, _ := boundedOwnershipFixture(t, true)
	if err := fixture.session.boundedIn.parsed.Parse(wire); err != nil {
		t.Fatal(err)
	}
	if next := validator.Validate(fixture.message); next != nil {
		t.Fatalf("valid input after rejection: %v", next)
	}
	fixture.session.boundedIn.parsed.Reset()
	fixture.session.Validator = nil
	assertBoundedRejectEqual(t, reject, RequiredTagMissing(54))

	// Retained immutable errors can be read concurrently; mutable validators and
	// messages still have one owner and are not shared between these readers.
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 100 {
				tag, present := reject.RefTagID()
				if tag != 54 || !present || reject.Error() != "Required tag missing" || reject.RejectReason() != 1 || reject.IsBusinessReject() || reject.BusinessRejectRefID() != "" {
					t.Error("retained required-field error changed")
					return
				}
			}
		}()
	}
	readers.Wait()
}

func TestSessionRequiredRejectCompletedWire(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		groups bool
	}{{"FIX40", false}, {"FIX44", true}} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newScopedOutboundBenchmarkWithDelivery(t, scenario.groups, "", true)
			defer fixture.close()
			wire, _ := boundedOwnershipFixture(t, scenario.groups)
			if err := fixture.session.boundedIn.parsed.Parse(wire); err != nil {
				t.Fatal(err)
			}
			message := fixture.session.boundedIn.parsed.Message()
			message.Body.Remove(54)
			reject := fixture.session.Validate(message)
			assertBoundedRejectEqual(t, reject, RequiredTagMissing(54))
			referenceSequence, err := message.Header.GetInt(tagMsgSeqNum)
			if err != nil {
				t.Fatal(err)
			}
			senderSequence := fixture.store.NextSenderMsgSeqNum()
			if err := fixture.session.doReject(message, reject); err != nil {
				t.Fatal(err)
			}
			if err := completeScopedControl(fixture); err != nil {
				t.Fatal(err)
			}
			if fixture.session.controls.busy || fixture.store.NextSenderMsgSeqNum() != senderSequence+1 {
				t.Fatal("reject did not finish its control/sequence scope")
			}
			reply := fixture.peer.Message()
			if typ, err := reply.Header.GetString(tagMsgType); err != nil || typ != "3" {
				t.Fatalf("type: %q %v", typ, err)
			}
			if seq, err := reply.Header.GetInt(tagMsgSeqNum); err != nil || seq != senderSequence {
				t.Fatalf("sequence: %d %v", seq, err)
			}
			if seq, err := reply.Body.GetInt(tagRefSeqNum); err != nil || seq != referenceSequence {
				t.Fatalf("reference sequence: %d %v", seq, err)
			}
			text := "Required tag missing"
			if scenario.groups {
				if reason, err := reply.Body.GetInt(tagSessionRejectReason); err != nil || reason != 1 {
					t.Fatalf("reason: %d %v", reason, err)
				}
				if tag, err := reply.Body.GetInt(tagRefTagID); err != nil || tag != 54 {
					t.Fatalf("reference tag: %d %v", tag, err)
				}
				if typ, err := reply.Body.GetString(tagRefMsgType); err != nil || typ != "D" {
					t.Fatalf("reference type: %q %v", typ, err)
				}
			} else {
				text += " (54)"
				if reply.Body.Has(tagSessionRejectReason) || reply.Body.Has(tagRefTagID) || reply.Body.Has(tagRefMsgType) {
					t.Fatal("FIX40 reject acquired later-version fields")
				}
			}
			if got, err := reply.Body.GetString(tagText); err != nil || got != text {
				t.Fatalf("text: %q %v", got, err)
			}
			if reply.Body.Has(tagBusinessRejectReason) || reply.Body.Has(tagBusinessRejectRefID) {
				t.Fatal("session reject acquired business-reject fields")
			}
			fixture.session.boundedIn.parsed.Reset()
			assertBoundedRejectEqual(t, reject, RequiredTagMissing(54))
		})
	}
}
