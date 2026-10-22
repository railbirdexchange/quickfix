package quickfix

import (
	"testing"

	"github.com/quickfixgo/quickfix/datadictionary"
)

func TestConfiguredMessageTypeStringSurvivesInputAndDictionaryMutation(t *testing.T) {
	dictionary := &datadictionary.DataDictionary{
		Messages: map[string]*datadictionary.MessageDef{"AJ": nil},
	}
	metadata := newSessionFieldMetadata(SessionID{}, nil, dictionary)
	message := NewMessage()
	message.useFieldMetadata(metadata)
	value := []byte("AJ")
	message.Header.SetBytes(tagMsgType, value)
	configured, reject := message.Header.GetString(tagMsgType)
	if reject != nil || configured != "AJ" {
		t.Fatalf("configured string=%q reject=%v", configured, reject)
	}
	value, reject = message.Header.GetBytes(tagMsgType)
	if reject != nil {
		t.Fatal(reject)
	}
	value[0], value[1] = 'Z', 'Z'
	delete(dictionary.Messages, "AJ")
	dictionary.Messages["ZZ"] = nil
	unknown, reject := message.Header.GetString(tagMsgType)
	if reject != nil || unknown != "ZZ" {
		t.Fatalf("unknown string=%q reject=%v", unknown, reject)
	}
	value[0], value[1] = 'A', 'J'
	again, reject := message.Header.GetString(tagMsgType)
	if reject != nil || configured != "AJ" || unknown != "ZZ" || again != "AJ" {
		t.Fatalf("retained strings changed: configured=%q unknown=%q again=%q reject=%v",
			configured, unknown, again, reject)
	}
}
