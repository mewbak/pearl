package pearl

import (
	"crypto/cipher"
	"encoding"
	"encoding/binary"
	"io"
	"sync"

	"go.uber.org/multierr"

	"github.com/mmcloughlin/pearl/check"
	"github.com/mmcloughlin/pearl/fork/sha1"
	"github.com/mmcloughlin/pearl/log"
	"github.com/mmcloughlin/pearl/torcrypto"
)

const (
	defaultCircuitChannelBuffer = 16
)

// GenerateCircID generates a 4-byte circuit ID with the given most significant bit.
func GenerateCircID(msb uint32) CircID {
	b := torcrypto.Rand(4)
	x := binary.BigEndian.Uint32(b)
	x = (x >> 1) | (msb << 31)
	return CircID(x)
}

type CircuitCryptoState struct {
	stream cipher.Stream
	prev   *sha1.Digest
	digest *sha1.Digest
}

func NewCircuitCryptoState(d, k []byte) *CircuitCryptoState {
	h := sha1.New()
	torcrypto.HashWrite(h, d)
	return &CircuitCryptoState{
		prev:   h,
		digest: h,
		stream: torcrypto.NewStream(k),
	}
}

func (c *CircuitCryptoState) Sum() []byte {
	return c.digest.Sum(nil)
}

func (c *CircuitCryptoState) Digest() uint32 {
	s := c.Sum()
	return binary.BigEndian.Uint32(s)
}

func (c *CircuitCryptoState) RewindDigest() {
	c.digest = c.prev
}

func (c *CircuitCryptoState) Decrypt(b []byte) {
	c.stream.XORKeyStream(b, b)

	// Backup digest
	c.prev = c.digest.Clone()

	// Update digest by hashing the relay cell with digest cleared.
	r := relayCell(b)
	d := r.Digest()
	r.ClearDigest()
	torcrypto.HashWrite(c.digest, b)
	r.SetDigest(d)
}

func (c *CircuitCryptoState) EncryptOrigin(b []byte) {
	// Backup digest
	c.prev = c.digest.Clone()

	// Update digest by hashing the relay cell with digest cleared.
	r := relayCell(b)
	r.ClearDigest()
	torcrypto.HashWrite(c.digest, b)

	// Set correct value of the digest field
	r.SetDigest(c.Digest())

	c.Encrypt(b)
}

func (c *CircuitCryptoState) Encrypt(b []byte) {
	c.stream.XORKeyStream(b, b)
}

// TransverseCircuit is a circuit transiting through the relay.
type TransverseCircuit struct {
	Router   *Router
	Conn     *Connection
	Metrics  *Metrics
	Forward  *CircuitCryptoState
	Backward *CircuitCryptoState

	Prev   CircuitLink
	Next   CircuitLink
	pch    *CellChan
	nch    *CellChan
	done   chan struct{}
	reason CircuitErrorCode
	once   sync.Once
	wg     sync.WaitGroup

	logger log.Logger
}

func NewTransverseCircuit(conn *Connection, id CircID, fwd, back *CircuitCryptoState, l log.Logger) *TransverseCircuit {
	done := make(chan struct{})
	pch := NewCellChan(make(chan Cell, defaultCircuitChannelBuffer), done)
	nch := NewCellChan(make(chan Cell, defaultCircuitChannelBuffer), done)
	r := conn.router
	circ := &TransverseCircuit{
		Router:   r,
		Conn:     conn,
		Metrics:  r.metrics,
		Forward:  fwd,
		Backward: back,

		Prev:   NewCircuitLink(conn, id, pch),
		Next:   nil,
		pch:    pch,
		nch:    nch,
		done:   done,
		reason: CircuitErrorNone,

		logger: log.ForComponent(l, "transverse_circuit").With("circid", id),
	}

	circ.Metrics.Circuits.Alloc()

	circ.wg.Add(1)
	go circ.loop()

	return circ
}

func (t *TransverseCircuit) Close() error {
	_ = t.destroy(CircuitErrorOrConnClosed) // XXX error reason
	t.wg.Wait()
	return nil
}

func (t *TransverseCircuit) ForwardSender() CellSenderCloser {
	return NewLink(t.pch, nil, t)
}

func (t *TransverseCircuit) BackwardSender() CellSenderCloser {
	return NewLink(t.nch, nil, t)
}

func (t *TransverseCircuit) loop() {
	var err error
	for err == nil {
		err = t.oneCell()
	}

	if err != nil && !check.EOF(err) {
		log.Err(t.logger, err, "circuit handling error")
	}

	if err := t.cleanup(); err != nil {
		log.WithErr(t.logger, err).Debug("circuit cleanup error")
	}

	t.wg.Done()
}

func (t *TransverseCircuit) oneCell() error {
	select {
	case <-t.done:
		return io.EOF
	default:
	}

	var cell Cell
	var handler func(Cell) error
	var other CircuitLink

	select {
	case <-t.done:
		return io.EOF
	case cell = <-t.pch.C:
		handler = t.handleForwardRelay
		other = t.Next
	case cell = <-t.nch.C:
		handler = t.handleBackwardRelay
		other = t.Prev
	}

	switch cell.Command() {
	case CommandRelay, CommandRelayEarly:
		// TODO(mbm): count relay early cells
		return handler(cell)
	case CommandDestroy:
		return t.handleDestroy(cell, other)
	default:
		t.logger.Error("unrecognized cell")
		return t.destroy(CircuitErrorProtocol)
	}
}

func (t *TransverseCircuit) cleanup() error {
	var result error

	for _, c := range []CircuitLink{t.Prev, t.Next} {
		if c == nil {
			continue
		}
		err := c.Destroy(t.reason)
		if err != nil {
			result = multierr.Append(result, err)
		}
	}

	t.logger.Info("cleanup circuit")
	t.Metrics.Circuits.Free()

	return result
}

func (t *TransverseCircuit) destroy(reason CircuitErrorCode) error {
	t.once.Do(func() {
		t.logger.With("reason", reason).Info("marking circuit for destruction")
		t.reason = reason
		close(t.done)
	})
	return io.EOF
}

func (t *TransverseCircuit) handleForwardRelay(c Cell) error {
	// Decrypt payload.
	p := c.Payload()
	t.Forward.Decrypt(p)

	// Parse as relay cell.
	r := NewRelayCellFromBytes(p)
	logger := RelayCellLogger(t.logger, r)
	logger.Debug("received relay cell")

	// Reference: https://github.com/torproject/torspec/blob/4074b891e53e8df951fc596ac6758d74da290c60/tor-spec.txt#L1369-L1375
	//
	//	   The OR then decides whether it recognizes the relay cell, by
	//	   inspecting the payload as described in section 6.1 below.  If the OR
	//	   recognizes the cell, it processes the contents of the relay cell.
	//	   Otherwise, it passes the decrypted relay cell along the circuit if
	//	   the circuit continues.  If the OR at the end of the circuit
	//	   encounters an unrecognized relay cell, an error has occurred: the OR
	//	   sends a DESTROY cell to tear down the circuit.
	//
	if !relayCellIsRecogized(r, t.Forward) {
		logger.Debug("forwarding unrecognized cell")
		return t.handleUnrecognizedCell(c)
	}

	switch r.RelayCommand() {
	case RelayExtend:
		return t.handleRelayExtend(r)
	case RelayExtend2:
		return t.handleRelayExtend2(r)
	default:
		logger.Error("no handler registered")
	}

	return nil
}

// handleUnrecognizedCell passes an unrecognized cell onto the next hop.
func (t *TransverseCircuit) handleUnrecognizedCell(c Cell) error {
	if t.Next == nil {
		t.logger.Warn("no next hop")
		return t.destroy(CircuitErrorProtocol)
	}

	// Clone the cell but swap out the circuit ID.
	// TODO(mbm): forwarding relay cell should not require a copy, rather just
	// a modification of the incoming cell
	f := NewFixedCell(t.Next.CircID(), c.Command())
	copy(f.Payload(), c.Payload())

	err := t.Next.SendCell(f)
	if err != nil {
		t.logger.Warn("could not forward cell")
		return t.destroy(CircuitErrorConnectfailed)
	}

	t.Metrics.RelayForward.Inc(int64(len(c.Payload())))

	return nil
}

type extendRequest interface {
	encoding.BinaryUnmarshaler
	ConnectionHint
	Handshake() []byte
}

type createdReply interface {
	CellUnmarshaler
	Payloaded
}

func (t *TransverseCircuit) handleRelayExtend(r RelayCell) error {
	return t.extendCircuit(
		r,
		&ExtendPayload{},
		CommandCreate,
		&CreatedCell{},
		RelayExtended,
	)
}

func (t *TransverseCircuit) handleRelayExtend2(r RelayCell) error {
	return t.extendCircuit(
		r,
		&Extend2Payload{},
		CommandCreate2,
		&Created2Cell{},
		RelayExtended2,
	)
}

func (t *TransverseCircuit) extendCircuit(r RelayCell, ext extendRequest,
	createCmd Command, created createdReply, extendedCmd RelayCommand) error {
	// Reference: https://github.com/torproject/torspec/blob/8aaa36d1a062b20ca263b6ac613b77a3ba1eb113/tor-spec.txt#L1253-L1260
	//
	//	   When an onion router receives an EXTEND relay cell, it sends a CREATE
	//	   cell to the next onion router, with the enclosed onion skin as its
	//	   payload.  As special cases, if the extend cell includes a digest of
	//	   all zeroes, or asks to extend back to the relay that sent the extend
	//	   cell, the circuit will fail and be torn down. The initiating onion
	//	   router chooses some circID not yet used on the connection between the
	//	   two onion routers.  (But see section 5.1.1 above, concerning choosing
	//	   circIDs based on lexicographic order of nicknames.)
	//

	if t.Next != nil {
		t.logger.Warn("extend cell on circuit that already has next hop")
		return t.destroy(CircuitErrorProtocol)
	}

	// Parse payload
	d, err := r.RelayData()
	if err != nil {
		log.Err(t.logger, err, "could not extract relay data")
		return t.destroy(CircuitErrorProtocol)
	}
	err = ext.UnmarshalBinary(d)
	if err != nil {
		log.Err(t.logger, err, "bad extend2 playload")
		return t.destroy(CircuitErrorProtocol)
	}

	// Obtain connection to referenced node.
	nextConn, err := t.Router.Connection(ext)
	if err != nil {
		log.Err(t.logger, err, "could not obtain connection to extend node")
		return t.destroy(CircuitErrorConnectfailed)
	}

	// Initialize circuit on the next connection
	nextID, err := nextConn.circuits.Add(t.BackwardSender())
	if err != nil {
		log.Err(t.logger, err, "could not register circuit with next connection")
		return t.destroy(CircuitErrorOrConnClosed)
	}
	t.Next = NewCircuitLink(nextConn, nextID, t.nch)

	// Send CREATE2 cell
	cell := NewFixedCell(t.Next.CircID(), createCmd)
	copy(cell.Payload(), ext.Handshake()) // BUG(mbm): overflow risk

	err = t.Next.SendCell(cell)
	if err != nil {
		log.Err(t.logger, err, "failed to send create cell")
		return t.destroy(CircuitErrorConnectfailed)
	}

	// Wait for CREATED2 cell
	t.logger.Debug("waiting for CREATED2")
	cell, err = t.Next.ReceiveCell()
	if err != nil {
		log.Err(t.logger, err, "failed to receive cell")
		return t.destroy(CircuitErrorConnectfailed)
	}

	err = created.UnmarshalCell(cell)
	if err != nil {
		log.Err(t.logger, err, "failed to parse created cell")
		return t.destroy(CircuitErrorProtocol)
	}

	// Reply with EXTENDED2
	cell = NewFixedCell(t.Prev.CircID(), CommandRelay)
	extended := NewRelayCell(extendedCmd, 0, created.Payload())
	copy(cell.Payload(), extended.Bytes())
	t.Backward.EncryptOrigin(cell.Payload())

	err = t.Prev.SendCell(cell)
	if err != nil {
		log.Err(t.logger, err, "failed to send relay extended cell")
		return t.destroy(CircuitErrorConnectfailed)
	}

	t.logger.Info("circuit extended")

	return nil
}
func (t *TransverseCircuit) handleDestroy(c Cell, other CircuitLink) error {
	var reason CircuitErrorCode
	d, err := ParseDestroyCell(c)
	if err != nil {
		log.Err(t.logger, err, "failed to parse destroy cell")
		reason = CircuitErrorNone
	} else if d != nil {
		reason = d.Reason
		t.logger.With("reason", reason).Debug("received destroy cell")
	}

	return t.destroy(reason)
}

func (t *TransverseCircuit) handleBackwardRelay(c Cell) error {
	// Encrypt payload.
	p := c.Payload()
	t.Backward.Encrypt(p)

	// Clone the cell but swap out the circuit ID.
	// TODO(mbm): forwarding relay cell should not require a copy, rather just
	// a modification of the incoming cell
	f := NewFixedCell(t.Prev.CircID(), c.Command())
	copy(f.Payload(), c.Payload())

	err := t.Prev.SendCell(f)
	if err != nil {
		t.logger.Warn("could not forward cell")
		return t.destroy(CircuitErrorConnectfailed)
	}

	t.Metrics.RelayBackward.Inc(int64(len(c.Payload())))

	return nil
}

func relayCellIsRecogized(r RelayCell, cs *CircuitCryptoState) bool {
	// Reference: https://github.com/torproject/torspec/blob/4074b891e53e8df951fc596ac6758d74da290c60/tor-spec.txt#L1446-L1452
	//
	//	   The 'recognized' field in any unencrypted relay payload is always set
	//	   to zero; the 'digest' field is computed as the first four bytes of
	//	   the running digest of all the bytes that have been destined for
	//	   this hop of the circuit or originated from this hop of the circuit,
	//	   seeded from Df or Db respectively (obtained in section 5.2 above),
	//	   and including this RELAY cell's entire payload (taken with the digest
	//	   field set to zero).
	//

	if r.Recognized() != 0 {
		return false
	}

	digest := cs.Digest()
	if digest != r.Digest() {
		cs.RewindDigest()
		return false
	}

	return true
}
