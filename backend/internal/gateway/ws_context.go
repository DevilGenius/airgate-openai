package gateway

import "net"

// A lease/RPC cancellation must also interrupt a blocked response.create write,
// before ReceiveWSResponse has started its read/keepalive lifecycle.
type requestBoundConn struct {
	net.Conn
	stop func() bool
}

func (c *requestBoundConn) Close() error {
	c.stop()
	return c.Conn.Close()
}
