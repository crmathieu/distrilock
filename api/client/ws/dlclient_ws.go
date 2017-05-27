package dlclientws

import (
	"encoding/gob"
	"fmt"
	"net"
	"time"

	"bitbucket.org/gdm85/go-distrilock/api"
	"bitbucket.org/gdm85/go-distrilock/api/client"
	"bitbucket.org/gdm85/go-distrilock/api/client/internal/base"

	"github.com/gorilla/websocket"
)

// WebsocketClient is a single-connection, non-concurrency-safe client to a distrilock websocket daemon in binary or JSON mode.
type WebsocketClient struct {
	endpoint                  string
	keepAlive                 time.Duration
	readTimeout, writeTimeout time.Duration
	conn                      *websocket.Conn
	messageType               int
}

// String returns a summary of the client connection and active locks.
func (c *WebsocketClient) String() string {
	return fmt.Sprintf("%v", c.conn)
}

// NewBinary returns a new binary distrilock websocket client; no connection is performed.
func NewBinary(endpoint string, keepAlive, readTimeout, writeTimeout time.Duration) client.Client {
	return bclient.New(&WebsocketClient{
		endpoint:     endpoint,
		readTimeout:  readTimeout,
		writeTimeout: writeTimeout,
		keepAlive:    keepAlive,
		messageType:  websocket.BinaryMessage,
	})
}

// NewJSON returns a new JSON distrilock websocket client; no connection is performed.
func NewJSON(endpoint string, keepAlive, readTimeout, writeTimeout time.Duration) client.Client {
	return bclient.New(&WebsocketClient{
		endpoint:     endpoint,
		keepAlive:    keepAlive,
		readTimeout:  readTimeout,
		writeTimeout: writeTimeout,
		messageType:  websocket.TextMessage,
	})
}

// acquireConn is called every time a connection would be necessary; it does nothing if connection has already been made. It will re-estabilish a connection if Client c had been closed before.
func (c *WebsocketClient) AcquireConn() error {
	if c.conn == nil {
		var err error
		c.conn, _, err = websocket.DefaultDialer.Dial(c.endpoint, nil)
		if err != nil {
			return err
		}
		if c.keepAlive != 0 {
			uc := c.conn.UnderlyingConn()
			conn, ok := uc.(*net.TCPConn)
			if !ok {
				return fmt.Errorf("found connection type %T, but %T expected", uc, conn)
			}

			// setup keep-alive
			err = conn.SetKeepAlive(true)
			if err != nil {
				return err
			}
			err = conn.SetKeepAlivePeriod(c.keepAlive)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *WebsocketClient) Do(req *api.LockRequest) (*api.LockResponse, error) {
	if c.writeTimeout != 0 {
		err := c.conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
		if err != nil {
			return nil, err
		}
	}

	w, err := c.conn.NextWriter(c.messageType)
	if err != nil {
		return nil, err
	}

	e := gob.NewEncoder(w)
	err = e.Encode(&req)
	w.Close()
	if err != nil {
		return nil, err
	}

	// wait for a response
	var res api.LockResponse
	if c.readTimeout != 0 {
		err := c.conn.SetReadDeadline(time.Now().Add(c.readTimeout))
		if err != nil {
			return nil, err
		}
	}

	messageType, r, err := c.conn.NextReader()
	if err != nil {
		return nil, err
	}
	if messageType != c.messageType {
		return nil, fmt.Errorf("got message type %s but %s expected", messageType, c.messageType)
	}
	d := gob.NewDecoder(r)
	err = d.Decode(&res)
	if err != nil {
		return nil, err
	}

	return &res, nil
}

func (c *WebsocketClient) Close() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	if err != nil {
		return err
	}

	err = c.conn.Close()
	if err != nil {
		return err
	}
	c.conn = nil

	return nil
}
