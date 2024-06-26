package client

import (
	"fmt"
	"io"
	"log"
	"time"

	"github.com/gorilla/websocket"
	"github.com/patrickmn/go-cache"
	"github.com/pchchv/govpn/common/cipher"
	"github.com/pchchv/govpn/common/config"
	"github.com/pchchv/govpn/common/netutil"
	"github.com/pchchv/govpn/vpn"
	"github.com/songgao/water"
	"github.com/songgao/water/waterutil"
)

func StartWSClient(config config.Config) {
	iface := vpn.CreateClientVpn(config.CIDR)
	c := cache.New(30*time.Minute, 10*time.Minute)
	packet := make([]byte, 1500)

	log.Printf("govpn ws client started,CIDR is %v", config.CIDR)

	for {
		n, err := iface.Read(packet)
		if err != nil || n == 0 {
			continue
		}

		b := packet[:n]
		if !waterutil.IsIPv4(b) {
			continue
		}

		srcAddr, dstAddr := netutil.GetAddr(b)
		if srcAddr == "" || dstAddr == "" {
			continue
		}

		var conn *websocket.Conn
		key := fmt.Sprintf("%v->%v", dstAddr, srcAddr)
		v, ok := c.Get(key)
		if ok {
			conn = v.(*websocket.Conn)
		} else {
			conn = netutil.ConnectWS(config)
			if conn == nil {
				continue
			}
			c.Set(key, conn, cache.DefaultExpiration)
			go wsToVpn(c, key, conn, iface)
		}

		b = cipher.XOR(b)
		conn.WriteMessage(websocket.BinaryMessage, b)
	}
}

func wsToVpn(c *cache.Cache, key string, wsConn *websocket.Conn, iface *water.Interface) {
	defer netutil.CloseWS(wsConn)

	for {
		wsConn.SetReadDeadline(time.Now().Add(time.Duration(30) * time.Second))
		_, b, err := wsConn.ReadMessage()
		if err != nil || err == io.EOF {
			break
		}

		b = cipher.XOR(b)
		if !waterutil.IsIPv4(b) {
			continue
		}

		iface.Write(b[:])
	}

	c.Delete(key)
}
