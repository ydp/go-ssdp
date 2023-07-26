package multicast

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/koron/go-ssdp/internal/ssdplog"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

// Conn is multicast connection.
type Conn struct {
	laddr  *net.UDPAddr
	conn   *net.UDPConn
	pconn  *ipv4.PacketConn
	iflist []net.Interface
}

func Control(network, address string, c syscall.RawConn) (err error) {
	return c.Control(func(fd uintptr) {
		err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
		if err != nil {
			return
		}
		err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		if err != nil {
			return
		}
		ifList, err := interfaces()
		if err != nil || len(ifList) != 1 {
			return
		}
		err = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, ifList[0].Name)
		if err != nil {
			return
		}
	})
}

// Listen starts to receiving multicast messages.
func Listen(r *AddrResolver) (*Conn, error) {
	// prepare parameters.
	laddr, err := r.resolve()
	if err != nil {
		return nil, err
	}

	// Set Control function
	listenConfig := net.ListenConfig{
		Control: Control,
	}

	// connect.
	packConn, err := listenConfig.ListenPacket(context.Background(), "udp4", laddr.String())
	if err != nil {
		return nil, err
	}
	conn := packConn.(*net.UDPConn)
	err = ipv4.NewPacketConn(conn).SetControlMessage(ipv4.FlagTTL|ipv4.FlagSrc|ipv4.FlagDst|ipv4.FlagInterface, true)
	if err != nil {
		return nil, err
	}

	// configure socket to use with multicast.
	pconn, iflist, err := newIPv4MulticastConn(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &Conn{
		laddr:  laddr,
		conn:   conn,
		pconn:  pconn,
		iflist: iflist,
	}, nil
}

func newIPv4MulticastConn(conn *net.UDPConn) (*ipv4.PacketConn, []net.Interface, error) {
	iflist, err := interfaces()
	if err != nil {
		return nil, nil, err
	}
	addr, err := SendAddr()
	if err != nil {
		return nil, nil, err
	}
	pconn, err := joinGroupIPv4(conn, iflist, addr)
	if err != nil {
		return nil, nil, err
	}
	return pconn, iflist, nil
}

// joinGroupIPv4 makes the connection join to a group on interfaces.
func joinGroupIPv4(conn *net.UDPConn, iflist []net.Interface, gaddr net.Addr) (*ipv4.PacketConn, error) {
	wrap := ipv4.NewPacketConn(conn)
	wrap.SetMulticastLoopback(true)
	// add interfaces to multicast group.
	joined := 0
	for _, ifi := range iflist {
		if err := wrap.JoinGroup(&ifi, gaddr); err != nil {
			ssdplog.Printf("failed to join group %s on %s: %s", gaddr.String(), ifi.Name, err)
			continue
		}
		joined++
		ssdplog.Printf("joined group %s on %s (#%d)", gaddr.String(), ifi.Name, ifi.Index)
	}
	if joined == 0 {
		return nil, errors.New("no interfaces had joined to group")
	}
	return wrap, nil
}

// Close closes a multicast connection.
func (mc *Conn) Close() error {
	if err := mc.pconn.Close(); err != nil {
		return err
	}
	// mc.conn is closed by mc.pconn.Close()
	return nil
}

// DataProvider provides a body of multicast message to send.
type DataProvider interface {
	Bytes(*net.Interface) []byte
}

//type multicastDataProviderFunc func(*net.Interface) []byte
//
//func (f multicastDataProviderFunc) Bytes(ifi *net.Interface) []byte {
//	return f(ifi)
//}

type BytesDataProvider []byte

func (b BytesDataProvider) Bytes(ifi *net.Interface) []byte {
	return []byte(b)
}

// WriteTo sends a multicast message to interfaces.
func (mc *Conn) WriteTo(dataProv DataProvider, to net.Addr) (int, error) {
	if uaddr, ok := to.(*net.UDPAddr); ok && !uaddr.IP.IsMulticast() {
		return mc.conn.WriteTo(dataProv.Bytes(nil), to)
	}
	sum := 0
	for _, ifi := range mc.iflist {
		if err := mc.pconn.SetMulticastInterface(&ifi); err != nil {
			return 0, err
		}
		n, err := mc.pconn.WriteTo(dataProv.Bytes(&ifi), nil, to)
		if err != nil {
			return 0, err
		}
		sum += n
	}
	return sum, nil
}

// LocalAddr returns local address to listen multicast packets.
func (mc *Conn) LocalAddr() net.Addr {
	return mc.laddr
}

// ReadPackets reads multicast packets.
func (mc *Conn) ReadPackets(timeout time.Duration, h PacketHandler) error {
	buf := make([]byte, 65535)
	if timeout > 0 {
		mc.pconn.SetReadDeadline(time.Now().Add(timeout))
	}
	for {
		n, _, addr, err := mc.pconn.ReadFrom(buf)
		if err != nil {
			if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
				return nil
			}
			if strings.Contains(err.Error(), "use of closed network connection") {
				return io.EOF
			}
			return err
		}
		if err := h(addr, buf[:n]); err != nil {
			return err
		}
	}
}
