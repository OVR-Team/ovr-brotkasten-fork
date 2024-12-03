package webrtc

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"sort"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type (
	whepSession struct {
		videoOutputs map[string]*videoOutput
		currentLayer atomic.Value
		//dataChannel    *webrtc.DataChannel
		peerConnection     *webrtc.PeerConnection
		waitingForKeyframe atomic.Bool
		socketConstraints  *socketConstraints
		newVideoTrack      *trackMultiCodec
		sequenceNumber     uint16
		timestamp          uint32
		packetsWritten     uint64
	}

	videoOutput struct {
		videoTrack     *trackMultiCodec
		sequenceNumber uint16
		timestamp      uint32
		packetsWritten uint64
	}

	simulcastLayerResponse struct {
		EncodingId string `json:"encodingId"`
	}
)

type WHEPMessage struct {
	Change string                    `json:"change"`
	Offer  webrtc.SessionDescription `json:"offer"`
}

func WHEPLayers(whepSessionId string) ([]byte, error) {
	streamMapLock.Lock()
	defer streamMapLock.Unlock()

	layers := []simulcastLayerResponse{}
	for streamKey := range streamMap {
		streamMap[streamKey].whepSessionsLock.Lock()
		defer streamMap[streamKey].whepSessionsLock.Unlock()

		if _, ok := streamMap[streamKey].whepSessions[whepSessionId]; ok {
			for i := range streamMap[streamKey].sourceVideoTracks {
				for j := range streamMap[streamKey].sourceVideoTracks[i].rtpVideoTracks {
					layers = append(layers, simulcastLayerResponse{EncodingId: streamMap[streamKey].sourceVideoTracks[i].rtpVideoTracks[j].rid})
				}
				break
			}
			break
		}
	}

	resp := map[string]map[string][]simulcastLayerResponse{
		"1": {
			"layers": layers,
		},
	}

	return json.Marshal(resp)
}

func WHEPChangeLayer(whepSessionId, layer string) error {
	streamMapLock.Lock()
	defer streamMapLock.Unlock()

	for streamKey := range streamMap {
		streamMap[streamKey].whepSessionsLock.Lock()
		defer streamMap[streamKey].whepSessionsLock.Unlock()

		if _, ok := streamMap[streamKey].whepSessions[whepSessionId]; ok {
			streamMap[streamKey].whepSessions[whepSessionId].currentLayer.Store(layer)
			streamMap[streamKey].whepSessions[whepSessionId].waitingForKeyframe.Store(true)
			streamMap[streamKey].pliChan <- true
		}
	}

	return nil
}

func WHEPCreate(streamKey string, socket *websocket.Conn) {
	streamMapLock.Lock()
	stream, err := getStream(streamKey, false)
	if err != nil {
		log.Println(err)
	}
	streamMapLock.Unlock()

	whepSessionId := uuid.New().String()

	whepPeerConnection, err := NewPeerConnection(apiWhep)
	if err != nil {
		log.Println(err)
	}

	if err := WHEPInitSession(stream, whepSessionId, whepPeerConnection, socket); err != nil {
		log.Println(err)
	}

	videoTracks := []*trackMultiCodec{}
	keys := make([]string, 0, len(stream.sourceVideoTracks))

	for key := range stream.sourceVideoTracks {
		keys = append(keys, key)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		return stream.sourceVideoTracks[keys[i]].name < stream.sourceVideoTracks[keys[j]].name
	})

	for _, k := range keys {
		videoTracks = append(videoTracks, &trackMultiCodec{id: stream.sourceVideoTracks[k].streamId, streamID: stream.sourceVideoTracks[k].name})
	}

	if _, err := whepPeerConnection.AddTrack(stream.audioTrack); err != nil {
		log.Println(err)
	}

	for _, track := range videoTracks {
		log.Println("WHEPCreate videoTracks: ", track.id, track.streamID, track)
		WHEPInitTrack(stream, track, whepSessionId)
	}

	whepPeerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		log.Printf("WHEP ICE Connection State has changed: %s\n", connectionState.String())
		if connectionState == webrtc.ICEConnectionStateFailed || connectionState == webrtc.ICEConnectionStateClosed {
			if err := whepPeerConnection.Close(); err != nil {
				log.Println(err)
			}

			peerConnectionDisconnected(streamKey, whepSessionId)
		} else if connectionState == webrtc.ICEConnectionStateConnected {
			if err := WHEPAddTracksToSession(stream, whepSessionId, videoTracks); err != nil {
				log.Println(err)
			}
		}
	})

	whepPeerConnection.OnSignalingStateChange(func(state webrtc.SignalingState) {
		log.Printf("WHEP Signaling State has changed: %s\n", state.String())
		if state == webrtc.SignalingStateStable && stream.whepSessions[whepSessionId] != nil && stream.whepSessions[whepSessionId].newVideoTrack != nil {
			log.Println("WHEP Signaling state stable")
			videoTrack := stream.whepSessions[whepSessionId].newVideoTrack
			if err := WHEPAddTracksToSession(stream, whepSessionId, []*trackMultiCodec{videoTrack}); err != nil {
				log.Println(err)
			}
			stream.whepSessions[whepSessionId].newVideoTrack = nil
		}
	})

	whepPeerConnection.OnNegotiationNeeded(func() {
		log.Println("WHEP OnNegotiationNeeded")
	})

	whepPeerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		log.Println("OnICECandidate:", candidate.ToJSON())

		message := &WebsocketMessage{}

		message.MessageType = "candidate"
		message.Candidate = candidate

		//log.Println("OnICECandidate: before sendSocket message")
		if err := stream.whepSessions[whepSessionId].socketConstraints.sendSocket(message); err != nil {
			log.Println(err.Error())
			if err.Error() == "websocket: close 1001 (going away)" || err.Error() == "websocket: close sent" {
				return
			} else {
				panic(err)
			}
		}
		//log.Println("OnICECandidate: after sendSocket message")
	})

	//whepPeerConnection.OnDataChannel(func(channel *webrtc.DataChannel) {
	//	log.Printf("New DataChannel %s %d\n", channel.Label(), channel.ID())
	//
	//	channel.OnOpen(func() {
	//		log.Printf("sendChannel has opened")
	//		stream.whepSessions[whepSessionId].dataChannel = channel
	//	})
	//
	//	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
	//		log.Printf("Message from DataChannel '%s': '%s'\n", channel.Label(), string(msg.Data))
	//	})
	//})

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
			candidate   webrtc.ICECandidateInit
			whepMessage WHEPMessage
		)

		switch {
		case json.Unmarshal(n, &whepMessage) == nil && whepMessage.Offer.SDP != "" && whepMessage.Change != "":
			log.Println("Received whepMessage from WHEP")

			switch {
			case whepMessage.Change == "create":
				log.Println("Creating WHEP session")
			case whepMessage.Change == "addTrack":
				if err := WHEPAddTrackToPeerConnection(stream, whepSessionId); err != nil {
					log.Println(err)
				}
			case whepMessage.Change == "removeTrack":
				if err := WHEPRemoveTrack(stream, whepSessionId); err != nil {
					log.Println(err)
				}
			default:
				panic("WHEP message - Unknown message")
			}
			log.Println("SocketConstraints:", stream.whepSessions[whepSessionId].socketConstraints)
			doSignaling(whepPeerConnection, whepMessage.Offer, stream.whepSessions[whepSessionId].socketConstraints)

		case json.Unmarshal(n, &candidate) == nil && candidate.Candidate != "":
			log.Println("Received ICE candidate from WHEP")
			if err = whepPeerConnection.AddICECandidate(candidate); err != nil {
				panic(err)
			}

		default:
			panic("Unknown message")
		}
	}
}

func WHEPAddTracksToSession(stream *stream, whepSessionId string, videoTracks []*trackMultiCodec) error {
	stream.whepSessionsLock.Lock()
	defer stream.whepSessionsLock.Unlock()

	for _, track := range videoTracks {
		log.Println("WHEPAddTracksToSession: ", track.id, track.streamID)
		stream.whepSessions[whepSessionId].videoOutputs[track.streamID] = &videoOutput{
			videoTrack: track,
			timestamp:  50000,
		}
	}

	return nil
}

func WHEPInitSession(stream *stream, whepSessionId string, whepPeerConnection *webrtc.PeerConnection, socket *websocket.Conn) error {
	stream.whepSessionsLock.Lock()
	defer stream.whepSessionsLock.Unlock()

	stream.whepSessions[whepSessionId] = &whepSession{
		videoOutputs:   make(map[string]*videoOutput),
		peerConnection: whepPeerConnection,
		socketConstraints: &socketConstraints{
			socket: socket,
		},
	}

	stream.whepSessions[whepSessionId].currentLayer.Store("")
	stream.whepSessions[whepSessionId].waitingForKeyframe.Store(false)

	return nil
}

func WHEPAddTrackToPeerConnection(stream *stream, whepSessionId string) error {
	var newSourceTrack *sourceVideoTrack
	for _, track := range stream.sourceVideoTracks {
		if !checkVideoOutputsForId(track.streamId, stream.whepSessions[whepSessionId].videoOutputs) {
			newSourceTrack = track
			break
		}
	}
	newVideoTrack := &trackMultiCodec{id: newSourceTrack.streamId, streamID: newSourceTrack.name}
	log.Println("WHEPAddTrack: track: ", newVideoTrack.id, newVideoTrack.streamID)

	WHEPInitTrack(stream, newVideoTrack, whepSessionId)
	stream.whepSessions[whepSessionId].newVideoTrack = newVideoTrack

	return nil
}

func WHEPInitTrack(stream *stream, newVideoTrack *trackMultiCodec, whepSessionId string) {
	rtpSender, err := stream.whepSessions[whepSessionId].peerConnection.AddTrack(newVideoTrack)
	if err != nil {
		log.Println(err)
	}

	go func() {
		for {
			rtcpPackets, _, rtcpErr := rtpSender.ReadRTCP()
			if rtcpErr != nil {
				return
			}

			for _, r := range rtcpPackets {
				if _, isPLI := r.(*rtcp.PictureLossIndication); isPLI {
					select {
					case stream.pliChan <- true:
					default:
					}
				}
			}
		}
	}()
}

func WHEPRemoveTrack(stream *stream, whepSessionId string) error {
	if stream.whepSessions[whepSessionId] == nil {
		return errors.New("WHEPRemoveTrack: connection closed")
	}

	var videoOutputToRemove *videoOutput
	for _, output := range stream.whepSessions[whepSessionId].videoOutputs {
		if !checkSourceTracksForId(output.videoTrack.id, stream.sourceVideoTracks) {
			videoOutputToRemove = output
			break
		}
	}

	log.Println("Removing track", videoOutputToRemove.videoTrack.id, videoOutputToRemove.videoTrack.streamID)

	delete(stream.whepSessions[whepSessionId].videoOutputs, videoOutputToRemove.videoTrack.streamID)

	if senders := stream.whepSessions[whepSessionId].peerConnection.GetSenders(); len(senders) != 0 {
		for _, sender := range senders {
			if sender != nil && sender.Track() != nil && sender.Track().StreamID() == videoOutputToRemove.videoTrack.streamID {
				log.Println("sender: ", "id:", sender.Track().ID(), "streamID: ", sender.Track().StreamID())
				if err := stream.whepSessions[whepSessionId].peerConnection.RemoveTrack(sender); err != nil {
					panic(err)
				}
			}
		}
	}

	return nil
}

func (w *whepSession) sendVideoPacket(rtpPkt *rtp.Packet, layer string, timeDiff int64, sequenceDiff int, codec videoTrackCodec, name string, isKeyframe bool) {
	if w.currentLayer.Load() == "" {
		w.currentLayer.Store(layer)
	} else if layer != w.currentLayer.Load() {
		return
	} else if w.waitingForKeyframe.Load() {
		if !isKeyframe {
			return
		}

		w.waitingForKeyframe.Store(false)
	}

	if w.videoOutputs[name] == nil {
		return
	}

	w.videoOutputs[name].packetsWritten += 1
	w.videoOutputs[name].sequenceNumber = uint16(int(w.videoOutputs[name].sequenceNumber) + sequenceDiff)
	w.videoOutputs[name].timestamp += uint32(int64(w.videoOutputs[name].timestamp) + timeDiff)

	rtpPkt.SequenceNumber = w.videoOutputs[name].sequenceNumber
	rtpPkt.Timestamp = w.videoOutputs[name].timestamp

	if err := w.videoOutputs[name].videoTrack.WriteRTP(rtpPkt, codec); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		log.Println(err)
	}
}

func checkVideoOutputsForId(id string, outputs map[string]*videoOutput) bool {
	for _, output := range outputs {
		if id == output.videoTrack.id {
			return true
		}
	}
	return false
}

func checkSourceTracksForId(id string, tracks map[string]*sourceVideoTrack) bool {
	for _, track := range tracks {
		if id == track.streamId {
			return true
		}
	}
	return false
}
