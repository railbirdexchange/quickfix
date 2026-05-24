package quickfix

import (
	"testing"

	"github.com/quickfixgo/quickfix/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitiatorSocketOutboundBufferSize(t *testing.T) {
	sessionID := SessionID{BeginString: BeginStringFIX42, SenderCompID: "sender", TargetCompID: "target"}
	sessionSettings := NewSessionSettings()
	sessionSettings.Set(config.BeginString, sessionID.BeginString)
	sessionSettings.Set(config.SenderCompID, sessionID.SenderCompID)
	sessionSettings.Set(config.TargetCompID, sessionID.TargetCompID)

	settings := NewSettings()
	settings.GlobalSettings().Set(config.SocketOutboundBufferSize, "128")
	_, err := settings.AddSession(sessionSettings)
	require.NoError(t, err)

	initiator := &Initiator{settings: settings}
	got, err := initiator.socketOutboundBufferSize(sessionID)
	require.NoError(t, err)
	assert.Equal(t, 128, got)

	sessionSettings.Set(config.SocketOutboundBufferSize, "256")
	got, err = initiator.socketOutboundBufferSize(sessionID)
	require.NoError(t, err)
	assert.Equal(t, 256, got)
}

func TestInitiatorSocketOutboundBufferSizeRejectsNegativeValue(t *testing.T) {
	settings := NewSettings()
	settings.GlobalSettings().Set(config.SocketOutboundBufferSize, "-1")

	initiator := &Initiator{settings: settings}
	_, err := initiator.socketOutboundBufferSize(SessionID{})
	require.Error(t, err)
}
