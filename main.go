package main

import (
	"bufio"
	"bytes"
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
	udpUserMap  map[string]*net.UDPAddr // userID -> UDP addr
	udpAddrMap  map[string]int          // UDP addr string -> userID

	mu       sync.RWMutex
	udpMu    sync.RWMutex
	callsMu  sync.RWMutex

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
		users:       make(map[int]*User),
		usersByConn: make(map[net.Conn]*User),
		calls:       make(map[string]*Call),
		udpUserMap:  make(map[string]*net.UDPAddr),
		udpAddrMap:  make(map[string]int),
		lastBWReset: time.Now(),
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

	return user
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

	msg := Message{
		SenderID:    sender.ID,
		RecipientID: recipientID,
		Content:     content,
		Timestamp:   time.Now().Format(time.RFC3339),
	}
	msgJSON, _ := json.Marshal(msg)

	if online {
		s.sendToConn(recipient.Conn, "MSG %s", string(msgJSON))
	}

	s.sendToConn(sender.Conn, "OK MSG")
	log.Printf("Message %d -> %d: %d bytes", sender.ID, recipientID, len(content))
}

func (s *Server) handleDisconnect(user *User) {
	s.handleHangup(user) // End any active calls

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

		// Find sender and relay to call partner
		s.udpMu.RLock()
		senderID, ok := s.udpAddrMap[clientAddr.String()]
		s.udpMu.RUnlock()

		if !ok {
			continue // Unknown sender, ignore
		}

		// Find partner in active call
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
