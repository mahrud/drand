package core

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"

	"github.com/dedis/drand/beacon"
	"github.com/dedis/drand/dkg"
	"github.com/dedis/drand/ecies"
	"github.com/dedis/drand/fs"
	"github.com/dedis/drand/key"
	"github.com/dedis/drand/net"
	"github.com/dedis/drand/protobuf/crypto"
	dkg_proto "github.com/dedis/drand/protobuf/dkg"
	"github.com/dedis/drand/protobuf/drand"
	"github.com/dedis/kyber"
	"github.com/nikkolasg/slog"
)

// Drand is the main logic of the program. It reads the keys / group file, it
// can start the DKG, read/write shars to files and can initiate/respond to TBlS
// signature requests.
type Drand struct {
	opts    *Config
	priv    *key.Pair
	group   *key.Group
	store   key.Store
	gateway net.Gateway

	dkg         *dkg.Handler
	beacon      *beacon.Handler
	beaconStore beacon.Store
	// dkg private share. can be nil if dkg not finished yet.
	share *key.Share
	// dkg public key. Can be nil if dkg not finished yet.
	pub     *key.DistPublic
	dkgDone bool

	state sync.Mutex
}

// NewDrand returns an drand struct that is ready to start the DKG protocol with
// the given group and then to serve randomness. It assumes the private key pair
// has been generated already.
func NewDrand(s key.Store, g *key.Group, c *Config) (*Drand, error) {
	d, err := initDrand(s, c)
	if err != nil {
		return nil, err
	}
	dkgConf := &dkg.Config{
		Suite:   key.G2.(dkg.Suite),
		Group:   g,
		Timeout: d.opts.dkgTimeout,
	}
	d.dkg, err = dkg.NewHandler(d.priv, dkgConf, d.dkgNetwork())
	d.group = g
	return d, err
}

// initDrand inits the drand struct by loading the private key, and by creating the
// gateway with the correct options.
func initDrand(s key.Store, c *Config) (*Drand, error) {
	if c.insecure == false && (c.certPath == "" || c.keyPath == "") {
		return nil, errors.New("config: need to set WithInsecure if no certificate and private key path given")
	}
	priv, err := s.LoadKeyPair()
	if err != nil {
		return nil, err
	}
	// trick to always set the listening address by default based on the
	// identity. If there is an option to set the address, it will override the
	// default set here..
	d := &Drand{
		store: s,
		priv:  priv,
		opts:  c,
	}

	a := c.ListenAddress(priv.Public.Address())
	if c.insecure {
		d.gateway = net.NewGrpcGatewayInsecure(a, d, d.opts.grpcOpts...)
	} else {
		d.gateway = net.NewGrpcGatewayFromCertManager(a, c.certPath, c.keyPath, c.certmanager, d, d.opts.grpcOpts...)
	}
	go d.gateway.Start()
	return d, nil
}

// LoadDrand restores a drand instance as it was running after a DKG instance
func LoadDrand(s key.Store, c *Config) (*Drand, error) {
	d, err := initDrand(s, c)
	if err != nil {
		return nil, err
	}
	d.group, err = s.LoadGroup()
	if err != nil {
		return nil, err
	}
	d.share, err = s.LoadShare()
	if err != nil {
		return nil, err
	}
	d.pub, err = s.LoadDistPublic()
	if err != nil {
		return nil, err
	}
	if err := d.initBeacon(); err != nil {
		return nil, err
	}
	slog.Debugf("drand: loaded and serving at %s", d.priv.Public.Address())
	return d, nil
}

// StartDKG starts the DKG protocol by sending the first packet of the DKG
// protocol to every other node in the group. It returns nil if the DKG protocol
// finished successfully or an error otherwise.
func (d *Drand) StartDKG() error {
	d.dkg.Start()
	return d.WaitDKG()
}

// WaitDKG waits messages from the DKG protocol started by a leader or some
// nodes, and then wait until completion.
func (d *Drand) WaitDKG() error {
	var err error
	select {
	case share := <-d.dkg.WaitShare():
		s := key.Share(share)
		d.share = &s
	case err = <-d.dkg.WaitError():
	}
	if err != nil {
		return err
	}
	d.store.SaveShare(d.share)
	d.store.SaveDistPublic(d.share.Public())
	// XXX See if needed to change to qualified group
	d.store.SaveGroup(d.group)
	return d.initBeacon()
}

var DefaultSeed = []byte("Truth is like the sun. You can shut it out for a time, but it ain't goin' away.")

// BeaconLoop starts periodically the TBLS protocol. The seed is the first
// message signed alongside with the current timestamp. All subsequent
// signatures are chained:
// s_i+1 = SIG(s_i || timestamp)
// For the moment, each resulting signature is stored in a file named
// beacons/<timestamp>.sig.
func (d *Drand) BeaconLoop() {
	// heuristic: we catchup when we can retrieve a beacon from the db
	// if there is an error we quit, if there is no beacon saved yet, we
	// run the loop as usual.
	var catchup = true
	b, err := d.beaconStore.Last()
	if err != nil {
		if err == beacon.ErrNoBeaconSaved {
			// we are starting the beacon generation
			catchup = false
		} else {
			// there's a serious error
			slog.Printf("drand: could not determine beacon state: %s", err)
			return
		}
	}
	if catchup {
		slog.Infof("drand: starting beacon loop in catch-up mode", err, b)
	} else {
		slog.Infof("drand: starting beacon loop")
	}
	d.beacon.Loop(DefaultSeed, d.opts.beaconPeriod, catchup)
}

func (d *Drand) Public(context.Context, *drand.PublicRandRequest) (*drand.PublicRandResponse, error) {
	beacon, err := d.beaconStore.Last()
	if err != nil {
		return nil, fmt.Errorf("can't retrieve beacon: %s", err)
	}
	return &drand.PublicRandResponse{
		Previous:   beacon.PreviousRand,
		Round:      beacon.Round,
		Randomness: beacon.Randomness,
	}, nil
}

func (d *Drand) Private(c context.Context, priv *drand.PrivateRandRequest) (*drand.PrivateRandResponse, error) {
	protoPoint := priv.GetRequest().GetEphemeral()
	point, err := crypto.ProtoToKyberPoint(protoPoint)
	if err != nil {
		return nil, err
	}
	groupable, ok := point.(kyber.Groupable)
	if !ok {
		return nil, errors.New("point is not on a registered curve")
	}
	if groupable.Group().String() != key.G2.String() {
		return nil, errors.New("point is not on the supported curve")
	}
	msg, err := ecies.Decrypt(key.G2, ecies.DefaultHash, d.priv.Key, priv.GetRequest())
	if err != nil {
		slog.Debugf("drand: received invalid ECIES private request:", err)
		return nil, errors.New("invalid ECIES request")
	}

	clientKey := key.G2.Point()
	if err := clientKey.UnmarshalBinary(msg); err != nil {
		return nil, errors.New("invalid client key")
	}
	var randomness [32]byte
	if n, err := rand.Read(randomness[:]); err != nil {
		return nil, errors.New("error gathering randomness")
	} else if n != 32 {
		return nil, errors.New("error gathering randomness")
	}

	obj, err := ecies.Encrypt(key.G2, ecies.DefaultHash, clientKey, randomness[:])
	return &drand.PrivateRandResponse{obj}, err
}

func (d *Drand) Setup(c context.Context, in *dkg_proto.DKGPacket) (*dkg_proto.DKGResponse, error) {
	if d.isDKGDone() {
		return nil, errors.New("drand: dkg finished already")
	}
	d.dkg.Process(c, in)
	return &dkg_proto.DKGResponse{}, nil
}

func (d *Drand) NewBeacon(c context.Context, in *drand.BeaconRequest) (*drand.BeaconResponse, error) {
	if !d.isDKGDone() {
		return nil, errors.New("drand: dkg not finished")
	}
	if d.beacon == nil {
		panic("that's not ever should happen so I'm panicking right now")
	}
	return d.beacon.ProcessBeacon(c, in)
}

func (d *Drand) Stop() {
	d.state.Lock()
	defer d.state.Unlock()
	d.gateway.Stop()
	if d.beacon != nil {
		d.beacon.Stop()
	}
}

// isDKGDone returns true if the DKG protocol has already been executed. That
// means that the only packet that this node should receive are TBLS packet.
func (d *Drand) isDKGDone() bool {
	d.state.Lock()
	defer d.state.Unlock()
	return d.dkgDone
}

func (d *Drand) initBeacon() error {
	d.state.Lock()
	defer d.state.Unlock()
	d.dkgDone = true
	fs.CreateSecureFolder(d.opts.DBFolder())
	store, err := beacon.NewBoltStore(d.opts.dbFolder, d.opts.boltOpts)
	if err != nil {
		return err
	}
	d.beaconStore = beacon.NewCallbackStore(store, d.beaconCallback)
	d.beacon = beacon.NewHandler(d.gateway.InternalClient, d.priv, d.share, d.group, d.beaconStore)
	return nil
}

func (d *Drand) beaconCallback(b *beacon.Beacon) {
	d.opts.callbacks(b)
}

// little trick to be able to capture when drand is using the DKG methods,
// instead of offloading that to an external struct without any vision of drand
// internals, or implementing a big "Send" method directly on drand.
func (d *Drand) sendDkgPacket(p net.Peer, pack *dkg_proto.DKGPacket) error {
	_, err := d.gateway.InternalClient.Setup(p, pack)
	return err
}

func (d *Drand) dkgNetwork() *dkgNetwork {
	return &dkgNetwork{d.sendDkgPacket}
}

type dkgNetwork struct {
	send func(net.Peer, *dkg_proto.DKGPacket) error
}

func (d *dkgNetwork) Send(p net.Peer, pack *dkg_proto.DKGPacket) error {
	return d.send(p, pack)
}
