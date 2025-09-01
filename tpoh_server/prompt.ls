#!/usr/local/bin/linescript4
# "CLOSELOOP_MODEL=gpt-4.1 closeloop"
"CLOSELOOP_MODEL=gpt-5 closeloop"
string
    SEE FILE: tpoh_server.go
    
    this is a work-in-progress file for
    an tcp proxy that runs over http
    
    it doesn't compile yet.
    
    in the serveSingleConnHTTP
    
    what's the downside to how I'm doing that?
    will clients be able to reuse the underlying tcp connections like with normal http handling?
    
    Thought process:
    - The file doesnât compile; but your question is conceptual about serveSingleConnHTTPâs approach.
    - Iâll explain the downsides and whether HTTP connection reuse/keep-alive/H2 multiplexing behaves like normal.
    
    Answer:
    The serveSingleConnHTTP pattern (wrapping a pre-accepted net.Conn in a single-use net.Listener and handing it to http.Server.Serve) works to service one accepted connection through the standard http stack, but it changes connection lifecycle and multiplexing semantics compared to a normal long-lived listener.
    
    Downsides:
    - No connection pooling/multiplexing across clients: Each call creates a one-shot listener that accepts exactly one connection. You lose the usual listenerâs ability to accept many connections and let the server pool and manage them.
    - Limited keep-alive utility: The HTTP server will honor keep-alive within that single TCP connection, but once that connection closes, thereâs no listener to accept a new connection. So clients canât reuse âthe underlying TCP connectionsâ across multiple separate connects; they can only keep-alive within the single connection they already have.
    - HTTP/2 multiplexing is confined to that one TCP connection: If ALPN negotiates h2, you can multiplex streams on that connection, but again, only for that one connection. Thereâs no accept loop to handle additional connections or distribute load.
    - No listener-level features: You miss standard server behavior like graceful shutdown across many conns, accept backpressure, TCP-level options on the listener, and integration with http.Serverâs lifecycle for multiple connections.
    - Resource/lifecycle quirks: Because you create a fresh http.Server per connection and call Serve on a single-conn listener, per-connection server objects may add overhead and make observability and shutdown trickier. Also, timeouts (ReadTimeout/IdleTimeout) need careful tuning since thereâs no outer listener to enforce global policies.
    
    Will clients be able to reuse the underlying TCP connections like with normal HTTP handling?
    - Within that single accepted connection: yes. HTTP/1.1 keep-alive and HTTP/2 stream reuse work as usual for that connection.
    - Across connections (i.e., making new TCP connections later and hoping the server keeps a pool): no. Thereâs no persistent listener to accept a new connection for reuse; connection reuse is inherently per-TCP-connection. Normal http servers keep a listener running so clients can reconnect and the server can manage many concurrent conns; your approach creates a server tied to one conn and then goes away.
    
    If your goal is to serve general HTTP traffic over a long-lived port with standard behavior (keep-alive, H2 multiplexing, multiple clients), prefer:
    - A single http.Server with a real net.Listener (e.g., tls.Listen on the access addr) and call Serve in a loop. Dispatch SNI/Host inside the handler or via tls.Config GetCertificate/GetConfigForClient.
    - If you must accept TLS yourself for SNI routing, still keep one long-lived listener and pass accepted conns into http.Server via Server.Serve on a custom listener that yields all accepted conns, not just one. That is, create a listener implementation that can queue multiple accepted conns rather than a singleConnListener.
    
    In short: your approach is fine for handling âthis one pre-accepted TLS connection as HTTP,â including keep-alive within that connection. Itâs not suitable if you want normal server-side connection reuse/pooling across many TCP connections or many clients.
    
    How can I make this for more normal? connection pooly, but still do my SNI breakout?
    
    Thought process:
    - You asked about the downsides of serving a single pre-accepted net.Conn via a one-shot listener and whether clients can reuse TCP like normal HTTP. I can answer conceptually without changing code. 
    - You also asked how to make it more ânormalâ (connection-pooly) while still doing SNI breakout. Iâll outline a pattern you can implement.
    
    Answer:
    Downsides of serveSingleConnHTTP:
    - It only ever serves that one TCP connection. http.Server will honor HTTP/1.1 keep-alive and HTTP/2 multiplexing within that connection, but thereâs no long-lived listener to accept more connections. So you lose listener-level connection management, accept backlog, and global timeouts.
    - Per-connection server instantiation overhead and lifecycle complexity. You end up creating an http.Server per conn, which complicates shutdown, metrics, and tuning.
    - No pooling/multiplexing across connections or clients. Only within that single conn (keep-alive/H2 streams) does reuse happen; thereâs no way for a later client connection to âreuseâ anything because thereâs no broader listener and no server connection pool.
    - You miss http.Server features that work across many accepted connections (graceful shutdown, limiting concurrent conns, etc.).
    
    Will clients be able to reuse TCP like normal?
    - Yes, within that single accepted connection: HTTP/1.1 keep-alive and HTTP/2 multiple streams on that one TCP session will work.
    - No, across separate TCP connections: since youâre not running a persistent listener feeding multiple connections into a single server instance, thereâs nothing to âreuseâ beyond the single conn you handed in.
    
    How to make it more normal while keeping SNI breakout:
    - Keep one long-lived listener (tls.Listener) with a tls.Config that uses GetConfigForClient or GetCertificate to dispatch by SNI.
    - Feed all accepted conns into a single http.Server.Serve using a custom net.Listener that can accept many pre-accepted connections. Concretely:
      1) Accept TLS at the edge (so you can examine SNI).
      2) Based on SNI, select an http.Handler (or a routing context) representing the target âtenantâ or backend behavior.
      3) For each SNI group, maintain a per-domain http.Server (or a shared server with per-request routing).
      4) Implement a multiConnListener that has an internal channel/queue. For each accepted tls.Conn that matches that SNI, push it into that listenerâs queue. Call server.Serve(multiConnListener) once, and it will keep serving all conns for that SNI normally, with keep-alive/H2 across many connections.
    - Alternative: Use a single http.Server on the TLS listener and route by r.Host/SNI inside the Handler. This is simplest and gives you full pooling/keep-alive/H2 across all clients, but you may need SNI at handshake time for cert selection or client cert policies; thatâs handled by tls.Config callbacks.
    - If you still need to forward raw TCP after SNI, you can keep a connection-per-session tunnel, but for HTTP traffic youâll get better semantics if you let http.Server manage the lifecycle with a real listener.
    
    Minimal sketch for multi-conn approach:
    - Use tls.Listen for access-addr with a tls.Config that sets:
      - GetConfigForClient to pick per-domain TLS settings (certs, client-auth policy).
    - For HTTP handling:
      - Option A: One http.Server with a mux that routes by Host header to different handlers/backends.
      - Option B: A map[domain]*http.Server, each started once with its own multiConnListener. When you accept a tls.Conn for that domain, enqueue it to that domainâs listener. This preserves normal keep-alive/H2 across many separate TCP connections for that domain.
    
    This gives you âconnection-poolyâ behavior: http.Server can maintain many concurrent, long-lived connections, reuse them for multiple requests, and multiplex HTTP/2 streams, while you still perform SNI-driven routing at accept/handshake time.
    
    
    Can you implement that?
    
    Please provide a standalone, compiling example file demonstrating SNI breakout to per-domain http.Server with a multi-connection listener
    
    but it in a subdirectory
end
drop string
    # add a Signal method to Signaler
    # Please change ChildStates to be a container/heap
    # update Cancel call Cancel on all the Child States
    # in the Cancel and Done functions, remove the cs from its parent ChildStates
    # you prob need to keep track of the parent
    
    # See removeChild, make it better, by keeping track of the Index, so you can use a heap operation
    
end
execBashStdinStream
say