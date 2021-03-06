package pearl

import (
	"encoding/binary"
	"net"
	"time"

	"github.com/pkg/errors"
)

// Reference: https://github.com/torproject/torspec/blob/master/tor-spec.txt#L681-L702
//
//	4.5. NETINFO cells
//
//	   If version 2 or higher is negotiated, each party sends the other a
//	   NETINFO cell.  The cell's payload is:
//
//	         Timestamp              [4 bytes]
//	         Other OR's address     [variable]
//	         Number of addresses    [1 byte]
//	         This OR's addresses    [variable]
//
//	   The address format is a type/length/value sequence as given in section
//	   6.4 below.  The timestamp is a big-endian unsigned integer number of
//	   seconds since the Unix epoch.
//
//	   Implementations MAY use the timestamp value to help decide if their
//	   clocks are skewed.  Initiators MAY use "other OR's address" to help
//	   learn which address their connections are originating from, if they do
//	   not know it.  [As of 0.2.3.1-alpha, nodes use neither of these values.]
//
//	   Initiators SHOULD use "this OR's address" to make sure
//	   that they have connected to another OR at its canonical address.
//	   (See 5.3.1 below.)
//

// Errors which can occur when parsing NETINFO cells.
var (
	ErrUnencodableAddress = errors.New("could not encode address")
	ErrParseIPFromAddress = errors.New("could not parse ip from address")
)

// NetInfoCell represents a NETINFO cell.
type NetInfoCell struct {
	Timestamp       time.Time
	ReceiverAddress net.IP
	SenderAddresses []net.IP
}

var _ CellBuilder = new(NetInfoCell)

// NewNetInfoCell builds a NetInfoCell with the given receiver and sender
// addresses.
func NewNetInfoCell(r net.IP, s []net.IP) *NetInfoCell {
	return &NetInfoCell{
		Timestamp:       time.Now(),
		ReceiverAddress: r,
		SenderAddresses: s,
	}
}

func NewNetInfoCellFromAddresses(raddr, laddr net.Addr) (*NetInfoCell, error) {
	remote := addrToIP(raddr)
	local := addrToIP(laddr)
	if remote == nil || local == nil {
		return nil, ErrParseIPFromAddress
	}
	return NewNetInfoCell(remote, []net.IP{local}), nil
}

// NewNetInfoCellFromConn constructs a NetInfoCell with local and remote
// addresses from conn.
func NewNetInfoCellFromConn(conn net.Conn) (*NetInfoCell, error) {
	return NewNetInfoCellFromAddresses(conn.RemoteAddr(), conn.LocalAddr())
}

func ParseNetInfoCell(c Cell) (*NetInfoCell, error) {
	if c.Command() != CommandNetinfo {
		return nil, ErrUnexpectedCommand
	}

	p := c.Payload()
	ni := &NetInfoCell{}

	// Timestamp
	if len(p) < 4 {
		return nil, ErrShortCellPayload
	}
	epoch := binary.BigEndian.Uint32(p)
	ni.Timestamp = time.Unix(int64(epoch), 0)
	p = p[4:]

	// ReceiverAddress
	var err error
	ni.ReceiverAddress, p, err = DecodeAddress(p)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode receiver address")
	}

	// SenderAddresses
	if len(p) < 1 {
		return nil, ErrShortCellPayload
	}
	n := int(p[0])
	p = p[1:]
	ni.SenderAddresses = make([]net.IP, n)
	for i := 0; i < n; i++ {
		ni.SenderAddresses[i], p, err = DecodeAddress(p)
		if err != nil {
			return nil, errors.Wrap(err, "failed to decode sender address")
		}
	}

	return ni, nil
}

// Cell actually constructs the cell.
func (n NetInfoCell) Cell() (Cell, error) {
	c := NewFixedCell(0, CommandNetinfo)
	payload := c.Payload()

	// timestamp
	epoch := n.Timestamp.Unix()
	binary.BigEndian.PutUint32(payload, uint32(epoch))
	ptr := 4

	// receiver address
	b := EncodeAddress(n.ReceiverAddress)
	if b == nil {
		return nil, ErrUnencodableAddress
	}
	copy(payload[ptr:], b)
	ptr += len(b)

	// sender address
	payload[ptr] = byte(len(n.SenderAddresses))
	ptr++

	for _, addr := range n.SenderAddresses {
		b = EncodeAddress(addr)
		if b == nil {
			return nil, ErrUnencodableAddress
		}
		copy(payload[ptr:], b)
		ptr += len(b)
	}

	return c, nil
}

// EncodeAddress encodes the given IP address into the byte format appropriate
// for NETINFO cells and other purposes.
func EncodeAddress(ip net.IP) []byte {
	// Referenced in tor spec but in relation to something else.
	//
	// Reference: https://github.com/torproject/torspec/blob/8aaa36d1a062b20ca263b6ac613b77a3ba1eb113/tor-spec.txt#L1659-L1669
	//
	//	       Type   (1 octet)
	//	       Length (1 octet)
	//	       Value  (variable-width)
	//	       TTL    (4 octets)
	//	   "Length" is the length of the Value field.
	//	   "Type" is one of:
	//	      0x00 -- Hostname
	//	      0x04 -- IPv4 address
	//	      0x06 -- IPv6 address
	//	      0xF0 -- Error, transient
	//	      0xF1 -- Error, nontransient
	//
	// Reference: https://github.com/torproject/tor/blob/51e47481fc6f131d4e421de061029459ccbb033e/src/or/relay.c#L3015-L3042
	//
	//	/** Append an encoded value of <b>addr</b> to <b>payload_out</b>, which must
	//	 * have at least 18 bytes of free space.  The encoding is, as specified in
	//	 * tor-spec.txt:
	//	 *   RESOLVED_TYPE_IPV4 or RESOLVED_TYPE_IPV6  [1 byte]
	//	 *   LENGTH                                    [1 byte]
	//	 *   ADDRESS                                   [length bytes]
	//	 * Return the number of bytes added, or -1 on error */
	//	int
	//	append_address_to_payload(uint8_t *payload_out, const tor_addr_t *addr)
	//	{
	//	  uint32_t a;
	//	  switch (tor_addr_family(addr)) {
	//	  case AF_INET:
	//	    payload_out[0] = RESOLVED_TYPE_IPV4;
	//	    payload_out[1] = 4;
	//	    a = tor_addr_to_ipv4n(addr);
	//	    memcpy(payload_out+2, &a, 4);
	//	    return 6;
	//	  case AF_INET6:
	//	    payload_out[0] = RESOLVED_TYPE_IPV6;
	//	    payload_out[1] = 16;
	//	    memcpy(payload_out+2, tor_addr_to_in6_addr8(addr), 16);
	//	    return 18;
	//	  case AF_UNSPEC:
	//	  default:
	//	    return -1;
	//	  }
	//	}
	//
	// Reference: https://github.com/torproject/tor/blob/506b4bfabaf823225c34172fae6dd405cfe1b58e/src/or/or.h#L665-L669
	//
	//	#define RESOLVED_TYPE_HOSTNAME 0
	//	#define RESOLVED_TYPE_IPV4 4
	//	#define RESOLVED_TYPE_IPV6 6
	//	#define RESOLVED_TYPE_ERROR_TRANSIENT 0xF0
	//	#define RESOLVED_TYPE_ERROR 0xF1
	//

	ip4 := ip.To4()
	if ip4 != nil {
		return append([]byte{4, 4}, ip4...)
	}

	ip16 := ip.To16()
	if ip16 != nil {
		return append([]byte{6, 16}, ip16...)
	}

	return nil
}

// DecodeAddress decodes the given bytes into an IP and returns the remaining.
func DecodeAddress(b []byte) (net.IP, []byte, error) {
	if len(b) < 6 {
		return nil, nil, errors.New("too short")
	}

	// IPv4
	if b[0] == 4 && b[1] == 4 {
		return net.IP(b[2:6]), b[6:], nil
	}

	// IPv6
	if len(b) < 18 {
		return nil, nil, errors.New("too short")
	}
	if b[0] == 6 && b[1] == 16 {
		return net.IP(b[2:18]), b[18:], nil
	}

	return nil, nil, errors.New("unrecognized format")
}

func addrToIP(addr net.Addr) net.IP {
	return addr.(*net.TCPAddr).IP
}
