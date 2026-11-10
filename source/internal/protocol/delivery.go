package protocol

const (
	MaxIDBytes   = 128
	MaxNameBytes = 256
)

type DeliveryState string

const (
	DeliveryUnclaimed      DeliveryState = "unclaimed"
	DeliveryClaimedUnknown DeliveryState = "claimed-unknown"
	DeliveryClaimed        DeliveryState = "claimed"
)
