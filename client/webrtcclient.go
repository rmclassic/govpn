package client

import (
	"encoding/json"
	"fmt"

	"github.com/pchchv/govpn/common/cipher"
	"github.com/pchchv/govpn/common/config"
	"github.com/pchchv/govpn/common/control"
	"github.com/pchchv/govpn/common/sdputil"
	"github.com/pchchv/govpn/vpn"
	"github.com/pion/webrtc/v3"
	"github.com/songgao/water"
)
func createConnection() (*webrtc.PeerConnection, error) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}
	peerConnection, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}

	return peerConnection, nil
}

func StartWebRTCClient(config config.Config) {
	peerConnection, err := createConnection()
	if err != nil {
		panic(err)
	}

	peerConnection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateFailed {
			fmt.Printf("\nICE Connection State has changed: %s\n\n", state.String())
			return
		}

		if state != webrtc.ICEConnectionStateClosed {
			fmt.Printf("\nICE Connection State has changed: %s\n\n", state.String())
		}
	})

	answer, err := sdputil.SDPPrompt()
	if err != nil {
		panic(err)
	}

	err = PrintSDP(peerConnection, answer)
	if err != nil {
		panic(err)
	}
	
	ifaceChan := make(chan *water.Interface)
	var iface *water.Interface
	var dataChannel *webrtc.DataChannel
	peerConnection.OnDataChannel(func(dc *webrtc.DataChannel) {
		println("New data channel created: ", dc.Label())

		if (dc.Label() == "data") {
			dataChannel = dc
			dc.OnMessage(newDataMessageHandler(&iface))
		} else if (dc.Label() == "control") {
			dc.OnMessage(newControlMessageHandler(ifaceChan))
		}
	})

	iface = <-ifaceChan

	packet := make([]byte, 1500)
	for {
		n, err := (*iface).Read(packet)
		if err != nil || n == 0 {
			continue
		}

		if dataChannel == nil || dataChannel.ReadyState() != webrtc.DataChannelStateOpen {
			println("channel not ready yet, not relaying")
			continue
		}

		println("relaying packet: ", len(packet[:n]), "bytes")
		b := cipher.XOR(packet[:n])
		err = dataChannel.Send(b)
		if err != nil {
			println(err.Error())
			continue
		}
	}
}

func PrintSDP(p *webrtc.PeerConnection, offer webrtc.SessionDescription) error {
	sdp, err := GenSDP(p, offer)
	if err != nil {
		return err
	}
	fmt.Println(sdp)
	return nil
}

func GenSDP(p *webrtc.PeerConnection, offer webrtc.SessionDescription) (string, error) {
	var sdp string
	err := p.SetRemoteDescription(offer)
	if err != nil {
		return sdp, err
	}

	answer, err := p.CreateAnswer(nil)
	if err != nil {
		return sdp, err
	}

	gatherDone := webrtc.GatheringCompletePromise(p)
	err = p.SetLocalDescription(answer)
	if err != nil {
		return sdp, err
	}
	<-gatherDone

	//Encode the SDP to base64
	sdp, err = cipher.Encode(p.LocalDescription())
	return sdp, err
}

func newDataMessageHandler(iface **water.Interface) func(msg webrtc.DataChannelMessage) {
	return func(msg webrtc.DataChannelMessage) {
		// relay packets
		b := cipher.XOR(msg.Data)

		println("incoming packet: ", len(b), "bytes")

		if (*iface) != nil {
			(*iface).Write(b)
		} else {
			println("interface is not ready, discarding packet")
		}
	}
}

func newControlMessageHandler(ifaceChan chan *water.Interface) func(msg webrtc.DataChannelMessage) {
	return func(data webrtc.DataChannelMessage) {
		fmt.Printf("received control-message\n")
		var msg control.ControlMessage
		err := json.Unmarshal(data.Data, &msg)
		if err != nil {
			return
		}

		switch msg.ID {
			case control.MessageIDIPAllocation:
				var ipmsg control.IPAllocationMessage
				err := json.Unmarshal(data.Data, &ipmsg)
				if err != nil {
					return
				}
		
				fmt.Printf("received ip allocation data, IP: %s Gateway: %s CIDR: %s\n", 
				ipmsg.IPAddress, ipmsg.GatewayAddress, ipmsg.CIDR)
				iface := vpn.CreateClientVpn(ipmsg.CIDR, ipmsg.IPAddress, ipmsg.GatewayAddress)
				ifaceChan <- iface
		}
	}
}
