package control

const (
	MessageIDIPAllocation = "ip.allocate"
)

type ControlMessage struct {
	ID string
}

type IPAllocationMessage struct {
	ID             string
	IPAddress      string
	GatewayAddress string
	CIDR           string
}
