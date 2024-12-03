package loadtest

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/ice/v3"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/stats"
	"github.com/pion/webrtc/v4"

	internalwebrtc "broadcast-box/internal/webrtc"
)

//go test -run TestClientStream

// How often to print WebRTC stats
const statsInterval = time.Second * 5

var (
	apiWhep *webrtc.API
	//sockets []*websocket.Conn
)

type (
	socketConstraints struct {
		socket *websocket.Conn
		mu     sync.Mutex
	}

	WebsocketMessageSDP struct {
		MessageType string                    `json:"messageType"`
		Sdp         webrtc.SessionDescription `json:"sdp"`
	}

	WebsocketMessageCandidate struct {
		MessageType string               `json:"messageType"`
		Candidate   *webrtc.ICECandidate `json:"candidate"`
	}

	WHEPMessage struct {
		Change string                    `json:"change"`
		Offer  webrtc.SessionDescription `json:"offer"`
	}

	client struct {
		statsGetter    stats.Getter
		audioTrack     *webrtc.TrackRemote
		videoTracks    map[string]*webrtc.TrackRemote
		peerConnection *webrtc.PeerConnection
	}
)

func TestClientLoad(t *testing.T) {
	numberOfClients := 4

	fmt.Printf("Starting test with %d client(s)\n", numberOfClients)

	mediaEngine := &webrtc.MediaEngine{}
	if err := internalwebrtc.PopulateMediaEngine(mediaEngine); err != nil {
		panic(err)
	}

	//if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
	//	panic(err)
	//}

	interceptorRegistry := &interceptor.Registry{}

	statsInterceptorFactory, err := stats.NewInterceptor()
	if err != nil {
		panic(err)
	}

	clients := make(map[int]client, numberOfClients)

	i := 0
	statsInterceptorFactory.OnNewPeerConnection(func(ids string, g stats.Getter) {
		clients[i] = client{
			statsGetter: g,
		}
		i++
	})
	interceptorRegistry.Add(statsInterceptorFactory)

	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		log.Fatal(err)
	}

	udpMuxCache := map[int]*ice.MultiUDPMuxDefault{}
	tcpMuxCache := map[string]ice.TCPMux{}

	apiWhep = webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
		webrtc.WithSettingEngine(internalwebrtc.CreateSettingEngine(true, udpMuxCache, tcpMuxCache)),
	)

	var wg sync.WaitGroup
	wg.Add(numberOfClients)

	for i := 0; i < numberOfClients; i++ {
		var wgInside sync.WaitGroup
		wgInside.Add(1)
		go func(i int) {
			socket, _, err := websocket.DefaultDialer.Dial("wss://localhost:8080/api/whep/connect?streamKey=stream", nil)
			if err != nil {
				log.Fatal("dial:", err)
			}

			socketConstraints := &socketConstraints{
				socket: socket,
			}

			peerConnection, err := internalwebrtc.NewPeerConnection(apiWhep)
			if err != nil {
				println(err)
			}

			if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
				println(err)
			}

			if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
				println(err)
			}

			peerConnection.OnTrack(func(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
				//fmt.Printf("New incoming track with codec for client %d: %s\n", i+1, track.Codec().MimeType)

				if track.Codec().MimeType == webrtc.MimeTypeOpus {
					if entry, ok := clients[i]; ok {
						entry.audioTrack = track
						clients[i] = entry
					}
				} else {
					if entry, ok := clients[i]; ok {
						if entry.videoTracks == nil {
							entry.videoTracks = make(map[string]*webrtc.TrackRemote)
						}
						entry.videoTracks[track.ID()] = track
						clients[i] = entry
					}
				}

				if entry, ok := clients[i]; ok {
					if entry.audioTrack != nil && len(entry.videoTracks) > 0 {
						wg.Done()
						wgInside.Done()
					}
				}

				rtpBuff := make([]byte, 1500)
				for {
					_, _, readErr := track.Read(rtpBuff)
					if readErr != nil {
						panic(readErr)
					}
				}
			})

			var iceConnectionState atomic.Value
			iceConnectionState.Store(webrtc.ICEConnectionStateNew)

			peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
				//log.Printf("WHEP ICE Connection State has changed: %s\n", connectionState.String())
				if connectionState == webrtc.ICEConnectionStateFailed || connectionState == webrtc.ICEConnectionStateClosed {
					if err := peerConnection.Close(); err != nil {
						log.Println(err)
					}
				}
				if connectionState == webrtc.ICEConnectionStateConnected {
					fmt.Printf("Client %d set up\n", i+1)
				}
				iceConnectionState.Store(connectionState)
			})

			peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
				if candidate == nil {
					return
				}

				message := &webrtc.ICECandidateInit{}

				message.Candidate = candidate.ToJSON().Candidate
				message.SDPMid = candidate.ToJSON().SDPMid
				message.SDPMLineIndex = candidate.ToJSON().SDPMLineIndex
				message.UsernameFragment = candidate.ToJSON().UsernameFragment

				if err := socketConstraints.sendSocket(message); err != nil {
					log.Println(err.Error())
					if err.Error() == "websocket: close 1001 (going away)" || err.Error() == "websocket: close sent" {
						return
					} else {
						panic(err)
					}
				}
			})

			if entry, ok := clients[i]; ok {
				entry.peerConnection = peerConnection
				clients[i] = entry
			}

			doSignaling(peerConnection, socketConstraints)

			defer socket.Close()
			for {
				_, n, err := socket.ReadMessage()
				if err != nil {
					log.Println(err.Error())
					if err.Error() == "websocket: close 1001 (going away)" {
						break
					} else {
						panic(err)
					}
				}

				var (
					websocketMessageSDP       WebsocketMessageSDP
					websocketMessageCandidate WebsocketMessageCandidate
				)

				switch {
				case json.Unmarshal(n, &websocketMessageSDP) == nil && websocketMessageSDP.Sdp.SDP != "":
					//log.Println("Received whepMessage from WHEP")

					if err := peerConnection.SetRemoteDescription(websocketMessageSDP.Sdp); err != nil {
						panic(err)
					}
				case json.Unmarshal(n, &websocketMessageCandidate) == nil && websocketMessageCandidate.Candidate.ToJSON().Candidate != "":
					//log.Println("Received ICE candidate from WHEP")

					if err = peerConnection.AddICECandidate(websocketMessageCandidate.Candidate.ToJSON()); err != nil {
						panic(err)
					}

				default:
					panic("Unknown message")
				}
			}
		}(i)
		wgInside.Wait()
	}
	wg.Wait()

	for {
		time.Sleep(statsInterval)

		overallPacketsReceived := uint64(0)
		overallPacketsLost := int64(0)
		overallJitter := float64(0)
		overallBytesRecevied := uint64(0)

		for _, client := range clients {
			allPacketsClientReceived := uint64(0)
			allPacketsClientLost := int64(0)
			clientJitter := float64(0)
			clientBytesRecevied := uint64(0)

			for _, track := range client.videoTracks {
				stats := client.statsGetter.Get(uint32(track.SSRC()))

				allPacketsClientReceived = allPacketsClientReceived + stats.InboundRTPStreamStats.PacketsReceived
				allPacketsClientLost = allPacketsClientLost + stats.InboundRTPStreamStats.PacketsLost
				clientJitter = clientJitter + stats.InboundRTPStreamStats.Jitter
				clientBytesRecevied = clientBytesRecevied + stats.InboundRTPStreamStats.BytesReceived

				//fmt.Printf("Client number: %d\n", clientId)
				//fmt.Printf("Stats for: %s\n", track.Codec().MimeType)
				//fmt.Println(stats.InboundRTPStreamStats)
			}

			overallPacketsReceived = overallPacketsReceived + allPacketsClientReceived
			overallPacketsLost = overallPacketsLost + allPacketsClientLost
			overallJitter = overallJitter + clientJitter
			overallBytesRecevied = overallBytesRecevied + clientBytesRecevied
		}

		averagePacketsReceived := overallPacketsReceived / uint64(len(clients))
		averagePacketsLost := overallPacketsLost / int64(len(clients))
		averageJitter := overallJitter / float64(len(clients))
		averageBytesRecevied := overallBytesRecevied / uint64(len(clients))

		fmt.Printf("_____________________\n\n")
		fmt.Printf("Total packets of all clients on average received: %d\n", averagePacketsReceived)
		fmt.Printf("Total packets of all clients on average lost: %d\n", averagePacketsLost)
		fmt.Printf("Total jitter of all clients on average: %f\n", averageJitter)
		fmt.Printf("Total bytes of all clients on average received: %d\n", averageBytesRecevied)
		fmt.Printf("_____________________\n")
	}
}

func (sC *socketConstraints) sendSocket(data interface{}) error {
	sC.mu.Lock()
	defer sC.mu.Unlock()

	return sC.socket.WriteJSON(data)
}

func doSignaling(peerConnection *webrtc.PeerConnection, socketConstraints *socketConstraints) {
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		println(err)
	}

	if err = peerConnection.SetLocalDescription(offer); err != nil {
		println(err)
	}

	message := &WHEPMessage{
		Offer:  offer,
		Change: "create",
	}

	if err := socketConstraints.sendSocket(message); err != nil {
		panic(err)
	}
}
