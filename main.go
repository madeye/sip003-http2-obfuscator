package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type Config struct {
	RemoteHost string
	RemotePort string
	LocalHost  string
	LocalPort  string
	Password   string
	Method     string
	PluginOpts string
	ServerMode bool
}

type PluginOpts struct {
	ServerName string
	Host       string
	Path       string
}

func parsePluginOpts(raw string) PluginOpts {
	o := PluginOpts{
		ServerName: "cloudfront.net",
		Host:       "d1x3x9x7x5x3x9.cloudfront.net",
		Path:       "/cdn-cgi/trace",
	}
	if raw == "" {
		return o
	}
	for _, pair := range strings.Split(raw, ";") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "server_name":
			o.ServerName = kv[1]
		case "host":
			o.Host = kv[1]
		case "path":
			o.Path = kv[1]
		}
	}
	return o
}

func main() {
	// SIP003: shadowsocks-rust v1.24+ passes config via env vars.
	// Fall back to command-line flags for manual testing.
	envRH := os.Getenv("SS_REMOTE_HOST")
	envRP := os.Getenv("SS_REMOTE_PORT")
	envLH := os.Getenv("SS_LOCAL_HOST")
	envLP := os.Getenv("SS_LOCAL_PORT")
	envPO := os.Getenv("SS_PLUGIN_OPTIONS")

	remoteHost := flag.String("remoteHost", envRH, "Remote host")
	remotePort := flag.String("remotePort", envRP, "Remote port")
	localHost := flag.String("localHost", envLH, "Local host")
	localPort := flag.String("localPort", envLP, "Local port")
	password := flag.String("password", "", "Password")
	method := flag.String("method", "", "Encryption method")
	pluginOpts := flag.String("pluginOpts", envPO, "server_name=x;host=y;path=z")
	serverMode := flag.Bool("server", false, "Run as HTTP/2 server (spawned by ss-server)")
	showHelp := flag.Bool("help", false, "Show help")

	flag.Parse()

	// Override from env if flag was not explicitly set (i.e. flag lookup vs env)
	// Use env values if flags are empty (default) and env vars are set.
	if *remoteHost == "" && envRH != "" {
		*remoteHost = envRH
	}
	if *remotePort == "" && envRP != "" {
		*remotePort = envRP
	}
	if *localHost == "" && envLH != "" {
		*localHost = envLH
	}
	if *localPort == "" && envLP != "" {
		*localPort = envLP
	}
	if *pluginOpts == "" && envPO != "" {
		*pluginOpts = envPO
	}

	if *showHelp {
		fmt.Println("HTTP/2 Obfuscator SIP003 Plugin v1.0")
		fmt.Println("Usage: http2-obfuscator -remoteHost <host> -remotePort <port> -localHost <host> -localPort <port>")
		fmt.Println("       [-server] [-password <pw>] [-method <method>] [-pluginOpts \"k=v;k2=v2\"]")
		fmt.Println("")
		fmt.Println("SIP003 env vars: SS_REMOTE_HOST, SS_REMOTE_PORT, SS_LOCAL_HOST, SS_LOCAL_PORT, SS_PLUGIN_OPTIONS")
		os.Exit(0)
	}

	if *remoteHost == "" || *remotePort == "" || *localHost == "" || *localPort == "" {
		log.Fatal("Missing -remoteHost, -remotePort, -localHost, -localPort (or set SS_REMOTE_HOST etc.)")
	}

	isServer := *serverMode || strings.Contains(*pluginOpts, "mode=server")

	// SIP003 env vars:
	//   ss-local: SS_LOCAL=listen-addr, SS_REMOTE=connect-to
	//   ss-server: SS_LOCAL=connect-to, SS_REMOTE=listen-addr
	// We always listen on "listen" and connect to "forward".
	listenHost := *localHost
	listenPort := *localPort
	forwardHost := *remoteHost
	forwardPort := *remotePort
	if isServer {
		listenHost, forwardHost = forwardHost, listenHost
		listenPort, forwardPort = forwardPort, listenPort
	}

	if err := startPlugin(Config{
		RemoteHost: forwardHost,
		RemotePort: forwardPort,
		LocalHost:  listenHost,
		LocalPort:  listenPort,
		Password:   *password,
		Method:     *method,
		PluginOpts: *pluginOpts,
		ServerMode: isServer,
	}); err != nil {
		log.Fatal(err)
	}
}

func startPlugin(config Config) error {
	log.Printf("http2-obfuscator: remote=%s:%s local=%s:%s server=%v opts=%s",
		config.RemoteHost, config.RemotePort, config.LocalHost, config.LocalPort,
		config.ServerMode, config.PluginOpts)

	listener, err := net.Listen("tcp", net.JoinHostPort(config.LocalHost, config.LocalPort))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()

	log.Printf("http2-obfuscator: listening on %s", listener.Addr())

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go handleConnection(conn, config)
	}
}

// serialFramer serializes all frame writes through a mutex so that
// individual HTTP/2 frame bytes are never interleaved on the wire.
type serialFramer struct {
	mu     sync.Mutex
	conn   net.Conn
	framer *http2.Framer
}

func newSerialFramer(conn net.Conn) *serialFramer {
	return &serialFramer{
		conn:   conn,
		framer: http2.NewFramer(conn, nil),
	}
}

func (sf *serialFramer) writePreface() error {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	_, err := sf.conn.Write([]byte(http2.ClientPreface))
	return err
}

func (sf *serialFramer) WriteSettings() error {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	return sf.framer.WriteSettings()
}

func (sf *serialFramer) WriteSettingsAck() error {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	return sf.framer.WriteSettingsAck()
}

func (sf *serialFramer) WriteWindowUpdate(streamID, incr uint32) error {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	return sf.framer.WriteWindowUpdate(streamID, incr)
}

func (sf *serialFramer) WritePing(ack bool, data [8]byte) error {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	return sf.framer.WritePing(ack, data)
}

func (sf *serialFramer) WriteHeaders(p http2.HeadersFrameParam) error {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	return sf.framer.WriteHeaders(p)
}

func (sf *serialFramer) WriteData(streamID uint32, endStream bool, data []byte) error {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	return sf.framer.WriteData(streamID, endStream, data)
}

func handleConnection(local net.Conn, config Config) {
	defer local.Close()

	remote, err := net.DialTimeout("tcp", net.JoinHostPort(config.RemoteHost, config.RemotePort), 10*time.Second)
	if err != nil {
		log.Printf("dial remote %s:%s: %v", config.RemoteHost, config.RemotePort, err)
		return
	}
	defer remote.Close()

	opts := parsePluginOpts(config.PluginOpts)
	var wg sync.WaitGroup
	wg.Add(2)

	if config.ServerMode {
		// Server mode: local = accepted from client plugin, remote = dialed to SS server.
		// HTTP/2 frames flow between plugins over "local".
		// Raw SS data flows between plugin and SS server over "remote".
		sf := newSerialFramer(local)
		br := bufio.NewReader(local)

		go func() {
			defer wg.Done()
			serverReadHTTP2(br, sf, remote)
		}()
		go func() {
			defer wg.Done()
			serverWriteHTTP2(remote, sf, opts)
			local.Close()
		}()
	} else {
		// Client mode: local = accepted from ss-local, remote = dialed to server.
		// HTTP/2 frames flow between plugins over "remote".
		// Raw SS data flows between plugin and ss-local over "local".
		sf := newSerialFramer(remote)
		br := bufio.NewReader(remote)

		go func() {
			defer wg.Done()
			clientWriteHTTP2(local, sf, opts)
		}()
		go func() {
			defer wg.Done()
			clientReadHTTP2(br, sf, local)
			local.Close()
		}()
	}

	wg.Wait()
}

// ==================== CLIENT MODE ====================

func clientWriteHTTP2(local io.Reader, sf *serialFramer, opts PluginOpts) {
	sf.writePreface()
	sf.WriteSettings()
	sf.WriteWindowUpdate(0, 1048576)

	var streamID uint32 = 1
	buf := make([]byte, 16384)

	for {
		n, err := local.Read(buf)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}
		data := buf[:n]

		var hbuf bytes.Buffer
		enc := hpack.NewEncoder(&hbuf)
		enc.WriteField(hpack.HeaderField{Name: ":method", Value: "POST"})
		enc.WriteField(hpack.HeaderField{Name: ":path", Value: opts.Path})
		enc.WriteField(hpack.HeaderField{Name: ":scheme", Value: "https"})
		enc.WriteField(hpack.HeaderField{Name: ":authority", Value: opts.Host})
		enc.WriteField(hpack.HeaderField{Name: "content-type", Value: "application/octet-stream"})
		enc.WriteField(hpack.HeaderField{Name: "content-length", Value: fmt.Sprintf("%d", n)})
		enc.WriteField(hpack.HeaderField{Name: "user-agent", Value: "Mozilla/5.0"})

		sf.WriteHeaders(http2.HeadersFrameParam{
			StreamID:      streamID,
			EndHeaders:    true,
			EndStream:     false,
			BlockFragment: hbuf.Bytes(),
		})
		sf.WriteData(streamID, true, data)
		streamID += 2
	}
}

func clientReadHTTP2(br *bufio.Reader, sf *serialFramer, local io.Writer) {
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(br, preface); err != nil {
		return
	}

	readFramer := http2.NewFramer(sf.conn, br)
	for {
		f, err := readFramer.ReadFrame()
		if err != nil {
			return
		}
		switch f := f.(type) {
		case *http2.DataFrame:
			if _, err := local.Write(f.Data()); err != nil {
				return
			}
		case *http2.SettingsFrame:
			if !f.IsAck() {
				sf.WriteSettingsAck()
			}
		case *http2.PingFrame:
			if !f.IsAck() {
				sf.WritePing(true, f.Data)
			}
		case *http2.GoAwayFrame:
			return
		case *http2.HeadersFrame:
		default:
		}
	}
}

// ==================== SERVER MODE ====================

func serverReadHTTP2(br *bufio.Reader, sf *serialFramer, local io.Writer) {
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(br, preface); err != nil {
		return
	}

	sf.writePreface()
	sf.WriteSettings()
	sf.WriteWindowUpdate(0, 1048576)

	readFramer := http2.NewFramer(sf.conn, br)
	for {
		f, err := readFramer.ReadFrame()
		if err != nil {
			return
		}
		switch f := f.(type) {
		case *http2.DataFrame:
			local.Write(f.Data())
		case *http2.SettingsFrame:
			if !f.IsAck() {
				sf.WriteSettingsAck()
			}
		case *http2.PingFrame:
			if !f.IsAck() {
				sf.WritePing(true, f.Data)
			}
		case *http2.GoAwayFrame:
			return
		case *http2.HeadersFrame:
		default:
		}
	}
}

func serverWriteHTTP2(local io.Reader, sf *serialFramer, opts PluginOpts) {
	var streamID uint32 = 2
	buf := make([]byte, 16384)

	for {
		n, err := local.Read(buf)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}
		data := buf[:n]

		var hbuf bytes.Buffer
		enc := hpack.NewEncoder(&hbuf)
		enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
		enc.WriteField(hpack.HeaderField{Name: "content-type", Value: "application/octet-stream"})
		enc.WriteField(hpack.HeaderField{Name: "content-length", Value: fmt.Sprintf("%d", n)})
		enc.WriteField(hpack.HeaderField{Name: "server", Value: "cloudflare"})
		enc.WriteField(hpack.HeaderField{Name: "date", Value: time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")})

		sf.WriteHeaders(http2.HeadersFrameParam{
			StreamID:      streamID,
			EndHeaders:    true,
			EndStream:     false,
			BlockFragment: hbuf.Bytes(),
		})
		sf.WriteData(streamID, true, data)
		streamID += 2
	}
}
