package main

import (
	. "github.com/drewlesueur/cc"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Embedded restart_tpoh_client.sh script. Restarts tpoh_client at midnight via cron.
const restartScript = `#!/bin/bash
# restart_tpoh_client.sh - called by cron to forcibly restart and ensure tpoh_client is running

set -e
CHECK_SCRIPT="/usr/local/bin/check_tpoh_client.sh"
CRON_ID="#TPOH_CLIENT_CRON"

pkill -x tpoh_client || true
sleep 2
$CHECK_SCRIPT $CRON_ID
`

// Embedded check_tpoh_client.sh script. Checks if tpoh_client is running, and starts if not.
const checkScript = `#!/bin/bash
# check_tpoh_client.sh - called by cron to ensure tpoh_client is running

set -e
SCRIPT=/usr/local/bin/tpoh_client
LOG=/tmp/tpoh_client.log
CRON_ID="$1"
if ! pgrep -x tpoh_client >/dev/null; then
    echo "[$(date)] $CRON_ID starting tpoh_client" >> $LOG
    nohup $SCRIPT >> $LOG 2>&1 &
    exit 1
fi
exit 0
`

// Embedded install_cron.sh script. It sets up tpoh_client to auto-restart via cron.
const installCronScript = `#!/bin/bash
#
# install_cron.sh - set up crontab to manage tpoh_client
#
# This will:
#   - Ensure tpoh_client is started every reboot
#   - Check every 5 minutes that it is running; if not, start it
#   - Restart tpoh_client every midnight
#
# Edits current user's crontab.
#
# Set the path to the check script here:
CHECK_SCRIPT="/usr/local/bin/check_tpoh_client.sh"
RESTART_SCRIPT="/usr/local/bin/restart_tpoh_client.sh"
CRON_ID="#TPOH_CLIENT_CRON"

function remove_old_tpoh_client_crons() {
    crontab -l 2>/dev/null | grep -v "$CRON_ID" || true
}

NEW_CRON="$(remove_old_tpoh_client_crons)"

# @reboot (ensure only one entry)
REBOOT_ENTRY="@reboot $CHECK_SCRIPT $CRON_ID"
NEW_CRON=$(printf "%s\n%s\n" "$NEW_CRON" "$REBOOT_ENTRY")

# Every 1 minutes: check and start if needed
CHECK_ENTRY="*/1 * * * * $CHECK_SCRIPT $CRON_ID"
NEW_CRON=$(printf "%s\n%s\n" "$NEW_CRON" "$CHECK_ENTRY")

# 12:01 AM restart: call separate restart script
RESTART_ENTRY="1 0 * * * $RESTART_SCRIPT $CRON_ID"
NEW_CRON=$(printf "%s\n%s\n" "$NEW_CRON" "$RESTART_ENTRY")

# Remove any leading/trailing newlines
NEW_CRON=$(echo "$NEW_CRON" | sed '/^\s*$/d')

# Install new crontab
echo "$NEW_CRON" | crontab -
echo "Crontab updated for tpoh_client."
`

/* -------------------------------------------------------------------------- */
/* ------------------------------  CONFIG  ----------------------------------- */

type Config struct {
	ClientID    string
	Endpoint    string
	PollTimeout time.Duration
	Verbose     bool
	InstallFlag bool
}

var defaultEndpoint = "https://example.com"

func ReadConfig() *Config {
	var cfg Config
	var installFlag bool
	flag.StringVar(&cfg.Endpoint, "endpoint", defaultEndpoint, "controller poll URL")
	flag.StringVar(&cfg.ClientID, "client-id", "", "client identifier (defaults to hostname if unset)")
	flag.DurationVar(&cfg.PollTimeout, "poll-timeout", 20*time.Second, "long poll timeout, default 20s")
	flag.BoolVar(&cfg.Verbose, "verbose", false, "enable verbose debug logging")
	flag.BoolVar(&cfg.InstallFlag, "install", false, "setup cron and install scripts/binary, then exit")
	flag.Parse()

	if cfg.ClientID == "" {
		hostname, err := os.Hostname()
		if err != nil {
			cfg.ClientID = "abc123_tpoh"
		}
		cfg.ClientID = hostname + "_tpoh"
	}

	if cfg.Endpoint == "" && !installFlag {
		log.Fatalf("endpoint is required")
	}
	return &cfg
}

/* -------------------------------------------------------------------------- */
/* ---------------------------  ACTION TYPES  -------------------------------- */

type Message struct {
	Action string
	ConnID string
	Addr   string
	Data   string // base64
	Error  string
}

type PollRequest struct {
	ClientID string
	Messages []*Message
}

type Conn struct {
	ID string
	*net.TCPConn
	LastActive         time.Time
	DestReaderClosed   bool
	SourceReaderClosed bool
	Addr               string
	// other metadata?

}
type Client struct {
	Config               *Config
	ID                   string
	OutboundMessages     []*Message
	OutboundSignaler     *Signaler
	HTTPClient           *http.Client
	Conns                map[string]*Conn
	Endpoint             string
	PollTimeout          time.Duration
	Verbose              bool
	LastPollReadSuccess  time.Time
	LastPollWriteSuccess time.Time
}

// debugf only prints if verbose is enabled
func (c *Client) Debugf(format string, args ...interface{}) {
	if c.Config.Verbose {
		log.Printf(format, args...)
	}
}

func NewClient(cfg *Config) *Client {
	return &Client{
		ID:               cfg.ClientID,
		PollTimeout:      cfg.PollTimeout,
		Endpoint:         cfg.Endpoint,
		Verbose:          cfg.Verbose,
		HTTPClient:       &http.Client{},
		OutboundMessages: make([]*Message, 0, 4096),
		OutboundSignaler: NewSignaler(),
		Conns:            make(map[string]*Conn),
	}
}

func (c *Client) AddConn(id string, tcpConn *net.TCPConn) {
	conn := &Conn{
		TCPConn:    tcpConn,
		ID:         id,
		LastActive: time.Now(),
	}
	c.Conns[id] = conn
	Spawn(func() {
		c.ReadForever(id, conn)
	})
}

func (c *Client) DeleteConn(id string) {
	conn, ok := c.Conns[id]
	delete(c.Conns, id)
	if ok {
		Await(func() {
			_ = conn.Close()
		})
	}
}

func (c *Client) ReadForever(id string, conn *Conn) {
	buf := make([]byte, 32*1024)
	for {
		var n int
		var err error
		Await(func() {
			n, err = conn.TCPConn.Read(buf)
		})
		if n > 0 {
			conn.LastActive = time.Now()
			c.OutboundMessages = append(c.OutboundMessages, &Message{
				Action: "dataFromDest",
				ConnID: id,
				Data:   base64.StdEncoding.EncodeToString(buf[0:n]),
			})
			c.OutboundSignaler.Signal()
		}
		if err != nil {
			if err == io.EOF {
				conn.LastActive = time.Now()
				c.OutboundMessages = append(c.OutboundMessages, &Message{
					Action: "eofFromDest",
					ConnID: id,
				})
				c.OutboundSignaler.Signal()
				conn.DestReaderClosed = true
				if conn.SourceReaderClosed {
					c.DeleteConn(id)
				}
			} else {
				conn.LastActive = time.Now()
				c.OutboundMessages = append(c.OutboundMessages, &Message{
					Action: "errorReadFromDest",
					Error:  err.Error(),
					ConnID: id,
				})
				c.OutboundSignaler.Signal()
				c.DeleteConn(id)
			}
			return
		}
	}
}

func (c *Client) ProcessMessage(m *Message) {
	switch m.Action {
	case "connectFromSource":
		var tcpConn *net.TCPConn
		var connectErr error
		// r.queueOutgoing(cliAction{Type: "close", ConnID: a.ConnID})
		Await(func() {
			dialer := net.Dialer{Timeout: 5 * time.Second}
			conn, err := dialer.Dial("tcp", m.Addr)
			if err != nil {
				log.Printf("connect %s (%s) failed: %v", m.ConnID, m.Addr, err)
				connectErr = err
				return
			}
			log.Printf("connected %s -> %s", m.ConnID, m.Addr)
			// r.addConn(a.ConnID, conn.(*net.TCPConn))
			tcpConn = conn.(*net.TCPConn)
		})
		if connectErr != nil {
			c.OutboundMessages = append(c.OutboundMessages, &Message{
				Action: "errorConnect",
				Error:  connectErr.Error(),
				ConnID: m.ConnID,
			})
			c.OutboundSignaler.Signal()
		} else {
			c.Conns[m.ConnID] = &Conn{
				ID:         m.ConnID,
				TCPConn:    tcpConn,
				LastActive: time.Now(),
				Addr:       m.Addr,
			}
		}
	case "dataFromSource":
		conn, ok := c.Conns[m.ConnID]
		if !ok {
			c.OutboundMessages = append(c.OutboundMessages, &Message{
				Action: "errorNoConnFromDest",
				Error:  "no conn with id " + m.ConnID,
				ConnID: m.ConnID,
			})
			c.OutboundSignaler.Signal()
			return
		}

		raw, err := base64.StdEncoding.DecodeString(m.Data)
		if err != nil {
			log.Printf("bad base64 from server, conn %s : %v", m.ConnID, err)
			return
		}

		var writeError error
		Await(func() {
			n := len(raw)
			written := 0
			for written < n {
				m, wErr := conn.Write(raw[written:n])
				if m > 0 {
					written += m
				}
				if wErr != nil {
					writeError = wErr
					return
				}
			}
		})
		if writeError != nil {
			c.OutboundMessages = append(c.OutboundMessages, &Message{
				Action: "errorWriteFromDest",
				Error:  writeError.Error(),
				ConnID: m.ConnID,
			})
			c.OutboundSignaler.Signal()
		}
	case "eofFromSource":
		conn, ok := c.Conns[m.ConnID]
		if !ok {
			c.OutboundMessages = append(c.OutboundMessages, &Message{
				Action: "errorNoConnFromDest",
				Error:  "no conn with id " + m.ConnID,
				ConnID: m.ConnID,
			})
			c.OutboundSignaler.Signal()
			return
		}
		conn.CloseWrite()
		conn.SourceReaderClosed = true
		if conn.DestReaderClosed {
			c.DeleteConn(conn.ID)
		}
	case "errorWriteFromSource":
		fallthrough
	case "errorReadFromSource":
		fallthrough
	case "errorNoConnFromSource":
		conn, ok := c.Conns[m.ConnID]
		if ok {
			c.DeleteConn(conn.ID)
		}
	}
}

func (c *Client) PollWriteLoop() {
	for {
		if len(c.OutboundMessages) == 0 {
			c.OutboundSignaler.Wait()
		}
		messages := c.OutboundMessages
		c.OutboundMessages = nil
		reqBody, _ := json.Marshal(PollRequest{
			ClientID: c.ID,
			Messages: messages,
		})
		var httpError error
		Await(func() {
			httpCtx, cancel := context.WithTimeout(context.Background(), c.PollTimeout+5*time.Second)
			req, _ := http.NewRequestWithContext(httpCtx, http.MethodPost, c.Endpoint+"/poll-write", bytes.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			resp, err := c.HTTPClient.Do(req)
			if resp != nil && resp.Body != nil {
				resp.Body.Close() // We get nothing back
			}
			cancel()
			httpError = err
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return
				}
				log.Printf("[relay][writePoll] poll error: %v", err)
				time.Sleep(1 * time.Second)
				return
			}
		})
		// TODO: test this
		if httpError == nil {
			c.LastPollWriteSuccess = time.Now()
		}
	}
}

func (c *Client) PollReadLoop() {
	for {
		var httpError error
		var messages []*Message
		Await(func() {
			httpCtx, cancel := context.WithTimeout(context.Background(), c.PollTimeout+5*time.Second)
			req, _ := http.NewRequestWithContext(httpCtx, http.MethodGet, c.Endpoint+"/poll-read?id="+url.QueryEscape(c.ID), nil)
			resp, err := c.HTTPClient.Do(req)
			httpError = err
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					cancel()
					return
				}
				log.Printf("[relay][readPoll] poll error: %v", err)
				cancel()
				time.Sleep(2 * time.Second)
				return
			}

			if err := json.NewDecoder(resp.Body).Decode(&messages); err != nil {
				log.Printf("[relay][readPoll] decode error: %v", err)
				resp.Body.Close()
				cancel()
				time.Sleep(1 * time.Second)
				return
			}
			resp.Body.Close()
			cancel()
		})

		if httpError != nil {
			continue
		}
		c.LastPollReadSuccess = time.Now()
		for _, m := range messages {
			c.ProcessMessage(m)
		}
	}
}

func runInstallCronScript() {
	// Write check_tpoh_client.sh to /usr/local/bin
	checkScriptPath := "/usr/local/bin/check_tpoh_client.sh"
	if err := os.WriteFile(checkScriptPath, []byte(checkScript), 0755); err != nil {
		log.Printf("Could not write %s: %v", checkScriptPath, err)
	} else {
		log.Printf("Wrote %s", checkScriptPath)
	}

	// Write restart_tpoh_client.sh to /usr/local/bin
	restartScriptPath := "/usr/local/bin/restart_tpoh_client.sh"
	if err := os.WriteFile(restartScriptPath, []byte(restartScript), 0755); err != nil {
		log.Printf("Could not write %s: %v", restartScriptPath, err)
	} else {
		log.Printf("Wrote %s", restartScriptPath)
	}

	// Write the cron install shell script to temp and run it
	scriptPath := filepath.Join(os.TempDir(), "tpoh_client_install_cron.sh")
	err := os.WriteFile(scriptPath, []byte(installCronScript), 0755)
	if err != nil {
		log.Printf("Could not write cron installer script: %v", err)
		return
	}
	defer os.Remove(scriptPath) // Clean up after running

	// Run the script
	cmd := exec.Command("bash", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		log.Printf("Failed to run cron installer: %v", err)
	} else {
		log.Printf("tpoh_client_install_cron.sh script executed (crontab updated)")
	}
}

// ensureInstalledToUsrLocalBin checks if running from /usr/local/bin.
// If not, copies the current executable to /usr/local/bin/htrelay.
func ensureInstalledToUsrLocalBin() {
	exePath, err := os.Executable()
	if err != nil {
		log.Printf("[relay] Could not get executable path: %v", err)
		return
	}
	exePath = filepath.Clean(exePath)
	targetDir := "/usr/local/bin"
	targetPath := filepath.Join(targetDir, "tpoh_client")
	if filepath.Dir(exePath) == targetDir {
		// Already running from /usr/local/bin. Do nothing.
		return
	}
	// Copy executable to /usr/local/bin/tpoh_client
	srcFile, err := os.Open(exePath)
	if err != nil {
		log.Printf("[relay] Could not open self binary for copying: %v", err)
		return
	}
	defer srcFile.Close()
	dstFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		log.Printf("[relay] Could not write to %s: %v", targetPath, err)
		return
	}
	defer dstFile.Close()
	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		log.Printf("[relay] Error copying binary to %s: %v", targetPath, err)
		return
	}
	log.Printf("[relay] Copied new binary to %s", targetPath)
}
func main() {
	cfg := ReadConfig()

	if cfg.InstallFlag {
		ensureInstalledToUsrLocalBin()
		runInstallCronScript()
		log.Printf("Install completed. Exiting as requested by -install flag.")
		return
	}

	ensureInstalledToUsrLocalBin()
	runInstallCronScript()

	log.Printf("relay starting  id=%s  endpoint=%s  pollTimeout=%s",
		cfg.ClientID, cfg.Endpoint, cfg.PollTimeout.String())

	client := NewClient(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Goroutine to disconnect stale connections (no activity >1hr).
	Spawn(func() {
		Await(func() {
			time.Sleep(10 * time.Minute)
		})
		for connID, conn := range client.Conns {
			if time.Since(conn.LastActive) > time.Hour {
				log.Printf("[relay] disconnecting conn %s due to inactivity >1hr", connID)
				client.DeleteConn(connID)
			}
		}
	})

	Spawn(func() {
		Await(func() {
			time.Sleep(10 * time.Second)
		})
		since := time.Since(client.LastPollReadSuccess)
		if since > 45*time.Second {
			log.Printf("No successful response from server in >45sec (%.1fs), exiting for cron restart", since.Seconds())
			os.Exit(1)
		}
	})

	Spawn(func() {
		client.PollWriteLoop()
	})
	Spawn(func() {
		client.PollReadLoop()
	})
	<-ctx.Done()
}
