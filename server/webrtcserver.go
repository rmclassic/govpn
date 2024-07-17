package server

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
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

func NewWebRTCServer(config config.Config) rtcServer {
	_, net, err := net.ParseCIDR(config.CIDR)
	if err != nil {
		panic(err)
	}

	ipAllocator, err := ipallocator.NewCIDRRange(net)
	if err != nil {
		panic(err)
	}

	return rtcServer{
		connCache:   cache.New(30*time.Minute, 10*time.Minute),
		config:      config,
		ipAllocator: ipAllocator,
	}
}

type rtcServer struct {
	config      config.Config
	connCache   *cache.Cache
	iface       *water.Interface
	gatewayIP   net.IP
	ipAllocator ipallocator.Interface
}

func NewRTCForwarder(iface *water.Interface, clientIP, gatewayIP net.IP, config config.Config) rtcForwarder {
	return rtcForwarder{
		iface:     iface,
		config:    config,
		clientIP:  clientIP,
		gatewayIP: gatewayIP,
	}
}

type rtcForwarder struct {
	peerConnection *webrtc.PeerConnection
	iface          *water.Interface
	config         config.Config
	clientIP       net.IP
	gatewayIP      net.IP
	controlChannel *webrtc.DataChannel
	datachannels   []*webrtc.DataChannel
}

func (f *rtcForwarder) handleICE(sdp string) (string, error) {
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

	f.peerConnection = peerConnection

	peerConnection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state != webrtc.ICEConnectionStateClosed {
			fmt.Printf("\nICE Connection State has changed: %s\n\n", state.String())
		}
		if state == webrtc.ICEConnectionStateFailed {
			panic("failed to connect")
		}
	})

	f.datachannels = make([]*webrtc.DataChannel, f.config.DataChannels)

	peerConnection.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch dc.Label() {
		case "control":
			f.controlChannel = dc
			f.controlChannel.OnOpen(f.allocateIP)
		default:
			f.datachannels = append(f.datachannels, dc)
			dc.OnMessage(f.handleDataMessage)
		}
	})

	offer, err := sdputil.SDPParse(sdp)
	if err != nil {
		panic(err)
	}

	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		panic(err)
	}

	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	gatherDone := webrtc.GatheringCompletePromise(peerConnection)
	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		return sdp, err
	}
	<-gatherDone

	answerStr, err := cipher.Encode(peerConnection.LocalDescription())
	if err != nil {
		panic(err)
	}

	go f.forward()

	return answerStr, nil
}

func (f *rtcForwarder) allocateIP() {
	msg := control.IPAllocationMessage{
		ID:             control.MessageIDIPAllocation,
		IPAddress:      f.clientIP.String(),
		GatewayAddress: f.gatewayIP.String(),
		CIDR:           f.config.CIDR,
	}

	msgb, _ := json.Marshal(&msg)
	println("sending ip allocation data to client")
	err := f.controlChannel.Send(msgb)
	if err != nil {
		return
	}
}

func (f *rtcForwarder) handleDataMessage(msg webrtc.DataChannelMessage) {
	// relay packets
	b := cipher.XOR(msg.Data)

	println("incoming packet: ", len(b), "bytes")

	f.iface.Write(b)
}

func (f *rtcForwarder) forward() {
	packet := make([]byte, 1500)

	for {
		n, err := f.iface.Read(packet)
		if err != nil || n == 0 {
			continue
		}
		b := packet[:n]

		var dc *webrtc.DataChannel
		// select a channel from pool
		if len(f.datachannels) == 0 {
			println("channel not ready yet, not relaying")
			continue
		}

		index, _ := rand.Int(rand.Reader, big.NewInt(int64(len(f.datachannels))))
		dc = f.datachannels[index.Int64()]

		if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
			println("channel not ready yet, not relaying")
			continue
		}

		srcAddr, dstAddr := netutil.GetAddr(b)
		if waterutil.IsIPv4(b) && srcAddr != "" && dstAddr != "" {
			fmt.Printf("relaying packet: %s -> %s: %d bytes\n", srcAddr, dstAddr, len(b))
		} else {
			fmt.Printf("[non-IP] relaying packet: %d bytes\n", len(b))
		}

		b = cipher.XOR(b)
		dc.Send(b)
	}
}

type httpReq struct {
	SDP string `json:"sdp"`
}

type httpResp struct {
	Answer    string   `json:"answer"`
	GatewayIP string   `json:"gateway_ip"`
	PublicIPs []string `json:"public_ips"`
}

func (s *rtcServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	clientIP, err := s.ipAllocator.AllocateNext()
	if err != nil {
		w.WriteHeader(http.StatusFailedDependency)
	}

	var req httpReq
	json.NewDecoder(r.Body).Decode(&req)

	log.Printf("Received new request, SDP: %s", req.SDP)

	forwarder := NewRTCForwarder(s.iface, clientIP, s.gatewayIP, s.config)
	answer, err := forwarder.handleICE(req.SDP)
	if err != nil {
		panic(err)
	}

	resp := httpResp{
		Answer: answer,
	}

	json.NewEncoder(w).Encode(resp)
}

func (s *rtcServer) Start(ctx context.Context) {
	// Allocate first IP from the range for gateway address
	s.gatewayIP, _ = s.ipAllocator.AllocateNext()
	s.iface = vpn.CreateServerVpn(s.config.CIDR, s.gatewayIP)

	fmt.Printf("govpn webrtc server started on %v,CIDR is %v", s.config.LocalAddr, s.config.CIDR)
	srv := http.Server{
		Addr:    s.config.LocalAddr,
		Handler: s,
	}
	go srv.ListenAndServe()
	<-ctx.Done()
	srv.Shutdown(ctx)
}
