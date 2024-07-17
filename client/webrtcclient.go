package client

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"

	"github.com/pchchv/govpn/common/cipher"
	"github.com/pchchv/govpn/common/config"
	"github.com/pchchv/govpn/common/control"
	"github.com/pchchv/govpn/common/sdputil"
	"github.com/pchchv/govpn/vpn"
	"github.com/pion/webrtc/v3"
	"github.com/songgao/water"
)

type WebRTCClient struct {
	config         config.Config
	dataChannels   []*webrtc.DataChannel
	controlChannel *webrtc.DataChannel
	iface          *water.Interface
}

func NewWebRTCClient(config config.Config) WebRTCClient {
	return WebRTCClient{
		config: config,
	}
}

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

func (c *WebRTCClient) Start(ctx context.Context) {
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

	ordered := false
	mplt := uint16(5000)
	c.controlChannel, err = peerConnection.CreateDataChannel("control", &webrtc.DataChannelInit{
		Ordered:           &ordered,
		MaxPacketLifeTime: &mplt,
	})
	if err != nil {
		panic(err)
	}

	handlerCtx, _ := context.WithCancel(ctx)
	c.controlChannel.OnMessage(c.handleControlMesssage(handlerCtx))

	c.dataChannels = make([]*webrtc.DataChannel, 0)
	for i := 0; i < c.config.DataChannels; i++ {
		dataChannel, err := peerConnection.CreateDataChannel("data", &webrtc.DataChannelInit{
			Ordered:           &ordered,
			MaxPacketLifeTime: &mplt,
		})
		if err != nil {
			panic(err)
		}

		c.dataChannels = append(c.dataChannels, dataChannel)
	}

	sdp, err := GenOffer(peerConnection)
	if err != nil {
		panic(err)
	}

	serverInfo, err := getVpnInfoFromServer("http://"+c.config.ServerAddr, sdp)
	if err != nil {
		panic(err)
	}

	answer, err := sdputil.SDPParse(serverInfo.Answer)
	if err != nil {
		panic(err)
	}

	peerConnection.SetRemoteDescription(answer)

	<-ctx.Done()
}

func (c *WebRTCClient) forwardPackets(ctx context.Context) {
	packet := make([]byte, 1500)
	for {
		select {
		default:
			n, err := c.iface.Read(packet)
			if err != nil || n == 0 {
				continue
			}

			var dc *webrtc.DataChannel
			// select a channel from pool
			if len(c.dataChannels) == 0 {
				println("channel not ready yet, not relaying")
				continue
			}

			index, _ := rand.Int(rand.Reader, big.NewInt(int64(len(c.dataChannels))))
			dc = c.dataChannels[index.Int64()]

			if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
				println("channel not ready yet, not relaying")
				continue
			}

			println("relaying packet: ", len(packet[:n]), "bytes")
			b := cipher.XOR(packet[:n])
			err = dc.Send(b)
			if err != nil {
				println(err.Error())
				continue
			}
		case <-ctx.Done():
			return
		}
	}
}

func GenOffer(p *webrtc.PeerConnection) (*webrtc.SessionDescription, error) {
	offer, err := p.CreateOffer(nil)
	if err != nil {
		return nil, err
	}
	c := webrtc.GatheringCompletePromise(p)
	err = p.SetLocalDescription(offer)
	<-c
	offer2 := p.LocalDescription()

	if err != nil {
		return nil, err
	}

	return offer2, err
}

func (c *WebRTCClient) handleDataMessage(msg webrtc.DataChannelMessage) {
	// relay packets
	b := cipher.XOR(msg.Data)

	println("incoming packet: ", len(b), "bytes")

	c.iface.Write(b)
}

func (c *WebRTCClient) handleControlMesssage(ctx context.Context) func(data webrtc.DataChannelMessage) {
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
			c.iface = vpn.CreateClientVpn(ipmsg.CIDR, ipmsg.IPAddress, ipmsg.GatewayAddress)

			for _, channel := range c.dataChannels {
				channel.OnMessage(c.handleDataMessage)
			}

			go c.forwardPackets(ctx)
		}
	}
}

type httpResp struct {
	Answer    string   `json:"answer"`
	GatewayIP string   `json:"gateway_ip"`
	PublicIPs []string `json:"public_ips"`
}

type httpReq struct {
	SDP string `json:"sdp"`
}

func getVpnInfoFromServer(url string, sdp *webrtc.SessionDescription) (httpResp, error) {
	sdpStr, err := cipher.Encode(*sdp)
	if err != nil {
		return httpResp{}, err
	}

	req := httpReq{
		SDP: sdpStr,
	}

	reqStr, _ := json.Marshal(&req)

	r := strings.NewReader(string(reqStr))

	resp, err := http.Post(url, "application/json", r)
	if err != nil {
		return httpResp{}, err
	}

	var data httpResp
	return data, json.NewDecoder(resp.Body).Decode(&data)
}
