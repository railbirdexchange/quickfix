package config

const (
	// OutboundFrameQueueCapacity bounds pending output frames. The writer
	// receives one additional progress slot. Default: 32.
	OutboundFrameQueueCapacity = "OutboundFrameQueueCapacity"
	// OutboundWriteTimeout bounds a complete socket write. Default: 30s.
	OutboundWriteTimeout = "OutboundWriteTimeout"
	// InboundFrameQueueCapacity bounds extra raw frame slots beyond retained
	// resend frames and two progress slots. Default: min(InChanCapacity, 32).
	// InChanCapacity remains a channel upper bound; vacant-slot exhaustion
	// backpressures the socket reader before a large channel fills.
	InboundFrameQueueCapacity = "InboundFrameQueueCapacity"
	// BoundedMaxFrameBytes includes the complete FIX wire frame. Default: 16384.
	BoundedMaxFrameBytes = "BoundedMaxFrameBytes"
	// BoundedMaxFields counts all header/body/trailer and repeated fields. Default: 512.
	BoundedMaxFields = "BoundedMaxFields"
	// BoundedMaxGroupEntries counts all nested group entries. Default: 128.
	BoundedMaxGroupEntries = "BoundedMaxGroupEntries"
	// BoundedMaxGroupDepth limits nested repeating groups. Default: 8.
	BoundedMaxGroupDepth = "BoundedMaxGroupDepth"
	// BoundedInboundStashCapacity limits retained out-of-order messages. Default: 32.
	// The ring reserves InboundFrameQueueCapacity + 2 more slots for progress.
	BoundedInboundStashCapacity = "BoundedInboundStashCapacity"
)
