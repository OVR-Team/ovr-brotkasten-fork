package webrtc

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

type TrackAssignment struct {
	StreamId string `json:"streamId"`
	Name     string `json:"name"`
}

type MediaAssigner struct {
	Assignments []TrackAssignment `json:"assignments"`
	Type        string            `json:"type"`
}

type WHIPMessage struct {
	MediaAssigner MediaAssigner             `json:"mediaAssigner"`
	Offer         webrtc.SessionDescription `json:"offer"`
}

type whipSession struct {
	//dataChannel    *webrtc.DataChannel
	peerConnection    *webrtc.PeerConnection
	socketConstraints *socketConstraints
}

var globalMediaAssigner *MediaAssigner

func audioWriter(remoteTrack *webrtc.TrackRemote, stream *stream) {
	rtpBuf := make([]byte, 1500)
	for {
		rtpRead, _, err := remoteTrack.Read(rtpBuf)
		switch {
		case errors.Is(err, io.EOF):
			return
		case err != nil:
			log.Println(err)
			return
		}

		stream.audioPacketsReceived.Add(1)
		if _, writeErr := stream.audioTrack.Write(rtpBuf[:rtpRead]); writeErr != nil && !errors.Is(writeErr, io.ErrClosedPipe) {
			log.Println(writeErr)
			return
		}
	}
}

func videoWriter(remoteTrack *webrtc.TrackRemote, stream *stream, peerConnection *webrtc.PeerConnection, s *stream, assignment TrackAssignment) {
	streamId := remoteTrack.StreamID()
	rid := remoteTrack.RID()
	streamName := assignment.Name

	log.Println("videoWriter streamId:", streamId, "rid:", rid, "streamName:", streamName)

	if rid == "" {
		rid = videoTrackLabelDefault
	}

	_, err := addSourceTrack(stream, streamName, streamId)
	if err != nil {
		log.Println(err)
		return
	}

	for _, track := range stream.sourceVideoTracks {
		log.Println("videoWriter stream.sourceVideoTracksName:", track.name, "StreamId:", track.streamId)
	}

	ridVideoTrack, err := addRIDTrack(s.sourceVideoTracks[streamName], rid)
	if err != nil {
		log.Println(err)
		return
	}

	go func() {
		for {
			select {
			case <-stream.whipActiveContext.Done():
				return
			case <-stream.pliChan:
				if sendErr := peerConnection.WriteRTCP([]rtcp.Packet{
					&rtcp.PictureLossIndication{
						MediaSSRC: uint32(remoteTrack.SSRC()),
					},
				}); sendErr != nil {
					return
				}
			}
		}
	}()

	stream.whepSessionsLock.Lock()

	message := &WebsocketMessage{}
	message.MessageType = "change"
	message.Change = "addTrack:" + streamName

	for _, whepSession := range stream.whepSessions {
		if err := whepSession.socketConstraints.sendSocket(message); err != nil {
			panic(err)
		}
	}
	stream.whepSessionsLock.Unlock()

	rtpBuf := make([]byte, 1500)
	rtpPkt := &rtp.Packet{}
	codec := getVideoTrackCodec(remoteTrack.Codec().RTPCodecCapability.MimeType)

	var depacketizer rtp.Depacketizer
	switch codec {
	case videoTrackCodecH264:
		depacketizer = &codecs.H264Packet{}
	case videoTrackCodecVP8:
		depacketizer = &codecs.VP8Packet{}
	case videoTrackCodecVP9:
		depacketizer = &codecs.VP9Packet{}
	}

	lastTimestamp := uint32(0)
	lastTimestampSet := false

	lastSequenceNumber := uint16(0)
	lastSequenceNumberSet := false

	for {
		rtpRead, _, err := remoteTrack.Read(rtpBuf)
		switch {
		case errors.Is(err, io.EOF):
			return
		case err != nil:
			log.Println(err)
			return
		}

		if err = rtpPkt.Unmarshal(rtpBuf[:rtpRead]); err != nil {
			log.Println(err)
			return
		}

		ridVideoTrack.packetsReceived.Add(1)

		// Keyframe detection has only been implemented for H264
		isKeyframe := isKeyframe(rtpPkt, codec, depacketizer)
		if isKeyframe && codec == videoTrackCodecH264 {
			ridVideoTrack.lastKeyFrameSeen.Store(time.Now())
		}

		rtpPkt.Extension = false
		rtpPkt.Extensions = nil

		timeDiff := int64(rtpPkt.Timestamp) - int64(lastTimestamp)
		switch {
		case !lastTimestampSet:
			timeDiff = 0
			lastTimestampSet = true
		case timeDiff < -(math.MaxUint32 / 10):
			timeDiff += (math.MaxUint32 + 1)
		}

		sequenceDiff := int(rtpPkt.SequenceNumber) - int(lastSequenceNumber)
		switch {
		case !lastSequenceNumberSet:
			lastSequenceNumberSet = true
			sequenceDiff = 0
		case sequenceDiff < -(math.MaxUint16 / 10):
			sequenceDiff += (math.MaxUint16 + 1)
		}

		lastTimestamp = rtpPkt.Timestamp
		lastSequenceNumber = rtpPkt.SequenceNumber

		s.whepSessionsLock.RLock()
		for i := range s.whepSessions {
			s.whepSessions[i].sendVideoPacket(rtpPkt, rid, timeDiff, sequenceDiff, codec, streamName, isKeyframe)
		}
		s.whepSessionsLock.RUnlock()

	}
}

func WHIPCreate(streamKey string, socket *websocket.Conn) {
	var err error

	streamMapLock.Lock()
	stream, err := getStream(streamKey, true)
	if err != nil {
		log.Println(err)
	}
	streamMapLock.Unlock()

	whipPeerConnection, err := NewPeerConnection(apiWhip)
	if err != nil {
		log.Println(err)
	}

	stream.whipSessionLock.Lock()
	stream.whipSession = &whipSession{
		peerConnection: whipPeerConnection,
		socketConstraints: &socketConstraints{
			socket: socket,
		},
	}
	stream.whipSessionLock.Unlock()

	whipPeerConnection.OnTrack(func(remoteTrack *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
		log.Println("Track has been added, StreamId:", remoteTrack.StreamID(), "audio:", strings.HasPrefix(remoteTrack.Codec().RTPCodecCapability.MimeType, "audio"))

		if strings.HasPrefix(remoteTrack.Codec().RTPCodecCapability.MimeType, "audio") {
			audioWriter(remoteTrack, stream)
		} else {
			var trackAssignment TrackAssignment
			for _, assignment := range globalMediaAssigner.Assignments {
				if assignment.StreamId == remoteTrack.StreamID() {
					trackAssignment = assignment
				}
			}
			videoWriter(remoteTrack, stream, whipPeerConnection, stream, trackAssignment)
		}
	})

	whipPeerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		message := &WebsocketMessage{}

		message.MessageType = "candidate"
		message.Candidate = candidate

		//if err := socket.WriteJSON(message); err != nil {
		//	panic(err)
		//}

		if err := stream.whipSession.socketConstraints.sendSocket(message); err != nil {
			panic(err)
		}
	})

	whipPeerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		log.Printf("WHIP ICE Connection State has changed: %s\n", connectionState.String())
		if connectionState == webrtc.ICEConnectionStateFailed || connectionState == webrtc.ICEConnectionStateClosed {
			if err := whipPeerConnection.Close(); err != nil {
				log.Println(err)
			}
			peerConnectionDisconnected(streamKey, "")
		}
	})

	//whipPeerConnection.OnDataChannel(func(channel *webrtc.DataChannel) {
	//	log.Printf("New WHIP DataChannel %s %d\n", channel.Label(), channel.ID())
	//
	//	channel.OnOpen(func() {
	//		log.Printf("WHIP sendChannel has opened")
	//	})
	//
	//	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
	//		log.Printf("Message from WHIP DataChannel '%s': '%s'\n", channel.Label(), string(msg.Data))
	//	})
	//})

	defer socket.Close()
	for {
		_, n, err := socket.ReadMessage()
		if err != nil {
			panic(err)
		}

		var (
			candidate   webrtc.ICECandidateInit
			whipMessage WHIPMessage
		)

		switch {
		case json.Unmarshal(n, &whipMessage) == nil && whipMessage.Offer.SDP != "" && whipMessage.MediaAssigner.Type != "":
			log.Println("Received whipMessage from WHIP")

			switch {
			case whipMessage.MediaAssigner.Type == "create":
				globalMediaAssigner = &whipMessage.MediaAssigner

			case whipMessage.MediaAssigner.Type == "addTrack":
				if err := WHIPAddTrack(streamKey, whipMessage.MediaAssigner); err != nil {
					log.Println(err)
				}

			case whipMessage.MediaAssigner.Type == "removeTrack":
				if err := WHIPRemoveTrack(streamKey, whipMessage.MediaAssigner); err != nil {
					log.Println(err)
				}
			default:
				panic("MediaAssigner - Unknown message")
			}

			doSignaling(whipPeerConnection, whipMessage.Offer, stream.whipSession.socketConstraints)

		case json.Unmarshal(n, &candidate) == nil && candidate.Candidate != "":
			log.Println("Received ICE candidate from WHIP: " + candidate.Candidate)
			if err = whipPeerConnection.AddICECandidate(candidate); err != nil {
				panic(err)
			}

		default:
			panic("Unknown message")
		}
	}
}

func WHIPAddTrack(streamKey string, newMediaAssigner MediaAssigner) error {
	newAssignment := newMediaAssigner.Assignments[0]

	index := -1
	for i, assignment := range globalMediaAssigner.Assignments {
		if assignment.Name == newAssignment.Name {
			index = i
		}
	}

	if index == -1 {
		globalMediaAssigner.Assignments = append(globalMediaAssigner.Assignments, newAssignment)
	} else {
		globalMediaAssigner.Assignments[index] = newAssignment
	}

	for _, assignment := range globalMediaAssigner.Assignments {
		log.Println("mediaAssigner.assignment.StreamId:", assignment.StreamId, "mediaAssigner.name:", assignment.Name)
	}

	return nil
}

func WHIPRemoveTrack(streamKey string, newMediaAssigner MediaAssigner) error {
	removeAssignment := newMediaAssigner.Assignments[0]

	log.Println("newMediaAssigner remove whip:", newMediaAssigner)

	streamMapLock.Lock()
	stream, err := getStream(streamKey, true)
	if err != nil {
		return err
	}
	streamMapLock.Unlock()

	for _, track := range stream.sourceVideoTracks {
		log.Println("WHIPRemoveTrack before stream.sourceVideoTracksName:", track.name, "StreamId:", track.streamId)
	}

	stream.sourceVideoTracksLock.Lock()
	delete(stream.sourceVideoTracks, removeAssignment.Name)
	stream.sourceVideoTracksLock.Unlock()

	for _, track := range stream.sourceVideoTracks {
		log.Println("WHIPRemoveTrack after stream.sourceVideoTracksName:", track.name, "StreamId:", track.streamId)
	}

	stream.whepSessionsLock.Lock()

	message := &WebsocketMessage{}
	message.MessageType = "change"
	message.Change = "removeTrack:" + removeAssignment.Name

	for _, whepSession := range stream.whepSessions {
		if err := whepSession.socketConstraints.sendSocket(message); err != nil {
			panic(err)
		}
	}
	stream.whepSessionsLock.Unlock()

	return nil
}
