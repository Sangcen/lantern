package enproxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"code.google.com/p/go-uuid/uuid"
	"github.com/getlantern/idletiming"
)

// Dial creates a Conn, opens a connection to the proxy and starts processing
// writes and reads on the Conn.
//
// addr: the host:port of the destination server that we're trying to reach
//
// config: configuration for this Conn
func Dial(addr string, config *Config) (net.Conn, error) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("Unable to resolve TCP addr %v: %v", addr, err)
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("Unable to split host and port on addr %v: %v", err)
	}

	c := &Conn{
		id:      uuid.NewRandom().String(),
		addr:    addr,
		tcpAddr: tcpAddr,
		host:    host,
		config:  config,
	}

	c.initDefaults()
	c.makeChannels()
	c.markActive()
	c.initRequestStrategy()

	go c.processWrites()
	go c.processReads()

	// Dial proxy
	proxyConn, err := c.dialProxy()
	if err != nil {
		return nil, fmt.Errorf("Unable to dial proxy: %s", err)
	}

	go c.processRequests(proxyConn)

	return idletiming.Conn(c, c.config.IdleTimeout, func() {
		c.Close()
	}), nil
}

func (c *Conn) initDefaults() {
	if c.config.FlushTimeout == 0 {
		c.config.FlushTimeout = defaultWriteFlushTimeout
	}
	if c.config.IdleTimeout == 0 {
		c.config.IdleTimeout = defaultIdleTimeoutClient
	}
	if c.config.NewRequest == nil {
		c.config.NewRequest = func(host string, method string, body io.Reader) (req *http.Request, err error) {
			if host == "" {
				host = c.host
			}
			return http.NewRequest(method, "http://"+host, body)
		}
	}
}

func (c *Conn) makeChannels() {
	c.initialResponseCh = make(chan hostWithResponse)
	c.writeRequestsCh = make(chan []byte)
	c.writeResponsesCh = make(chan rwResponse)
	c.doneWritingCh = make(chan bool)
	c.readRequestsCh = make(chan []byte)
	c.readResponsesCh = make(chan rwResponse)
	c.doneReadingCh = make(chan bool)
	c.requestOutCh = make(chan *request)
	c.requestFinishedCh = make(chan error)
	c.asyncErrCh = make(chan error, 2)
	c.doneRequestingCh = make(chan bool)
}

func (c *Conn) initRequestStrategy() {
	if c.config.BufferRequests {
		c.rs = &bufferingRequestStrategy{
			c: c,
		}
	} else {
		c.rs = &streamingRequestStrategy{
			c: c,
		}
	}
}

func (c *Conn) dialProxy() (*connInfo, error) {
	conn, err := c.config.DialProxy(c.addr)
	if err != nil {
		return nil, fmt.Errorf("Unable to dial proxy: %s", err)
	}
	proxyConn := &connInfo{
		bufReader: bufio.NewReader(conn),
	}
	proxyConn.conn = idletiming.Conn(conn, c.config.IdleTimeout, func() {
		// When the underlying connection times out, mark the connInfo closed
		proxyConn.closedMutex.Lock()
		defer proxyConn.closedMutex.Unlock()
		proxyConn.closed = true
	})
	return proxyConn, nil
}

func (c *Conn) redialProxyIfNecessary(proxyConn *connInfo) (*connInfo, error) {
	proxyConn.closedMutex.Lock()
	defer proxyConn.closedMutex.Unlock()
	if proxyConn.closed || proxyConn.conn.TimesOutIn() < oneSecond {
		proxyConn.conn.Close()
		return c.dialProxy()
	} else {
		return proxyConn, nil
	}
}

func (c *Conn) doRequest(proxyConn *connInfo, host string, op string, request *request) (resp *http.Response, err error) {
	var body io.Reader
	if request != nil {
		body = request.body
	}
	req, err := c.config.NewRequest(host, "POST", body)
	if err != nil {
		err = fmt.Errorf("Unable to construct request to proxy: %s", err)
		return
	}
	req.Header.Set(X_ENPROXY_OP, op)
	// Always send our connection id
	req.Header.Set(X_ENPROXY_ID, c.id)
	// Always send the address that we're trying to reach
	req.Header.Set(X_ENPROXY_DEST_ADDR, c.addr)
	req.Header.Set("Content-type", "application/octet-stream")
	if request != nil && request.length > 0 {
		// Force identity encoding to appeas CDNs like Fastly that can't
		// handle chunked encoding on requests
		req.TransferEncoding = []string{"identity"}
		req.ContentLength = int64(request.length)
	} else {
		req.ContentLength = 0
	}

	err = req.Write(proxyConn.conn)
	if err != nil {
		err = fmt.Errorf("Error sending request to proxy: %s", err)
		return
	}

	resp, err = http.ReadResponse(proxyConn.bufReader, req)
	if err != nil {
		err = fmt.Errorf("Error reading response from proxy: %s", err)
		return
	}

	// Check response status
	responseOK := resp.StatusCode >= 200 && resp.StatusCode < 300
	if !responseOK {
		err = fmt.Errorf("Bad response status for read: %s", resp.Status)
		resp.Body.Close()
		resp = nil
	}

	return
}

func (c *Conn) markActive() {
	c.lastActivityMutex.Lock()
	defer c.lastActivityMutex.Unlock()
	c.lastActivityTime = time.Now()
}

func (c *Conn) isIdle() bool {
	c.lastActivityMutex.RLock()
	defer c.lastActivityMutex.RUnlock()
	timeSinceLastActivity := time.Now().Sub(c.lastActivityTime)
	return timeSinceLastActivity > c.config.IdleTimeout
}

type closer struct {
	io.Reader
}

func (r *closer) Close() error {
	return nil
}