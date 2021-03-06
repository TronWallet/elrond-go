package libp2p

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	logger "github.com/ElrondNetwork/elrond-go-logger"
	"github.com/ElrondNetwork/elrond-go/config"
	"github.com/ElrondNetwork/elrond-go/core"
	"github.com/ElrondNetwork/elrond-go/core/check"
	"github.com/ElrondNetwork/elrond-go/core/throttler"
	"github.com/ElrondNetwork/elrond-go/p2p"
	connMonitorFactory "github.com/ElrondNetwork/elrond-go/p2p/libp2p/connectionMonitor/factory"
	"github.com/ElrondNetwork/elrond-go/p2p/libp2p/disabled"
	discoveryFactory "github.com/ElrondNetwork/elrond-go/p2p/libp2p/discovery/factory"
	"github.com/ElrondNetwork/elrond-go/p2p/libp2p/metrics"
	"github.com/ElrondNetwork/elrond-go/p2p/libp2p/networksharding/factory"
	randFactory "github.com/ElrondNetwork/elrond-go/p2p/libp2p/rand/factory"
	"github.com/ElrondNetwork/elrond-go/p2p/loadBalancer"
	"github.com/btcsuite/btcd/btcec"
	logging "github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p"
	libp2pCrypto "github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
)

// ListenAddrWithIp4AndTcp defines the listening address with ip v.4 and TCP
const ListenAddrWithIp4AndTcp = "/ip4/0.0.0.0/tcp/"

// ListenLocalhostAddrWithIp4AndTcp defines the local host listening ip v.4 address and TCP
const ListenLocalhostAddrWithIp4AndTcp = "/ip4/127.0.0.1/tcp/"

// DirectSendID represents the protocol ID for sending and receiving direct P2P messages
const DirectSendID = protocol.ID("/erd/directsend/1.0.0")

const durationBetweenSends = time.Microsecond * 10
const durationCheckConnections = time.Second
const refreshPeersOnTopic = time.Second * 3
const ttlPeersOnTopic = time.Second * 10
const pubsubTimeCacheDuration = 10 * time.Minute
const broadcastGoRoutines = 1000
const timeBetweenPeerPrints = time.Second * 20
const timeBetweenExternalLoggersCheck = time.Second * 20
const defaultThresholdMinConnectedPeers = 3
const minRangePortValue = 1025

//TODO remove the header size of the message when commit d3c5ecd3a3e884206129d9f2a9a4ddfd5e7c8951 from
// https://github.com/libp2p/go-libp2p-pubsub/pull/189/commits will be part of a new release
var messageHeader = 64 * 1024 //64kB
var maxSendBuffSize = (1 << 20) - messageHeader

var log = logger.GetOrCreate("p2p/libp2p")

var _ p2p.Messenger = (*networkMessenger)(nil)
var externalPackages = []string{"dht", "nat", "basichost", "pubsub"}

func init() {
	for _, external := range externalPackages {
		_ = logger.GetOrCreate(fmt.Sprintf("external/%s", external))
	}
}

//TODO refactor this struct to have be a wrapper (with logic) over a glue code
type networkMessenger struct {
	ctx        context.Context
	cancelFunc context.CancelFunc
	p2pHost    ConnectableHost
	pb         *pubsub.PubSub
	ds         p2p.DirectSender
	//TODO refactor this (connMonitor & connMonitorWrapper)
	connMonitor         ConnectionMonitor
	connMonitorWrapper  p2p.ConnectionMonitorWrapper
	peerDiscoverer      p2p.PeerDiscoverer
	sharder             p2p.CommonSharder
	peerShardResolver   p2p.PeerShardResolver
	mutTopics           sync.RWMutex
	processors          map[string]p2p.MessageProcessor
	topics              map[string]*pubsub.Topic
	subscriptions       map[string]*pubsub.Subscription
	outgoingPLB         p2p.ChannelLoadBalancer
	poc                 *peersOnChannel
	goRoutinesThrottler *throttler.NumGoRoutinesThrottler
	ip                  *identityProvider
	connectionsMetric   *metrics.Connections
	mutMessageIdCacher  sync.RWMutex
	messageIdCacher     p2p.Cacher
}

// ArgsNetworkMessenger defines the options used to create a p2p wrapper
type ArgsNetworkMessenger struct {
	ListenAddress string
	P2pConfig     config.P2PConfig
}

// NewNetworkMessenger creates a libP2P messenger by opening a port on the current machine
func NewNetworkMessenger(args ArgsNetworkMessenger) (*networkMessenger, error) {
	p2pPrivKey, err := createP2PPrivKey(args.P2pConfig.Node.Seed)
	if err != nil {
		return nil, err
	}

	port, err := getPort(args.P2pConfig.Node.Port, checkFreePort)
	if err != nil {
		return nil, err
	}

	address := fmt.Sprintf(args.ListenAddress+"%d", port)
	opts := []libp2p.Option{
		libp2p.ListenAddrStrings(address),
		libp2p.Identity(p2pPrivKey),
		libp2p.DefaultMuxers,
		libp2p.DefaultSecurity,
		libp2p.DefaultTransports,
		//we need the disable relay option in order to save the node's bandwidth as much as possible
		libp2p.DisableRelay(),
		libp2p.NATPortMap(),
	}

	setupExternalP2PLoggers()

	ctx, cancelFunc := context.WithCancel(context.Background())
	h, err := libp2p.New(ctx, opts...)
	if err != nil {
		return nil, err
	}

	p2pNode, err := createMessenger(args, h, ctx, cancelFunc, true)
	if err != nil {
		log.LogIfError(h.Close())
		return nil, err
	}

	return p2pNode, nil
}

func setupExternalP2PLoggers() {
	for _, external := range externalPackages {
		logLevel := logger.GetLoggerLogLevel("external/" + external)
		if logLevel > logger.LogTrace {
			continue
		}

		_ = logging.SetLogLevel(external, "DEBUG")
	}
}

func createP2PPrivKey(seed string) (*libp2pCrypto.Secp256k1PrivateKey, error) {
	randReader, err := randFactory.NewRandFactory(seed)
	if err != nil {
		return nil, err
	}

	prvKey, _ := ecdsa.GenerateKey(btcec.S256(), randReader)

	return (*libp2pCrypto.Secp256k1PrivateKey)(prvKey), nil
}

func createMessenger(
	args ArgsNetworkMessenger,
	p2pHost host.Host,
	ctx context.Context,
	cancelFunc context.CancelFunc,
	withMessageSigning bool,
) (*networkMessenger, error) {
	var err error
	netMes := networkMessenger{
		ctx:               ctx,
		cancelFunc:        cancelFunc,
		p2pHost:           NewConnectableHost(p2pHost),
		processors:        make(map[string]p2p.MessageProcessor),
		topics:            make(map[string]*pubsub.Topic),
		subscriptions:     make(map[string]*pubsub.Subscription),
		outgoingPLB:       loadBalancer.NewOutgoingChannelLoadBalancer(),
		peerShardResolver: &unknownPeerShardResolver{},
		messageIdCacher:   &disabled.Cacher{},
	}

	err = netMes.createPubSub(withMessageSigning)
	if err != nil {
		return nil, err
	}

	err = netMes.createSharder(args.P2pConfig)
	if err != nil {
		return nil, err
	}

	err = netMes.createDiscoverer(args.P2pConfig)
	if err != nil {
		return nil, err
	}

	err = netMes.createConnectionMonitor(args.P2pConfig)
	if err != nil {
		return nil, err
	}

	netMes.createConnectionsMetric()

	netMes.ds, err = NewDirectSender(ctx, p2pHost, netMes.directMessageHandler)
	if err != nil {
		return nil, err
	}

	netMes.goRoutinesThrottler, err = throttler.NewNumGoRoutinesThrottler(broadcastGoRoutines)
	if err != nil {
		return nil, err
	}

	netMes.printLogs()

	return &netMes, nil
}

func (netMes *networkMessenger) createPubSub(withMessageSigning bool) error {
	optsPS := []pubsub.Option{
		pubsub.WithMessageSigning(withMessageSigning),
	}

	pubsub.TimeCacheDuration = pubsubTimeCacheDuration

	var err error
	netMes.pb, err = pubsub.NewGossipSub(netMes.ctx, netMes.p2pHost, optsPS...)
	if err != nil {
		return err
	}

	netMes.poc, err = newPeersOnChannel(
		netMes.pb.ListPeers,
		refreshPeersOnTopic,
		ttlPeersOnTopic)
	if err != nil {
		return err
	}

	go func(plb p2p.ChannelLoadBalancer) {
		for {
			select {
			case <-time.After(durationBetweenSends):
			case <-netMes.ctx.Done():
				return
			}

			sendableData := plb.CollectOneElementFromChannels()
			if sendableData == nil {
				continue
			}

			netMes.mutTopics.RLock()
			topic := netMes.topics[sendableData.Topic]
			netMes.mutTopics.RUnlock()

			if topic == nil {
				log.Warn("writing on a topic that the node did not register on - message dropped",
					"topic", sendableData.Topic,
				)

				continue
			}

			errPublish := topic.Publish(netMes.ctx, sendableData.Buff)
			if errPublish != nil {
				log.Trace("error sending data", "error", errPublish)
			}
		}
	}(netMes.outgoingPLB)

	return nil
}

func (netMes *networkMessenger) createSharder(p2pConfig config.P2PConfig) error {
	args := factory.ArgsSharderFactory{
		PeerShardResolver:       &unknownPeerShardResolver{},
		Pid:                     netMes.p2pHost.ID(),
		MaxConnectionCount:      p2pConfig.Sharding.TargetPeerCount,
		MaxIntraShardValidators: int(p2pConfig.Sharding.MaxIntraShardValidators),
		MaxCrossShardValidators: int(p2pConfig.Sharding.MaxCrossShardValidators),
		MaxIntraShardObservers:  int(p2pConfig.Sharding.MaxIntraShardObservers),
		MaxCrossShardObservers:  int(p2pConfig.Sharding.MaxCrossShardObservers),
		Type:                    p2pConfig.Sharding.Type,
	}

	var err error
	netMes.sharder, err = factory.NewSharder(args)

	return err
}

func (netMes *networkMessenger) createDiscoverer(p2pConfig config.P2PConfig) error {
	var err error
	netMes.peerDiscoverer, err = discoveryFactory.NewPeerDiscoverer(
		netMes.ctx,
		netMes.p2pHost,
		netMes.sharder,
		p2pConfig,
	)

	return err
}

func (netMes *networkMessenger) createConnectionMonitor(p2pConfig config.P2PConfig) error {
	reconnecter, ok := netMes.peerDiscoverer.(p2p.Reconnecter)
	if !ok {
		return fmt.Errorf("%w when converting peerDiscoverer to reconnecter interface", p2p.ErrWrongTypeAssertion)
	}

	args := connMonitorFactory.ArgsConnectionMonitorFactory{
		Reconnecter:                reconnecter,
		Sharder:                    netMes.sharder,
		ThresholdMinConnectedPeers: defaultThresholdMinConnectedPeers,
		TargetCount:                p2pConfig.Sharding.TargetPeerCount,
	}
	var err error
	netMes.connMonitor, err = connMonitorFactory.NewConnectionMonitor(args)
	if err != nil {
		return err
	}

	cmw := newConnectionMonitorWrapper(
		netMes.p2pHost.Network(),
		netMes.connMonitor,
		&nilBlacklistHandler{},
	)
	netMes.p2pHost.Network().Notify(cmw)
	netMes.connMonitorWrapper = cmw

	go func() {
		for {
			cmw.CheckConnectionsBlocking()
			time.Sleep(durationCheckConnections)
		}
	}()

	return nil
}

func (netMes *networkMessenger) createConnectionsMetric() {
	netMes.connectionsMetric = metrics.NewConnections()
	netMes.p2pHost.Network().Notify(netMes.connectionsMetric)
}

func (netMes *networkMessenger) printLogs() {
	addresses := make([]interface{}, 0)
	for i, address := range netMes.p2pHost.Addrs() {
		addresses = append(addresses, fmt.Sprintf("addr%d", i))
		addresses = append(addresses, address.String()+"/p2p/"+netMes.ID().Pretty())
	}
	log.Info("listening on addresses", addresses...)

	go netMes.printLogsStats()
	go netMes.checkExternalLoggers()
}

func (netMes *networkMessenger) printLogsStats() {
	for {
		select {
		case <-netMes.ctx.Done():
			return
		case <-time.After(timeBetweenPeerPrints):
		}

		conns := netMes.connectionsMetric.ResetNumConnections()
		disconns := netMes.connectionsMetric.ResetNumDisconnections()

		peersInfo := netMes.GetConnectedPeersInfo()
		log.Debug("network connection status",
			"known peers", len(netMes.Peers()),
			"connected peers", len(netMes.ConnectedPeers()),
			"intra shard validators", len(peersInfo.IntraShardValidators),
			"intra shard observers", len(peersInfo.IntraShardObservers),
			"cross shard validators", len(peersInfo.CrossShardValidators),
			"cross shard observers", len(peersInfo.CrossShardObservers),
			"unknown", len(peersInfo.UnknownPeers),
		)

		connsPerSec := conns / uint32(timeBetweenPeerPrints/time.Second)
		disconnsPerSec := disconns / uint32(timeBetweenPeerPrints/time.Second)

		log.Debug("network connection metrics",
			"connections/s", connsPerSec,
			"disconnections/s", disconnsPerSec,
		)
	}
}

func (netMes *networkMessenger) checkExternalLoggers() {
	for {
		select {
		case <-netMes.ctx.Done():
			return
		case <-time.After(timeBetweenExternalLoggersCheck):
		}

		setupExternalP2PLoggers()
	}
}

// ApplyOptions can set up different configurable options of a networkMessenger instance
func (netMes *networkMessenger) ApplyOptions(opts ...Option) error {
	for _, opt := range opts {
		err := opt(netMes)
		if err != nil {
			return err
		}
	}
	return nil
}

// Close closes the host, connections and streams
func (netMes *networkMessenger) Close() error {
	log.Debug("closing network messenger's host...")

	var err error
	errHost := netMes.p2pHost.Close()
	if errHost != nil {
		err = errHost
		log.Warn("networkMessenger.Close",
			"component", "host",
			"error", err)
	}

	log.Debug("closing network messenger's outgoing load balancer...")

	errOplb := netMes.outgoingPLB.Close()
	if errOplb != nil {
		err = errOplb
		log.Warn("networkMessenger.Close",
			"component", "outgoingPLB",
			"error", err)
	}

	log.Debug("closing network messenger's components through the context...")

	netMes.cancelFunc()

	if err == nil {
		log.Info("network messenger closed successfully")
	}

	return err
}

// ID returns the messenger's ID
func (netMes *networkMessenger) ID() core.PeerID {
	h := netMes.p2pHost

	return core.PeerID(h.ID())
}

// Peers returns the list of all known peers ID (including self)
func (netMes *networkMessenger) Peers() []core.PeerID {
	peers := make([]core.PeerID, 0)

	for _, p := range netMes.p2pHost.Peerstore().Peers() {
		peers = append(peers, core.PeerID(p))
	}
	return peers
}

// Addresses returns all addresses found in peerstore
func (netMes *networkMessenger) Addresses() []string {
	addrs := make([]string, 0)

	for _, address := range netMes.p2pHost.Addrs() {
		addrs = append(addrs, address.String()+"/p2p/"+netMes.ID().Pretty())
	}

	return addrs
}

// ConnectToPeer tries to open a new connection to a peer
func (netMes *networkMessenger) ConnectToPeer(address string) error {
	return netMes.p2pHost.ConnectToPeer(netMes.ctx, address)
}

// Bootstrap will start the peer discovery mechanism
func (netMes *networkMessenger) Bootstrap() error {
	return netMes.peerDiscoverer.Bootstrap()
}

// IsConnected returns true if current node is connected to provided peer
func (netMes *networkMessenger) IsConnected(peerID core.PeerID) bool {
	h := netMes.p2pHost

	connectedness := h.Network().Connectedness(peer.ID(peerID))

	return connectedness == network.Connected
}

// ConnectedPeers returns the current connected peers list
func (netMes *networkMessenger) ConnectedPeers() []core.PeerID {
	h := netMes.p2pHost

	connectedPeers := make(map[core.PeerID]struct{})

	for _, conn := range h.Network().Conns() {
		p := core.PeerID(conn.RemotePeer())

		if netMes.IsConnected(p) {
			connectedPeers[p] = struct{}{}
		}
	}

	peerList := make([]core.PeerID, len(connectedPeers))

	index := 0
	for k := range connectedPeers {
		peerList[index] = k
		index++
	}

	return peerList
}

// ConnectedAddresses returns all connected peer's addresses
func (netMes *networkMessenger) ConnectedAddresses() []string {
	h := netMes.p2pHost
	conns := make([]string, 0)

	for _, c := range h.Network().Conns() {
		conns = append(conns, c.RemoteMultiaddr().String()+"/p2p/"+c.RemotePeer().Pretty())
	}
	return conns
}

// PeerAddresses returns the peer's addresses or empty slice if the peer is unknown
func (netMes *networkMessenger) PeerAddresses(pid core.PeerID) []string {
	h := netMes.p2pHost
	result := make([]string, 0)

	//check if the peer is connected to return it's connected address
	for _, c := range h.Network().Conns() {
		if string(c.RemotePeer()) == string(pid.Bytes()) {
			result = append(result, c.RemoteMultiaddr().String())
			break
		}
	}

	//check in peerstore (maybe it is known but not connected)
	addresses := h.Peerstore().Addrs(peer.ID(pid.Bytes()))
	for _, addr := range addresses {
		result = append(result, addr.String())
	}

	return result
}

// ConnectedPeersOnTopic returns the connected peers on a provided topic
func (netMes *networkMessenger) ConnectedPeersOnTopic(topic string) []core.PeerID {
	return netMes.poc.ConnectedPeersOnChannel(topic)
}

// CreateTopic opens a new topic using pubsub infrastructure
func (netMes *networkMessenger) CreateTopic(name string, createChannelForTopic bool) error {
	netMes.mutTopics.Lock()
	defer netMes.mutTopics.Unlock()
	_, found := netMes.topics[name]
	if found {
		return nil
	}

	topic, err := netMes.pb.Join(name)
	if err != nil {
		return fmt.Errorf("%w for topic %s", err, name)
	}

	netMes.topics[name] = topic
	subscrRequest, err := topic.Subscribe()
	if err != nil {
		return fmt.Errorf("%w for topic %s", err, name)
	}

	netMes.subscriptions[name] = subscrRequest
	if createChannelForTopic {
		err = netMes.outgoingPLB.AddChannel(name)
	}

	//just a dummy func to consume messages received by the newly created topic
	go func() {
		var errSubscrNext error
		for {
			_, errSubscrNext = subscrRequest.Next(netMes.ctx)
			if errSubscrNext != nil {
				log.Debug("closed subscription",
					"topic", subscrRequest.Topic(),
					"err", errSubscrNext,
				)
				return
			}
		}
	}()

	return err
}

// HasTopic returns true if the topic has been created
func (netMes *networkMessenger) HasTopic(name string) bool {
	netMes.mutTopics.RLock()
	_, found := netMes.topics[name]
	netMes.mutTopics.RUnlock()

	return found
}

// HasTopicValidator returns true if the topic has a validator set
func (netMes *networkMessenger) HasTopicValidator(name string) bool {
	netMes.mutTopics.RLock()
	validator := netMes.processors[name]
	netMes.mutTopics.RUnlock()

	return validator != nil
}

// BroadcastOnChannelBlocking tries to send a byte buffer onto a topic using provided channel
// It is a blocking method. It needs to be launched on a go routine
func (netMes *networkMessenger) BroadcastOnChannelBlocking(channel string, topic string, buff []byte) error {
	if len(buff) > maxSendBuffSize {
		return p2p.ErrMessageTooLarge
	}

	if !netMes.goRoutinesThrottler.CanProcess() {
		return p2p.ErrTooManyGoroutines
	}

	netMes.goRoutinesThrottler.StartProcessing()

	sendable := &p2p.SendableData{
		Buff:  buff,
		Topic: topic,
	}
	netMes.outgoingPLB.GetChannelOrDefault(channel) <- sendable
	netMes.goRoutinesThrottler.EndProcessing()
	return nil
}

// BroadcastOnChannel tries to send a byte buffer onto a topic using provided channel
func (netMes *networkMessenger) BroadcastOnChannel(channel string, topic string, buff []byte) {
	go func() {
		err := netMes.BroadcastOnChannelBlocking(channel, topic, buff)
		if err != nil {
			log.Warn("p2p broadcast", "error", err.Error())
		}
	}()
}

// Broadcast tries to send a byte buffer onto a topic using the topic name as channel
func (netMes *networkMessenger) Broadcast(topic string, buff []byte) {
	netMes.BroadcastOnChannel(topic, topic, buff)
}

// RegisterMessageProcessor registers a message process on a topic
func (netMes *networkMessenger) RegisterMessageProcessor(topic string, handler p2p.MessageProcessor) error {
	if check.IfNil(handler) {
		return p2p.ErrNilValidator
	}

	netMes.mutTopics.Lock()
	defer netMes.mutTopics.Unlock()
	validator := netMes.processors[topic]
	if !check.IfNil(validator) {
		return fmt.Errorf("%w, operation RegisterMessageProcessor, topic %s",
			p2p.ErrTopicValidatorOperationNotSupported,
			topic,
		)
	}

	err := netMes.pb.RegisterTopicValidator(topic, netMes.pubsubCallback(handler))
	if err != nil {
		return err
	}

	netMes.processors[topic] = handler
	return nil
}

func (netMes *networkMessenger) pubsubCallback(handler p2p.MessageProcessor) func(ctx context.Context, pid peer.ID, message *pubsub.Message) bool {
	return func(ctx context.Context, pid peer.ID, message *pubsub.Message) bool {
		wrappedMsg, err := NewMessage(message)
		if err != nil {
			log.Trace("p2p validator - new message", "error", err.Error(), "topics", message.TopicIDs)
			return false
		}

		identifier := append(message.From, message.Seqno...)
		netMes.mutMessageIdCacher.RLock()
		has, _ := netMes.messageIdCacher.HasOrAdd(identifier, struct{}{}, len(identifier))
		netMes.mutMessageIdCacher.RUnlock()
		if has {
			//not reprocessing nor rebrodcasting the same message over and over again
			log.Trace("received an old message",
				"originator pid", p2p.MessageOriginatorPid(wrappedMsg),
				"from connected pid", p2p.PeerIdToShortString(core.PeerID(pid)),
				"sequence", hex.EncodeToString(wrappedMsg.SeqNo()),
			)
			return false
		}

		fromConnectedPeer := core.PeerID(pid)
		err = handler.ProcessReceivedMessage(wrappedMsg, fromConnectedPeer)
		if err != nil {
			log.Trace("p2p validator",
				"error", err.Error(),
				"topics", message.TopicIDs,
				"originator", p2p.MessageOriginatorPid(wrappedMsg),
				"from connected peer", p2p.PeerIdToShortString(fromConnectedPeer),
				"seq no", p2p.MessageOriginatorSeq(wrappedMsg),
			)

			return false
		}

		return true
	}
}

// UnregisterAllMessageProcessors will unregister all message processors for topics
func (netMes *networkMessenger) UnregisterAllMessageProcessors() error {
	netMes.mutTopics.Lock()
	defer netMes.mutTopics.Unlock()

	for topic, validator := range netMes.processors {
		if check.IfNil(validator) {
			continue
		}

		err := netMes.pb.UnregisterTopicValidator(topic)
		if err != nil {
			return err
		}

		delete(netMes.processors, topic)
	}
	return nil
}

// UnjoinAllTopics call close on all topics
func (netMes *networkMessenger) UnjoinAllTopics() error {
	netMes.mutMessageIdCacher.Lock()
	defer netMes.mutMessageIdCacher.Unlock()

	var errFound error
	for topicName, t := range netMes.topics {
		subscr := netMes.subscriptions[topicName]
		if subscr != nil {
			subscr.Cancel()
		}

		err := t.Close()
		if err != nil {
			log.Warn("error closing topic",
				"topic", topicName,
				"error", err,
			)
			errFound = err
		}

		delete(netMes.topics, topicName)
	}

	return errFound
}

// UnregisterMessageProcessor unregisters a message processes on a topic
func (netMes *networkMessenger) UnregisterMessageProcessor(topic string) error {
	netMes.mutTopics.Lock()
	defer netMes.mutTopics.Unlock()

	validator := netMes.processors[topic]
	if check.IfNil(validator) {
		return nil
	}

	err := netMes.pb.UnregisterTopicValidator(topic)
	if err != nil {
		return err
	}

	netMes.processors[topic] = nil
	return nil
}

// SendToConnectedPeer sends a direct message to a connected peer
func (netMes *networkMessenger) SendToConnectedPeer(topic string, buff []byte, peerID core.PeerID) error {
	return netMes.ds.Send(topic, buff, peerID)
}

func (netMes *networkMessenger) directMessageHandler(message p2p.MessageP2P, fromConnectedPeer core.PeerID) error {
	var processor p2p.MessageProcessor

	netMes.mutTopics.RLock()
	processor = netMes.processors[message.Topics()[0]]
	netMes.mutTopics.RUnlock()

	if processor == nil {
		return p2p.ErrNilValidator
	}

	go func(msg p2p.MessageP2P) {
		//we won't recheck the message id against the cacher here as there might be collisions since we are using
		// a separate sequence counter for direct sender
		err := processor.ProcessReceivedMessage(msg, fromConnectedPeer)
		if err != nil {
			log.Trace("p2p validator",
				"error", err.Error(),
				"topics", msg.Topics(),
				"originator", p2p.MessageOriginatorPid(msg),
				"from connected peer", p2p.PeerIdToShortString(fromConnectedPeer),
				"seq no", p2p.MessageOriginatorSeq(msg),
			)
		}
	}(message)

	return nil
}

// IsConnectedToTheNetwork returns true if the current node is connected to the network
func (netMes *networkMessenger) IsConnectedToTheNetwork() bool {
	netw := netMes.p2pHost.Network()
	return netMes.connMonitor.IsConnectedToTheNetwork(netw)
}

// SetThresholdMinConnectedPeers sets the minimum connected peers before triggering a new reconnection
func (netMes *networkMessenger) SetThresholdMinConnectedPeers(minConnectedPeers int) error {
	if minConnectedPeers < 0 {
		return p2p.ErrInvalidValue
	}

	netw := netMes.p2pHost.Network()
	netMes.connMonitor.SetThresholdMinConnectedPeers(minConnectedPeers, netw)

	return nil
}

// ThresholdMinConnectedPeers returns the minimum connected peers before triggering a new reconnection
func (netMes *networkMessenger) ThresholdMinConnectedPeers() int {
	return netMes.connMonitor.ThresholdMinConnectedPeers()
}

// SetPeerShardResolver sets the peer shard resolver component that is able to resolve the link
// between p2p.PeerID and shardId
func (netMes *networkMessenger) SetPeerShardResolver(peerShardResolver p2p.PeerShardResolver) error {
	if check.IfNil(peerShardResolver) {
		return p2p.ErrNilPeerShardResolver
	}

	err := netMes.sharder.SetPeerShardResolver(peerShardResolver)
	if err != nil {
		return err
	}

	netMes.peerShardResolver = peerShardResolver

	return nil
}

// SetPeerBlackListHandler sets the peer black list handler
//TODO decide if we continue on using setters or switch to options. Refactor if necessary
func (netMes *networkMessenger) SetPeerBlackListHandler(handler p2p.PeerBlacklistHandler) error {
	return netMes.connMonitorWrapper.SetBlackListHandler(handler)
}

// SetMessageIdsCacher sets the message id cacher
func (netMes *networkMessenger) SetMessageIdsCacher(cacher p2p.Cacher) error {
	if check.IfNil(cacher) {
		return fmt.Errorf("%w in networkMessenger.SetMessageIdsCacher", p2p.ErrNilCacher)
	}

	netMes.mutMessageIdCacher.Lock()
	netMes.messageIdCacher = cacher
	netMes.mutMessageIdCacher.Unlock()

	log.Debug("added message ids cacher for the p2p network messenger")

	return nil
}

// GetConnectedPeersInfo gets the current connected peers information
func (netMes *networkMessenger) GetConnectedPeersInfo() *p2p.ConnectedPeersInfo {
	peers := netMes.p2pHost.Network().Peers()
	connPeerInfo := &p2p.ConnectedPeersInfo{
		UnknownPeers:         make([]string, 0),
		IntraShardValidators: make([]string, 0),
		IntraShardObservers:  make([]string, 0),
		CrossShardValidators: make([]string, 0),
		CrossShardObservers:  make([]string, 0),
	}
	selfPeerInfo := netMes.peerShardResolver.GetPeerInfo(netMes.ID())

	for _, p := range peers {
		conns := netMes.p2pHost.Network().ConnsToPeer(p)
		connString := "[invalid connection string]"
		if len(conns) > 0 {
			connString = conns[0].RemoteMultiaddr().String() + "/p2p/" + p.Pretty()
		}

		peerInfo := netMes.peerShardResolver.GetPeerInfo(core.PeerID(p))
		switch peerInfo.PeerType {
		case core.UnknownPeer:
			connPeerInfo.UnknownPeers = append(connPeerInfo.UnknownPeers, connString)
		case core.ValidatorPeer:
			if selfPeerInfo.ShardID != peerInfo.ShardID {
				connPeerInfo.CrossShardValidators = append(connPeerInfo.CrossShardValidators, connString)
			} else {
				connPeerInfo.IntraShardValidators = append(connPeerInfo.IntraShardValidators, connString)
			}
		case core.ObserverPeer:
			if selfPeerInfo.ShardID != peerInfo.ShardID {
				connPeerInfo.CrossShardObservers = append(connPeerInfo.CrossShardObservers, connString)
			} else {
				connPeerInfo.IntraShardObservers = append(connPeerInfo.IntraShardObservers, connString)
			}
		}
	}

	return connPeerInfo
}

// IsInterfaceNil returns true if there is no value under the interface
func (netMes *networkMessenger) IsInterfaceNil() bool {
	return netMes == nil
}
