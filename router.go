package pearl

import (
	"crypto/rsa"
	"fmt"
	"net"
	"time"

	"github.com/mmcloughlin/pearl/log"
	"github.com/mmcloughlin/pearl/torconfig"
	"github.com/mmcloughlin/pearl/tordir"
	"github.com/mmcloughlin/pearl/torexitpolicy"
	"github.com/mmcloughlin/pearl/torkeys"
	"github.com/pkg/errors"
)

// Router is a Tor router.
type Router struct {
	config *torconfig.Config

	idKey    *rsa.PrivateKey
	onionKey *rsa.PrivateKey
	ntorKey  *torkeys.Curve25519KeyPair

	fingerprint []byte

	logger log.Logger
}

// TODO(mbm): determine which parts of Router struct are required for client and
// server. Perhaps a stripped down struct can be used for client-only.

// NewRouter constructs a router based on the given config.
func NewRouter(config *torconfig.Config, logger log.Logger) (*Router, error) {
	idKey, err := torkeys.GenerateRSA()
	if err != nil {
		return nil, err
	}

	onionKey, err := torkeys.GenerateRSA()
	if err != nil {
		return nil, err
	}

	ntorKey, err := torkeys.GenerateCurve25519KeyPair()
	if err != nil {
		return nil, err
	}

	fingerprint, err := torkeys.Fingerprint(&idKey.PublicKey)
	if err != nil {
		return nil, errors.Wrap(err, "failed to compute fingerprint")
	}

	logger = log.ForComponent(logger, "router")
	logger = log.WithBytes(logger, "fingerprint", fingerprint)
	return &Router{
		config:      config,
		idKey:       idKey,
		onionKey:    onionKey,
		ntorKey:     ntorKey,
		fingerprint: fingerprint,
		logger:      logger,
	}, nil
}

// IdentityKey returns the identity key of the router.
func (r *Router) IdentityKey() *rsa.PrivateKey {
	return r.idKey
}

// Fingerprint returns the router fingerprint.
func (r *Router) Fingerprint() []byte {
	return r.fingerprint
}

// Serve starts a listener and enters a main loop handling connections.
func (r *Router) Serve() error {
	laddr := fmt.Sprintf("localhost:%d", r.config.ORPort)
	r.logger.With("laddr", laddr).Info("creating listener")
	ln, err := net.Listen("tcp", laddr)
	if err != nil {
		return errors.Wrap(err, "could not create listener")
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			return errors.Wrap(err, "error accepting connection")
		}

		c, err := NewServer(r, conn, r.logger)
		if err != nil {
			return errors.Wrap(err, "error building connection")
		}

		go c.Handle()
	}
}

func (r *Router) Connect(raddr string) (*Connection, error) {
	conn, err := net.Dial("tcp", raddr)
	if err != nil {
		return nil, errors.Wrap(err, "dial failed")
	}

	c, err := NewClient(r, conn, r.logger)
	if err != nil {
		return nil, errors.Wrap(err, "building connection failed")
	}

	// TODO(mbm): should we be calling this here?
	err = c.clientHandshake()
	if err != nil {
		return nil, errors.Wrap(err, "handshake failed")
	}

	return c, nil
}

// Descriptor returns a server descriptor for this router.
func (r *Router) Descriptor() *tordir.ServerDescriptor {
	s := tordir.NewServerDescriptor()
	s.SetRouter(r.config.Nickname, net.IPv4(127, 0, 0, 1), r.config.ORPort, 0)
	s.SetPlatform(r.config.Platform)
	s.SetBandwidth(1000, 2000, 500)
	s.SetPublishedTime(time.Now())
	s.SetExitPolicy(torexitpolicy.RejectAllPolicy)
	s.SetSigningKey(r.IdentityKey())
	s.SetOnionKey(&r.onionKey.PublicKey)
	s.SetNtorOnionKey(r.ntorKey)
	return s
}
