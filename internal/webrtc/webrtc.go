package webrtc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/dtls/v3/pkg/crypto/elliptic"
	"github.com/pion/ice/v3"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

const (
	videoTrackLabelDefault = "default"

	videoTrackCodecH264 videoTrackCodec = iota + 1
	videoTrackCodecVP8
	videoTrackCodecVP9
	videoTrackCodecAV1
)

type (
	stream struct {
		// Does this stream have a publisher?
		// If stream was created by a WHEP request hasWHIPClient == false
		hasWHIPClient atomic.Bool

		firstSeenEpoch uint64

		sourceVideoTracksLock sync.RWMutex
		sourceVideoTracks     map[string]*sourceVideoTrack

		audioTrack           *webrtc.TrackLocalStaticRTP
		audioPacketsReceived atomic.Uint64

		pliChan chan any

		whipActiveContext       context.Context
		whipActiveContextCancel func()

		whepSessionsLock sync.RWMutex
		whepSessions     map[string]*whepSession

		whipSessionLock sync.RWMutex
		whipSession     *whipSession
	}

	sourceVideoTrack struct {
		streamId       string
		name           string //equals to track streamID
		rtpVideoTracks []*rtpVideoTrack
	}

	rtpVideoTrack struct {
		rid              string
		packetsReceived  atomic.Uint64
		lastKeyFrameSeen atomic.Value
	}

	videoTrackCodec int

	socketConstraints struct {
		socket *websocket.Conn
		mu     sync.Mutex
	}
)

var (
	streamMap        map[string]*stream
	streamMapLock    sync.Mutex
	apiWhip, apiWhep *webrtc.API

	// nolint
	videoRTCPFeedback = []webrtc.RTCPFeedback{{Type: "goog-remb", Parameter: ""}, {Type: "ccm", Parameter: "fir"}, {Type: "nack", Parameter: ""}, {Type: "nack", Parameter: "pli"}}
)

type WebsocketMessage struct {
	MessageType string                    `json:"messageType"`
	Sdp         webrtc.SessionDescription `json:"sdp"`
	Candidate   *webrtc.ICECandidate      `json:"candidate"`
	Change      string                    `json:"change"`
}

//type customLogger struct{}
//
//func (c customLogger) Trace(string)                  {}
//func (c customLogger) Tracef(string, ...interface{}) {}
//
//func (c customLogger) Debug(msg string) { fmt.Printf("customLogger Debug: %s\n", msg) }
//func (c customLogger) Debugf(format string, args ...interface{}) {
//	c.Debug(fmt.Sprintf(format, args...))
//}
//func (c customLogger) Info(msg string) { fmt.Printf("customLogger Info: %s\n", msg) }
//func (c customLogger) Infof(format string, args ...interface{}) {
//	c.Trace(fmt.Sprintf(format, args...))
//}
//func (c customLogger) Warn(msg string) { fmt.Printf("customLogger Warn: %s\n", msg) }
//func (c customLogger) Warnf(format string, args ...interface{}) {
//	c.Warn(fmt.Sprintf(format, args...))
//}
//func (c customLogger) Error(msg string) { fmt.Printf("customLogger Error: %s\n", msg) }
//func (c customLogger) Errorf(format string, args ...interface{}) {
//	c.Error(fmt.Sprintf(format, args...))
//}
//
//type customLoggerFactory struct{}
//
//func (c customLoggerFactory) NewLogger(subsystem string) logging.LeveledLogger {
//	fmt.Printf("Creating logger for %s \n", subsystem)
//	return customLogger{}
//}

func doSignaling(peerConnection *webrtc.PeerConnection, offer webrtc.SessionDescription, socketConstraints *socketConstraints) {
	if err := peerConnection.SetRemoteDescription(offer); err != nil {
		panic(err)
	}

	maybePrintOfferAnswer(appendAnswer(peerConnection.RemoteDescription().SDP), true)

	answer, answerErr := peerConnection.CreateAnswer(nil)
	if answerErr != nil {
		panic(answerErr)
	}

	if err := peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}

	maybePrintOfferAnswer(appendAnswer(peerConnection.LocalDescription().SDP), false)

	message := &WebsocketMessage{}

	message.MessageType = "sdp"
	message.Sdp = answer

	if err := socketConstraints.sendSocket(message); err != nil {
		panic(err)
	}
}

func getVideoTrackCodec(in string) videoTrackCodec {
	downcased := strings.ToLower(in)
	switch {
	case strings.Contains(downcased, strings.ToLower(webrtc.MimeTypeH264)):
		return videoTrackCodecH264
	case strings.Contains(downcased, strings.ToLower(webrtc.MimeTypeVP8)):
		return videoTrackCodecVP8
	case strings.Contains(downcased, strings.ToLower(webrtc.MimeTypeVP9)):
		return videoTrackCodecVP9
	case strings.Contains(downcased, strings.ToLower(webrtc.MimeTypeAV1)):
		return videoTrackCodecAV1
	}

	return 0
}

func getStream(streamKey string, forWHIP bool) (*stream, error) {
	foundStream, ok := streamMap[streamKey]
	if !ok {
		audioTrack, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
		if err != nil {
			return nil, err
		}

		whipActiveContext, whipActiveContextCancel := context.WithCancel(context.Background())

		foundStream = &stream{
			audioTrack:              audioTrack,
			pliChan:                 make(chan any, 50),
			whepSessions:            map[string]*whepSession{},
			whipActiveContext:       whipActiveContext,
			whipActiveContextCancel: whipActiveContextCancel,
			firstSeenEpoch:          uint64(time.Now().Unix()),
			sourceVideoTracks:       map[string]*sourceVideoTrack{},
		}
		streamMap[streamKey] = foundStream
	}

	if forWHIP {
		foundStream.hasWHIPClient.Store(true)
	}

	return foundStream, nil
}

func peerConnectionDisconnected(streamKey string, whepSessionId string) {
	streamMapLock.Lock()
	defer streamMapLock.Unlock()

	stream, ok := streamMap[streamKey]
	if !ok {
		return
	}

	stream.whepSessionsLock.Lock()
	defer stream.whepSessionsLock.Unlock()

	if whepSessionId != "" {
		delete(stream.whepSessions, whepSessionId)
	} else {
		stream.hasWHIPClient.Store(false)
		for _, track := range stream.sourceVideoTracks {
			track.rtpVideoTracks = nil
		}
	}

	// Only delete stream if all WHEP Sessions are gone and have no WHIP Client
	if len(stream.whepSessions) != 0 || stream.hasWHIPClient.Load() {
		return
	}

	stream.whipActiveContextCancel()
	delete(streamMap, streamKey)
}

func addSourceTrack(stream *stream, name string, streamId string) (*sourceVideoTrack, error) {
	streamMapLock.Lock()
	defer streamMapLock.Unlock()

	for _, track := range stream.sourceVideoTracks {
		if streamId == track.streamId {
			return track, nil
		}
	}

	t := &sourceVideoTrack{name: name, streamId: streamId}
	stream.sourceVideoTracks[name] = t
	return t, nil
}

func addRIDTrack(sourceVideoTrack *sourceVideoTrack, rid string) (*rtpVideoTrack, error) {
	streamMapLock.Lock()
	defer streamMapLock.Unlock()

	for _, track := range sourceVideoTrack.rtpVideoTracks {
		if rid == track.rid {
			return track, nil
		}
	}

	t := &rtpVideoTrack{rid: rid}
	t.lastKeyFrameSeen.Store(time.Time{})
	sourceVideoTrack.rtpVideoTracks = append(sourceVideoTrack.rtpVideoTracks, t)
	return t, nil
}

func getPublicIP() string {
	log.Println("getPublicIP")
	req, err := http.Get("http://ip-api.com/json/")
	if err != nil {
		log.Fatal(err)
	}
	defer req.Body.Close()

	log.Println("getPublicIP", req)

	body, err := io.ReadAll(req.Body)
	if err != nil {
		log.Fatal(err)
	}

	ip := struct {
		Query string
	}{}
	if err = json.Unmarshal(body, &ip); err != nil {
		log.Fatal(err)
	}

	if ip.Query == "" {
		log.Fatal("Query entry was not populated")
	}

	return ip.Query
}

func CreateSettingEngine(isWHIP bool, udpMuxCache map[int]*ice.MultiUDPMuxDefault, tcpMuxCache map[string]ice.TCPMux) (settingEngine webrtc.SettingEngine) {
	var (
		NAT1To1IPs []string
		udpMuxPort int
		udpMuxOpts []ice.UDPMuxFromPortOption
		err        error
	)
	networkTypes := []webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6}

	if os.Getenv("INCLUDE_PUBLIC_IP_IN_NAT_1_TO_1_IP") != "" {
		NAT1To1IPs = append(NAT1To1IPs, getPublicIP())
	}

	if os.Getenv("NAT_1_TO_1_IP") != "" {
		NAT1To1IPs = append(NAT1To1IPs, strings.Split(os.Getenv("NAT_1_TO_1_IP"), "|")...)
	}

	natICECandidateType := webrtc.ICECandidateTypeHost
	if os.Getenv("NAT_ICE_CANDIDATE_TYPE") == "srflx" {
		natICECandidateType = webrtc.ICECandidateTypeSrflx
	}

	if len(NAT1To1IPs) != 0 {
		settingEngine.SetNAT1To1IPs(NAT1To1IPs, natICECandidateType)
	}

	if os.Getenv("INTERFACE_FILTER") != "" {
		interfaceFilter := func(i string) bool {
			return i == os.Getenv("INTERFACE_FILTER")
		}

		settingEngine.SetInterfaceFilter(interfaceFilter)
		udpMuxOpts = append(udpMuxOpts, ice.UDPMuxFromPortWithInterfaceFilter(interfaceFilter))
	}

	if isWHIP && os.Getenv("UDP_MUX_PORT_WHIP") != "" {
		if udpMuxPort, err = strconv.Atoi(os.Getenv("UDP_MUX_PORT_WHIP")); err != nil {
			log.Fatal(err)
		}
	} else if !isWHIP && os.Getenv("UDP_MUX_PORT_WHEP") != "" {
		if udpMuxPort, err = strconv.Atoi(os.Getenv("UDP_MUX_PORT_WHEP")); err != nil {
			log.Fatal(err)
		}
	} else if os.Getenv("UDP_MUX_PORT") != "" {
		if udpMuxPort, err = strconv.Atoi(os.Getenv("UDP_MUX_PORT")); err != nil {
			log.Fatal(err)
		}
	}

	if udpMuxPort != 0 {
		udpMux, ok := udpMuxCache[udpMuxPort]
		if !ok {
			if udpMux, err = ice.NewMultiUDPMuxFromPort(udpMuxPort, udpMuxOpts...); err != nil {
				log.Fatal(err)
			}
			udpMuxCache[udpMuxPort] = udpMux
		}

		settingEngine.SetICEUDPMux(udpMux)
	}

	if os.Getenv("TCP_MUX_ADDRESS") != "" {
		tcpMux, ok := tcpMuxCache[os.Getenv("TCP_MUX_ADDRESS")]
		if !ok {
			tcpAddr, err := net.ResolveTCPAddr("tcp", os.Getenv("TCP_MUX_ADDRESS"))
			if err != nil {
				log.Fatal(err)
			}

			tcpListener, err := net.ListenTCP("tcp", tcpAddr)
			if err != nil {
				log.Fatal(err)
			}

			tcpMux = webrtc.NewICETCPMux(nil, tcpListener, 8)
			tcpMuxCache[os.Getenv("TCP_MUX_ADDRESS")] = tcpMux
		}
		settingEngine.SetICETCPMux(tcpMux)

		if os.Getenv("TCP_MUX_FORCE") != "" {
			networkTypes = []webrtc.NetworkType{webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6}
		} else {
			networkTypes = append(networkTypes, webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6)
		}
	}

	settingEngine.SetDTLSEllipticCurves(elliptic.X25519, elliptic.P384, elliptic.P256)
	settingEngine.SetNetworkTypes(networkTypes)
	settingEngine.DisableSRTCPReplayProtection(true)
	settingEngine.DisableSRTPReplayProtection(true)
	settingEngine.SetIncludeLoopbackCandidate(os.Getenv("INCLUDE_LOOPBACK_CANDIDATE") != "")

	return
}

func PopulateMediaEngine(m *webrtc.MediaEngine) error {
	for _, codec := range []webrtc.RTPCodecParameters{
		{
			// nolint
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1", RTCPFeedback: nil},
			PayloadType:        111,
		},
	} {
		if err := m.RegisterCodec(codec, webrtc.RTPCodecTypeAudio); err != nil {
			return err
		}
	}

	for _, codecDetails := range []struct {
		payloadType uint8
		mimeType    string
		sdpFmtpLine string
	}{
		{102, webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f"},
		{104, webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42001f"},
		{106, webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
		{108, webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f"},
		{39, webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=4d001f"},
		{45, webrtc.MimeTypeAV1, ""},
		{98, webrtc.MimeTypeVP9, "profile-id=0"},
		{100, webrtc.MimeTypeVP9, "profile-id=2"},
	} {
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     codecDetails.mimeType,
				ClockRate:    90000,
				Channels:     0,
				SDPFmtpLine:  codecDetails.sdpFmtpLine,
				RTCPFeedback: videoRTCPFeedback,
			},
			PayloadType: webrtc.PayloadType(codecDetails.payloadType),
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}

		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     "video/rtx",
				ClockRate:    90000,
				Channels:     0,
				SDPFmtpLine:  fmt.Sprintf("apt=%d", codecDetails.payloadType),
				RTCPFeedback: nil,
			},
			PayloadType: webrtc.PayloadType(codecDetails.payloadType + 1),
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
	}

	return nil
}

func NewPeerConnection(api *webrtc.API) (*webrtc.PeerConnection, error) {
	cfg := webrtc.Configuration{}
	log.Println("NewPeerConnection", cfg)

	cfg.ICEServers = append(cfg.ICEServers, webrtc.ICEServer{
		URLs: []string{},
		//Username:   "user-1",
		//Credential: "pass-1",
	})

	//if stunServers := os.Getenv("STUN_SERVERS"); stunServers != "" {
	//	for _, stunServer := range strings.Split(stunServers, "|") {
	//		cfg.ICEServers = append(cfg.ICEServers, webrtc.ICEServer{
	//			URLs:       []string{stunServer},
	//			Username:   "user-1",
	//			Credential: "pass-1",
	//		})
	//	}
	//}

	return api.NewPeerConnection(cfg)
}

func appendAnswer(in string) string {
	if extraCandidate := os.Getenv("APPEND_CANDIDATE"); extraCandidate != "" {
		index := strings.Index(in, "a=end-of-candidates")
		in = in[:index] + extraCandidate + in[index:]
	}

	return in
}

func maybePrintOfferAnswer(sdp string, isOffer bool) string {
	if os.Getenv("DEBUG_PRINT_OFFER") != "" && isOffer {
		fmt.Println(sdp)
	}

	if os.Getenv("DEBUG_PRINT_ANSWER") != "" && !isOffer {
		fmt.Println(sdp)
	}

	return sdp
}

func Configure() {
	streamMap = map[string]*stream{}

	mediaEngine := &webrtc.MediaEngine{}
	if err := PopulateMediaEngine(mediaEngine); err != nil {
		panic(err)
	}

	interceptorRegistry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		log.Fatal(err)
	}

	udpMuxCache := map[int]*ice.MultiUDPMuxDefault{}
	tcpMuxCache := map[string]ice.TCPMux{}

	apiWhip = webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
		webrtc.WithSettingEngine(CreateSettingEngine(true, udpMuxCache, tcpMuxCache)),
	)

	apiWhep = webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
		webrtc.WithSettingEngine(CreateSettingEngine(false, udpMuxCache, tcpMuxCache)),
	)
}

type SourceStreamStatusVideo struct {
	ID              string                 `json:"id"`
	RTPVideoStreams []RTPStreamStatusVideo `json:"rtpVideoStreams"`
}

type RTPStreamStatusVideo struct {
	RID              string    `json:"rid"`
	PacketsReceived  uint64    `json:"packetsReceived"`
	LastKeyFrameSeen time.Time `json:"lastKeyFrameSeen"`
}

type StreamStatus struct {
	StreamKey            string                    `json:"streamKey"`
	FirstSeenEpoch       uint64                    `json:"firstSeenEpoch"`
	AudioPacketsReceived uint64                    `json:"audioPacketsReceived"`
	SourceVideoStreams   []SourceStreamStatusVideo `json:"sourceVideoStreams"`
	WHEPSessions         []whepSessionStatus       `json:"whepSessions"`
}

type whepSessionStatus struct {
	ID             string              `json:"id"`
	CurrentLayer   string              `json:"currentLayer"`
	VideoOutputs   []videoOutputStatus `json:"videoOutputs"`
	SequenceNumber uint16              `json:"sequenceNumber"`
	Timestamp      uint32              `json:"timestamp"`
	PacketsWritten uint64              `json:"packetsWritten"`
}

type videoOutputStatus struct {
	SequenceNumber uint16 `json:"sequenceNumber"`
	Timestamp      uint32 `json:"timestamp"`
	PacketsWritten uint64 `json:"packetsWritten"`
}

func GetStreamStatuses() []StreamStatus {
	streamMapLock.Lock()
	defer streamMapLock.Unlock()

	out := []StreamStatus{}

	for streamKey, stream := range streamMap {
		whepSessions := []whepSessionStatus{}
		stream.whepSessionsLock.Lock()
		for id, whepSession := range stream.whepSessions {
			currentLayer, ok := whepSession.currentLayer.Load().(string)
			if !ok {
				continue
			}

			videoOutputs := []videoOutputStatus{}
			for _, videoOutput := range whepSession.videoOutputs {
				videoOutputs = append(videoOutputs, videoOutputStatus{
					SequenceNumber: videoOutput.sequenceNumber,
					Timestamp:      videoOutput.timestamp,
					PacketsWritten: videoOutput.packetsWritten,
				})
			}

			whepSessions = append(whepSessions, whepSessionStatus{
				ID:             id,
				CurrentLayer:   currentLayer,
				VideoOutputs:   videoOutputs,
				SequenceNumber: whepSession.sequenceNumber,
				Timestamp:      whepSession.timestamp,
				PacketsWritten: whepSession.packetsWritten,
			})
		}
		stream.whepSessionsLock.Unlock()

		stream.sourceVideoTracksLock.Lock()
		sourceStreamStatusVideo := []SourceStreamStatusVideo{}
		for _, sourceVideoTrack := range stream.sourceVideoTracks {

			rtpVideoStreams := []RTPStreamStatusVideo{}
			for _, videoTrack := range sourceVideoTrack.rtpVideoTracks {
				var lastKeyFrameSeen time.Time
				if v, ok := videoTrack.lastKeyFrameSeen.Load().(time.Time); ok {
					lastKeyFrameSeen = v
				}

				rtpVideoStreams = append(rtpVideoStreams, RTPStreamStatusVideo{
					RID:              videoTrack.rid,
					PacketsReceived:  videoTrack.packetsReceived.Load(),
					LastKeyFrameSeen: lastKeyFrameSeen,
				})
			}

			sourceStreamStatusVideo = append(sourceStreamStatusVideo, SourceStreamStatusVideo{
				ID:              sourceVideoTrack.streamId,
				RTPVideoStreams: rtpVideoStreams,
			})
		}
		stream.sourceVideoTracksLock.Unlock()

		out = append(out, StreamStatus{
			StreamKey:            streamKey,
			FirstSeenEpoch:       stream.firstSeenEpoch,
			AudioPacketsReceived: stream.audioPacketsReceived.Load(),
			SourceVideoStreams:   sourceStreamStatusVideo,
			WHEPSessions:         whepSessions,
		})
	}

	return out
}

func (sC *socketConstraints) sendSocket(data interface{}) error {
	sC.mu.Lock()
	defer sC.mu.Unlock()

	return sC.socket.WriteJSON(data)
}

//causes iceconnection to disconnect after some seconds
//func (sC *socketConstraints) readSocket() ([]byte, error) {
//	sC.mu.Lock()
//	defer sC.mu.Unlock()
//
//	_, message, err := sC.socket.ReadMessage()
//	if err != nil {
//		return nil, err
//	}
//
//	return message, nil
//}
