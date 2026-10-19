package quickfix

import (
	"fmt"
	"sync"
	"testing"
)

func TestMissingFieldRejectCompletedWireAndLogLifetime(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		groups bool
	}{
		{name: "FIX40"},
		{name: "FIX44", groups: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newScopedOutboundBenchmark(t, scenario.groups, "")
			defer fixture.close()
			fixture.session.application = &scopedControlApplication{}
			log := &boundedEventLegacyLog{}
			fixture.session.log = log
			fixture.session.boundedIn.eventLog = nil

			const originalTag Tag = 9999
			_, reject := fixture.message.Body.GetString(originalTag)
			if reject == nil || reject.RefTagID() == nil {
				t.Fatal("missing field did not return a referenced reject")
			}
			reference := reject.RefTagID()
			*reference = 10000
			const reason = "Conditionally Required Field Missing (9999)"
			if reject.Error() != reason || reject.RejectReason() != 8 || !reject.IsBusinessReject() || reject.BusinessRejectRefID() != "" {
				t.Fatalf("missing-field reject changed its original text or classification: %v", reject)
			}
			referenceSequence, err := fixture.message.Header.GetInt(tagMsgSeqNum)
			if err != nil {
				t.Fatal(err)
			}
			senderSequence := fixture.store.NextSenderMsgSeqNum()
			if err := fixture.session.doReject(fixture.message, reject); err != nil {
				t.Fatal(err)
			}
			// This waits for the completed socket write and checks output-slot return.
			if err := completeScopedControl(fixture); err != nil {
				t.Fatal(err)
			}
			if fixture.session.controls.busy {
				t.Fatal("completed reject retained the session control slot")
			}

			message := fixture.peer.Message()
			wantType, wantText := "3", reason+" (10000)"
			if scenario.groups {
				wantType, wantText = "j", reason
				if value, err := message.Body.GetInt(tagBusinessRejectReason); err != nil || value != 8 {
					t.Fatalf("business reject reason = %d, %v; want 8", value, err)
				}
				if value, err := message.Body.GetString(tagRefMsgType); err != nil || value != "D" {
					t.Fatalf("reference message type = %q, %v; want D", value, err)
				}
			} else if message.Body.Has(tagBusinessRejectReason) || message.Body.Has(tagRefMsgType) {
				t.Fatal("FIX 4.0 reject unexpectedly contained later-version reference fields")
			}
			if value, err := message.Header.GetString(tagMsgType); err != nil || value != wantType {
				t.Fatalf("reject message type = %q, %v; want %q", value, err, wantType)
			}
			if value, err := message.Body.GetString(tagText); err != nil || value != wantText {
				t.Fatalf("reject text = %q, %v; want %q", value, err, wantText)
			}
			if value, err := message.Header.GetInt(tagMsgSeqNum); err != nil || value != senderSequence {
				t.Fatalf("sender sequence = %d, %v; want %d", value, err, senderSequence)
			}
			if value, err := message.Body.GetInt(tagRefSeqNum); err != nil || value != referenceSequence {
				t.Fatalf("reference sequence = %d, %v; want %d", value, err, referenceSequence)
			}
			if message.Body.Has(tagRefTagID) || message.Body.Has(tagSessionRejectReason) || message.Body.Has(tagBusinessRejectRefID) {
				t.Fatal("missing-field reject unexpectedly contained a session reason or reference field")
			}
			if log.calls != 1 || log.format != "Message Rejected: %v" || len(log.args) != 1 || log.args[0] != reason {
				t.Fatalf("reject log = %q %v (%d calls)", log.format, log.args, log.calls)
			}
			retainedFormat, retainedArguments := log.format, log.args

			// A legacy logger may retain its arguments after the control, source,
			// and peer message scopes finish. Later reference mutation must not
			// rewrite either the original error text or that retained log value.
			fixture.message.Body.Clear()
			fixture.peer.Reset()
			next, acquireErr := fixture.session.acquireControl("0")
			if acquireErr != nil {
				t.Fatal(acquireErr)
			}
			next.Body.SetString(tagText, "replacement control text")
			fixture.session.releaseControl()
			*reference = 10001
			if reject.RefTagID() != reference || reject.Error() != reason {
				t.Fatal("retained reject changed after message scope reuse")
			}
			if value := fmt.Sprintf(retainedFormat, retainedArguments...); value != "Message Rejected: "+reason {
				t.Fatalf("retained reject log changed: %q", value)
			}
		})
	}
}

func TestMissingFieldRejectConcurrentErrorAndReferenceMutation(t *testing.T) {
	reject := ConditionallyRequiredFieldMissing(44)
	reference := reject.RefTagID()
	const want = "Conditionally Required Field Missing (44)"
	start := make(chan struct{})
	var workers sync.WaitGroup
	for reader := 0; reader < 4; reader++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for iteration := 0; iteration < 1000; iteration++ {
				if got := reject.Error(); got != want {
					t.Errorf("concurrent error text = %q, want %q", got, want)
					return
				}
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-start
		for iteration := 0; iteration < 1000; iteration++ {
			*reference = Tag(iteration)
		}
	}()
	close(start)
	workers.Wait()
	// Only inspect the mutable reference after its writer has finished.
	if reject.RefTagID() != reference || *reference != 999 || reject.Error() != want {
		t.Fatal("reference mutation changed the original error text or pointer")
	}
}
