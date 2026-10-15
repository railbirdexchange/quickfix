package config

const (
	// BoundedInbound enables fixed inbound frame/message slots. It requires a
	// quickfix.BorrowedApplication and quickfix.BorrowedLog. Defaults to N.
	BoundedInbound = "BoundedInbound"
	// BoundedMaxFrameBytes includes the complete FIX wire frame. Default: 16384.
	BoundedMaxFrameBytes = "BoundedMaxFrameBytes"
	// BoundedMaxFields counts all header/body/trailer and repeated fields. Default: 512.
	BoundedMaxFields = "BoundedMaxFields"
	// BoundedMaxGroupEntries counts all nested group entries. Default: 128.
	BoundedMaxGroupEntries = "BoundedMaxGroupEntries"
	// BoundedMaxGroupDepth limits nested repeating groups. Default: 8.
	BoundedMaxGroupDepth = "BoundedMaxGroupDepth"
	// BoundedInboundStashCapacity limits retained out-of-order messages. Default: 32.
	// The ring reserves InChanCapacity + 2 additional slots for progress.
	BoundedInboundStashCapacity = "BoundedInboundStashCapacity"
)
