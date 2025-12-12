package main

import (
	"bufio"
	"bytes"
	"hash/crc32"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// === CONFIG ===
const (
	TCP_PORT = 5000
	UDP_PORT = 5001

	// Audio: 48kbps Opus per direction
	AUDIO_BITRATE_KBPS = 48

	// Bandwidth limits (bits per second)
	MAX_UPLOAD_BPS   = 100_000_000 // 100 Mbps
	MAX_DOWNLOAD_BPS = 100_000_000 // 100 Mbps

	// Per call bandwidth: 48kbps in + 48kbps out = 96kbps per participant
	// Server relays: 96kbps received, 96kbps sent per call = 192kbps per call
	BANDWIDTH_PER_CALL_BPS = 192_000

	// Max simultaneous calls = bandwidth / per_call_bandwidth with 20% safety margin
	MAX_CALLS = 400 // ~80% of theoretical 520

	// Heartbeat timeout
	HEARTBEAT_TIMEOUT = 30 * time.Second
	HEARTBEAT_CHECK   = 10 * time.Second

	// UDP packet size limit
	MAX_UDP_PACKET = 1500

	// PHP backend base URL
	PHP_BASE_URL = "https://wlm.fragmount.net/"
)

// === DATA STRUCTURES ===

type User struct {
	ID           int
	Username     string
	SessionToken string
	Conn         net.Conn
	UDPAddr      *net.UDPAddr
	LastHB       time.Time
	Status       string // online, away, busy, offline
	mu           sync.Mutex
}

type Call struct {
	ID         string
	CallerID   int
	ReceiverID int
	StartedAt  time.Time
	Active     bool
}

// GroupCall for multi-user voice in group chats
type GroupCall struct {
	ID          string
	GroupID     int
	Participants map[int]bool // userID -> active
	StartedAt   time.Time
	Active      bool
	mu          sync.RWMutex
}

type Message struct {
	SenderID    int    `json:"sender_id"`
	RecipientID int    `json:"recipient_id"`
	Content     string `json:"content"`
	Timestamp   string `json:"timestamp"`
}

type Server struct {
	users       map[int]*User          // userID -> User
	usersByConn map[net.Conn]*User     // conn -> User
	calls       map[string]*Call       // callID -> Call
	groupCalls  map[int]*GroupCall     // groupID -> GroupCall
	udpUserMap  map[string]*net.UDPAddr // userID -> UDP addr
	udpAddrMap  map[string]int          // UDP addr string -> userID

	// Real-time subscriptions: channelID -> set of userIDs
	subscriptions map[int64]map[int]bool
	// User's subscribed channels: userID -> set of channelIDs
	userSubs      map[int]map[int64]bool
	// User's friends (for status broadcasts): userID -> set of friendIDs
	userFriends   map[int]map[int]bool

	mu          sync.RWMutex
	udpMu       sync.RWMutex
	callsMu     sync.RWMutex
	groupCallMu sync.RWMutex
	subsMu      sync.RWMutex

	// Bandwidth tracking
	uploadBytes   atomic.Int64
	downloadBytes atomic.Int64
	lastBWReset   time.Time
	bwMu          sync.Mutex

	// UDP socket
	udpConn *net.UDPConn
}

// === MAIN ===

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("Whelm Voice Server starting...")
	log.Printf("Config: %dkbps audio, %dMbps up/down limit, max %d calls",
		AUDIO_BITRATE_KBPS, MAX_UPLOAD_BPS/1_000_000, MAX_CALLS)

	srv := &Server{
		users:         make(map[int]*User),
		usersByConn:   make(map[net.Conn]*User),
		calls:         make(map[string]*Call),
		groupCalls:    make(map[int]*GroupCall),
		udpUserMap:    make(map[string]*net.UDPAddr),
		udpAddrMap:    make(map[string]int),
		subscriptions: make(map[int64]map[int]bool),
		userSubs:      make(map[int]map[int64]bool),
		userFriends:   make(map[int]map[int]bool),
		lastBWReset:   time.Now(),
	}

	// Start UDP relay
	go srv.startUDPRelay()

	// Start heartbeat checker
	go srv.heartbeatChecker()

	// Start bandwidth reset ticker
	go srv.bandwidthResetLoop()

	// Start TCP listener
	srv.startTCPListener()
}

// === TCP SIGNALING ===

func (s *Server) startTCPListener() {
	addr := fmt.Sprintf(":%d", TCP_PORT)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("TCP listen failed: %v", err)
	}
	defer listener.Close()
	log.Printf("TCP signaling server listening on %s", addr)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("TCP accept error: %v", err)
			continue
		}
		go s.handleTCPClient(conn)
	}
}

func (s *Server) handleTCPClient(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	var user *User

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, " ", 2)
		cmd := parts[0]
		var payload string
		if len(parts) > 1 {
			payload = parts[1]
		}

		switch cmd {
		case "AUTH":
			// AUTH <session_token>
			user = s.handleAuth(conn, payload)
			if user != nil {
				s.sendToConn(conn, "OK AUTH %d %s", user.ID, user.Username)
			} else {
				s.sendToConn(conn, "ERROR Invalid token")
			}

		case "HEARTBEAT":
			// HEARTBEAT [status]
			if user != nil {
				user.mu.Lock()
				user.LastHB = time.Now()
				if payload != "" {
					user.Status = payload
				}
				user.mu.Unlock()
				s.updateUserStatus(user.ID, user.Status)
				s.sendToConn(conn, "OK HEARTBEAT")
			}

		case "CALL":
			// CALL <target_user_id>
			if user == nil {
				s.sendToConn(conn, "ERROR Not authenticated")
				continue
			}
			targetID, _ := strconv.Atoi(payload)
			s.handleCall(user, targetID)

		case "ACCEPT":
			// ACCEPT <caller_id>
			if user == nil {
				s.sendToConn(conn, "ERROR Not authenticated")
				continue
			}
			callerID, _ := strconv.Atoi(payload)
			s.handleAccept(user, callerID)

		case "REJECT":
			// REJECT <caller_id>
			if user == nil {
				s.sendToConn(conn, "ERROR Not authenticated")
				continue
			}
			callerID, _ := strconv.Atoi(payload)
			s.handleReject(user, callerID)

		case "HANGUP":
			// HANGUP
			if user != nil {
				s.handleHangup(user)
			}

		case "MSG":
			// MSG <recipient_id> <content>
			if user == nil {
				s.sendToConn(conn, "ERROR Not authenticated")
				continue
			}
			msgParts := strings.SplitN(payload, " ", 2)
			if len(msgParts) < 2 {
				s.sendToConn(conn, "ERROR Invalid MSG format")
				continue
			}
			recipientID, _ := strconv.Atoi(msgParts[0])
			content := msgParts[1]
			s.handleMessage(user, recipientID, content)

		case "JOIN_GROUP_CALL":
			// JOIN_GROUP_CALL <group_id>
			if user == nil {
				s.sendToConn(conn, "ERROR Not authenticated")
				continue
			}
			groupID, _ := strconv.Atoi(payload)
			s.handleJoinGroupCall(user, groupID)

		case "LEAVE_GROUP_CALL":
			// LEAVE_GROUP_CALL <group_id>
			if user == nil {
				s.sendToConn(conn, "ERROR Not authenticated")
				continue
			}
			groupID, _ := strconv.Atoi(payload)
			s.handleLeaveGroupCall(user, groupID)

		case "SUBSCRIBE":
			// SUBSCRIBE <channel_id> - subscribe to real-time updates for a channel
			if user == nil {
				s.sendToConn(conn, "ERROR Not authenticated")
				continue
			}
			channelID, _ := strconv.ParseInt(payload, 10, 64)
			s.handleSubscribe(user, channelID)

		case "UNSUBSCRIBE":
			// UNSUBSCRIBE <channel_id>
			if user == nil {
				s.sendToConn(conn, "ERROR Not authenticated")
				continue
			}
			channelID, _ := strconv.ParseInt(payload, 10, 64)
			s.handleUnsubscribe(user, channelID)

		case "GROUP_MSG":
			// GROUP_MSG <group_id> <content> - send message to group with real-time relay
			if user == nil {
				s.sendToConn(conn, "ERROR Not authenticated")
				continue
			}
			msgParts := strings.SplitN(payload, " ", 2)
			if len(msgParts) < 2 {
				s.sendToConn(conn, "ERROR Invalid GROUP_MSG format")
				continue
			}
			groupID, _ := strconv.Atoi(msgParts[0])
			content := msgParts[1]
			s.handleGroupMessage(user, groupID, content)

		case "STATUS":
			// STATUS <status> - broadcast status change to connected friends
			if user == nil {
				s.sendToConn(conn, "ERROR Not authenticated")
				continue
			}
			s.handleStatusBroadcast(user, payload)

		case "TYPING":
			// TYPING <channel_id> - notify typing indicator
			if user == nil {
				s.sendToConn(conn, "ERROR Not authenticated")
				continue
			}
			channelID, _ := strconv.ParseInt(payload, 10, 64)
			s.handleTyping(user, channelID)

		case "INVITE_NOTIFY":
			// INVITE_NOTIFY <user_id> <group_id> <group_name> - notify user of group invite
			if user == nil {
				s.sendToConn(conn, "ERROR Not authenticated")
				continue
			}
			parts := strings.SplitN(payload, " ", 3)
			if len(parts) < 3 {
				s.sendToConn(conn, "ERROR Invalid INVITE_NOTIFY format")
				continue
			}
			targetID, _ := strconv.Atoi(parts[0])
			groupID, _ := strconv.Atoi(parts[1])
			groupName := parts[2]
			s.handleInviteNotify(user, targetID, groupID, groupName)

		case "PROFILE_UPDATE":
			// PROFILE_UPDATE <json> - broadcast profile changes to friends
			if user == nil {
				s.sendToConn(conn, "ERROR Not authenticated")
				continue
			}
			s.handleProfileUpdate(user, payload)

		default:
			s.sendToConn(conn, "ERROR Unknown command: %s", cmd)
		}
	}

	// Cleanup on disconnect
	if user != nil {
		s.handleDisconnect(user)
	}
}

func (s *Server) handleAuth(conn net.Conn, token string) *User {
	if token == "" {
		return nil
	}

	userID, username, err := s.fetchUserFromPHP(token)
	if err != nil {
		log.Printf("Auth failed for token via PHP: %v", err)
		return nil
	}

	user := &User{
		ID:           userID,
		Username:     username,
		SessionToken: token,
		Conn:         conn,
		LastHB:       time.Now(),
		Status:       "online",
	}

	s.mu.Lock()
	// Kick existing connection if any
	if existing, ok := s.users[userID]; ok {
		s.sendToConn(existing.Conn, "KICKED Another session connected")
		existing.Conn.Close()
		delete(s.usersByConn, existing.Conn)
	}
	s.users[userID] = user
	s.usersByConn[conn] = user
	s.mu.Unlock()

	s.updateUserStatus(userID, "online")
	log.Printf("User %d (%s) authenticated", userID, username)

	// Load friends list for status broadcasts, then notify them we're online
	go func() {
		s.loadUserFriends(userID, token)
		// Notify friends about this user's online status.
		s.broadcastFriendOnline(user)
		// Also tell the newly-authenticated user which of their friends are already online.
		s.sendOnlineFriendsToUser(user)
	}()

	return user
}

func (s *Server) sendOnlineFriendsToUser(user *User) {
	s.subsMu.RLock()
	friends := s.userFriends[user.ID]
	s.subsMu.RUnlock()

	if len(friends) == 0 {
		return
	}

	s.mu.RLock()
	for friendID := range friends {
		friend, ok := s.users[friendID]
		if !ok {
			continue
		}

		friend.mu.Lock()
		status := friend.Status
		friend.mu.Unlock()

		onlineData := map[string]interface{}{
			"user_id":  friend.ID,
			"username": friend.Username,
			"status":   status,
		}
		onlineJSON, _ := json.Marshal(onlineData)
		s.sendToConn(user.Conn, "FRIEND_ONLINE %s", string(onlineJSON))
	}
	s.mu.RUnlock()
}

func (s *Server) handleCall(caller *User, targetID int) {
	s.callsMu.RLock()
	callCount := len(s.calls)
	s.callsMu.RUnlock()

	// Check call limit - the "fuck off" error
	if callCount >= MAX_CALLS {
		s.sendToConn(caller.Conn, "ERROR Server at capacity, try again later")
		return
	}

	// Check bandwidth
	if !s.checkBandwidth() {
		s.sendToConn(caller.Conn, "ERROR Server bandwidth exceeded, try again later")
		return
	}

	s.mu.RLock()
	target, online := s.users[targetID]
	s.mu.RUnlock()

	if !online {
		s.sendToConn(caller.Conn, "ERROR User offline")
		return
	}

	// Check if either party is already in a call
	if s.userInCall(caller.ID) || s.userInCall(targetID) {
		s.sendToConn(caller.Conn, "ERROR User busy")
		return
	}

	// Send incoming call notification
	s.sendToConn(target.Conn, "INCOMING %d %s", caller.ID, caller.Username)
	s.sendToConn(caller.Conn, "OK CALLING %d", targetID)

	log.Printf("Call request: %d -> %d", caller.ID, targetID)
}

func (s *Server) handleAccept(receiver *User, callerID int) {
	s.mu.RLock()
	caller, ok := s.users[callerID]
	s.mu.RUnlock()

	if !ok {
		s.sendToConn(receiver.Conn, "ERROR Caller offline")
		return
	}

	// Create call
	callID := fmt.Sprintf("%d_%d_%d", callerID, receiver.ID, time.Now().Unix())
	call := &Call{
		ID:         callID,
		CallerID:   callerID,
		ReceiverID: receiver.ID,
		StartedAt:  time.Now(),
		Active:     true,
	}

	s.callsMu.Lock()
	s.calls[callID] = call
	s.callsMu.Unlock()

	// Notify both parties
	s.sendToConn(caller.Conn, "START_CALL %d %s", receiver.ID, receiver.Username)
	s.sendToConn(receiver.Conn, "START_CALL %d %s", callerID, caller.Username)

	log.Printf("Call started: %s (%d <-> %d)", callID, callerID, receiver.ID)
}

func (s *Server) handleReject(receiver *User, callerID int) {
	s.mu.RLock()
	caller, ok := s.users[callerID]
	s.mu.RUnlock()

	if ok {
		s.sendToConn(caller.Conn, "REJECTED %d", receiver.ID)
	}
	s.sendToConn(receiver.Conn, "OK REJECTED")

	log.Printf("Call rejected: %d rejected %d", receiver.ID, callerID)
}

func (s *Server) handleHangup(user *User) {
	s.callsMu.Lock()
	defer s.callsMu.Unlock()

	for callID, call := range s.calls {
		if call.CallerID == user.ID || call.ReceiverID == user.ID {
			call.Active = false
			delete(s.calls, callID)

			// Notify the other party
			var otherID int
			if call.CallerID == user.ID {
				otherID = call.ReceiverID
			} else {
				otherID = call.CallerID
			}

			s.mu.RLock()
			other, ok := s.users[otherID]
			s.mu.RUnlock()

			if ok {
				s.sendToConn(other.Conn, "HANGUP %d", user.ID)
			}

			log.Printf("Call ended: %s", callID)
			break
		}
	}

	s.sendToConn(user.Conn, "OK HANGUP")
}

// === GROUP VOICE CALLS ===

func (s *Server) handleJoinGroupCall(user *User, groupID int) {
	if groupID <= 0 {
		s.sendToConn(user.Conn, "ERROR Invalid group ID")
		return
	}

	// Check if user is already in a 1:1 call
	if s.userInCall(user.ID) {
		s.sendToConn(user.Conn, "ERROR Already in a call")
		return
	}

	s.groupCallMu.Lock()
	gc, exists := s.groupCalls[groupID]
	if !exists {
		// Create new group call
		gc = &GroupCall{
			ID:           fmt.Sprintf("gc_%d_%d", groupID, time.Now().Unix()),
			GroupID:      groupID,
			Participants: make(map[int]bool),
			StartedAt:    time.Now(),
			Active:       true,
		}
		s.groupCalls[groupID] = gc
	}
	s.groupCallMu.Unlock()

	gc.mu.Lock()
	gc.Participants[user.ID] = true
	participantCount := len(gc.Participants)
	gc.mu.Unlock()

	// Notify user they joined
	s.sendToConn(user.Conn, "OK JOIN_GROUP_CALL %d %d", groupID, participantCount)

	// Notify other participants that someone joined
	gc.mu.RLock()
	for uid := range gc.Participants {
		if uid != user.ID {
			s.mu.RLock()
			other, ok := s.users[uid]
			s.mu.RUnlock()
			if ok {
				s.sendToConn(other.Conn, "GROUP_CALL_JOIN %d %d %s", groupID, user.ID, user.Username)
			}
		}
	}
	gc.mu.RUnlock()

	log.Printf("User %d joined group call %d (now %d participants)", user.ID, groupID, participantCount)
}

func (s *Server) handleLeaveGroupCall(user *User, groupID int) {
	s.groupCallMu.RLock()
	gc, exists := s.groupCalls[groupID]
	s.groupCallMu.RUnlock()

	if !exists {
		s.sendToConn(user.Conn, "ERROR Not in this group call")
		return
	}

	gc.mu.Lock()
	delete(gc.Participants, user.ID)
	remaining := len(gc.Participants)
	gc.mu.Unlock()

	s.sendToConn(user.Conn, "OK LEAVE_GROUP_CALL %d", groupID)

	// Notify others
	gc.mu.RLock()
	for uid := range gc.Participants {
		s.mu.RLock()
		other, ok := s.users[uid]
		s.mu.RUnlock()
		if ok {
			s.sendToConn(other.Conn, "GROUP_CALL_LEAVE %d %d", groupID, user.ID)
		}
	}
	gc.mu.RUnlock()

	// Clean up if empty
	if remaining == 0 {
		s.groupCallMu.Lock()
		delete(s.groupCalls, groupID)
		s.groupCallMu.Unlock()
		log.Printf("Group call %d ended (no participants)", groupID)
	}

	log.Printf("User %d left group call %d (%d remaining)", user.ID, groupID, remaining)
}

func (s *Server) getGroupCallParticipants(userID int) (int, []int) {
	// Returns groupID and list of other participant IDs if user is in a group call
	s.groupCallMu.RLock()
	defer s.groupCallMu.RUnlock()

	for groupID, gc := range s.groupCalls {
		gc.mu.RLock()
		if gc.Participants[userID] {
			var others []int
			for uid := range gc.Participants {
				if uid != userID {
					others = append(others, uid)
				}
			}
			gc.mu.RUnlock()
			return groupID, others
		}
		gc.mu.RUnlock()
	}
	return 0, nil
}

func (s *Server) handleMessage(sender *User, recipientID int, content string) {
	// Persist via PHP backend and try to deliver in real-time
	if strings.TrimSpace(content) == "" {
		s.sendToConn(sender.Conn, "ERROR Empty message")
		return
	}

	if sender.SessionToken == "" {
		s.sendToConn(sender.Conn, "ERROR No session token")
		return
	}

	payload := map[string]interface{}{
		"token":        sender.SessionToken,
		"recipient_id": recipientID,
		"content":      content,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Failed to marshal send_message payload: %v", err)
		s.sendToConn(sender.Conn, "ERROR Failed to send message")
		return
	}

	req, err := http.NewRequest("POST", PHP_BASE_URL+"send_message.php", bytes.NewReader(body))
	if err != nil {
		log.Printf("Failed to create send_message request: %v", err)
		s.sendToConn(sender.Conn, "ERROR Failed to send message")
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("send_message.php HTTP error: %v", err)
		s.sendToConn(sender.Conn, "ERROR Failed to send message")
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("send_message.php read error: %v", err)
		s.sendToConn(sender.Conn, "ERROR Failed to send message")
		return
	}

	var phpResp struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(respBody, &phpResp); err != nil {
		log.Printf("send_message.php JSON error: %v - body: %s", err, string(respBody))
		s.sendToConn(sender.Conn, "ERROR Failed to send message")
		return
	}
	if !phpResp.Success {
		log.Printf("send_message.php returned error: %s", phpResp.Error)
		s.sendToConn(sender.Conn, "ERROR "+phpResp.Error)
		return
	}

	// Try to deliver in real-time if recipient is online
	s.mu.RLock()
	recipient, online := s.users[recipientID]
	s.mu.RUnlock()

	// Channel ID must match PHP (send_message.php) behavior: crc32("min_max")
	minID := sender.ID
	maxID := recipientID
	if minID > maxID {
		minID, maxID = maxID, minID
	}
	key := fmt.Sprintf("%d_%d", minID, maxID)
	channelID := int64(crc32.ChecksumIEEE([]byte(key)))

	msgData := map[string]interface{}{
		"sender_id":   sender.ID,
		"sender_name": sender.Username,
		"recipient_id": recipientID,
		"channel_id":  channelID,
		"content":     content,
		"timestamp":   time.Now().Format(time.RFC3339),
	}
	msgJSON, _ := json.Marshal(msgData)

	if online {
		// Client expects NEW_MSG
		s.sendToConn(recipient.Conn, "NEW_MSG %s", string(msgJSON))
	}

	s.sendToConn(sender.Conn, "OK MSG")
	log.Printf("Message %d -> %d: %d bytes", sender.ID, recipientID, len(content))
}

func (s *Server) handleDisconnect(user *User) {
	s.handleHangup(user) // End any active 1:1 calls

	// Leave any group calls
	s.groupCallMu.RLock()
	for groupID, gc := range s.groupCalls {
		gc.mu.RLock()
		inCall := gc.Participants[user.ID]
		gc.mu.RUnlock()
		if inCall {
			s.groupCallMu.RUnlock()
			s.handleLeaveGroupCall(user, groupID)
			s.groupCallMu.RLock()
		}
	}
	s.groupCallMu.RUnlock()

	s.mu.Lock()
	delete(s.users, user.ID)
	delete(s.usersByConn, user.Conn)
	s.mu.Unlock()

	s.udpMu.Lock()
	key := strconv.Itoa(user.ID)
	delete(s.udpUserMap, key)
	// Clean up reverse map
	for addr, uid := range s.udpAddrMap {
		if uid == user.ID {
			delete(s.udpAddrMap, addr)
			break
		}
	}
	s.udpMu.Unlock()

	// Broadcast offline to friends BEFORE cleanup (need friends list)
	s.broadcastFriendOffline(user)

	// Cleanup channel subscriptions and friends cache
	s.cleanupUserSubscriptions(user.ID)

	s.updateUserStatus(user.ID, "offline")
	log.Printf("User %d disconnected", user.ID)
}

// === UDP VOICE RELAY ===

func (s *Server) startUDPRelay() {
	addr := &net.UDPAddr{Port: UDP_PORT}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Fatalf("UDP listen failed: %v", err)
	}
	s.udpConn = conn
	log.Printf("UDP voice relay listening on :%d", UDP_PORT)

	buf := make([]byte, MAX_UDP_PACKET)

	for {
		n, clientAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("UDP read error: %v", err)
			continue
		}

		// Track bandwidth
		s.uploadBytes.Add(int64(n))

		// Handle HELLO registration: "HELLO <user_id>"
		if n > 6 && string(buf[:6]) == "HELLO " {
			userIDStr := strings.TrimSpace(string(buf[6:n]))
			userID, err := strconv.Atoi(userIDStr)
			if err != nil {
				continue
			}

			s.udpMu.Lock()
			s.udpUserMap[userIDStr] = clientAddr
			s.udpAddrMap[clientAddr.String()] = userID
			s.udpMu.Unlock()

			log.Printf("UDP registered: user %d from %s", userID, clientAddr)
			continue
		}

		// Find sender and relay to call partner(s)
		s.udpMu.RLock()
		senderID, ok := s.udpAddrMap[clientAddr.String()]
		s.udpMu.RUnlock()

		if !ok {
			continue // Unknown sender, ignore
		}

		// Check if in a group call first
		groupID, groupPartners := s.getGroupCallParticipants(senderID)
		if groupID > 0 && len(groupPartners) > 0 {
			// Relay to all group call participants
			for _, partnerID := range groupPartners {
				s.udpMu.RLock()
				partnerAddr, ok := s.udpUserMap[strconv.Itoa(partnerID)]
				s.udpMu.RUnlock()

				if ok && partnerAddr != nil {
					written, err := conn.WriteToUDP(buf[:n], partnerAddr)
					if err != nil {
						log.Printf("UDP group relay error: %v", err)
						continue
					}
					s.downloadBytes.Add(int64(written))
				}
			}
			continue
		}

		// Find partner in active 1:1 call
		partnerID := s.getCallPartner(senderID)
		if partnerID == 0 {
			continue // Not in a call
		}

		// Get partner's UDP address
		s.udpMu.RLock()
		partnerAddr, ok := s.udpUserMap[strconv.Itoa(partnerID)]
		s.udpMu.RUnlock()

		if !ok || partnerAddr == nil {
			continue // Partner not registered for UDP
		}

		// Relay the packet
		written, err := conn.WriteToUDP(buf[:n], partnerAddr)
		if err != nil {
			log.Printf("UDP relay error: %v", err)
			continue
		}
		s.downloadBytes.Add(int64(written))
	}
}

func (s *Server) getCallPartner(userID int) int {
	s.callsMu.RLock()
	defer s.callsMu.RUnlock()

	for _, call := range s.calls {
		if !call.Active {
			continue
		}
		if call.CallerID == userID {
			return call.ReceiverID
		}
		if call.ReceiverID == userID {
			return call.CallerID
		}
	}
	return 0
}

// === HEARTBEAT & STATUS ===

func (s *Server) heartbeatChecker() {
	ticker := time.NewTicker(HEARTBEAT_CHECK)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		var toDisconnect []*User

		s.mu.RLock()
		for _, user := range s.users {
			user.mu.Lock()
			if now.Sub(user.LastHB) > HEARTBEAT_TIMEOUT {
				toDisconnect = append(toDisconnect, user)
			}
			user.mu.Unlock()
		}
		s.mu.RUnlock()

		for _, user := range toDisconnect {
			log.Printf("User %d timed out (no heartbeat)", user.ID)
			user.Conn.Close()
			s.handleDisconnect(user)
		}
	}
}

func (s *Server) updateUserStatus(userID int, status string) {
	// Inform PHP backend about status via heartbeat.php
	s.mu.RLock()
	user, ok := s.users[userID]
	s.mu.RUnlock()
	if !ok || user.SessionToken == "" {
		return
	}

	payload := map[string]interface{}{
		"token": user.SessionToken,
	}
	if status != "" {
		payload["status"] = status
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Failed to marshal heartbeat payload: %v", err)
		return
	}

	req, err := http.NewRequest("POST", PHP_BASE_URL+"heartbeat.php", bytes.NewReader(body))
	if err != nil {
		log.Printf("Failed to create heartbeat request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("heartbeat.php HTTP error: %v", err)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// === PHP BACKEND HELPERS ===

func (s *Server) fetchUserFromPHP(token string) (int, string, error) {
	payload := map[string]string{
		"token": token,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, "", fmt.Errorf("marshal whoami payload: %w", err)
	}

	req, err := http.NewRequest("POST", PHP_BASE_URL+"whoami.php", bytes.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("create whoami request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("whoami HTTP error: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", fmt.Errorf("whoami read error: %w", err)
	}

	var phpResp struct {
		Success  bool   `json:"success"`
		ID       int    `json:"id"`
		Username string `json:"username"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(respBody, &phpResp); err != nil {
		return 0, "", fmt.Errorf("whoami JSON error: %w (body: %s)", err, string(respBody))
	}
	if !phpResp.Success {
		return 0, "", fmt.Errorf("whoami error: %s", phpResp.Error)
	}

	return phpResp.ID, phpResp.Username, nil
}

// === BANDWIDTH MANAGEMENT ===

func (s *Server) checkBandwidth() bool {
	s.bwMu.Lock()
	defer s.bwMu.Unlock()

	// Check if we're within limits
	upload := s.uploadBytes.Load()
	download := s.downloadBytes.Load()

	elapsed := time.Since(s.lastBWReset).Seconds()
	if elapsed < 1 {
		elapsed = 1
	}

	uploadRate := float64(upload*8) / elapsed
	downloadRate := float64(download*8) / elapsed

	return uploadRate < float64(MAX_UPLOAD_BPS)*0.9 && downloadRate < float64(MAX_DOWNLOAD_BPS)*0.9
}

func (s *Server) bandwidthResetLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		s.bwMu.Lock()
		s.uploadBytes.Store(0)
		s.downloadBytes.Store(0)
		s.lastBWReset = time.Now()
		s.bwMu.Unlock()
	}
}

// === HELPERS ===

func (s *Server) sendToConn(conn net.Conn, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...) + "\n"
	conn.Write([]byte(msg))
}

func (s *Server) userInCall(userID int) bool {
	s.callsMu.RLock()
	defer s.callsMu.RUnlock()

	for _, call := range s.calls {
		if call.Active && (call.CallerID == userID || call.ReceiverID == userID) {
			return true
		}
	}
	return false
}

func (s *Server) getChannelID(user1, user2 int) int64 {
	// Same algorithm as PHP backend
	if user1 > user2 {
		user1, user2 = user2, user1
	}
	key := fmt.Sprintf("%d_%d", user1, user2)
	
	// Simple CRC32-like hash
	var hash int64
	for _, c := range key {
		hash = hash*31 + int64(c)
	}
	return hash & 0x7FFFFFFF
}

// === REAL-TIME MESSAGING ===

func (s *Server) handleSubscribe(user *User, channelID int64) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()

	// Add user to channel subscribers
	if s.subscriptions[channelID] == nil {
		s.subscriptions[channelID] = make(map[int]bool)
	}
	s.subscriptions[channelID][user.ID] = true

	// Track user's subscriptions
	if s.userSubs[user.ID] == nil {
		s.userSubs[user.ID] = make(map[int64]bool)
	}
	s.userSubs[user.ID][channelID] = true

	s.sendToConn(user.Conn, "OK SUBSCRIBE %d", channelID)
	log.Printf("User %d subscribed to channel %d", user.ID, channelID)
}

func (s *Server) handleUnsubscribe(user *User, channelID int64) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()

	if subs, ok := s.subscriptions[channelID]; ok {
		delete(subs, user.ID)
		if len(subs) == 0 {
			delete(s.subscriptions, channelID)
		}
	}

	if userChannels, ok := s.userSubs[user.ID]; ok {
		delete(userChannels, channelID)
	}

	s.sendToConn(user.Conn, "OK UNSUBSCRIBE %d", channelID)
}

func (s *Server) handleGroupMessage(user *User, groupID int, content string) {
	// Channel ID for groups is negative
	channelID := int64(-groupID)

	// Store message via PHP backend
	go s.storeGroupMessagePHP(user.SessionToken, groupID, content)

	// Build message JSON for real-time delivery
	msgData := map[string]interface{}{
		"sender_id":   user.ID,
		"sender_name": user.Username,
		"group_id":    groupID,
		"channel_id":  channelID,
		"content":     content,
		"timestamp":   time.Now().Format(time.RFC3339),
	}
	msgJSON, _ := json.Marshal(msgData)

	// Relay to all subscribers of this channel
	s.subsMu.RLock()
	subscribers := s.subscriptions[channelID]
	s.subsMu.RUnlock()

	s.mu.RLock()
	for subUserID := range subscribers {
		if subUserID == user.ID {
			continue // Don't echo back to sender
		}
		if subUser, ok := s.users[subUserID]; ok {
			s.sendToConn(subUser.Conn, "NEW_MSG %s", string(msgJSON))
		}
	}
	s.mu.RUnlock()

	s.sendToConn(user.Conn, "OK GROUP_MSG")
}

func (s *Server) handleStatusBroadcast(user *User, status string) {
	user.mu.Lock()
	user.Status = status
	user.mu.Unlock()

	// Update status in PHP backend
	s.updateUserStatus(user.ID, status)

	// Broadcast to online friends
	s.subsMu.RLock()
	friends := s.userFriends[user.ID]
	s.subsMu.RUnlock()

	statusData := map[string]interface{}{
		"user_id":  user.ID,
		"username": user.Username,
		"status":   status,
	}
	statusJSON, _ := json.Marshal(statusData)

	s.mu.RLock()
	for friendID := range friends {
		if friend, ok := s.users[friendID]; ok {
			s.sendToConn(friend.Conn, "STATUS_UPDATE %s", string(statusJSON))
		}
	}
	s.mu.RUnlock()

	s.sendToConn(user.Conn, "OK STATUS")
}

func (s *Server) handleTyping(user *User, channelID int64) {
	typingData := map[string]interface{}{
		"user_id":    user.ID,
		"username":   user.Username,
		"channel_id": channelID,
	}
	typingJSON, _ := json.Marshal(typingData)

	// Relay to all subscribers of this channel
	s.subsMu.RLock()
	subscribers := s.subscriptions[channelID]
	s.subsMu.RUnlock()

	s.mu.RLock()
	for subUserID := range subscribers {
		if subUserID == user.ID {
			continue
		}
		if subUser, ok := s.users[subUserID]; ok {
			s.sendToConn(subUser.Conn, "TYPING %s", string(typingJSON))
		}
	}
	s.mu.RUnlock()
}

func (s *Server) handleInviteNotify(sender *User, targetID int, groupID int, groupName string) {
	s.mu.RLock()
	target, online := s.users[targetID]
	s.mu.RUnlock()

	if !online {
		s.sendToConn(sender.Conn, "OK INVITE_NOTIFY offline")
		return
	}

	inviteData := map[string]interface{}{
		"inviter_id":   sender.ID,
		"inviter_name": sender.Username,
		"group_id":     groupID,
		"group_name":   groupName,
	}
	inviteJSON, _ := json.Marshal(inviteData)

	s.sendToConn(target.Conn, "INVITE %s", string(inviteJSON))
	s.sendToConn(sender.Conn, "OK INVITE_NOTIFY")
}

func (s *Server) handleProfileUpdate(user *User, jsonPayload string) {
	// Broadcast profile changes to online friends
	s.subsMu.RLock()
	friends := s.userFriends[user.ID]
	s.subsMu.RUnlock()

	profileData := map[string]interface{}{
		"user_id": user.ID,
		"update":  jsonPayload,
	}
	profileJSON, _ := json.Marshal(profileData)

	s.mu.RLock()
	for friendID := range friends {
		if friend, ok := s.users[friendID]; ok {
			s.sendToConn(friend.Conn, "PROFILE_UPDATE %s", string(profileJSON))
		}
	}
	s.mu.RUnlock()

	s.sendToConn(user.Conn, "OK PROFILE_UPDATE")
}

func (s *Server) storeGroupMessagePHP(token string, groupID int, content string) {
	payload := map[string]interface{}{
		"token":    token,
		"group_id": groupID,
		"content":  content,
	}
	jsonData, _ := json.Marshal(payload)

	resp, err := http.Post(PHP_BASE_URL+"send_group_message.php", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		log.Printf("Failed to store group message via PHP: %v", err)
		return
	}
	defer resp.Body.Close()
}

func (s *Server) loadUserFriends(userID int, token string) {
	client := &http.Client{Timeout: 10 * time.Second}
	url := fmt.Sprintf("%sget_friends.php?debug_user_id=%d", PHP_BASE_URL, userID)

	maxAttempts := 3
	backoff := 300 * time.Millisecond

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := client.Get(url)
		if err != nil {
			log.Printf("Failed to fetch friends for user %d (attempt %d/%d): %v", userID, attempt, maxAttempts, err)
		} else {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode != 200 {
				log.Printf("get_friends.php non-200 for user %d (attempt %d/%d): HTTP %d", userID, attempt, maxAttempts, resp.StatusCode)
			} else {
				var result struct {
					Success bool `json:"success"`
					Friends []struct {
						ID               int    `json:"id"`
						FriendshipStatus string `json:"friendship_status"`
					} `json:"friends"`
				}

				if err := json.Unmarshal(body, &result); err != nil {
					log.Printf("Failed to parse friends response for user %d (attempt %d/%d): %v", userID, attempt, maxAttempts, err)
				} else if !result.Success {
					log.Printf("get_friends.php returned success=false for user %d (attempt %d/%d)", userID, attempt, maxAttempts)
				} else {
					newFriends := make(map[int]bool)
					for _, friend := range result.Friends {
						if friend.FriendshipStatus == "accepted" {
							newFriends[friend.ID] = true
						}
					}

					s.subsMu.Lock()
					s.userFriends[userID] = newFriends
					s.subsMu.Unlock()
					log.Printf("Loaded %d accepted friends for user %d", len(newFriends), userID)
					return
				}
			}
		}

		if attempt < maxAttempts {
			time.Sleep(backoff)
			backoff *= 2
		}
	}

	// IONOS is flaky - ALWAYS use fallback mode so presence/calls work
	log.Printf("IONOS failed for user %d, using fallback friends (all connected users)", userID)
	s.setFallbackFriendsForUser(userID)
}

func (s *Server) setFallbackFriendsForUser(userID int) {
	newFriends := make(map[int]bool)

	s.mu.RLock()
	for otherID := range s.users {
		if otherID == userID {
			continue
		}
		newFriends[otherID] = true
	}
	s.mu.RUnlock()

	s.subsMu.Lock()
	if len(newFriends) > 0 {
		s.userFriends[userID] = newFriends
	}
	s.subsMu.Unlock()
}

func (s *Server) cleanupUserSubscriptions(userID int) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()

	// Remove from all channel subscriptions
	if channels, ok := s.userSubs[userID]; ok {
		for channelID := range channels {
			if subs, ok := s.subscriptions[channelID]; ok {
				delete(subs, userID)
				if len(subs) == 0 {
					delete(s.subscriptions, channelID)
				}
			}
		}
		delete(s.userSubs, userID)
	}

	// Remove friends cache
	delete(s.userFriends, userID)
}

func (s *Server) broadcastFriendOnline(user *User) {
	s.subsMu.RLock()
	friends := s.userFriends[user.ID]
	s.subsMu.RUnlock()

	if len(friends) == 0 {
		return
	}

	user.mu.Lock()
	status := user.Status
	user.mu.Unlock()

	onlineData := map[string]interface{}{
		"user_id":  user.ID,
		"username": user.Username,
		"status":   status,
	}
	onlineJSON, _ := json.Marshal(onlineData)

	s.mu.RLock()
	for friendID := range friends {
		if friend, ok := s.users[friendID]; ok {
			s.sendToConn(friend.Conn, "FRIEND_ONLINE %s", string(onlineJSON))
		}
	}
	s.mu.RUnlock()

	log.Printf("Broadcast FRIEND_ONLINE for user %d to %d friends", user.ID, len(friends))
}

func (s *Server) broadcastFriendOffline(user *User) {
	s.subsMu.RLock()
	friends := s.userFriends[user.ID]
	s.subsMu.RUnlock()

	if len(friends) == 0 {
		return
	}

	offlineData := map[string]interface{}{
		"user_id":  user.ID,
		"username": user.Username,
	}
	offlineJSON, _ := json.Marshal(offlineData)

	s.mu.RLock()
	for friendID := range friends {
		if friend, ok := s.users[friendID]; ok {
			s.sendToConn(friend.Conn, "FRIEND_OFFLINE %s", string(offlineJSON))
		}
	}
	s.mu.RUnlock()

	log.Printf("Broadcast FRIEND_OFFLINE for user %d to %d friends", user.ID, len(friends))
}
