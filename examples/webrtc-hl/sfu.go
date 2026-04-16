package main

import (
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/yohimik/goxash3d-fwgs/pkg"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type SFUNet struct {
	*goxash3d_fwgs.BaseNet
}

func NewSFUNet() *SFUNet {
	return &SFUNet{
		BaseNet: goxash3d_fwgs.NewBaseNet(goxash3d_fwgs.BaseNetOptions{
			HostName: "webxash",
			HostID:   3000,
		}),
	}
}

var sfuNet = NewSFUNet()
var pool = goxash3d_fwgs.NewBytesPool(256)

func (n *SFUNet) SendTo(fd int, packet goxash3d_fwgs.Packet, flags int) int {
	conn := connections[packet.Addr.IP[0]]
	if conn == nil {
		return -1
	}
	nn, err := conn.Write(packet.Data)
	if err != nil {
		return -1
	}
	return nn
}

func (n *SFUNet) SendToBatch(fd int, packets []goxash3d_fwgs.Packet, flags int) int {
	sum := 0
	for _, packet := range packets {
		nn := n.SendTo(fd, packet, flags)
		if nn == -1 {
			return -1
		}
		sum += nn
	}
	return sum
}

var connections = make([]io.Writer, 256)

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			allowedOrigins := os.Getenv("ALLOWED_ORIGINS")
			if allowedOrigins == "" || allowedOrigins == "*" {
				return true
			}

			origin := r.Header.Get("Origin")
			for _, o := range strings.Split(allowedOrigins, ",") {
				if strings.TrimSpace(o) == origin {
					return true
				}
			}
			sfuLog.Warnf("Origin blocked: %s", origin)
			return false
		},
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   4096,
		WriteBufferSize:  4096,
	}

	api *webrtc.API

	// lock for peerConnections and trackLocals
	listLock        sync.RWMutex
	peerConnections []peerConnectionState
	trackLocals     map[string]*webrtc.TrackLocalStaticRTP

	maxConnections = func() int {
		if m, err := strconv.Atoi(os.Getenv("MAX_CONNECTIONS")); err == nil && m > 0 {
			return m
		}
		return 32
	}()

	sfuLog = logging.NewDefaultLoggerFactory().NewLogger("sfu-ws")
)

type websocketMessage struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

type peerConnectionState struct {
	peerConnection *webrtc.PeerConnection
	websocket      *threadSafeWriter
}

// Add to list of tracks and fire renegotation for all PeerConnections.
func addTrack(t *webrtc.TrackRemote) *webrtc.TrackLocalStaticRTP { // nolint
	listLock.Lock()

	// Create a new TrackLocal with the same codec as our incoming
	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), t.StreamID())
	if err != nil {
		listLock.Unlock()
		panic(err)
	}

	trackLocals[t.ID()] = trackLocal

	listLock.Unlock()
	signalPeerConnections()

	return trackLocal
}

// Remove from list of tracks and fire renegotation for all PeerConnections.
func removeTrack(t *webrtc.TrackLocalStaticRTP) {
	listLock.Lock()
	delete(trackLocals, t.ID())
	listLock.Unlock()

	signalPeerConnections()
}

// signalPeerConnections updates each PeerConnection so that it is getting all the expected media tracks.
func signalPeerConnections() { // nolint
	listLock.Lock()

	attemptSync := func() (tryAgain bool) {
		for i := range peerConnections {
			if peerConnections[i].peerConnection.ConnectionState() == webrtc.PeerConnectionStateClosed {
				peerConnections = append(peerConnections[:i], peerConnections[i+1:]...)

				return true // We modified the slice, start from the beginning
			}

			// map of sender we already are seanding, so we don't double send
			existingSenders := map[string]bool{}

			for _, sender := range peerConnections[i].peerConnection.GetSenders() {
				if sender.Track() == nil {
					continue
				}

				existingSenders[sender.Track().ID()] = true

				// If we have a RTPSender that doesn't map to a existing track remove and signal
				if _, ok := trackLocals[sender.Track().ID()]; !ok {
					if err := peerConnections[i].peerConnection.RemoveTrack(sender); err != nil {
						return true
					}
				}
			}

			// Don't receive videos we are sending, make sure we don't have loopback
			for _, receiver := range peerConnections[i].peerConnection.GetReceivers() {
				if receiver.Track() == nil {
					continue
				}

				existingSenders[receiver.Track().ID()] = true
			}

			// Add all track we aren't sending yet to the PeerConnection
			for trackID := range trackLocals {
				if _, ok := existingSenders[trackID]; !ok {
					if _, err := peerConnections[i].peerConnection.AddTrack(trackLocals[trackID]); err != nil {
						return true
					}
				}
			}

			// Apenas cria offer se estiver em estado estável - EVITA SPAM
			if peerConnections[i].peerConnection.SignalingState() != webrtc.SignalingStateStable {
				continue
			}

			offer, err := peerConnections[i].peerConnection.CreateOffer(nil)
			if err != nil {
				sfuLog.Errorf("Failed to create offer: %v", err)
				return true
			}

			if err = peerConnections[i].peerConnection.SetLocalDescription(offer); err != nil {
				sfuLog.Errorf("Failed to set local description: %v", err)
				return true
			}

			offerString, err := json.Marshal(offer)
			if err != nil {
				sfuLog.Errorf("Failed to marshal offer to json: %v", err)

				return true
			}

			sfuLog.Infof("Sending offer to client")

			if err = peerConnections[i].websocket.WriteJSON(&websocketMessage{
				Event: "offer",
				Data:  string(offerString),
			}); err != nil {
				sfuLog.Errorf("Failed to send offer: %v", err)
				return true
			}
		}

		return false
	}

	for syncAttempt := 0; ; syncAttempt++ {
		if syncAttempt == 25 {
			// Release the lock and attempt a sync in 3 seconds. We might be blocking a RemoveTrack or AddTrack
			listLock.Unlock()
			go func() {
				time.Sleep(time.Second * 3)
				signalPeerConnections()
			}()

			return
		}

		if !attemptSync() {
			break
		}
	}

	listLock.Unlock()
	dispatchKeyFrame()
}

// dispatchKeyFrame sends a keyframe to all PeerConnections, used everytime a new user joins the call.
func dispatchKeyFrame() {
	listLock.Lock()
	defer listLock.Unlock()

	for i := range peerConnections {
		for _, receiver := range peerConnections[i].peerConnection.GetReceivers() {
			if receiver.Track() == nil {
				continue
			}

			_ = peerConnections[i].peerConnection.WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{
					MediaSSRC: uint32(receiver.Track().SSRC()),
				},
			})
		}
	}
}

const messageSize = 1024 * 8

func ReadLoop(d io.Reader, ip [4]byte) {
	for {
		buffer := make([]byte, messageSize)
		n, err := d.Read(buffer)
		if err != nil {
			fmt.Println("Datachannel closed; Exit the readloop:", err)

			return
		}
		sfuNet.PushPacket(goxash3d_fwgs.Packet{
			Addr: goxash3d_fwgs.Addr{
				IP:   ip,
				Port: 1000,
			},
			Data: buffer[:n],
		})
	}
}

// Handle incoming websockets.
func websocketHandler(w http.ResponseWriter, r *http.Request) { // nolint
	listLock.RLock()
	currentConnections := len(peerConnections)
	listLock.RUnlock()

	if currentConnections >= maxConnections {
		sfuLog.Warnf("Connection rejected: max connections reached (%d/%d)", currentConnections, maxConnections)
		http.Error(w, "Server is full", http.StatusServiceUnavailable)
		return
	}

	sfuLog.Infof("New connection from %s (%d/%d active)", r.RemoteAddr, currentConnections+1, maxConnections)

	// Upgrade HTTP request to Websocket
	unsafeConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		sfuLog.Errorf("Failed to upgrade HTTP to Websocket: %v", err)
		return
	}

	c := &threadSafeWriter{unsafeConn, sync.Mutex{}} // nolint

	// When this frame returns close the Websocket
	defer c.Close() //nolint

	// Create new PeerConnection
	stunServer := os.Getenv("STUN_SERVER")
	if stunServer == "" {
		stunServer = "stun:stun.l.google.com:19302"
	}

	iceServers := []webrtc.ICEServer{
		{
			URLs: []string{stunServer},
		},
	}

	// Adiciona servidor TURN se configurado
	if turnURL := os.Getenv("TURN_SERVER"); turnURL != "" {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:       []string{turnURL},
			Username:   os.Getenv("TURN_USERNAME"),
			Credential: os.Getenv("TURN_PASSWORD"),
		})
	} else {
		// Fallback para servidor público apenas se não houver TURN próprio
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:       []string{"turn:openrelay.metered.ca:80"},
			Username:   "openrelayproject",
			Credential: "openrelayproject",
		})
	}

	peerConnection, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})
	if err != nil {
		sfuLog.Errorf("Failed to creates a PeerConnection: %v", err)
		return
	}

	// When this frame returns close the PeerConnection
	defer peerConnection.Close() //nolint

	// Accept one audio and one video track incoming
	for _, typ := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeAudio} {
		if _, err := peerConnection.AddTransceiverFromKind(typ, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			sfuLog.Errorf("Failed to add transceiver: %v", err)

			return
		}
	}

	// Add our new PeerConnection to global list
	listLock.Lock()
	peerConnections = append(peerConnections, peerConnectionState{peerConnection, c})
	listLock.Unlock()

	f := false
	var z uint16 = 0
	ip := [4]byte{}
	for i := range ip {
		ip[i] = byte(rand.Intn(256))
	}
	index, _ := pool.TryGet()
	ip[0] = index
	defer pool.TryPut(index)

	writeChannel, err := peerConnection.CreateDataChannel("write", &webrtc.DataChannelInit{
		Ordered:        &f,
		MaxRetransmits: &z,
	})
	if err != nil {
		sfuLog.Errorf("Failed to creates a data channel: %v", err)

		return
	}
	var readChannel *webrtc.DataChannel
	defer func() {
		if readChannel != nil {
			readChannel.Close()
		}
	}()
	writeChannel.OnOpen(func() {
		sfuLog.Infof("DataChannel 'write' opened successfully")
		d, err := writeChannel.Detach()
		if err != nil {
			sfuLog.Errorf("Failed to detach write channel: %v", err)
			return
		}
		connections[index] = d

		rc, err := peerConnection.CreateDataChannel("read", &webrtc.DataChannelInit{
			Ordered:        &f,
			MaxRetransmits: &z,
		})
		if err != nil {
			sfuLog.Errorf("Failed to create read data channel: %v", err)
			return
		}
		readChannel = rc
		readChannel.OnOpen(func() {
			sfuLog.Infof("DataChannel 'read' opened successfully")
			d, err := readChannel.Detach()
			if err != nil {
				sfuLog.Errorf("Failed to detach read channel: %v", err)
				return
			}
			go ReadLoop(d, ip)
		})
	})

	writeChannel.OnClose(func() {
		sfuLog.Infof("DataChannel 'write' closed")
	})

	peerConnection.OnDataChannel(func(dc *webrtc.DataChannel) {
		sfuLog.Infof("Received incoming DataChannel: %s", dc.Label())
	})
	defer writeChannel.Close()

	peerConnection.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i == nil {
			sfuLog.Infof("SERVER ICE gathering finished")
			return
		}

		sfuLog.Infof("SERVER ICE candidate: %v", i.ToJSON())

		candidateString, err := json.Marshal(i.ToJSON())
		if err != nil {
			sfuLog.Errorf("Failed to marshal candidate to json: %v", err)
			return
		}

		if writeErr := c.WriteJSON(&websocketMessage{
			Event: "candidate",
			Data:  string(candidateString),
		}); writeErr != nil {
			sfuLog.Errorf("Failed to write JSON: %v", writeErr)
		}
	})

	// If PeerConnection is closed remove it from global list
	peerConnection.OnConnectionStateChange(func(p webrtc.PeerConnectionState) {
		sfuLog.Infof("Connection state change: %s", p)

		switch p {
		case webrtc.PeerConnectionStateFailed:
			if err := peerConnection.Close(); err != nil {
				sfuLog.Errorf("Failed to close PeerConnection: %v", err)
			}
		case webrtc.PeerConnectionStateClosed:
			signalPeerConnections()
		default:
		}
	})

	peerConnection.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		sfuLog.Infof("Got remote track: Kind=%s, ID=%s, PayloadType=%d", t.Kind(), t.ID(), t.PayloadType())

		// Create a track to fan out our incoming video to all peers
		trackLocal := addTrack(t)
		defer removeTrack(trackLocal)

		buf := make([]byte, 1500)
		rtpPkt := &rtp.Packet{}

		for {
			i, _, err := t.Read(buf)
			if err != nil {
				return
			}

			if err = rtpPkt.Unmarshal(buf[:i]); err != nil {
				sfuLog.Errorf("Failed to unmarshal incoming RTP packet: %v", err)

				return
			}

			rtpPkt.Extension = false
			rtpPkt.Extensions = nil

			if err = trackLocal.WriteRTP(rtpPkt); err != nil {
				return
			}
		}
	})

	peerConnection.OnICEConnectionStateChange(func(is webrtc.ICEConnectionState) {
		sfuLog.Infof("ICE connection state changed: %s", is)
	})

	// Signal for the new PeerConnection
	signalPeerConnections()

	message := &websocketMessage{}
	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			sfuLog.Errorf("Failed to read message: %v", err)

			return
		}

		sfuLog.Infof("Got message: %s", raw)

		if err := json.Unmarshal(raw, &message); err != nil {
			sfuLog.Errorf("Failed to unmarshal json to message: %v", err)

			return
		}

		switch message.Event {
		case "candidate":
			candidate := webrtc.ICECandidateInit{}
			if err := json.Unmarshal([]byte(message.Data), &candidate); err != nil {
				sfuLog.Errorf("Failed to unmarshal json to candidate: %v", err)

				return
			}

			sfuLog.Infof("Got candidate: %v", candidate)

			if err := peerConnection.AddICECandidate(candidate); err != nil {
				sfuLog.Errorf("Failed to add ICE candidate: %v", err)

				return
			}
		case "answer":
			answer := webrtc.SessionDescription{}
			if err := json.Unmarshal([]byte(message.Data), &answer); err != nil {
				sfuLog.Errorf("Failed to unmarshal json to answer: %v", err)

				return
			}

			sfuLog.Infof("Got answer: %v", answer)

			if err := peerConnection.SetRemoteDescription(answer); err != nil {
				sfuLog.Errorf("Failed to set remote description: %v", err)
				return
			}
		default:
			sfuLog.Errorf("unknown message: %+v", message)
		}
	}
}

// Helper to make Gorilla Websockets threadsafe.
type threadSafeWriter struct {
	*websocket.Conn
	sync.Mutex
}

func (t *threadSafeWriter) WriteJSON(v interface{}) error {
	t.Lock()
	defer t.Unlock()

	return t.Conn.WriteJSON(v)
}

func runSFU() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "27016"
	}

	addr := ":" + port

	sfuLog.Infof("Starting websocket server on %s", addr)

	settingEngine := webrtc.SettingEngine{}
	settingEngine.DetachDataChannels()

	m := &webrtc.MediaEngine{}
	err := m.RegisterDefaultCodecs()
	if err != nil {
		panic(err)
	}

	i := &interceptor.Registry{}
	err = webrtc.RegisterDefaultInterceptors(m, i)
	if err != nil {
		panic(err)
	}
	api = webrtc.NewAPI(webrtc.WithSettingEngine(settingEngine), webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i))

	// Init other state
	trackLocals = map[string]*webrtc.TrackLocalStaticRTP{}

	// Middleware CORS
	corsMiddleware := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusOK)
				return
			}

			next(w, r)
		}
	}

	// Rota raiz para health check do Render
	http.HandleFunc("/", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("server online"))
	}))

	// websocket handler
	http.HandleFunc("/websocket", corsMiddleware(websocketHandler))

	// request a keyframe every 3 seconds
	go func() {
		for range time.NewTicker(time.Second * 3).C {
			dispatchKeyFrame()
		}
	}()
	// start HTTP server
	if err := http.ListenAndServe(addr, nil); err != nil { //nolint: gosec
		sfuLog.Errorf("Failed to start http server: %v", err)
	}
}
