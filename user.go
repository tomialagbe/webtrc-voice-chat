package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v2"
)

var (
	// only support unified plan
	cfg = webrtc.Configuration{
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlanWithFallback,
	}

	setting webrtc.SettingEngine

	errChanClosed     = errors.New("channel closed")
	errInvalidTrack   = errors.New("track is nil")
	errInvalidPacket  = errors.New("packet is nil")
	errInvalidPC      = errors.New("pc is nil")
	errInvalidOptions = errors.New("invalid options")
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second
	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second
	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10
	// Maximum message size allowed from peer.
	maxMessageSize = 51200
)

var (
	newline = []byte{'\n'}
	space   = []byte{' '}
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// User is a middleman between the websocket connection and the hub.
type User struct {
	ID   int
	room *Room
	conn *websocket.Conn        // The websocket connection.
	send chan []byte            // Buffered channel of outbound messages.
	pc   *webrtc.PeerConnection // WebRTC Peer Connection
	// Tracks         map[uint32]*webrtc.Track // WebRTC incoming audio tracks
	// Track *webrtc.Track
	// inTracks      map[uint32]*webrtc.Track
	inTrack       *webrtc.Track
	inTracksLock  sync.RWMutex
	outTracks     map[uint32]*webrtc.Track
	outTracksLock sync.RWMutex

	rtpCh chan *rtp.Packet

	stop bool
}

// readPump pumps messages from the websocket connection to the hub.
func (u *User) readPump() {
	defer func() {
		u.stop = true
		u.room.Leave(u)
		u.conn.Close()
	}()
	u.conn.SetReadLimit(maxMessageSize)
	u.conn.SetReadDeadline(time.Now().Add(pongWait))
	u.conn.SetPongHandler(func(string) error { u.conn.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	for {
		_, message, err := u.conn.ReadMessage()
		if err != nil {
			log.Println(err)
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("error: %v", err)
				log.Println(err)
			}
			break
		}
		message = bytes.TrimSpace(bytes.Replace(message, newline, space, -1))
		go func() {
			err := u.HandleEvent(message)
			if err != nil {
				log.Println(err)
				u.SendErr(err)
			}
		}()
	}
}

// writePump pumps messages from the hub to the websocket connection.
//
// A goroutine running writePump is started for each connection. The
// application ensures that there is at most one writer to a connection by
// executing all writes from this goroutine.
func (u *User) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		u.conn.Close()
	}()
	for {
		select {
		case message, ok := <-u.send:
			u.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// The hub closed the channel.
				u.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := u.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)
			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			u.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := u.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// Event represents web socket user event
type Event struct {
	Type string `json:"type"`

	Offer     *webrtc.SessionDescription `json:"offer,omitempty"`
	Answer    *webrtc.SessionDescription `json:"answer,omitempty"`
	Candidate *webrtc.ICECandidateInit   `json:"candidate,omitempty"`
	Desc      string                     `json:"desc,omitempty"`
}

// SendJSON sends json body to web socket
func (u *User) SendJSON(body interface{}) error {
	json, err := json.Marshal(body)
	if err != nil {
		return err
	}
	u.send <- json
	return nil
}

// SendErr sends error in json format to web socket
func (u *User) SendErr(err error) error {
	return u.SendJSON(Event{Type: "error", Desc: fmt.Sprint(err)})
}

// HandleEvent handles user event
func (u *User) HandleEvent(eventRaw []byte) error {
	var event *Event
	err := json.Unmarshal(eventRaw, &event)
	if err != nil {
		return err
	}

	log.Println("handle event", event.Type)
	if event.Type == "offer" {
		if event.Offer == nil {
			return u.SendErr(errors.New("empty offer"))
		}
		err := u.HandleOffer(*event.Offer)
		if err != nil {
			return err
		}
		return nil
	} else if event.Type == "answer" {
		if event.Answer == nil {
			return u.SendErr(errors.New("empty answer"))
		}
		u.pc.SetRemoteDescription(*event.Answer)
		return nil
	} else if event.Type == "candidate" {
		if event.Candidate == nil {
			return u.SendErr(errors.New("empty candidate"))
		}
		log.Println("adding candidate", event.Candidate)
		u.pc.AddICECandidate(*event.Candidate)
		return nil
	}

	return u.SendErr(errors.New("not implemented"))
}

// GetRoomTracks returns list of room incoming tracks
func (u *User) GetRoomTracks() []*webrtc.Track {
	tracks := []*webrtc.Track{}
	for _, user := range u.room.GetUsers() {
		if user.inTrack != nil {
			tracks = append(tracks, user.inTrack)
		}
	}
	return tracks
}

func (u *User) supportOpus(offer webrtc.SessionDescription) bool {
	mediaEngine := webrtc.MediaEngine{}
	mediaEngine.PopulateFromSDP(offer)
	var payloadType uint8
	// Search for Payload type. If the offer doesn't support codec exit since
	// since they won't be able to decode anything we send them
	for _, audioCodec := range mediaEngine.GetCodecsByKind(webrtc.RTPCodecTypeAudio) {
		if audioCodec.Name == "OPUS" {
			payloadType = audioCodec.PayloadType
			break
		}
	}
	if payloadType == 0 {
		return false
	}
	return true
}

// HandleOffer handles webrtc offer
func (u *User) HandleOffer(offer webrtc.SessionDescription) error {
	if ok := u.supportOpus(offer); !ok {
		return errors.New("remote peer does not support opus codec")
	}
	tracks := u.GetRoomTracks()
	if len(tracks) == 0 {
		_, err := u.pc.AddTransceiver(webrtc.RTPCodecTypeAudio, webrtc.RtpTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
		if err != nil {
			return err
		}
	}
	fmt.Println("attach ", len(tracks), "tracks to new user")
	for _, track := range tracks {
		err := u.AddTrack(track.SSRC())
		if err != nil {
			log.Println("ERROR Add remote track as peerConnection local track", err)
			panic(err)
		}
	}

	// Set the remote SessionDescription
	if err := u.pc.SetRemoteDescription(offer); err != nil {
		return err
	}
	err := u.SendAnswer()
	if err != nil {
		return err
	}

	return nil
}

// Offer return a offer
func (u *User) Offer() (webrtc.SessionDescription, error) {
	offer, err := u.pc.CreateOffer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, err
	}
	err = u.pc.SetLocalDescription(offer)
	if err != nil {
		return webrtc.SessionDescription{}, err
	}
	return offer, nil
}

// SendOffer creates webrtc offer
func (u *User) SendOffer() error {
	offer, err := u.Offer()
	err = u.SendJSON(Event{Type: "offer", Offer: &offer})
	if err != nil {
		panic(err)
	}
	return nil
}

// SendCandidate sends ice candidate to peer
func (u *User) SendCandidate(iceCandidate *webrtc.ICECandidate) error {
	if iceCandidate == nil {
		return errors.New("nil ice candidate")
	}
	iceCandidateInit := iceCandidate.ToJSON()
	err := u.SendJSON(Event{Type: "candidate", Candidate: &iceCandidateInit})
	if err != nil {
		return err
	}
	return nil
}

// Answer creates webrtc answer
func (u *User) Answer() (webrtc.SessionDescription, error) {
	answer, err := u.pc.CreateAnswer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, err
	}
	// Sets the LocalDescription, and starts our UDP listeners
	if err = u.pc.SetLocalDescription(answer); err != nil {
		return webrtc.SessionDescription{}, err
	}
	return answer, nil
}

// SendAnswer creates answer and send it via websocket
func (u *User) SendAnswer() error {
	answer, err := u.Answer()
	if err != nil {
		return err
	}
	err = u.SendJSON(Event{Type: "answer", Answer: &answer})
	return nil
}

// receiveInTrackRTP receive all incoming tracks' rtp and sent to one channel
func (u *User) receiveInTrackRTP(remoteTrack *webrtc.Track) {
	for {
		// if u.stop {
		// 	return
		// }
		rtp, err := remoteTrack.ReadRTP()
		if err != nil {
			if err == io.EOF {
				return
			}
			log.Fatalf("rtp err => %v", err)
		}
		u.rtpCh <- rtp
	}
}

// ReadRTP read rtp packet
func (u *User) ReadRTP() (*rtp.Packet, error) {
	rtp, ok := <-u.rtpCh
	if !ok {
		return nil, errChanClosed
	}
	return rtp, nil
}

// WriteRTP send rtp packet to user outgoing tracks
func (u *User) WriteRTP(pkt *rtp.Packet) error {
	if pkt == nil {
		return errInvalidPacket
	}
	u.outTracksLock.RLock()
	track := u.outTracks[pkt.SSRC]
	u.outTracksLock.RUnlock()

	if track == nil {
		log.Printf("WebRTCTransport.WriteRTP track==nil pkt.SSRC=%d", pkt.SSRC)
		return errInvalidTrack
	}

	// log.Debugf("WebRTCTransport.WriteRTP pkt=%v", pkt)
	err := track.WriteRTP(pkt)
	if err != nil {
		// log.Errorf(err.Error())
		// u.writeErrCnt++
		return err
	}
	return nil
}

func (u *User) broadcastIncomingRTP() {
	for {
		rtp, err := u.ReadRTP()
		if err != nil {
			panic(err)
		}
		for _, user := range u.room.GetOtherUsers(u) {
			err := user.WriteRTP(rtp)
			if err != nil {
				// panic(err)
				fmt.Println(err)
			}
		}
	}
}

// GetOutTracks return incoming tracks
func (u *User) GetOutTracks() map[uint32]*webrtc.Track {
	u.outTracksLock.RLock()
	defer u.outTracksLock.RUnlock()
	return u.outTracks
}

// AddTrack adds track dynamically with renegotiation
func (u *User) AddTrack(ssrc uint32) error {
	track, err := u.pc.NewTrack(webrtc.DefaultPayloadTypeOpus, ssrc, "pion", "pion")
	if err != nil {
		return err
	}
	if _, err := u.pc.AddTrack(track); err != nil {
		log.Println("ERROR Add remote track as peerConnection local track", err)
		return err
	}

	u.outTracksLock.Lock()
	u.outTracks[track.SSRC()] = track
	u.outTracksLock.Unlock()
	return nil
}

// AddTrack add track to pc
// func (w *WebRTCTransport) AddTrack(ssrc uint32, pt uint8, streamID string, trackID string) (*webrtc.Track, error) {
// 	if w.pc == nil {
// 		return nil, errInvalidPC
// 	}
// 	track, err := w.pc.NewTrack(pt, ssrc, trackID, streamID)
// 	if err != nil {
// 		return nil, err
// 	}
// 	if _, err = w.pc.AddTrack(track); err != nil {
// 		return nil, err
// 	}

// 	w.outTrackLock.Lock()
// 	w.outTracks[ssrc] = track
// 	w.outTrackLock.Unlock()
// 	return track, nil
// }

var count = 0

// Watch for debug
func (u *User) Watch() {
	ticker := time.NewTicker(time.Second * 5)
	for range ticker.C {
		if u.stop {
			ticker.Stop()
			return
		}
		fmt.Println("ID:", u.ID, "out: ", u.GetOutTracks())
	}
}

// serveWs handles websocket requests from the peer.
func serveWs(rooms *Rooms, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	mediaEngine := webrtc.MediaEngine{}
	mediaEngine.RegisterCodec(webrtc.NewRTPOpusCodec(webrtc.DefaultPayloadTypeOpus, 48000))

	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))
	peerConnection, err := api.NewPeerConnection(peerConnectionConfig)

	roomID := strings.ReplaceAll(r.URL.Path, "/", "")
	room := rooms.GetOrCreate(roomID)

	log.Println("ws connection to room:", roomID, len(room.GetUsers()), "users")

	count++
	user := &User{
		ID:        count,
		room:      room,
		conn:      conn,
		send:      make(chan []byte, 256),
		pc:        peerConnection,
		outTracks: make(map[uint32]*webrtc.Track),
		rtpCh:     make(chan *rtp.Packet, 100),
	}

	user.pc.OnICECandidate(func(iceCandidate *webrtc.ICECandidate) {
		if iceCandidate != nil {
			err := user.SendCandidate(iceCandidate)
			if err != nil {
				log.Println("fail send candidate", err)
			}
		}
	})

	user.pc.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		log.Printf("Connection State has changed %s \n", connectionState.String())
		if connectionState == webrtc.ICEConnectionStateConnected {
			log.Println("user joined")
			// room.MembersCount++
			log.Println("now members count is", len(user.room.GetUsers()))
		} else if connectionState == webrtc.ICEConnectionStateDisconnected ||
			connectionState == webrtc.ICEConnectionStateFailed ||
			connectionState == webrtc.ICEConnectionStateClosed {
			log.Println("user leaved")
			// delete(r.Users, user.ID)
			log.Println("now members count is", len(user.room.GetUsers()))
		}
	})

	user.pc.OnTrack(func(remoteTrack *webrtc.Track, receiver *webrtc.RTPReceiver) {
		log.Println("user id: ", user.ID, "peerConnection.OnTrack")
		user.inTrack = remoteTrack

		for _, roomUser := range user.room.GetOtherUsers(user) {
			if err := roomUser.AddTrack(remoteTrack.SSRC()); err != nil {
				panic(err)
			}
			err := roomUser.SendOffer()
			if err != nil {
				panic(err)
			}
		}
		log.Printf("Track has started, of type %d: %s, ssrc: %d \n", remoteTrack.PayloadType(), remoteTrack.Codec().Name, remoteTrack.SSRC())
		go user.receiveInTrackRTP(remoteTrack)
		go user.broadcastIncomingRTP()
	})

	user.room.Join(user)

	// Allow collection of memory referenced by the caller by doing all work in
	// new goroutines.
	go user.writePump()
	go user.readPump()
	go user.Watch()
}
