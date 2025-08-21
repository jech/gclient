package gclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

var Debug bool

const (
	maxToken = 4096
)

// GroupStatus is a group status, as returned by the server.
type GroupStatus struct {
	Name              string `json:"name"`
	Redirect          string `json:"redirect,omitempty"`
	Location          string `json:"location,omitempty"`
	Endpoint          string `json:"endpoint,omitempty"`
	DisplayName       string `json:"displayName,omitempty"`
	Description       string `json:"description,omitempty"`
	AuthServer        string `json:"authServer,omitempty"`
	AuthPortal        string `json:"authPortal,omitempty"`
	Locked            bool   `json:"locked,omitempty"`
	ClientCount       *int   `json:"clientCount,omitempty"`
	CanChangePassword bool   `json:"canChangePassword,omitempty"`
}

// Message is a message of Galene's protocol.
type Message struct {
	Type             string                   `json:"type"`
	Version          []string                 `json:"version,omitempty"`
	Kind             string                   `json:"kind,omitempty"`
	Error            string                   `json:"error,omitempty"`
	Id               string                   `json:"id,omitempty"`
	Replace          string                   `json:"replace,omitempty"`
	Source           string                   `json:"source,omitempty"`
	Dest             string                   `json:"dest,omitempty"`
	Username         *string                  `json:"username,omitempty"`
	Password         string                   `json:"password,omitempty"`
	Token            string                   `json:"token,omitempty"`
	Privileged       bool                     `json:"privileged,omitempty"`
	Permissions      []string                 `json:"permissions,omitempty"`
	Status           *GroupStatus             `json:"status,omitempty"`
	Data             map[string]interface{}   `json:"data,omitempty"`
	Group            string                   `json:"group,omitempty"`
	Value            interface{}              `json:"value,omitempty"`
	NoEcho           bool                     `json:"noecho,omitempty"`
	Time             string                   `json:"time,omitempty"`
	SDP              string                   `json:"sdp,omitempty"`
	Candidate        *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	Label            string                   `json:"label,omitempty"`
	Request          interface{}              `json:"request,omitempty"`
	RTCConfiguration *webrtc.Configuration    `json:"rtcConfiguration,omitempty"`
}

// MakeID returns a random string suitable for usage as an id.
func MakeId() string {
	rawId := make([]byte, 8)
	rand.Read(rawId)
	return base64.RawURLEncoding.EncodeToString(rawId)
}

// Client represents a client-side connection.
type Client struct {
	EventCh chan any
	Id      string

	mu               sync.Mutex
	ws               *websocket.Conn
	httpClient       *http.Client
	api              *webrtc.API
	dialer           *websocket.Dialer
	status           *GroupStatus
	statusURL        string
	group, username  string
	rtcConfiguration *webrtc.Configuration
	up, down         map[string]*webrtc.PeerConnection
}

// NewClient creates a new client connection.
// Use [*Client.Connect] to actually connect to the server.
func NewClient() *Client {
	return &Client{
		EventCh:    make(chan any, 8),
		Id:         MakeId(),
		httpClient: http.DefaultClient,
		dialer:     websocket.DefaultDialer,
		api:        webrtc.NewAPI(),
		up:         make(map[string]*webrtc.PeerConnection),
		down:       make(map[string]*webrtc.PeerConnection),
	}
}

// SetHTTPClient sets the [http.Client] used for HTTP requests
// If this is not called, [http.DefaultClient] will be used.
func (c *Client) SetHTTPClient(hc *http.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.httpClient = hc
}

// SetDialer sets the [websocket.Dialer] used for connecting to the server
// If this is not called, [websocket.DefaultDialer] will be used.
func (c *Client) SetDialer(dialer *websocket.Dialer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dialer = dialer
}

// SetAPI sets the [webrtc.API] used for creating peer connections
// If this is not called, Pion's default API will be used.
func (c *Client) SetAPI(api *webrtc.API) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.api = api
}

// getGroupStatus returns the status dictionary of a group.
// group is the full URL
func getGroupStatus(ctx context.Context, group string, client *http.Client) (*GroupStatus, error) {
	s, err := url.Parse(group)
	if err != nil {
		return nil, err
	}
	s.RawQuery = ""
	s.Path = path.Join(s.Path, ".status.json")
	s.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, "GET", s.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}

	decoder := json.NewDecoder(resp.Body)
	var status GroupStatus
	err = decoder.Decode(&status)
	if err != nil {
		return nil, err
	}
	return &status, nil
}

// GetGroupStatus returns the status dictionary for a group.
// It caches values, and might therefore return a stale value.
func (c *Client) GetGroupStatus(ctx context.Context, group string) (*GroupStatus, error) {
	if c.statusURL != group {
		if c.group != "" {
			c.mu.Lock()
			client := c.httpClient
			c.mu.Unlock()
			return getGroupStatus(ctx, group, client)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.statusURL = ""
		c.status = nil
		status, err := getGroupStatus(ctx, group, c.httpClient)
		if err != nil {
			return nil, err
		}
		c.statusURL = group
		c.status = status
		return status, nil
	}
	return c.status, nil
}

func logMessage(prefix string, m *Message) {
	j, err := json.Marshal(m)
	if err != nil {
		j = []byte(err.Error())
	}
	log.Printf("%s %s", prefix, j)
}

// Write writes a message to the server
func (c *Client) Write(m *Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.write(m)
}

func (c *Client) write(m *Message) error {
	if Debug {
		logMessage("->", m)
	}
	if c.ws == nil {
		return io.ErrClosedPipe
	}
	return c.ws.WriteJSON(m)
}

// fetchToken requests a token from an auth server
func fetchToken(ctx context.Context, server, group, username, password string, client *http.Client) (string, error) {
	request := map[string]any{
		"username": username,
		"location": group,
		"password": password,
	}
	body, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(
		ctx, "POST", server, bytes.NewReader(body),
	)
	if err != nil {
		return "", err
	}
	req.Header.Add("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return "", nil
	}

	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}

	t, err := io.ReadAll(io.LimitReader(resp.Body, maxToken+1))
	if err != nil {
		return "", err
	}
	if len(t) > maxToken {
		return "", errors.New("token too long")
	}
	return string(t), nil
}

// getToken returns a token, either from the group URL or the auth server.
// It returns the empty string if no token was provided
func (c *Client) getToken(ctx context.Context, group, username, password string) (string, error) {
	gurl, err := url.Parse(group)
	if err != nil {
		return "", err
	}

	token := gurl.Query().Get("token")
	gurl.RawQuery = ""

	if token == "" {
		status, err := c.GetGroupStatus(ctx, group)
		if err != nil {
			return "", err
		}
		if status.AuthServer != "" {
			c.mu.Lock()
			client := c.httpClient
			c.mu.Unlock()

			var err error
			token, err = fetchToken(
				ctx, status.AuthServer, gurl.String(),
				username, password, client,
			)
			if err != nil {
				return "", err
			}
		}
	}
	return token, nil
}

// GroupName returns the name of the group that we have joined,
// or the empty string.
func (c *Client) GroupName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.group
}

// RTCConfiguration returns the configuration suggested by the server.
func (c *Client) RTCConfiguration() *webrtc.Configuration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rtcConfiguration
}

// Connect connects to the server.
func (c *Client) Connect(ctx context.Context, group string) error {
	status, err := c.GetGroupStatus(ctx, group)
	if err != nil {
		return err
	}
	if status.Endpoint == "" {
		return errors.New("no endpoint")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	ws, _, err := c.dialer.DialContext(ctx, status.Endpoint, nil)
	if err != nil {
		return err
	}

	protocolError := func() error {
		c.ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(
				websocket.CloseProtocolError, "",
			),
			time.Now().Add(100*time.Millisecond),
		)
		return c.ws.Close()
	}

	m := &Message{
		Type:    "handshake",
		Version: []string{"2"},
		Id:      c.Id,
	}
	if Debug {
		logMessage("->", m)
	}
	err = ws.WriteJSON(m)
	if err != nil {
		ws.Close()
		return err
	}

	err = ws.ReadJSON(&m)
	if err != nil {
		protocolError()
		return err
	}

	if Debug {
		logMessage("<-", m)
	}

	if m.Type != "handshake" {
		protocolError()
		return errors.New("unexpected message " + m.Type)
	}

	if c.api == nil {
		c.api = webrtc.NewAPI()
	}

	c.ws = ws
	go c.readerLoop()
	return nil
}

// Join requests joining a group.
func (c *Client) Join(ctx context.Context, group, username, password string) error {
	status, err := c.GetGroupStatus(ctx, group)
	if err != nil {
		return err
	}
	token, err := c.getToken(ctx, group, username, password)
	if err != nil {
		return err
	}

	m := &Message{
		Type:     "join",
		Kind:     "join",
		Group:    status.Name,
		Username: &username,
	}
	if token != "" {
		m.Token = token
	} else if password != "" {
		// don't leak passwords if we obtained a token
		m.Password = password
	}

	return c.Write(m)
}

// Close gracefully closes a connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ws == nil {
		return io.ErrClosedPipe
	}
	c.ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(100*time.Millisecond),
	)
	return c.ws.Close()
}

// JoinedEvent indicates that we either joined or left a group.
type JoinedEvent struct {
	Kind     string
	Group    string
	Username string
	Value    string
}

// UserEvent indicates that a user has joined or left the group.
type UserEvent struct {
	Kind     string
	Id       string
	Username string
}

// UserMessageEvent indicates that we have received a "usermessage".
type UserMessageEvent struct {
	Source string
	Kind   string
	Value  any
}

// DownConnEvent indicates that we have received a new down connection.
type DownConnEvent struct {
	Id   string
	Conn *webrtc.PeerConnection
}

// DownTrackEvent indicates that we have received a new down track.
type DownTrackEvent struct {
	Id       string
	Track    *webrtc.TrackRemote
	Receiver *webrtc.RTPReceiver
}

// CloseEvent indicates that the server has closed a down connection.
type CloseEvent struct {
	Id string
}

// AbortEvent indicates that we have closed an up connection.
type AbortEvent struct {
	Id string
}

// ChatEvent indicates that we received a chat message.
type ChatEvent struct {
	Kind, Id, Source, Username, Dest string
	Privileged                       bool
	Time                             string
	Value                            string
	History                          bool
}

func (c *Client) readerLoop() {
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.group = ""
		c.username = ""
		c.rtcConfiguration = nil
		c.ws.Close()
		c.ws = nil
	}()

	c.mu.Lock()
	ws := c.ws
	c.mu.Unlock()

	for {
		var m Message
		err := ws.ReadJSON(&m)
		if err != nil {
			if err != io.EOF {
				c.EventCh <- err
			}
			close(c.EventCh)
			return
		}
		if Debug {
			logMessage("<-", &m)
		}
		switch m.Type {
		case "offer":
			username := ""
			if m.Username != nil {
				username = *m.Username
			}
			err := c.gotOffer(
				m.Id, m.Label, m.Source, username,
				m.SDP, m.Replace,
			)
			if err != nil {
				c.EventCh <- err
			}
		case "answer":
			err := c.gotAnswer(m.Id, m.SDP)
			if err != nil && err != os.ErrNotExist {
				c.EventCh <- err
			}
		case "close":
			err := c.gotClose(m.Id)
			if err != nil && err != os.ErrNotExist {
				c.EventCh <- err
			}
		case "renegotiate":
			err := c.gotRenegotiate(m.Id)
			if err != nil && Debug {
				log.Printf("Renegotiate: %v", err)
			}
		case "abort":
			err := c.CloseUpConn(m.Id)
			if err != nil && Debug {
				log.Printf("Abort: %v", err)
			}
			c.EventCh <- AbortEvent{
				Id: m.Id,
			}
		case "ice":
			err := c.gotRemoteIce(m.Id, m.Candidate)
			if err != nil && Debug {
				log.Printf("AddICECandidate: %v", err)
			}
		case "joined":
			var username string
			if m.Username != nil {
				username = *m.Username
			}
			switch m.Kind {
			case "join", "change":
				c.mu.Lock()
				c.group = m.Group
				c.username = username
				c.rtcConfiguration = m.RTCConfiguration
				c.mu.Unlock()
			case "leave":
				c.mu.Lock()
				c.group = ""
				c.username = ""
				c.rtcConfiguration = nil
				c.mu.Unlock()
			}
			value, ok := m.Value.(string)
			if !ok {
				value = fmt.Sprintf("%v", m.Value)
			}
			c.EventCh <- JoinedEvent{
				Kind:     m.Kind,
				Group:    m.Group,
				Username: username,
				Value:    value,
			}
		case "user":
			username := ""
			if m.Username != nil {
				username = *m.Username
			}
			c.EventCh <- UserEvent{
				Kind:     m.Kind,
				Id:       m.Id,
				Username: username,
			}
		case "chat", "chathistory":
			v, ok := m.Value.(string)
			if !ok {
				v = fmt.Sprintf("%v", m.Value)
			}
			var u string
			if m.Username != nil {
				u = *m.Username
			}
			c.EventCh <- ChatEvent{
				Kind:       m.Kind,
				Id:         m.Id,
				Source:     m.Source,
				Username:   u,
				Dest:       m.Dest,
				Privileged: m.Privileged,
				Time:       m.Time,
				Value:      v,
				History:    m.Type == "chathistory",
			}
		case "usermessage":
			c.EventCh <- UserMessageEvent{
				Source: m.Source,
				Kind:   m.Kind,
				Value:  m.Value,
			}
		case "ping":
			c.Write(&Message{
				Type: "pong",
			})
		case "pong":
		default:
		}
	}
}

// UserMessage sends a "usermessage" to the server.
func (c *Client) UserMessage(dest string, kind string, value any) error {
	return c.Write(&Message{
		Type:   "usermessage",
		Source: c.Id,
		Dest:   dest,
		Kind:   kind,
		Value:  value,
	})
}

func (c *Client) newDown(id string) (*webrtc.PeerConnection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pc := c.down[id]; pc != nil {
		return pc, nil
	}
	if c.rtcConfiguration == nil {
		return nil, errors.New("no configuration")
	}
	pc, err := c.api.NewPeerConnection(*c.rtcConfiguration)
	if err != nil {
		return nil, err
	}
	_, err1 := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio)
	_, err2 := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo)
	// succeed if we managed to add at least one transceiver
	if err1 != nil && err2 != nil {
		pc.Close()
		return nil, errors.Join(err1, err2)
	}
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		init := candidate.ToJSON()
		c.Write(&Message{
			Type:      "ice",
			Id:        id,
			Candidate: &init,
		})
	})
	pc.OnTrack(func(t *webrtc.TrackRemote, r *webrtc.RTPReceiver) {
		if Debug {
			log.Println("Got remote track")
		}
		c.EventCh <- DownTrackEvent{
			Id:       id,
			Track:    t,
			Receiver: r,
		}
	})
	c.down[id] = pc
	return pc, nil
}

func (c *Client) closeDown(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	conn := c.down[id]
	if conn != nil {
		conn.Close()
		delete(c.down, id)
		return true
	}
	return false
}

func (c *Client) gotOffer(id, label, source, username, offer, replace string) error {
	abort := func(id string) {
		c.Write(&Message{
			Type: "abort",
			Id:   id,
		})
	}

	if replace != "" {
		c.closeDown(replace)
	}

	pc, err := c.newDown(id)
	if err != nil {
		abort(id)
		return err
	}

	err = pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offer,
	})
	if err != nil {
		abort(id)
		c.closeDown(id)
		return err
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		c.closeDown(id)
		return err
	}

	err = pc.SetLocalDescription(answer)
	if err != nil {
		c.closeDown(id)
		return err
	}

	c.Write(&Message{
		Type: "answer",
		Id:   id,
		SDP:  pc.LocalDescription().SDP,
	})

	c.EventCh <- DownConnEvent{
		Id:   id,
		Conn: pc,
	}

	return nil
}

func (c *Client) gotClose(id string) error {
	found := c.closeDown(id)
	if !found {
		return os.ErrNotExist
	}
	c.EventCh <- CloseEvent{
		Id: id,
	}
	return nil
}

// Request indicates which tracks the server should send.
func (c *Client) Request(request map[string][]string) error {
	return c.Write(&Message{
		Type:    "request",
		Request: request,
	})
}

// Abort requests that the server close a down connection.
func (c *Client) Abort(id string) {
	c.Write(&Message{
		Type: "abort",
		Id:   id,
	})
}

func (c *Client) gotRemoteIce(id string, candidate *webrtc.ICECandidateInit) error {
	if candidate == nil {
		return nil
	}
	c.mu.Lock()
	pc := c.down[id]
	if pc == nil {
		pc = c.up[id]
	}
	c.mu.Unlock()

	if pc == nil {
		return errors.New("got ICE for unknown connection")
	}

	return pc.AddICECandidate(*candidate)
}

// NewUpConn creates a new connection for sending data to the server.
func (c *Client) NewUpConn(id string, pc *webrtc.PeerConnection, label string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	_, ok := c.up[id]
	if ok {
		return os.ErrExist
	}

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		init := candidate.ToJSON()
		c.Write(&Message{
			Type:      "ice",
			Id:        id,
			Candidate: &init,
		})
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}

	err = pc.SetLocalDescription(offer)
	if err != nil {
		return err
	}

	username := c.username
	err = c.write(&Message{
		Type:     "offer",
		Source:   c.Id,
		Username: &username,
		Label:    label,
		Id:       id,
		SDP:      pc.LocalDescription().SDP,
	})
	if err != nil {
		return err
	}
	c.up[id] = pc

	return nil

}

func (c *Client) gotAnswer(id string, sdp string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	pc, ok := c.up[id]
	if !ok {
		return os.ErrNotExist
	}

	return pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	})
}

func (c *Client) gotRenegotiate(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	pc, ok := c.up[id]
	if !ok {
		return os.ErrNotExist
	}

	offer, err := pc.CreateOffer(&webrtc.OfferOptions{
		ICERestart: true,
	})
	if err != nil {
		return err
	}
	err = pc.SetLocalDescription(offer)
	if err != nil {
		return err
	}

	username := c.username
	err = c.write(&Message{
		Type:     "offer",
		Source:   c.Id,
		Username: &username,
		Id:       id,
		SDP:      pc.LocalDescription().SDP,
	})
	if err != nil {
		return err
	}

	return nil
}

// CloseUpConn closes a sending connection.
func (c *Client) CloseUpConn(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	pc, ok := c.up[id]
	if !ok {
		return os.ErrNotExist
	}

	pc.Close()
	delete(c.up, id)
	return c.write(&Message{
		Type: "close",
		Id:   id,
	})
}

// Chat sends a chat message to the server.
func (c *Client) Chat(kind, dest, value string) error {
	username := c.username
	return c.Write(&Message{
		Type:     "chat",
		Kind:     kind,
		Source:   c.Id,
		Username: &username,
		Dest:     dest,
		Value:    value,
	})
}
