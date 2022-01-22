package discordgo

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/nacl/secretbox"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type VideoConnection struct {
	sync.RWMutex

	Debug        bool // If true, print extra logging -- DEPRECATED
	LogLevel     int
	Ready        bool // If true, voice is ready to send/receive audio
	UserID       string
	GuildID      string
	ChannelID    string
	StreamKey    string
	reconnecting bool // If true, voice connection is trying to reconnect

	speaking int

	H264Send chan []byte // Chan for sending h264 video

	wsConn  *websocket.Conn
	wsMutex sync.Mutex
	udpConn *net.UDPConn
	session *Session

	sessionID string
	serverID  string
	token     string
	endpoint  string

	// Used to send a close signal to goroutines
	close chan struct{}

	// Used to allow blocking until connected
	connected chan bool

	// Used to pass the sessionid from onVoiceStateUpdate
	// sessionRecv chan string UNUSED ATM

	op4 videoOP4
	op2 videoOP2

	videoSsrc int

	voiceSpeakingUpdateHandlers []VideoStreamingUpdateHandler
	UdpAddress                  string
}

// VideoStreamingUpdateHandler type provides a function definition for the
// VideoStreamingUpdate event
type VideoStreamingUpdateHandler func(vc *VideoConnection, vs *VideoStreamingUpdate)

// Speaking sends a speaking notification to Discord over the voice websocket.
// This must be sent as true prior to sending audio and should be set to false
// once finished sending audio.
//  b  : Send true if speaking, false if not.
func (v *VideoConnection) Speaking(b int) (err error) {

	v.log(LogDebug, "called (%t)", b)

	type videoSpeakingData struct {
		Speaking int    `json:"speaking"`
		Delay    int    `json:"delay"`
		Ssrc     uint32 `json:"ssrc"`
	}

	type videoSpeakingOp struct {
		Op   int               `json:"op"` // Always 5
		Data videoSpeakingData `json:"d"`
	}

	if v.wsConn == nil {
		return fmt.Errorf("no VideoConnection websocket")
	}

	data := videoSpeakingOp{5, videoSpeakingData{b, 0, v.op2.SSRC}}
	v.wsMutex.Lock()
	err = v.wsConn.WriteJSON(data)
	v.wsMutex.Unlock()

	v.Lock()
	defer v.Unlock()
	if err != nil {
		v.speaking = 0
		v.log(LogError, "Speaking() write json error, %s", err)
		return
	}

	v.speaking = 0

	return
}

// ChangeChannel sends Discord a request to change channels within a Guild
// !!! NOTE !!! This function may be removed in favour of just using ChannelVoiceJoin
func (v *VideoConnection) ChangeChannel(channelID string) (err error) {

	v.log(LogInformational, "called")

	data := createStreamOp{18, createStreamData{v.GuildID, channelID, nil, "guild"}}
	v.session.wsMutex.Lock()
	err = v.session.wsConn.WriteJSON(data)
	v.session.wsMutex.Unlock()
	if err != nil {
		return err
	}
	v.ChannelID = channelID
	v.speaking = 0

	keyData := createStreamKeyOp{22,
		createStreamKeyData{false, fmt.Sprintf("%s:%s:%s", v.GuildID, v.ChannelID, v.session.State.User.ID)}}
	v.session.wsMutex.Lock()
	err = v.session.wsConn.WriteJSON(keyData)
	v.session.wsMutex.Unlock()
	if err != nil {
		return err
	}

	return
}

// Disconnect disconnects from this voice channel and closes the websocket
// and udp connections to Discord.
func (v *VideoConnection) Disconnect() (err error) {

	// Send a OP4 with a nil channel to disconnect
	v.Lock()
	if v.sessionID != "" {
		data := voiceChannelJoinOp{4, voiceChannelJoinData{&v.GuildID, nil, true, true}}
		v.session.wsMutex.Lock()
		err = v.session.wsConn.WriteJSON(data)
		v.session.wsMutex.Unlock()
		v.sessionID = ""
	}
	v.Unlock()

	// Close websocket and udp connections
	v.Close()

	v.log(LogInformational, "Deleting VideoConnection %s", v.GuildID)

	v.session.Lock()
	v.session.VideoConnection = nil
	v.session.Unlock()

	return
}

// Close closes the voice ws and udp connections
func (v *VideoConnection) Close() {

	v.log(LogInformational, "called")

	v.Lock()
	defer v.Unlock()

	v.Ready = false
	v.speaking = 0

	if v.close != nil {
		v.log(LogInformational, "closing v.close")
		close(v.close)
		v.close = nil
	}

	if v.udpConn != nil {
		v.log(LogInformational, "closing udp")
		err := v.udpConn.Close()
		if err != nil {
			v.log(LogError, "error closing udp connection, %s", err)
		}
		v.udpConn = nil
	}

	if v.wsConn != nil {
		v.log(LogInformational, "sending close frame")

		// To cleanly close a connection, a client should send a close
		// frame and wait for the server to close the connection.
		v.wsMutex.Lock()
		err := v.wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		v.wsMutex.Unlock()
		if err != nil {
			v.log(LogError, "error closing websocket, %s", err)
		}

		// TODO: Wait for Discord to actually close the connection.
		time.Sleep(1 * time.Second)

		v.log(LogInformational, "closing websocket")
		err = v.wsConn.Close()
		if err != nil {
			v.log(LogError, "error closing websocket, %s", err)
		}

		v.wsConn = nil
	}
}

// AddHandler adds a Handler for VideoStreamingUpdate events.
func (v *VideoConnection) AddHandler(h VideoStreamingUpdateHandler) {
	v.Lock()
	defer v.Unlock()

	v.voiceSpeakingUpdateHandlers = append(v.voiceSpeakingUpdateHandlers, h)
}

// VideoStreamingUpdate is a struct for a VideoStreamingUpdate event.
type VideoStreamingUpdate struct {
	UserID   string `json:"user_id"`
	SSRC     int    `json:"ssrc"`
	Speaking bool   `json:"speaking"`
}

// ------------------------------------------------------------------------------------------------
// Unexported Internal Functions Below.
// ------------------------------------------------------------------------------------------------
type maxResolution struct {
	Height int    `json:"height"`
	Type   string `json:"type"`
	Width  int    `json:"width"`
}

type stream struct {
	Quality       int           `json:"quality"`
	Rid           string        `json:"rid"`
	Type          string        `json:"type"`
	Active        bool          `json:"active,omitempty"`
	RtxSsrc       int           `json:"rtx_ssrc,omitempty"`
	Ssrc          int           `json:"ssrc,omitempty"`
	MaxBitrate    int           `json:"max_bitrate,omitempty"`
	MaxFramerate  int           `json:"max_framerate,omitempty"`
	MaxResolution maxResolution `json:"max_resolution"`
}
type op12Data struct {
	AudioSsrc int      `json:"audio_ssrc"`
	RtxSsrc   int      `json:"rtx_ssrc"`
	Streams   []stream `json:"streams"`
	VideoSsrc int      `json:"video_ssrc"`
}

type op12Op struct {
	Op   int      `json:"op"`
	Data op12Data `json:"d"`
}

// A videoOP4 stores the data for the voice operation 4 websocket event
// which provides us with the NaCl SecretBox encryption key
type videoOP4 struct {
	SecretKey      [32]byte `json:"secret_key"`
	MediaSessionId string   `json:"media_session_id"`
	Mode           string   `json:"mode"`
	VideoCodec     string   `json:"video_codec"`
}

// A videoOP2 stores the data for the voice operation 2 websocket event
// which is sort of like the voice READY packet
type videoOP2 struct {
	SSRC              uint32        `json:"ssrc"`
	Port              int           `json:"port"`
	Modes             []string      `json:"modes"`
	HeartbeatInterval time.Duration `json:"heartbeat_interval"`
	IP                string        `json:"ip"`
	Streams           []stream      `json:"streams"`
}

// WaitUntilConnected waits for the Voice Connection to
// become ready, if it does not become ready it returns an err
func (v *VideoConnection) waitUntilConnected() error {

	v.log(LogInformational, "called")

	i := 0
	for {
		v.RLock()
		ready := v.Ready
		v.RUnlock()
		if ready {
			return nil
		}

		if i > 10 {
			return fmt.Errorf("timeout waiting for voice")
		}

		time.Sleep(1 * time.Second)
		i++
	}
}

// Open opens a voice connection.  This should be called
// after VoiceChannelJoin is used and the data VOICE websocket events
// are captured.
func (v *VideoConnection) open() (err error) {

	v.log(LogInformational, "called")

	v.Lock()
	defer v.Unlock()

	// Don't open a websocket if one is already open
	if v.wsConn != nil {
		v.log(LogWarning, "refusing to overwrite non-nil websocket")
		return
	}

	// TODO temp? loop to wait for the SessionID
	i := 0
	for {
		if v.sessionID != "" {
			break
		}
		if i > 20 { // only loop for up to 1 second total
			return fmt.Errorf("did not receive voice Session ID in time")
		}
		time.Sleep(50 * time.Millisecond)
		i++
	}

	// Connect to VideoConnection Websocket
	vg := "wss://" + strings.TrimSuffix(v.endpoint, ":80") + "?v=5"
	v.log(LogInformational, "connecting to voice endpoint %s", vg)
	v.wsConn, _, err = websocket.DefaultDialer.Dial(vg, nil)
	if err != nil {
		v.log(LogWarning, "error connecting to voice endpoint %s, %s", vg, err)
		v.log(LogDebug, "voice struct: %#v\n", v)
		return
	}

	type videoHandshakeData struct {
		ServerID  string   `json:"server_id"`
		UserID    string   `json:"user_id"`
		SessionID string   `json:"session_id"`
		Token     string   `json:"token"`
		Video     bool     `json:"video"`
		Streams   []stream `json:"streams"`
	}
	type videoHandshakeOp struct {
		Op   int                `json:"op"` // Always 0
		Data videoHandshakeData `json:"d"`
	}
	data := videoHandshakeOp{0, videoHandshakeData{v.serverID, v.UserID, v.sessionID, v.token, true,
		[]stream{{
			Quality: 100,
			Rid:     "100",
			Type:    "screen",
		}}}}

	err = v.wsConn.WriteJSON(data)
	if err != nil {
		v.log(LogWarning, "error sending init packet, %s", err)
		return
	}

	v.close = make(chan struct{})
	go v.wsListen(v.wsConn, v.close)

	// add loop/check for Ready bool here?
	// then return false if not ready?
	// but then wsListen will also err.

	return
}

// wsListen listens on the voice websocket for messages and passes them
// to the voice event handler.  This is automatically called by the Open func
func (v *VideoConnection) wsListen(wsConn *websocket.Conn, close <-chan struct{}) {

	v.log(LogInformational, "called")

	for {
		_, message, err := v.wsConn.ReadMessage()
		if err != nil {
			// 4014 indicates a manual disconnection by someone in the guild;
			// we shouldn't reconnect.
			if websocket.IsCloseError(err, 4014) {
				v.log(LogInformational, "received 4014 manual disconnection")

				// Abandon the voice WS connection
				v.Lock()
				v.wsConn = nil
				v.Unlock()

				v.session.Lock()
				delete(v.session.VoiceConnections, v.GuildID)
				v.session.Unlock()

				v.Close()

				return
			}

			// Detect if we have been closed manually. If a Close() has already
			// happened, the websocket we are listening on will be different to the
			// current session.
			v.RLock()
			sameConnection := v.wsConn == wsConn
			v.RUnlock()
			if sameConnection {

				v.log(LogError, "voice endpoint %s websocket closed unexpectantly, %s", v.endpoint, err)

				// Start reconnect goroutine then exit.
				go v.reconnect()
			}
			return
		}

		// Pass received message to voice event handler
		select {
		case <-close:
			return
		default:
			go v.onEvent(message)
		}
	}
}

// wsEvent handles any voice websocket events. This is only called by the
// wsListen() function.
func (v *VideoConnection) onEvent(message []byte) {

	v.log(LogDebug, "received: %s", string(message))

	var e Event
	if err := json.Unmarshal(message, &e); err != nil {
		v.log(LogError, "unmarshall error, %s", err)
		return
	}

	switch e.Operation {

	case 2: // READY

		if err := json.Unmarshal(e.RawData, &v.op2); err != nil {
			v.log(LogError, "OP2 unmarshall error, %s, %s", err, string(e.RawData))
			return
		}

		v.videoSsrc = v.op2.Streams[0].Ssrc

		// Start the voice websocket heartbeat to keep the connection alive
		go v.wsHeartbeat(v.wsConn, v.close, 30000)
		// TODO monitor a chan/bool to verify this was successful

		// Start the UDP connection
		err := v.udpOpen()
		if err != nil {
			v.log(LogError, "error opening udp connection, %s", err)
			return
		}

		// Start the h264 sender.
		if v.H264Send == nil {
			v.H264Send = make(chan []byte, 2)
		}
		go v.h264Sender(v.udpConn, v.close, v.H264Send, 48000, 960)

		return

	case 3: // HEARTBEAT response
		// add code to use this to track latency?
		return

	case 4: // udp encryption secret key
		v.Lock()
		defer v.Unlock()

		v.op4 = videoOP4{}
		if err := json.Unmarshal(e.RawData, &v.op4); err != nil {
			v.log(LogError, "OP4 unmarshall error, %s, %s", err, string(e.RawData))
			return
		}
		return

	case 5:
		if len(v.voiceSpeakingUpdateHandlers) == 0 {
			return
		}

		voiceSpeakingUpdate := &VideoStreamingUpdate{}
		if err := json.Unmarshal(e.RawData, voiceSpeakingUpdate); err != nil {
			v.log(LogError, "OP5 unmarshall error, %s, %s", err, string(e.RawData))
			return
		}

		for _, h := range v.voiceSpeakingUpdateHandlers {
			h(v, voiceSpeakingUpdate)
		}
	case 15:
		streamData := v.op2.Streams[0]
		streamData.MaxFramerate = 30
		streamData.MaxResolution = maxResolution{
			Height: 720,
			Type:   "fixed",
			Width:  1280,
		}
		streamData.MaxBitrate = 0
		streamData.Active = true
		data12 := op12Op{12, op12Data{
			AudioSsrc: int(v.op2.SSRC),
			RtxSsrc:   0,
			Streams:   []stream{streamData},
			VideoSsrc: v.videoSsrc,
		}}
		err := v.wsConn.WriteJSON(data12)
		if err != nil {
			v.log(LogWarning, "error sending init packet, %s", err)
			return
		}

		go v.Speaking(2)

	default:
		v.log(LogDebug, "unknown voice operation, %d, %s", e.Operation, string(e.RawData))
	}

	return
}

type videoHeartbeatOp struct {
	Op   int `json:"op"` // Always 3
	Data int `json:"d"`
}

// NOTE :: When a guild voice server changes how do we shut this down
// properly, so a new connection can be setup without fuss?
//
// wsHeartbeat sends regular heartbeats to voice Discord so it knows the client
// is still connected.  If you do not send these heartbeats Discord will
// disconnect the websocket connection after a few seconds.
func (v *VideoConnection) wsHeartbeat(wsConn *websocket.Conn, close <-chan struct{}, i time.Duration) {

	if close == nil || wsConn == nil {
		return
	}

	var err error
	ticker := time.NewTicker(i * time.Millisecond)
	defer ticker.Stop()
	for {
		v.log(LogDebug, "sending heartbeat packet")
		v.wsMutex.Lock()
		err = wsConn.WriteJSON(videoHeartbeatOp{3, int(time.Now().Unix())})
		v.wsMutex.Unlock()
		if err != nil {
			v.log(LogError, "error sending heartbeat to voice endpoint %s, %s", v.endpoint, err)
			return
		}

		select {
		case <-ticker.C:
			// continue loop and send heartbeat
		case <-close:
			return
		}
	}
}

// ------------------------------------------------------------------------------------------------
// Code related to the VideoConnection UDP connection
// ------------------------------------------------------------------------------------------------

type videoUDPData struct {
	Address string `json:"address"` // Public IP of machine running this code
	Port    uint16 `json:"port"`    // UDP Port of machine running this code
	Mode    string `json:"mode"`    // always "xsalsa20_poly1305"
}

type codec struct {
	Name           string `json:"name"`
	PayloadType    int    `json:"payload_type"`
	Priority       int    `json:"priority"`
	RtxPayloadType int    `json:"rtx_payload_type"`
	Type           string `json:"type"`
}

type videoUDPD struct {
	Protocol string       `json:"protocol"` // Always "udp" ?
	Data     videoUDPData `json:"data"`
	Codecs   []codec      `json:"codecs"`
}

type videoUDPOp struct {
	Op   int       `json:"op"` // Always 1
	Data videoUDPD `json:"d"`
}

// udpOpen opens a UDP connection to the voice server and completes the
// initial required handshake.  This connection is left open in the session
// and can be used to send or receive audio.  This should only be called
// from voice.wsEvent OP2
func (v *VideoConnection) udpOpen() (err error) {

	v.Lock()
	defer v.Unlock()

	if v.wsConn == nil {
		return fmt.Errorf("nil voice websocket")
	}

	if v.udpConn != nil {
		return fmt.Errorf("udp connection already open")
	}

	if v.close == nil {
		return fmt.Errorf("nil close channel")
	}

	if v.endpoint == "" {
		return fmt.Errorf("empty endpoint")
	}

	host := v.op2.IP + ":" + strconv.Itoa(v.op2.Port)
	addr, err := net.ResolveUDPAddr("udp", host)
	if err != nil {
		v.log(LogWarning, "error resolving udp host %s, %s", host, err)
		return
	}

	v.log(LogInformational, "connecting to udp addr %s", addr.String())
	v.udpConn, err = net.DialUDP("udp", nil, addr)
	if err != nil {
		v.log(LogWarning, "error connecting to udp addr %s, %s", addr.String(), err)
		return
	}
	v.UdpAddress = addr.String()

	// Create a 70 byte array and put the SSRC code from the Op 2 VideoConnection event
	// into it.  Then send that over the UDP connection to Discord
	sb := make([]byte, 70)
	binary.BigEndian.PutUint32(sb, uint32(v.videoSsrc))
	_, err = v.udpConn.Write(sb)
	if err != nil {
		v.log(LogWarning, "udp write error to %s, %s", addr.String(), err)
		return
	}

	// Create a 70 byte array and listen for the initial handshake response
	// from Discord.  Once we get it parse the IP and PORT information out
	// of the response.  This should be our public IP and PORT as Discord
	// saw us.
	rb := make([]byte, 70)
	rlen, _, err := v.udpConn.ReadFromUDP(rb)
	if err != nil {
		v.log(LogWarning, "udp read error, %s, %s", addr.String(), err)
		return
	}

	if rlen < 70 {
		v.log(LogWarning, "received udp packet too small")
		return fmt.Errorf("received udp packet too small")
	}

	// Loop over position 4 through 20 to grab the IP address
	// Should never be beyond position 20.
	var ip string
	for i := 4; i < 20; i++ {
		if rb[i] == 0 {
			break
		}
		ip += string(rb[i])
	}

	// Grab port from position 68 and 69
	port := binary.BigEndian.Uint16(rb[68:70])

	// Take the data from above and send it back to Discord to finalize
	// the UDP connection handshake.
	data := videoUDPOp{1, videoUDPD{"udp", videoUDPData{ip, port, "xsalsa20_poly1305"}, []codec{{
		Name:           "H264",
		PayloadType:    101,
		Priority:       1000,
		RtxPayloadType: 102,
		Type:           "video",
	}}}}

	v.wsMutex.Lock()
	err = v.wsConn.WriteJSON(data)
	v.wsMutex.Unlock()
	if err != nil {
		v.log(LogWarning, "udp write error, %#v, %s", data, err)
		return
	}

	streamData := v.op2.Streams[0]
	streamData.MaxFramerate = 30
	streamData.MaxResolution = maxResolution{
		Height: 720,
		Type:   "fixed",
		Width:  1280,
	}
	streamData.MaxBitrate = 0
	streamData.Active = true
	data12 := op12Op{12, op12Data{
		AudioSsrc: int(v.op2.SSRC),
		RtxSsrc:   0,
		Streams:   []stream{streamData},
		VideoSsrc: v.videoSsrc,
	}}

	err = v.wsConn.WriteJSON(data12)
	if err != nil {
		v.log(LogWarning, "error sending init packet, %s", err)
		return
	}

	// start udpKeepAlive
	go v.udpKeepAlive(v.udpConn, v.close, 5*time.Second)
	// TODO: find a way to check that it fired off okay

	return
}

// udpKeepAlive sends a udp packet to keep the udp connection open
// This is still a bit of a "proof of concept"
func (v *VideoConnection) udpKeepAlive(udpConn *net.UDPConn, close <-chan struct{}, i time.Duration) {

	if udpConn == nil || close == nil {
		return
	}

	var err error
	var sequence uint64

	packet := make([]byte, 8)

	ticker := time.NewTicker(i)
	defer ticker.Stop()
	for {

		binary.LittleEndian.PutUint64(packet, sequence)
		sequence++

		_, err = udpConn.Write(packet)
		if err != nil {
			v.log(LogError, "write error, %s", err)
			return
		}

		select {
		case <-ticker.C:
			// continue loop and send keepalive
		case <-close:
			return
		}
	}
}

// h264Sender will listen on the given channel and send any
// pre-encoded opus audio to Discord.  Supposedly.
func (v *VideoConnection) h264Sender(udpConn *net.UDPConn, close <-chan struct{}, opus <-chan []byte, rate, size int) {

	if udpConn == nil || close == nil {
		return
	}

	// VideoConnection is now ready to receive audio packets
	// TODO: this needs reviewed as I think there must be a better way.
	v.Lock()
	v.Ready = true
	v.Unlock()
	defer func() {
		v.Lock()
		v.Ready = false
		v.Unlock()
	}()

	var sequence uint16 = uint16(rand.Intn(1 << 16))
	var timestamp uint32 = rand.Uint32()
	var recvbuf []byte
	var ok bool
	udpHeader := make([]byte, 12)
	var nonce [24]byte

	// build the parts that don't change in the udpHeader
	udpHeader[0] = 0x90
	udpHeader[1] = 0x65
	binary.BigEndian.PutUint32(udpHeader[8:], uint32(v.videoSsrc))

	// start a send loop that loops until buf chan is closed
	ticker := time.NewTicker(time.Millisecond * time.Duration(size/(rate/1000)))
	defer ticker.Stop()
	for {

		// Get data from chan.  If chan is closed, return.
		select {
		case <-close:
			return
		case recvbuf, ok = <-opus:
			if !ok {
				return
			}
			// else, continue loop
		}

		// Add sequence and timestamp to udpPacket
		binary.BigEndian.PutUint16(udpHeader[2:], sequence)
		binary.BigEndian.PutUint32(udpHeader[4:], timestamp)

		// encrypt the opus data
		copy(nonce[:], udpHeader)
		//block, _ := aes.NewCipher(v.op4.SecretKey[:])
		//gcm, err := cipher.NewGCM(block)
		//if err != nil {
		//	return
		//}
		//gcm.Seal(udpHeader, nonce[:], recvbuf, nil)
		v.RLock()
		sendbuf := secretbox.Seal(udpHeader, recvbuf, &nonce, &v.op4.SecretKey)
		v.RUnlock()

		// block here until we're exactly at the right time :)
		// Then send rtp audio packet to Discord over UDP
		select {
		case <-close:
			return
		case <-ticker.C:
			continue
		default:

		}
		_, err := udpConn.Write(sendbuf)

		if err != nil {
			v.log(LogError, "udp write error, %s", err)
			v.log(LogDebug, "voice struct: %#v\n", v)
			return
		}
		size = len(recvbuf) * 8

		if (sequence) == 0xFFFF {
			sequence = 0
		} else {
			sequence++
		}

		if (timestamp + uint32(size)) >= 0xFFFFFFFF {
			timestamp = 0
		} else {
			timestamp += uint32(size)
		}
	}
}

// Reconnect will close down a voice connection then immediately try to
// reconnect to that session.
// NOTE : This func is messy and a WIP while I find what works.
// It will be cleaned up once a proven stable option is flushed out.
// aka: this is ugly shit code, please don't judge too harshly.
func (v *VideoConnection) reconnect() {

	v.log(LogInformational, "called")

	v.Lock()
	if v.reconnecting {
		v.log(LogInformational, "already reconnecting to channel %s, exiting", v.ChannelID)
		v.Unlock()
		return
	}
	v.reconnecting = true
	v.Unlock()

	defer func() { v.reconnecting = false }()

	// Close any currently open connections
	v.Close()

	wait := time.Duration(1)
	for {

		<-time.After(wait * time.Second)
		wait *= 2
		if wait > 600 {
			wait = 600
		}

		if v.session.DataReady == false || v.session.wsConn == nil {
			v.log(LogInformational, "cannot reconnect to channel %s with unready session", v.ChannelID)
			continue
		}

		v.log(LogInformational, "trying to reconnect to channel %s", v.ChannelID)

		_, err := v.session.ChannelVoiceJoin(v.GuildID, v.ChannelID, true, true)
		if err == nil {
			v.log(LogInformational, "successfully reconnected to channel %s", v.ChannelID)
			return
		}

		v.log(LogInformational, "error reconnecting to channel %s, %s", v.ChannelID, err)

		// if the reconnect above didn't work lets just send a disconnect
		// packet to reset things.
		// Send a OP4 with a nil channel to disconnect
		data := voiceChannelJoinOp{4, voiceChannelJoinData{&v.GuildID, nil, true, true}}
		v.session.wsMutex.Lock()
		err = v.session.wsConn.WriteJSON(data)
		v.session.wsMutex.Unlock()
		if err != nil {
			v.log(LogError, "error sending disconnect packet, %s", err)
		}

	}
}
