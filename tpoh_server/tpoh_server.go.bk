package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"io"
	"os/exec"
	"encoding/base64"
	"context"
	"encoding/json"
	"sync"
	"github.com/redis/go-redis/v9"
	"crypto/tls"
	. "app/localprinter/cc"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	
)

// Config file format: [incomingPort] [relayid] [host:port]
type ProxyType int
const (
	ProxyPort ProxyType = iota
	ProxyDomain
)

type configEntry struct {
	ProxyType    ProxyType
	ListenPort   int      // Only valid for ProxyPort
	Domain       string   // For ProxyDomain
	ClientID      string
	Forward      string // host:port only
	TLS          bool
	RequiresClientCert bool
}

// Reads config from a text file, each line: [listenPort] [relayid] [host:port]
func loadConfig(path string) ([]configEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []configEntry
	sc := bufio.NewScanner(f)
	lineno := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		lineno++
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			return nil, fmt.Errorf("bad config line %d: %q", lineno, line)
		}
		first := parts[0]
		var entry configEntry
		// Identify full domains as "anything containing a dot" for HTTP(S) routing
		if strings.Contains(first, ".") {
			entry.ProxyType = ProxyDomain
			entry.Domain = first
		} else {
			p, err := strconv.Atoi(first)
			if err != nil {
				return nil, fmt.Errorf("bad port on line %d: %v", lineno, err)
			}
			entry.ProxyType = ProxyPort
			entry.ListenPort = p
		}
		entry.ClientID = parts[1]
		// ---- Now parse Forward and ForwardPath
		forward := parts[2]
		entry.Forward = forward
		if entry.ProxyType == ProxyDomain {
			lowerForward := strings.ToLower(forward)
			if strings.HasPrefix(lowerForward, "tls://") {
				entry.Forward = forward[6:]
				entry.TLS = true
			}
		}
		
		if len(parts) >= 4 {
		    if parts[3] == "client_cert:true" {
		        entry.RequiresClientCert = true
		    }
		}
		
		entries = append(entries, entry)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

type Message struct {
	Action string
	ConnID string
	Addr   string
	Data   string // base64
	Error  string
}

type Conn struct {
    *net.TCPConn
    ID string
    ClientID string
    
    SourceReaderClosed bool
    DestReaderClosed bool
    LastActive time.Time
}

// TODO: close after inactivity
type TPOHClient struct {
    ClientID string
    MessagesFromSource []*Message
    MessagesFromSourceSignaler *Signaler
    MessagesFromDest []*Message
    MessagesFromDestSignaler *Signaler
    LastActive time.Time
    
	ProxyType    ProxyType
	ListenPort   int      // Only valid for ProxyPort
	Domain       string   // For ProxyDomain
	ForwardAddr      string // host:port only
	TLS          bool
	RequiresClientCert bool
}

type Server struct {
    Conns map[string]*Conn
    ConfigEntries []configEntry
    TPOHClients map[string]*TPOHClient
    NextConnID int
}

func (s *Server) SpawnListener(entry configEntry) {
	if entry.ProxyType != ProxyPort {
		// Only Port entries use spawnListener; Domain proxying is handled elsewhere
		return
	}
	addr := fmt.Sprintf("0.0.0.0:%d", entry.ListenPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen on %s failed: %v", addr, err)
	}
	log.Printf("[init] Listening on %s, clientid=%s, forward=%s", addr, entry.ClientID, entry.Forward)
	Spawn(func(){
		for {
		    var conn net.Conn
		    var err error
		    Await(func() {
				conn, err = ln.Accept()
		    })
			if err != nil {
				log.Printf("accept failed: %v", err)
				Await(func() {
					time.Sleep(1 * time.Second)
				})
				continue
			}
			Spawn(func() {
				err := s.HandleIncomingTCP(entry, conn)
				if err != nil {
					log.Printf("HandleIncomingTCP: %v", err)
				}
			})
		}
	})
}

// HandleIncomingTCP receives a local inbound connection. Instead of dialing forward locally,
// we assign a new conn_id, track the session, and instruct the relay (via poll response) to connect on the *relay's* machine.
// New signature: no longer depend on tun.entries or listenPort, just pass explicit forwardAddr (host:port).
func (s *Server) HandleIncomingTCP(entry configEntry, clientConn net.Conn) error {
	connID := "c_" + time.Now().Format("2006_01_02_15_04_05") + "_" + entry.ClientID + "_" + strconv.Itoa(s.NextConnID)
	s.NextConnID++
	
	conn := &Conn{
		ID:     connID,
		TCPConn: clientConn.(*net.TCPConn),
		ClientID:    entry.ClientID,
	}
	s.Conns[connID] = conn
	
    message := &Message{
        Action: "connectFromSource",
		Addr: entry.ForwardAddr,
		ConnID: connID,
    }
    
	conn.MessagesFromSource = append(conn.MessagesFromSource, message)
	conn.MessaagesFromSouceSignaler.Signal()

	// Only launch background goroutines if the session has a real net.Conn.
	if clientConn != nil {
		// Read from client → relay
		go func(sess *clientConnSession) {
			buf := make([]byte, 32*1024)
			for {
				n, err := sess.clientConn.Read(buf)
				if n > 0 {
					chunk := make([]byte, n)
					copy(chunk, buf[:n])
					log.Println("======sending", string(chunk))
					sess.mu.Lock()
					if !sess.closed {
						sess.sendBuf = append(sess.sendBuf, chunk)
						sess.cond.Broadcast()
						tun.cond.Broadcast()
						log.Printf("[BACKEND] Data read from local client: relayid=%s connid=%s len=%d", relayid, connID, n)
					}
					sess.mu.Unlock()
				}
				if err != nil {
					log.Printf("[BACKEND] Local client disconnected: relayid=%s connid=%s err=%v", relayid, connID, err)
					break
				}
			}
			// Client disconnected, queue "disconnect" for relay, mark session closed
			tun.mu.Lock()
			tun.serverActions = append(tun.serverActions, ServerAction{
				Type:   "disconnect",
				ConnID: sess.connID,
			})
			log.Printf("[BACKEND] Queue DISCONNECT for relay: relayid=%s connid=%s", relayid, connID)
			tun.cond.Broadcast()
			tun.mu.Unlock()
			sess.mu.Lock()
			sess.closed = true
			sess.cond.Broadcast()
			sess.mu.Unlock()
			_ = sess.clientConn.Close()
			tun.mu.Lock()
			delete(tun.active, sess.connID)
			tun.mu.Unlock()
		}(session)

		// Write from relay → client
		go func(sess *clientConnSession) {
			for {
				sess.mu.Lock()
				// Wait for data or closed
				for len(sess.recvBuf) == 0 && !sess.closed {
					sess.cond.Wait()
				}
				// If buffer is empty and peer has sent "close", mark as closed
				if len(sess.recvBuf) == 0 && sess.peerClosed && !sess.closed {
					sess.closed = true
					sess.cond.Broadcast()
				}
				// If buffer is empty and session is closed, exit and cleanup
				if len(sess.recvBuf) == 0 && sess.closed {
					sess.mu.Unlock()
					_ = sess.clientConn.Close()
					tun.mu.Lock()
					delete(tun.active, sess.connID)
					tun.mu.Unlock()
					return
				}
				// else, write all queued data even after closed
				chunk := sess.recvBuf[0]
				sess.recvBuf = sess.recvBuf[1:]
				sess.mu.Unlock()
				if len(chunk) == 0 {
					continue
				}
				n, err := sess.clientConn.Write(chunk)
				log.Printf("[BACKEND] Data written to local client: relayid=%s connid=%s len=%d", relayid, connID, n)
				if err != nil {
					log.Printf("[BACKEND] Failed client write: relayid=%s connid=%s err=%v", relayid, connID, err)
					sess.mu.Lock()
					sess.closed = true
					sess.cond.Broadcast()
					sess.mu.Unlock()
					_ = sess.clientConn.Close()
					tun.mu.Lock()
					delete(tun.active, sess.connID)
					tun.mu.Unlock()
					return
				}
			}
		}(session)
	}
	return nil
}

func main() {
	var (
		configFile      = flag.String("config", "endpoints.conf", "Config file path")
		pollingAddr     = flag.String("polling-addr", ":443", "listen addr (for HTTPS polling endpoints)")
		accessAddr      = flag.String("access-addr", ":8443", "listen addr for access/subdomain routing (TLS/SNI required)")
		certFile        = flag.String("cert", "fullchain.pem", "TLS certificate file (fullchain)")
		keyFile         = flag.String("key", "privkey.pem", "TLS private key file")
		plainHTTP       = flag.Bool("plain-http", false, "Serve HTTP instead of HTTPS (dev)")
		pathPrefix      = flag.String("path-prefix", "", "Prefix to prepend to all HTTP/HTTPS paths (default \"\")")
		redisEndpoint   = flag.String("redis-endpoint", "", "Redis host:port for relay transport (optional, always runs HTTP too)")
		redisEndpointDB = flag.Int("redis-endpoint-db", 0, "Redis DB number (optional, default 0)")
	)
	flag.Parse()
	prefix := *pathPrefix
	if prefix != "" && !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}

	entries, err := loadConfig(*configFile)
	if err != nil {
		log.Fatalf("loadConfig: %v", err)
	}
	server := &Server{
	    ConfigEntries: entries,
	    Conns: map[string]*Conn{},
	    TPOHClients: map[string]*TPOHClient{},
	}

	for _, ent := range entries {
		tpohClient, ok := server.TPOHClient
		if !ok {
			tpohClient := &tpohClient{
			    ClientID: ent.ClientID,
			    ConfigEntry: ent,
			    MessaagesFromSouceSignaler: NewSignaler,
			    MessaagesFromDestSignaler: NewSignaler,
			}
		}
		if ent.ProxyType == ProxyPort {
			server.SpawnPortListener(tpohClient)
		}
	}

	// Start the SNI/TLS multiplexer for domain entries on accessAddr
	go func() {
		var domainEntries []configEntry
		for _, ent := range entries {
			if ent.ProxyType == ProxyDomain {
				domainEntries = append(domainEntries, ent)
			}
		}
		if len(domainEntries) == 0 {
			log.Printf("[main] No domain entries found in config for access-addr")
			return
		}
		
		err := startAccessAddrListener(*accessAddr, domainEntries, *certFile, *keyFile)
		if err != nil {
			log.Fatalf("startAccessAddrListener failed: %v", err)
		}
	}()

	// Continue with polling listener setup below
	handler := http.NewServeMux()
	handler.HandleFunc(prefix+"/test", func(w http.ResponseWriter, r *http.Request) {
	    log.Printf("#yellow /test hit yay")
	    fmt.Fprintf(w, "yaaay\n")
	})
	handler.HandleFunc(prefix+"/poll-read", BackendHTTPPollRead)
	handler.HandleFunc(prefix+"/poll-write", BackendHTTPPollWrite)
	handler.HandleFunc(prefix+"/htrelay", func(w http.ResponseWriter, r *http.Request) {
		tmpExe := "htrelay_bin"
		_ = os.Remove(tmpExe)
		buildArgs := []string{"build", "-o", tmpExe, "../htrelay"}
		buildEnv := os.Environ()
		buildEnv = append(buildEnv, "GOOS=linux")
		buildEnv = append(buildEnv, "GOARCH=arm")
		cmd := exec.Command("go", buildArgs...)
		cmd.Env = buildEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			http.Error(w, "build failed: "+string(out), 500)
			return
		}
		defer os.Remove(tmpExe)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="htrelay"`)
		f, err := os.Open(tmpExe)
		if err != nil {
			http.Error(w, "could not open binary: "+err.Error(), 500)
			return
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		if err != nil {
			log.Printf("failed to send file: %v", err)
		}
	})
	AddMainPiHandler(handler, prefix)
	handler.HandleFunc(prefix+"/install", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-sh")
		// Must generate /htrelay and /main_pi links with prefix
		pp := prefix
		if pp == "" {
			pp = ""
		}
		w.Write([]byte(`#!/bin/sh
set -ex
HTRELAY_URL="${HTRELAY_URL:-https://` + r.Host + pp + `/htrelay}"
MAINPI_URL="${MAINPI_URL:-https://` + r.Host + pp + `/main_pi}"
curl -fsSL "$HTRELAY_URL" -o htrelay
curl -fsSL "$MAINPI_URL" -o main_pi
chmod +x htrelay main_pi
./htrelay -install
./main_pi -install
/usr/local/bin/check_htrelay.sh
/usr/local/bin/check_main_pi.sh
`))
	})
	log.Printf("htendpoint: serving /poll, /poll-read and /poll-write at %s (TLS cert: %s, key: %s)", *pollingAddr, *certFile, *keyFile)

	// ---- REDIS SUPPORT: Optionally also serve relay communication over Redis queues ----
	if *redisEndpoint != "" {
		rdsClient := redis.NewClient(&redis.Options{
			Addr: *redisEndpoint,
			DB:   *redisEndpointDB,
		})
		ctx := context.Background()
		log.Printf("[main] Starting Redis relay transport (endpoint=%s, db=%d)", *redisEndpoint, *redisEndpointDB)
		go func() {
			// For every relayid in config file, spin up Redis relay goroutines for it.
			for _, ent := range entries {
				relayid := ent.RelayID
				go handleRedisRelay(ctx, rdsClient, relayid)
			}
		}()
	}

	// ---- HTTP(S) SERVER START ----
	if *plainHTTP {
		log.Printf("WARNING: running in HTTP (not HTTPS!) mode on %s", *pollingAddr)
		server := &http.Server{
			Addr: *pollingAddr,
			// Handler: h2c.NewHandler(handler, &http2.Server{}),
			Handler: handler,
		}
		log.Printf("Serving on http://localhost%s", *pollingAddr)
		log.Fatal(server.ListenAndServe())
		return
	}
	
	s := &http.Server{
		Addr:    *pollingAddr,
		Handler: handler,
	}
	log.Fatal(s.ListenAndServeTLS(*certFile, *keyFile))
}

// handleRedisRelay runs a goroutine that brokers between relay queues and backend, for a relayid.
// func handleRedisRelay(ctx context.Context, redisClient *redis.Client, relayid string) {
// 	writeQueue := "relay:" + relayid + ":to-server"
// 	readQueue := "relay:" + relayid + ":from-server"
// 	log.Printf("[redis-server] relayid=%s writeQ=%s  readQ=%s", relayid, writeQueue, readQueue)
// 
// 	// Use separate goroutine for writing outbound actions to relay's readQueue (from-server)
// 	go func() {
// 		tun := backend.getTunnel(relayid)
// 		for {
// 			// Fetch outbound actions for this relayid (if any), with timeout for responsiveness
// 			ctxTimeout, cancel := context.WithTimeout(ctx, 5*time.Second)
// 			actions, err := tun.waitPollRead(ctxTimeout)
// 			cancel()
// 			if err != nil && err != context.DeadlineExceeded && err != context.Canceled {
// 				log.Printf("[redis-server] relayid=%s waitPollRead error: %v", relayid, err)
// 				time.Sleep(2 * time.Second)
// 				continue
// 			}
// 			if len(actions) == 0 {
// 				// no actions this round, sleep briefly to throttle
// 				time.Sleep(100 * time.Millisecond)
// 				continue
// 			}
// 			jdata, err := json.Marshal(actions)
// 			if err != nil {
// 				log.Printf("[redis-server] json marshal error: %v", err)
// 				continue
// 			}
// 			res := redisClient.LPush(ctx, readQueue, jdata)
// 			if err := res.Err(); err != nil {
// 				log.Printf("[redis-server] error lpush to %s: %v", readQueue, err)
// 			}
// 		}
// 	}()
// 	// Main loop: read BRPOP of write actions and dispatch to backend handler (like HTTP poll-write).
// 	for {
// 		res, err := redisClient.BRPop(ctx, 0, writeQueue).Result()
// 		if err != nil {
// 			if err == redis.Nil {
// 				continue
// 			}
// 			log.Printf("[redis-server] relayid=%s BRPop error: %v", relayid, err)
// 			time.Sleep(2 * time.Second)
// 			continue
// 		}
// 		if len(res) != 2 {
// 			continue
// 		}
// 		val := res[1]
// 		// Expect a PollRequest JSON
// 		var pollReq PollRequest
// 		if err := json.Unmarshal([]byte(val), &pollReq); err != nil {
// 			log.Printf("[redis-server] Bad PollRequest JSON: %v", err)
// 			continue
// 		}
// 		// Dispatch like HTTP's PollWrite
// 		err = backend.PollWrite(nil, &pollReq)
// 		if err != nil {
// 			log.Printf("[redis-server] backend PollWrite error: %v", err)
// 		}
// 	}
// }



// Structures mirroring the htrelay protocol.
type PollRequest struct {
	RelayID string        `json:"relay_id"`
	Actions []PollAction  `json:"actions"`
}

// PollAction: action sent from relay to backend.
type PollAction struct {
	Type        string            `json:"type"`
	ConnID      string            `json:"conn_id"`
	Data        string            `json:"data,omitempty"`
}
type ServerAction struct {
	Type   string `json:"type"` // connect/data/disconnect/httpRequest/httpRequestBodyData/httpRequestBodyDone/httpRequestAbort/httpResponse/httpResponseBodyData/httpResponseBodyDone
	ConnID string `json:"conn_id,omitempty"`
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port,omitempty"`
	Data   string `json:"data,omitempty"`
}
type backendManager struct {
	mu sync.Mutex
	cond *sync.Cond
	// relayID -> *relayTunnelMgr
	tunnels map[string]*relayTunnelMgr
}

func newBackendManager() *backendManager {
	bm := &backendManager{}
	bm.cond = sync.NewCond(&bm.mu)
	bm.tunnels = make(map[string]*relayTunnelMgr)
	return bm
}

// Gets tunnel for relayID, creating if missing.
func (bm *backendManager) getTunnel(relayid string) *relayTunnelMgr {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	tun, ok := bm.tunnels[relayid]
	if !ok {
		tun = newRelayTunnelMgr(relayid)
		bm.tunnels[relayid] = tun
	}
	return tun
}


// /poll-read: Only delivers outbound (server->relay) data and long-poll.
func (bm *backendManager) PollRead(r *http.Request, pr *PollRequest) ([]ServerAction, error) {
	tun := bm.getTunnel(pr.RelayID)
	return tun.waitPollRead(r.Context())
}

// /poll-write: Only handles actions from relay to server (push data, closes).
func (bm *backendManager) PollWrite(r *http.Request, pr *PollRequest) error {
	tun := bm.getTunnel(pr.RelayID)
	return tun.handleWriteActions(pr.Actions)
}

//---------------------------------------------------------
type clientConnSession struct {
	connID     string
	listenPort int
	clientConn net.Conn
	relayID    string
	// Instead of channels:
	mu       sync.Mutex
	cond     *sync.Cond
	sendBuf  [][]byte // buffer of byte slices to relay
	recvBuf  [][]byte // buffer of byte slices to clientConn
	closed   bool
	peerClosed bool // True if the relay sent us a "close" (but we may still have data buffered)
}

type relayTunnelMgr struct {
	relayID string
	mu          sync.Mutex
	cond        *sync.Cond
	entries     map[int]string                                // listenPort -> forward addr (still registered for mapping, unused otherwise)
	serverActions []ServerAction                              // actions to send on next poll
	isPollWaiting bool
	lastPoll     time.Time
	active       map[string]*clientConnSession                // connID -> session
	nextConnID   int
}
func newRelayTunnelMgr(relayid string) *relayTunnelMgr {
	t := &relayTunnelMgr{
		relayID: relayid,
		entries: make(map[int]string),
		active:  make(map[string]*clientConnSession),
	}
	t.cond = sync.NewCond(&t.mu)
	return t
}

// Poll timeout (overridable for tests)
var pollTimeout = 20 * time.Second

// Implements the "read" poll: only delivers outbound (server->relay) data and long-polls.
func (t *relayTunnelMgr) waitPollRead(ctx context.Context) ([]ServerAction, error) {
	now := time.Now()
	deadline := now.Add(pollTimeout)
	t.mu.Lock()
	t.lastPoll = now
	actsToSend := t.serverActions
	t.serverActions = nil
	// Also flush any queued sendBuf data from sessions (data destined for relay)
	for connID, cs := range t.active {
		cs.mu.Lock()
		for len(cs.sendBuf) > 0 {
			chunk := cs.sendBuf[0]
			log.Printf("#gold yay1 body data %q", string(chunk))
			cs.sendBuf = cs.sendBuf[1:]
			actsToSend = append(actsToSend, ServerAction{
				Type:   "data",
				ConnID: connID,
				Data:   base64.StdEncoding.EncodeToString(chunk),
			})
		}
		cs.mu.Unlock()
	}
	// Respond immediately if anything is queued.
	if len(actsToSend) > 0 {
		t.mu.Unlock()
		return actsToSend, nil
	}
	// Otherwise, wait with timeout.
	for {
		wait := deadline.Sub(time.Now())
		if wait <= 0 {
			break
		}
		timer := time.AfterFunc(wait, func() { t.cond.Broadcast() })
		t.cond.Wait()
		timer.Stop()
		// After wake, check again
		actsToSend = t.serverActions
		t.serverActions = nil
		for connID, cs := range t.active {
			cs.mu.Lock()
			for len(cs.sendBuf) > 0 {
				chunk := cs.sendBuf[0]
				log.Printf("#gold yay2 body data %q", string(chunk))
				cs.sendBuf = cs.sendBuf[1:]
				actsToSend = append(actsToSend, ServerAction{
					Type:   "data",
					ConnID: connID,
					Data:   base64.StdEncoding.EncodeToString(chunk),
				})
			}
			cs.mu.Unlock()
		}
		if len(actsToSend) > 0 {
			break
		}
	}
	t.mu.Unlock()
	return actsToSend, nil
}

// Implements only the "write" poll: processes relay->backend actions (data, close); does *not* return anything.
func (t *relayTunnelMgr) handleWriteActions(actions []PollAction) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, act := range actions {
		log.Printf("[backend/write] recv from relay: relayid=%s act=%#v", t.relayID, act)
		cs, ok := t.active[act.ConnID]
		switch act.Type {
		case "data":
			if ok {
				b, err := base64.StdEncoding.DecodeString(act.Data)
			    log.Printf("#yellow Here is stuff from the relay: %q", string(b))
				if err == nil && len(b) > 0 {
					cs.mu.Lock()
					if !cs.closed {
						cs.recvBuf = append(cs.recvBuf, b)
						cs.cond.Broadcast()
						log.Printf("[BACKEND] recv data from relay: relayid=%s connid=%s len=%d", t.relayID, act.ConnID, len(b))
					}
					cs.mu.Unlock()
				}
			}
		case "close":
			if ok {
				cs.mu.Lock()
				if !cs.closed {
					cs.peerClosed = true
					log.Printf("[BACKEND] recv CLOSE from relay: relayid=%s connid=%s", t.relayID, act.ConnID)
					// Only mark closed if recvBuf is empty, else let goroutine finish draining
					if len(cs.recvBuf) == 0 {
						cs.closed = true
						cs.cond.Broadcast()
						cs.mu.Unlock()
						if cs.clientConn != nil {
							_ = cs.clientConn.Close()
						}
						delete(t.active, act.ConnID)
						break
					}
					// else: let goroutine close when drained
					cs.cond.Broadcast()
				}
				cs.mu.Unlock()
			}
		}
	}
	return nil
}

// Appends a server action and signals possibly waiting poll.
func (t *relayTunnelMgr) pushServerAction(act ServerAction) {
	t.mu.Lock()
	t.serverActions = append(t.serverActions, act)
	t.cond.Broadcast()
	t.mu.Unlock()
}


// Export: Package-wide backend manager (singleton)
var backend *backendManager = newBackendManager()

// Handle /poll-read endpoint: receives POST, returns []ServerAction (from backend.PollRead)
func BackendHTTPPollRead(w http.ResponseWriter, r *http.Request) {
	log.Println("#yellow poll read")
	if r.Method != "POST" {
		http.Error(w, "only POST supported", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var pollReq PollRequest
	if err := json.NewDecoder(r.Body).Decode(&pollReq); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	log.Printf("[POLL-READ] relay_id=%s remote=%s", pollReq.RelayID, r.RemoteAddr)
	log.Printf("[DEBUG] PollRead request received for relayid=%s from %s", pollReq.RelayID, r.RemoteAddr)
	res, err := backend.PollRead(r, &pollReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// Handle /poll-write endpoint: receives POST with relay→server actions, no response beyond 200 OK
func BackendHTTPPollWrite(w http.ResponseWriter, r *http.Request) {
	log.Println("#yellow poll write")
	log.Printf("[POLL-WRITE] from=%s", r.RemoteAddr)
	if r.Method != "POST" {
		http.Error(w, "only POST supported", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var pollReq PollRequest
	if err := json.NewDecoder(r.Body).Decode(&pollReq); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	log.Printf("[POLL-WRITE] relay_id=%s", pollReq.RelayID)
	log.Printf("[DEBUG] PollWrite request received for relayid=%s from %s", pollReq.RelayID, r.RemoteAddr)
	err := backend.PollWrite(r, &pollReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// startAccessAddrListener launches a TLS server on accessAddr.
// It dispatches by SNI (server name in TLS hello) to entries from config.
func startAccessAddrListener(addr string, entries []configEntry, certFile, keyFile string) error {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("could not load cert/key: %v", err)
	}
	sniToEntry := make(map[string]configEntry)
	for _, ent := range entries {
		sniToEntry[ent.Domain] = ent
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			// Optionally, restrict handshake or log SNI
			return nil, nil // fallback to Certificates
		},
	}
	ln, err := tls.Listen("tcp", addr, tlsConfig)
	if err != nil {
		return fmt.Errorf("TLS Listen failed: %v", err)
	}
	log.Printf("[access-addr] Listening for SNI/domain on %s", addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("[access-addr] Accept failed: %v", err)
			continue
		}
		go func(c net.Conn) {
			tlsConn, ok := c.(*tls.Conn)
			if !ok {
				log.Printf("[access-addr] Not a tls.Conn!")
				c.Close()
				return
			}
			err := tlsConn.Handshake()
			if err != nil {
				log.Printf("[access-addr] TLS handshake error: %v", err)
				tlsConn.Close()
				return
			}
			connState := tlsConn.ConnectionState()
			sni := connState.ServerName
			entry, ok := sniToEntry[sni]
			if !ok {
				// Instead of closing, plug this TLS connection into an HTTP handler.
				// Example: serve a simple HTTP handler over this established TLS stream.
				log.Printf("[access-addr] Unknown domain (SNI %q), serving via fallback HTTP handler", sni)
				go serveSingleConnHTTP(tlsConn, fallbackHTTPHandler())
				return
			}
			log.Printf("#cyan entry: %#v", entry)
			if entry.RequiresClientCert {
    			if len(connState.PeerCertificates) == 0 {
					log.Println("No client certificate provided")
					return
				}
				cert := connState.PeerCertificates[0]
				fp := fingerprint(cert)
				log.Printf("Client fingerprint: %s", fp)
   
				if !allowedFingerprints[fp] {
					log.Println("❌ Unauthorized client cert")
					tlsConn.Close()
					return
				}
   
				log.Printf("✅ Authorized: %s\n", cert.Subject.CommonName)
			}
			log.Printf("[access-addr] Accepted SNI/domain %q – Dispatching to backend", sni)
			// Call backend.HandleIncomingTCP for the correct relay
			err = backend.HandleIncomingTCP(entry.RelayID, entry.Forward, tlsConn)
			if err != nil {
				log.Printf("[access-addr] backend handler error: %v", err)
				tlsConn.Close()
			}
			// Handler is now responsible for closing the connection.
		}(conn)
	}
}


// Add your trusted client fingerprints here
var allowedFingerprints = map[string]bool{
	// Replace this with the real fingerprint you get from openssl
	"your_64_char_sha256_fingerprint_here": true,
}

func fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// fallbackHTTPHandler returns an example HTTP handler to use for unknown SNI connections.
func fallbackHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "Hello from fallback HTTP handler. SNI not recognized. Host=%s\n", r.Host)
	})
	return mux
}

// serveSingleConnHTTP serves a single net.Conn with the provided HTTP handler.
// It creates a one-shot in-memory listener that yields this single connection to http.Serve.
func serveSingleConnHTTP(c net.Conn, handler http.Handler) {
	one := &singleConnListener{conn: c, done: make(chan struct{})}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	// When http.Serve returns, close.
	_ = server.Serve(one)
	_ = c.Close()
}

// singleConnListener implements net.Listener for a single pre-accepted connection.
type singleConnListener struct {
	conn net.Conn
	done chan struct{}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.conn == nil {
		<-l.done
		return nil, io.EOF
	}
	c := l.conn
	l.conn = nil
	close(l.done)
	return c, nil
}
func (l *singleConnListener) Close() error {
	if l.conn != nil {
		_ = l.conn.Close()
		l.conn = nil
	}
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}
func (l *singleConnListener) Addr() net.Addr {
	// Return a dummy address
	return &net.TCPAddr{IP: net.IPv4zero, Port: 0}
}




