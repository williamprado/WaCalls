package main

import (
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"

	"wacalls/internal/voip/media"

	"github.com/pion/webrtc/v4"
)

// pcmChannelLabel is the data channel the browser opens to carry raw 16 kHz mono
// Int16 LE PCM in both directions. The browser side must create it with this label.
const pcmChannelLabel = "pcm"

// browserAPI is built once and shared by every browser-leg PeerConnection.
// When WACALLS_PUBLIC_IP and WACALLS_UDP_PORT are set, the browser leg binds a
// single fixed port (UDP + ICE-TCP on the same port) via a shared mux and
// advertises the public IP as a host candidate (NAT 1:1). ICE-TCP keeps audio
// working when inbound UDP is blocked (common on cloud hosts). Without these
// envs it falls back to pion defaults (ephemeral ports) — fine for LAN/local.
var (
	browserAPIOnce sync.Once
	browserAPI     *webrtc.API
)

// detectOutboundIP finds the default-route source IP without sending packets.
// On a host-network container whose interface holds the public IP, this returns
// that public IP.
func detectOutboundIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}

func getBrowserAPI(log *slog.Logger) *webrtc.API {
	browserAPIOnce.Do(func() {
		publicIP := os.Getenv("WACALLS_PUBLIC_IP")
		udpPort, _ := strconv.Atoi(os.Getenv("WACALLS_UDP_PORT"))
		if publicIP == "auto" {
			if ip := detectOutboundIP(); ip != "" {
				publicIP = ip
				log.Info("browser webrtc: auto-detected public IP", "ip", publicIP)
			} else {
				log.Warn("browser webrtc: could not auto-detect public IP")
			}
		}
		if publicIP == "" || udpPort == 0 {
			log.Warn("browser webrtc: WACALLS_PUBLIC_IP/UDP_PORT not set — LAN/localhost only")
			browserAPI = webrtc.NewAPI()
			return
		}
		se := webrtc.SettingEngine{}
		se.SetNAT1To1IPs([]string{publicIP}, webrtc.ICECandidateTypeHost)
		se.SetNetworkTypes([]webrtc.NetworkType{
			webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6,
			webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6,
		})
		udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: udpPort})
		if err != nil {
			log.Error("browser webrtc: udp mux bind failed; using ephemeral ports", "port", udpPort, "err", err)
			browserAPI = webrtc.NewAPI()
			return
		}
		se.SetICEUDPMux(webrtc.NewICEUDPMux(nil, udpConn))
		log.Info("browser webrtc: fixed udp port + nat1to1 enabled", "public_ip", publicIP, "udp_port", udpPort)
		if tcpListener, terr := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4zero, Port: udpPort}); terr == nil {
			se.SetICETCPMux(webrtc.NewICETCPMux(nil, tcpListener, 8))
			log.Info("browser webrtc: ice-tcp fallback enabled", "tcp_port", udpPort)
		} else {
			log.Error("browser webrtc: ice-tcp bind failed", "port", udpPort, "err", terr)
		}
		browserAPI = webrtc.NewAPI(webrtc.WithSettingEngine(se))
	})
	return browserAPI
}

// Bridge is the browser-leg adapter: it carries raw PCM between the browser and
// the CallManager over a WebRTC data channel. The call core only ever sees
// []float32 PCM, so it stays unaware of the transport (no Opus here anymore).
type Bridge struct {
	pc  *webrtc.PeerConnection
	dc  atomic.Pointer[webrtc.DataChannel]
	log *slog.Logger

	// OnBrowserPCM is invoked with decoded 16 kHz mono PCM captured from the browser mic.
	OnBrowserPCM func(pcm []float32)
	// OnTerminalICE fires when the peer connection fails or closes.
	OnTerminalICE func()
}

func NewBridge(offerSDP string, log *slog.Logger) (*Bridge, string, error) {
	pc, err := getBrowserAPI(log).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, "", err
	}
	br := &Bridge{pc: pc, log: log}

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != pcmChannelLabel {
			return
		}
		br.dc.Store(dc)
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if cb := br.OnBrowserPCM; cb != nil && len(msg.Data) > 0 {
				cb(media.PCMInt16LEToFloat32(msg.Data))
			}
		})
	})

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		log.Debug("browser ice state", "state", s.String())
		if s == webrtc.ICEConnectionStateFailed || s == webrtc.ICEConnectionStateClosed {
			if br.OnTerminalICE != nil {
				br.OnTerminalICE()
			}
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		pc.Close()
		return nil, "", err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		return nil, "", err
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		return nil, "", err
	}
	<-gatherComplete

	return br, pc.LocalDescription().SDP, nil
}

// WritePCM sends 16 kHz mono float32 PCM to the browser as Int16 LE over the data
// channel. It is a no-op until the channel is open.
func (b *Bridge) WritePCM(pcm []float32) error {
	dc := b.dc.Load()
	if dc == nil || len(pcm) == 0 {
		return nil
	}
	return dc.Send(media.PCMFloat32ToInt16LE(pcm))
}

func (b *Bridge) Close() {
	if b.pc != nil {
		_ = b.pc.Close()
	}
}
