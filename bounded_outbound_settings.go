package quickfix

import (
	"github.com/quickfixgo/quickfix/config"
	"time"
)

func (s *session) configureBoundedOutbound(settings *SessionSettings, application Application) error {
	if !settings.HasSetting(config.BoundedOutbound) {
		return nil
	}
	enabled, err := settings.BoolSetting(config.BoundedOutbound)
	if err != nil || !enabled {
		return err
	}
	app, ok := application.(BorrowedOutboundApplication)
	store, storeOK := s.store.(*BoundedStore)
	if !ok || !storeOK || s.boundedIn == nil || s.DisableMessagePersist {
		return ErrBoundedOutboundConfiguration
	}
	queue, builderBytes := 32, 32768
	if settings.HasSetting(config.BoundedOutboundQueueDepth) {
		queue, err = settings.IntSetting(config.BoundedOutboundQueueDepth)
		if err != nil {
			return err
		}
	}
	if settings.HasSetting(config.BoundedBuilderBytes) {
		builderBytes, err = settings.IntSetting(config.BoundedBuilderBytes)
		if err != nil {
			return err
		}
	}
	timeout := 5 * time.Second
	if settings.HasSetting(config.BoundedWriteTimeout) {
		timeout, err = settings.DurationSetting(config.BoundedWriteTimeout)
		if err != nil {
			return err
		}
	}
	b, err := newBoundedOutbound(s, app, store, queue, builderBytes, timeout)
	if err != nil {
		return err
	}
	s.boundedOut = b
	for i := range s.boundedIn.ring.slots {
		s.boundedIn.ring.slots[i].parsed.outbound = b
	}
	return nil
}

func isOutboundCapacityError(err error) bool {
	return err == ErrJournalFull || err == ErrJournalMessageCapacity || err == ErrJournalRangeUnavailable || err == ErrOutboundBuilderCapacity || err == ErrBufferTooSmall || err == ErrFieldCapacity
}
