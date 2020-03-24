package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v2"

	"net/http"
)

// Allows compressing offer/answer to bypass terminal input limits.
const compress = false

var audioChan = make(chan *rtp.Packet)

// Prepare the configuration
var peerConnectionConfig = webrtc.Configuration{
	ICEServers: []webrtc.ICEServer{
		{
			URLs: []string{"stun:stun.l.google.com:19302"},
		},
	},
}

// User is a member of chat room
type User struct {
	ID    int64
	Track *webrtc.Track
}

// Room is a voice chat room
type Room struct {
	Users map[int64]*User
}

var room = Room{
	Users: make(map[int64]*User),
}

// GetUsers converts map[int64]*User to list
func (r *Room) GetUsers() []*User {
	users := []*User{}
	for _, user := range r.Users {
		users = append(users, user)
	}
	return users
}

// AddUser adds user to the room
func (r *Room) AddUser(offer webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	// Wait for the offer to be pasted

	// We make our own mediaEngine so we can place the sender's codecs in it. Since we are echoing their RTP packet
	// back to them we are actually codec agnostic - we can accept all their codecs. This also ensures that we use the
	// dynamic media type from the sender in our answer.
	mediaEngine := webrtc.MediaEngine{}

	// Add codecs to the mediaEngine. Note that even though we are only going to echo back the sender's video we also
	// add audio codecs. This is because createAnswer will create an audioTransceiver and associated SDP and we currently
	// cannot tell it not to. The audio SDP must match the sender's codecs too...
	err := mediaEngine.PopulateFromSDP(offer)
	if err != nil {
		return nil, err
	}

	audioCodecs := mediaEngine.GetCodecsByKind(webrtc.RTPCodecTypeAudio)
	if len(audioCodecs) == 0 {
		return nil, errors.New("Offer contained no audio codecs")
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(peerConnectionConfig)
	if err != nil {
		return nil, err
	}
	// Create Track that we audio back to client on
	userTrack, err := peerConnection.NewTrack(audioCodecs[0].PayloadType, rand.Uint32(), "audio", "pion")
	if err != nil {
		return nil, err
	}

	// Add this newly created track to the PeerConnection
	if _, err = peerConnection.AddTrack(userTrack); err != nil {
		return nil, err
	}

	// Set the remote SessionDescription
	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		return nil, err
	}

	user := User{
		ID:    time.Now().UnixNano(),
		Track: userTrack,
	}
	r.Users[user.ID] = &user

	// Set a handler for when a new remote track starts, this handler copies inbound RTP packets,
	// replaces the SSRC and sends them back
	peerConnection.OnTrack(func(remoteTrack *webrtc.Track, receiver *webrtc.RTPReceiver) {
		fmt.Println("peerConnection.OnTrack")
		// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
		// This is a temporary fix until we implement incoming RTCP events, then we would push a PLI only when a viewer requests it
		go func() {
			ticker := time.NewTicker(time.Second * 3)
			for range ticker.C {
				errSend := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: remoteTrack.SSRC()}})
				if errSend != nil {
					fmt.Println(errSend)
				}
			}
		}()

		fmt.Printf("Track has started, of type %d: %s \n", remoteTrack.PayloadType(), remoteTrack.Codec().Name)

		for {
			// Read RTP packets being sent to Pion
			rtp, readErr := remoteTrack.ReadRTP()
			if readErr != nil {
				panic(readErr)
			}

			for _, roomUser := range r.GetUsers() {
				// dont send rtp packets to owner
				if roomUser.ID == user.ID {
					continue
				}
				// Replace the SSRC with the SSRC of the outbound track.
				// The only change we are making replacing the SSRC, the RTP packets are unchanged otherwise
				rtp.SSRC = roomUser.Track.SSRC()

				if writeErr := roomUser.Track.WriteRTP(rtp); writeErr != nil {
					panic(writeErr)
				}
			}
		}
	})

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Connection State has changed %s \n", connectionState.String())
		if connectionState == webrtc.ICEConnectionStateConnected {
			fmt.Println("user joined")
			// room.MembersCount++
			fmt.Println("now members count is", len(room.GetUsers()))
		} else if connectionState == webrtc.ICEConnectionStateDisconnected {
			fmt.Println("user leaved")
			delete(r.Users, user.ID)
			fmt.Println("now members count is", len(room.GetUsers()))
		}
	})
	// Create an answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}
	// Sets the LocalDescription, and starts our UDP listeners
	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		panic(err)
	}

	// Output the answer in base64 so we can paste it in browser
	return &answer, nil
}

func main() {

	handlePing := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "pong")
	}
	handleOffer := func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("server: icoming offer request")
		w.Header().Add("Access-Control-Allow-Headers", "*")
		w.Header().Add("Access-Control-Allow-Origin", "*")
		if r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		// buf := make([]byte, )
		var offer webrtc.SessionDescription
		err := json.NewDecoder(r.Body).Decode(&offer)
		if err != nil {
			http.Error(w, "invalid offer format", 400)
			return
		}

		answer, err := room.AddUser(offer)
		if err != nil {
			http.Error(w, fmt.Sprint("cant accept offer:", err), http.StatusBadRequest)
			return
		}
		// json.Marshal(obj)
		// io.Write(w, `{"ok": true}`)
		bytes, err := json.Marshal(answer)
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(200)
		w.Write(bytes)
		return
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
		log.Printf("Defaulting to port %s", port)
	}
	addr := fmt.Sprintf(":%s", port)
	fmt.Printf("listening on %s\n", addr)
	http.HandleFunc("/", handlePing)
	http.HandleFunc("/offer", handleOffer)
	log.Fatal(http.ListenAndServe(addr, nil))
}
