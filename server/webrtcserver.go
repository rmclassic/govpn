package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/cilium/ipam/service/ipallocator"
	"github.com/patrickmn/go-cache"
	"github.com/pchchv/govpn/common/cipher"
	"github.com/pchchv/govpn/common/config"
	"github.com/pchchv/govpn/common/control"
	"github.com/pchchv/govpn/common/netutil"
	"github.com/pchchv/govpn/common/sdputil"
	"github.com/pchchv/govpn/vpn"
	"github.com/songgao/water"
	"github.com/songgao/water/waterutil"
	"golang.org/x/net/context"

	"github.com/pion/webrtc/v3"
)

type RTCForwarder struct {
	connCache      *cache.Cache
	peerConnection *webrtc.PeerConnection
}

func (f *RTCForwarder) forward(iface *water.Interface, channel *webrtc.DataChannel) {
	packet := make([]byte, 1500)

	for {
		n, err := iface.Read(packet)
		if err != nil || n == 0 {
			continue
		}
		b := packet[:n]

		srcAddr, dstAddr := netutil.GetAddr(b)
		if waterutil.IsIPv4(b) && srcAddr != "" && dstAddr != "" {
			fmt.Printf("relaying packet: %s -> %s: %d bytes\n", srcAddr, dstAddr, len(b))
		} else {
			fmt.Printf("[non-IP] relaying packet: %d bytes\n", len(b))
		}

		b = cipher.XOR(b)
		channel.Send(b)
	}
}

func StartWebRTCServer(ctx context.Context, config config.Config) {
	_, net, err := net.ParseCIDR(config.CIDR)
	if err != nil {
		panic(err)
	}

	ipAllocator, err := ipallocator.NewCIDRRange(net)
	if err != nil {
		panic(err)
	}
	gatewayIP, _ := ipAllocator.AllocateNext()

	iface := vpn.CreateServerVpn(config.CIDR, gatewayIP)
	rtcConfig := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}
	peerConnection, err := webrtc.NewPeerConnection(rtcConfig)
	if err != nil {
		log.Fatalln("failed to setup peer connection:", err)
	}

	peerConnection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state != webrtc.ICEConnectionStateClosed {
			fmt.Printf("\nICE Connection State has changed: %s\n\n", state.String())
		}
		if state == webrtc.ICEConnectionStateFailed {
			panic("failed to connect")
		}
	})

	ordered := false
	mplt := uint16(5000)
	dataChannel, err := peerConnection.CreateDataChannel("data", &webrtc.DataChannelInit{
		Ordered:           &ordered,
		MaxPacketLifeTime: &mplt,
	})
	if err != nil {
		panic(err)
	}

	controlChannel, err := peerConnection.CreateDataChannel("control", &webrtc.DataChannelInit{
		Ordered:           &ordered,
		MaxPacketLifeTime: &mplt,
	})
	if err != nil {
		panic(err)
	}

	offer, err := GenOffer(peerConnection)
	if err != nil {
		panic(err)
	}

	println("Offer: ", offer)

	log.Printf("govpn webrtc server started on %v,CIDR is %v", config.LocalAddr, config.CIDR)

	forwarder := &RTCForwarder{connCache: cache.New(30*time.Minute, 10*time.Minute), peerConnection: peerConnection}
	go forwarder.forward(iface, dataChannel)

	sdp, err := sdputil.SDPPrompt()
	if err != nil {
		println(err.Error())
	}

	err = peerConnection.SetRemoteDescription(sdp)
	if err != nil {
		println(err.Error())
	}

	controlChannel.OnOpen(func() {
		clientIP, _ := ipAllocator.AllocateNext()
		msg := control.IPAllocationMessage{
			ID:             control.MessageIDIPAllocation,
			IPAddress:      clientIP.String(),
			GatewayAddress: gatewayIP.String(),
			CIDR:           config.CIDR,
		}
	
		msgb, _ := json.Marshal(&msg)
		println("sending ip allocation data to client")
		err = controlChannel.Send(msgb)
		if err != nil {
			return
		}
	})

	dataChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
		b := cipher.XOR(msg.Data)

		iface.Write(b)

		srcAddr, dstAddr := netutil.GetAddr(b)

		fmt.Printf("responding packet: %s -> %s: %d bytes\n", srcAddr, dstAddr, len(b))

		if waterutil.IsIPv4(b) && srcAddr != "" && dstAddr != "" {
			key := fmt.Sprintf("%v->%v", srcAddr, dstAddr)
			forwarder.connCache.Set(key, "", cache.DefaultExpiration)
		}
	})

	<-ctx.Done()
}

func GenOffer(p *webrtc.PeerConnection) (string, error) {
	offer, err := p.CreateOffer(nil)
	if err != nil {
		return "", err
	}
	c := webrtc.GatheringCompletePromise(p)
	err = p.SetLocalDescription(offer)
	<-c
	offer2 := p.LocalDescription()

	if err != nil {
		return "", err
	}

	encoded, err := cipher.Encode(offer2)
	return encoded, err
}
