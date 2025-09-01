package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// multiConnListener is a net.Listener that serves many pre-accepted net.Conn.
// http.Server.Serve can run on this listener and will accept connections
// from its internal queue until Close is called and the queue is drained.
type multiConnListener struct {
	mu     sync.Mutex
	cond   *sync.Cond
	conns  []net.Conn
	closed bool
}

func newMultiConnListener() *multiConnListener {
	m := &multiConnListener{}
	m.cond = sync.NewCond(&m.mu)
	return m
}

func (m *multiConnListener) Accept() (net.Conn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		// If there is a queued connection, return it.
		if len(m.conns) > 0 {
			c := m.conns[0]
			m.conns = m.conns[1:]
			return c, nil
		}
		// If closed and empty, end Serve loop.
		if m.closed {
			return nil, fmt.Errorf("listener closed")
		}
		m.cond.Wait()
	}
}

func (m *multiConnListener) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	m.cond.Broadcast()
	// Close any queued conns to unblock server goroutines cleanly.
	for _, c := range m.conns {
		_ = c.Close()
	}
	m.conns = nil
	return nil
}

func (m *multiConnListener) Addr() net.Addr {
	// Return a dummy address
	return &net.TCPAddr{IP: net.IPv4zero, Port: 0}
}

// enqueue adds a pre-accepted connection into the listener queue.
func (m *multiConnListener) enqueue(c net.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		_ = c.Close()
		return
	}
	m.conns = append(m.conns, c)
	m.cond.Broadcast()
}

// domainServer holds a long-lived http.Server and a multiConnListener.
// Accepted TLS conns for that SNI are enqueued into mcl; server.Serve runs once.
type domainServer struct {
	name   string
	server *http.Server
	mcl    *multiConnListener
	once   sync.Once
}

func newDomainServer(name string, handler http.Handler) *domainServer {
	mcl := newMultiConnListener()
	s := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return &domainServer{
		name:   name,
		server: s,
		mcl:    mcl,
	}
}

func (d *domainServer) start() {
	d.once.Do(func() {
		go func() {
			log.Printf("[domain %s] http.Server starting", d.name)
			err := d.server.Serve(d.mcl)
			if err != nil && err != http.ErrServerClosed {
				log.Printf("[domain %s] http.Server error: %v", d.name, err)
			} else {
				log.Printf("[domain %s] http.Server stopped", d.name)
			}
		}()
	})
}

func (d *domainServer) enqueueConn(c net.Conn) {
	d.start()
	d.mcl.enqueue(c)
}

func (d *domainServer) close() error {
	_ = d.server.Close()
	return d.mcl.Close()
}

func handlerForDomain(name string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Hello from domain %s\nHost=%s\nProto=%s\n", name, r.Host, r.Proto)
	})
	mux.HandleFunc("/time", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "domain=%s time=%s\n", name, time.Now().Format(time.RFC3339Nano))
	})
	return mux
}

func main() {
	addr := flag.String("addr", ":8443", "TLS listen address")
	// For a simple demo, we use a single cert for both domains. In real deployments,
	// you would either use a multi-SAN cert or select per-SNI certificates in GetConfigForClient.
	certFile := flag.String("cert", "server.crt", "TLS certificate file (PEM)")
	keyFile := flag.String("key", "server.key", "TLS private key file (PEM)")
	// Demo SNI hostnames:
	domainA := flag.String("domainA", "a.example.test", "SNI domain A")
	domainB := flag.String("domainB", "b.example.test", "SNI domain B")
	flag.Parse()

	// Build per-domain servers.
	domainMap := map[string]*domainServer{
		*domainA: newDomainServer(*domainA, handlerForDomain(*domainA)),
		*domainB: newDomainServer(*domainB, handlerForDomain(*domainB)),
	}

	// Load certificate. For a quick test, you can generate a self-signed cert:
	// openssl req -x509 -newkey rsa:2048 -keyout server.key -out server.crt -days 365 -nodes -subj "/CN=localhost"
	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		log.Fatalf("failed to load cert/key: %v", err)
	}

	// TLS config with SNI breakout via GetConfigForClient.
	baseTLS := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert}, // fallback
		// For a real deployment, you might return a per-SNI tls.Config with per-domain certs.
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			// We keep a simple model here: same cert for all, but we inspect SNI to route later.
			return nil, nil
		},
		// Enable HTTP/2 ALPN by default; http.Server will negotiate based on this.
		NextProtos: []string{"h2", "http/1.1"},
	}

	ln, err := tls.Listen("tcp", *addr, baseTLS)
	if err != nil {
		log.Fatalf("TLS listen failed on %s: %v", *addr, err)
	}
	log.Printf("SNI dispatcher listening on https://%s", *addr)
	log.Printf("Domains: %q and %q", *domainA, *domainB)

	// Accept loop: handshake, get SNI, enqueue conn to the correct domain server.
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}
		go func(conn net.Conn) {
			tlsConn, ok := conn.(*tls.Conn)
			if !ok {
				log.Printf("non-TLS conn, closing")
				_ = conn.Close()
				return
			}
			// Complete handshake to populate ConnectionState.
			if err := tlsConn.Handshake(); err != nil {
				log.Printf("TLS handshake error: %v", err)
				_ = tlsConn.Close()
				return
			}
			state := tlsConn.ConnectionState()
			sni := state.ServerName
			if sni == "" {
				// Some clients may omit SNI; choose a default.
				sni = *domainA
			}
			ds, ok := domainMap[sni]
			if !ok {
				// Unknown domain: simple fallback handler
				log.Printf("unknown SNI %q, using fallback handler", sni)
				ds = newDomainServer("fallback", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/plain")
					fmt.Fprintf(w, "Fallback handler for unknown SNI. Host=%s\n", r.Host)
				}))
				// Start and keep a reference so it isn't GC-ed immediately.
				domainMap[sni] = ds
			}
			// Enqueue this TLS connection to the per-domain listener. The http.Server
			// for that domain will handle HTTP/1.1 keep-alive and HTTP/2 multiplexing
			// as usual across many connections, since Serve runs once on a multi-conn listener.
			ds.enqueueConn(tlsConn)
		}(c)
	}
}


/*
is this valid code, will it handle http/2 lifecyvle and flow and/or keealive?


Your code is **generally valid** (syntax-wise, compiles, and works as intended for a demonstration)—it **routes connections based on SNI** and creates an independent `http.Server` for each "domain" bound to its own connection queue. Let's answer your two specific questions in detail:

---

### 1. **Is this valid code?**

- **Yes.**  
- It creates a TLS listener, does a handshake, then dispatches each connection to a per-domain `http.Server` via a custom `multiConnListener` (acting as a manual queue/dispatcher).

---

### 2. **Will it handle HTTP/2 lifecycle/flow and/or keepalive?**

#### **A. HTTP/2 lifecycle and flow**

**Yes!**  
- *Why?*  
    - Each per-domain `http.Server` is run in its own goroutine, on top of a custom listener that provides connections for that "domain."  
    - The `http.Server` in Go's stdlib (since Go 1.6-ish) **natively supports HTTP/2** **by default** when the connection is TLS and the inbound ALPN protocol list contains `h2` ("http/2").
        - You include this via:  
          ```go
          NextProtos: []string{"h2", "http/1.1"},
          ```
    - **If the client negotiates HTTP/2** via ALPN during the handshake, any `http.Server` handed that connection will enter "http2 mode" and handle HTTP/2 flow control, streams, multiplexing, etc—**as usual**.

#### **B. Keepalive**

**Yes, for both HTTP/1.1 and HTTP/2.**

- The per-domain `http.Server` will fully handle persistent keepalives and HTTP/2 sessions, because:
    - You do NOT close the connection after handling a request—instead, once enqueued to the server via `Serve`, the server handles the full connection lifetime (including protocol negotiation, keepalive, and multiplexing for HTTP/2).
    - HTTP/2 allows many streams on one connection. The server (in Go's implementation) will keep it open as needed, not return from the conn until done.

---

#### ****Caveats & Risks** (with this approach)

- **No graceful shutdown coordination:**  
  Each server runs forever—no central way to shut down all.
- **No per-domain TLS configuration:**  
  Your code always uses the same cert for every SNI. In real deployments, you must use `GetConfigForClient` to select cert by SNI (returning a per-domain `*tls.Config`).
- **Resource leak for arbitrary/fake SNI values:**  
  Since you create a new `domainServer` if a new SNI is seen ("unknown domain"), a high rate of random SNI could cause you to create many listeners and servers.
- **Listener Addr() is a dummy:**  
  Not a problem, but be aware.
- **Performance is slightly less optimal than direct `http.Server` + `GetConfigForClient` return:**
    - Here, you must accept, handshake, read SNI, _then_ route the connection (i.e., can't select cert based on SNI in handshake, unless you enhance `GetConfigForClient`).
    - You cannot easily support non-TLS listeners this way.
    - Unnecessary goroutines spawned for each connection.

---

### **Alternative in Standard Library**

Often, with **Go's HTTP/2 support**, if you only want to select certificates (and maybe handlers) based on SNI, you'd simply use:

- `GetConfigForClient`: to serve diff certs per SNI,
- One `http.Server` with routing in the handler (e.g., mux on `r.Host`), **not** separate listeners.

However, your approach **is correct and works for your stated model**.

---

### **Summary Table**

| Feature                       | Supported in this code?        | Notes                                         |
|-------------------------------|-------------------------------|-----------------------------------------------|
| HTTP/1.1 Keepalive            | ✅                            | fully handled by `http.Server`                |
| HTTP/2 multiplex/keepalive    | ✅                            | via ALPN + Go's http.Server HTTP/2 mode       |
| SNI dispatch/routing          | ✅                            | you do it manually after handshake            |
| Per-domain certificate        | ❌ (as-is)                    | must extend `GetConfigForClient`              |
| Graceful shutdown all domains | ❌                            | you can add, see `domainServer.close()`       |

---

**Bottom line:**  
> **Yes, _http.Server_ will correctly handle HTTP/2 multiplexing, keepalives, and protocol lifecycle per connection on your custom listener. There is no need for you to manage them.**

You can test clients with `-v` or `--http2-prior-knowledge` (curl) to check.  
Just make sure your certs, SNI, and ALPN settings match.

Let me know if you'd like code
*/