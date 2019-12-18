package dcrlnd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v2"
	"github.com/decred/dcrd/dcrec/secp256k1/v2"
	"github.com/decred/dcrd/dcrutil/v2"
	"github.com/decred/dcrd/txscript/v2"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrlnd/autopilot"
	"github.com/decred/dcrlnd/build"
	"github.com/decred/dcrlnd/chanacceptor"
	"github.com/decred/dcrlnd/chanbackup"
	"github.com/decred/dcrlnd/chanfitness"
	"github.com/decred/dcrlnd/channeldb"
	"github.com/decred/dcrlnd/channelnotifier"
	"github.com/decred/dcrlnd/contractcourt"
	"github.com/decred/dcrlnd/discovery"
	"github.com/decred/dcrlnd/feature"
	"github.com/decred/dcrlnd/htlcswitch"
	"github.com/decred/dcrlnd/input"
	"github.com/decred/dcrlnd/invoices"
	"github.com/decred/dcrlnd/lncfg"
	"github.com/decred/dcrlnd/lnrpc"
	"github.com/decred/dcrlnd/lnrpc/invoicesrpc"
	"github.com/decred/dcrlnd/lnrpc/routerrpc"
	"github.com/decred/dcrlnd/lntypes"
	"github.com/decred/dcrlnd/lnwallet"
	"github.com/decred/dcrlnd/lnwallet/chainfee"
	"github.com/decred/dcrlnd/lnwire"
	"github.com/decred/dcrlnd/macaroons"
	"github.com/decred/dcrlnd/monitoring"
	"github.com/decred/dcrlnd/peernotifier"
	"github.com/decred/dcrlnd/record"
	"github.com/decred/dcrlnd/routing"
	"github.com/decred/dcrlnd/routing/route"
	"github.com/decred/dcrlnd/signal"
	"github.com/decred/dcrlnd/sweep"
	"github.com/decred/dcrlnd/watchtower"
	"github.com/decred/dcrlnd/zpay32"
	"github.com/decred/dcrwallet/wallet/v3/txauthor"

	"github.com/grpc-ecosystem/go-grpc-middleware"
	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/tv42/zbase32"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// MaxDcrPaymentMAtoms is the maximum allowed Decred payment currently
	// permitted as defined in BOLT-0002. This is the same as the maximum
	// channel size.
	maxDcrPaymentMAtoms = lnwire.MilliAtom(MaxDecredFundingAmount * 1000)

	scriptVersion uint16 = 0
)

var (
	// MaxPaymentMAtoms is the maximum allowed payment currently permitted
	// as defined in BOLT-002. This value depends on which chain is active.
	// It is set to the value under the Decred chain as default.
	MaxPaymentMAtoms = maxDcrPaymentMAtoms

	// defaultAcceptorTimeout is the time after which an RPCAcceptor will time
	// out and return false if it hasn't yet received a response.
	//
	// TODO: Make this configurable
	defaultAcceptorTimeout = 15 * time.Second

	// readPermissions is a slice of all entities that allow read
	// permissions for authorization purposes, all lowercase.
	readPermissions = []bakery.Op{
		{
			Entity: "onchain",
			Action: "read",
		},
		{
			Entity: "offchain",
			Action: "read",
		},
		{
			Entity: "address",
			Action: "read",
		},
		{
			Entity: "message",
			Action: "read",
		},
		{
			Entity: "peers",
			Action: "read",
		},
		{
			Entity: "info",
			Action: "read",
		},
		{
			Entity: "invoices",
			Action: "read",
		},
		{
			Entity: "signer",
			Action: "read",
		},
	}

	// writePermissions is a slice of all entities that allow write
	// permissions for authorization purposes, all lowercase.
	writePermissions = []bakery.Op{
		{
			Entity: "onchain",
			Action: "write",
		},
		{
			Entity: "offchain",
			Action: "write",
		},
		{
			Entity: "address",
			Action: "write",
		},
		{
			Entity: "message",
			Action: "write",
		},
		{
			Entity: "peers",
			Action: "write",
		},
		{
			Entity: "info",
			Action: "write",
		},
		{
			Entity: "invoices",
			Action: "write",
		},
		{
			Entity: "signer",
			Action: "generate",
		},
		{
			Entity: "macaroon",
			Action: "generate",
		},
	}

	// invoicePermissions is a slice of all the entities that allows a user
	// to only access calls that are related to invoices, so: streaming
	// RPCs, generating, and listening invoices.
	invoicePermissions = []bakery.Op{
		{
			Entity: "invoices",
			Action: "read",
		},
		{
			Entity: "invoices",
			Action: "write",
		},
		{
			Entity: "address",
			Action: "read",
		},
		{
			Entity: "address",
			Action: "write",
		},
		{
			Entity: "onchain",
			Action: "read",
		},
	}

	// TODO(guggero): Refactor into constants that are used for all
	// permissions in this file. Also expose the list of possible
	// permissions in an RPC when per RPC permissions are
	// implemented.
	validActions  = []string{"read", "write", "generate"}
	validEntities = []string{
		"onchain", "offchain", "address", "message",
		"peers", "info", "invoices", "signer", "macaroon",
		"address",
	}
)

// stringInSlice returns true if a string is contained in the given slice.
func stringInSlice(a string, slice []string) bool {
	for _, b := range slice {
		if b == a {
			return true
		}
	}
	return false
}

// mainRPCServerPermissions returns a mapping of the main RPC server calls to
// the permissions they require.
func mainRPCServerPermissions() map[string][]bakery.Op {
	return map[string][]bakery.Op{
		"/lnrpc.Lightning/SendCoins": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/ListUnspent": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/SendMany": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/NewAddress": {{
			Entity: "address",
			Action: "write",
		}},
		"/lnrpc.Lightning/SignMessage": {{
			Entity: "message",
			Action: "write",
		}},
		"/lnrpc.Lightning/VerifyMessage": {{
			Entity: "message",
			Action: "read",
		}},
		"/lnrpc.Lightning/ConnectPeer": {{
			Entity: "peers",
			Action: "write",
		}},
		"/lnrpc.Lightning/DisconnectPeer": {{
			Entity: "peers",
			Action: "write",
		}},
		"/lnrpc.Lightning/OpenChannel": {{
			Entity: "onchain",
			Action: "write",
		}, {
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/OpenChannelSync": {{
			Entity: "onchain",
			Action: "write",
		}, {
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/CloseChannel": {{
			Entity: "onchain",
			Action: "write",
		}, {
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/AbandonChannel": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/GetInfo": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/ListPeers": {{
			Entity: "peers",
			Action: "read",
		}},
		"/lnrpc.Lightning/WalletBalance": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/EstimateFee": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/ChannelBalance": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/PendingChannels": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/ListChannels": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/SubscribeChannelEvents": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/ClosedChannels": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/SendPayment": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/SendPaymentSync": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/SendToRoute": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/SendToRouteSync": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/AddInvoice": {{
			Entity: "invoices",
			Action: "write",
		}},
		"/lnrpc.Lightning/LookupInvoice": {{
			Entity: "invoices",
			Action: "read",
		}},
		"/lnrpc.Lightning/ListInvoices": {{
			Entity: "invoices",
			Action: "read",
		}},
		"/lnrpc.Lightning/SubscribeInvoices": {{
			Entity: "invoices",
			Action: "read",
		}},
		"/lnrpc.Lightning/SubscribeTransactions": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/GetTransactions": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/DescribeGraph": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/GetChanInfo": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/GetNodeInfo": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/QueryRoutes": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/GetNetworkInfo": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/StopDaemon": {{
			Entity: "info",
			Action: "write",
		}},
		"/lnrpc.Lightning/SubscribeChannelGraph": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/ListPayments": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/DeleteAllPayments": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/DebugLevel": {{
			Entity: "info",
			Action: "write",
		}},
		"/lnrpc.Lightning/DecodePayReq": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/FeeReport": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/UpdateChannelPolicy": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/ForwardingHistory": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/RestoreChannelBackups": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/ExportChannelBackup": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/VerifyChanBackup": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/ExportAllChannelBackups": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/SubscribeChannelBackups": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/ChannelAcceptor": {{
			Entity: "onchain",
			Action: "write",
		}, {
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/BakeMacaroon": {{
			Entity: "macaroon",
			Action: "generate",
		}},
		"/lnrpc.Lightning/SubscribePeerEvents": {{
			Entity: "peers",
			Action: "read",
		}},
	}
}

// rpcServer is a gRPC, RPC front end to the lnd daemon.
// TODO(roasbeef): pagination support for the list-style calls
type rpcServer struct {
	started  int32 // To be used atomically.
	shutdown int32 // To be used atomically.

	server *server

	// subServers are a set of sub-RPC servers that use the same gRPC and
	// listening sockets as the main RPC server, but which maintain their
	// own independent service. This allows us to expose a set of
	// micro-service like abstractions to the outside world for users to
	// consume.
	subServers []lnrpc.SubServer

	// grpcServer is the main gRPC server that this RPC server, and all the
	// sub-servers will use to register themselves and accept client
	// requests from.
	grpcServer *grpc.Server

	// listeners is a list of listeners to use when starting the grpc
	// server. We make it configurable such that the grpc server can listen
	// on custom interfaces.
	listeners []*ListenerWithSignal

	// listenerCleanUp are a set of closures functions that will allow this
	// main RPC server to clean up all the listening socket created for the
	// server.
	listenerCleanUp []func()

	// restDialOpts are a set of gRPC dial options that the REST server
	// proxy will use to connect to the main gRPC server.
	restDialOpts []grpc.DialOption

	// restProxyDest is the address to forward REST requests to.
	restProxyDest string

	// tlsCfg is the TLS config that allows the REST server proxy to
	// connect to the main gRPC server to proxy all incoming requests.
	tlsCfg *tls.Config

	// routerBackend contains the backend implementation of the router
	// rpc sub server.
	routerBackend *routerrpc.RouterBackend

	// chanPredicate is used in the bidirectional ChannelAcceptor streaming
	// method.
	chanPredicate *chanacceptor.ChainedAcceptor

	quit chan struct{}

	// macService is the macaroon service that we need to mint new
	// macaroons.
	macService *macaroons.Service

	// selfNode is our own pubkey.
	selfNode route.Vertex
}

// A compile time check to ensure that rpcServer fully implements the
// LightningServer gRPC service.
var _ lnrpc.LightningServer = (*rpcServer)(nil)

// newRPCServer creates and returns a new instance of the rpcServer. The
// rpcServer will handle creating all listening sockets needed by it, and any
// of the sub-servers that it maintains. The set of serverOpts should be the
// base level options passed to the grPC server. This typically includes things
// like requiring TLS, etc.
func newRPCServer(s *server, macService *macaroons.Service,
	subServerCgs *subRPCServerConfigs, serverOpts []grpc.ServerOption,
	restDialOpts []grpc.DialOption, restProxyDest string,
	atpl *autopilot.Manager, invoiceRegistry *invoices.InvoiceRegistry,
	tower *watchtower.Standalone, tlsCfg *tls.Config,
	getListeners rpcListeners,
	chanPredicate *chanacceptor.ChainedAcceptor) (*rpcServer, error) {

	// Set up router rpc backend.
	channelGraph := s.chanDB.ChannelGraph()
	selfNode, err := channelGraph.SourceNode()
	if err != nil {
		return nil, err
	}
	graph := s.chanDB.ChannelGraph()
	routerBackend := &routerrpc.RouterBackend{
		MaxPaymentMAtoms: MaxPaymentMAtoms,
		SelfNode:         selfNode.PubKeyBytes,
		FetchChannelCapacity: func(chanID uint64) (dcrutil.Amount,
			error) {

			info, _, _, err := graph.FetchChannelEdgesByID(chanID)
			if err != nil {
				return 0, err
			}
			return info.Capacity, nil
		},
		FetchChannelEndpoints: func(chanID uint64) (route.Vertex,
			route.Vertex, error) {

			info, _, _, err := graph.FetchChannelEdgesByID(
				chanID,
			)
			if err != nil {
				return route.Vertex{}, route.Vertex{},
					fmt.Errorf("unable to fetch channel "+
						"edges by channel ID %d: %v",
						chanID, err)
			}

			return info.NodeKey1Bytes, info.NodeKey2Bytes, nil
		},
		FindRoute:        s.chanRouter.FindRoute,
		MissionControl:   s.missionControl,
		ActiveNetParams:  activeNetParams.Params,
		Tower:            s.controlTower,
		MaxTotalTimelock: cfg.MaxOutgoingCltvExpiry,
	}

	genInvoiceFeatures := func() *lnwire.FeatureVector {
		return s.featureMgr.Get(feature.SetInvoice)
	}

	var (
		subServers     []lnrpc.SubServer
		subServerPerms []lnrpc.MacaroonPerms
	)

	// Before we create any of the sub-servers, we need to ensure that all
	// the dependencies they need are properly populated within each sub
	// server configuration struct.
	err = subServerCgs.PopulateDependencies(
		s.cc, networkDir, macService, atpl, invoiceRegistry,
		s.htlcSwitch, activeNetParams.Params, s.chanRouter,
		routerBackend, s.nodeSigner, s.chanDB, s.sweeper, tower,
		s.towerClient, cfg.net.ResolveTCPAddr, genInvoiceFeatures,
	)
	if err != nil {
		return nil, err
	}

	// Now that the sub-servers have all their dependencies in place, we
	// can create each sub-server!
	registeredSubServers := lnrpc.RegisteredSubServers()
	for _, subServer := range registeredSubServers {
		subServerInstance, macPerms, err := subServer.New(subServerCgs)
		if err != nil {
			return nil, err
		}

		// We'll collect the sub-server, and also the set of
		// permissions it needs for macaroons so we can apply the
		// interceptors below.
		subServers = append(subServers, subServerInstance)
		subServerPerms = append(subServerPerms, macPerms)
	}

	// Next, we need to merge the set of sub server macaroon permissions
	// with the main RPC server permissions so we can unite them under a
	// single set of interceptors.
	permissions := mainRPCServerPermissions()
	for _, subServerPerm := range subServerPerms {
		for method, ops := range subServerPerm {
			// For each new method:ops combo, we also ensure that
			// non of the sub-servers try to override each other.
			if _, ok := permissions[method]; ok {
				return nil, fmt.Errorf("detected duplicate "+
					"macaroon constraints for path: %v",
					method)
			}

			permissions[method] = ops
		}
	}

	// If macaroons aren't disabled (a non-nil service), then we'll set up
	// our set of interceptors which will allow us to handle the macaroon
	// authentication in a single location.
	macUnaryInterceptors := []grpc.UnaryServerInterceptor{}
	macStrmInterceptors := []grpc.StreamServerInterceptor{}
	if macService != nil {
		unaryInterceptor := macService.UnaryServerInterceptor(permissions)
		macUnaryInterceptors = append(macUnaryInterceptors, unaryInterceptor)

		strmInterceptor := macService.StreamServerInterceptor(permissions)
		macStrmInterceptors = append(macStrmInterceptors, strmInterceptor)
	}

	// Get interceptors for Prometheus to gather gRPC performance metrics.
	// If monitoring is disabled, GetPromInterceptors() will return empty
	// slices.
	promUnaryInterceptors, promStrmInterceptors := monitoring.GetPromInterceptors()

	// Concatenate the slices of unary and stream interceptors respectively.
	unaryInterceptors := append(macUnaryInterceptors, promUnaryInterceptors...)
	strmInterceptors := append(macStrmInterceptors, promStrmInterceptors...)

	// We'll also add our logging interceptors as well, so we can
	// automatically log all errors that happen during RPC calls.
	unaryInterceptors = append(
		unaryInterceptors, errorLogUnaryServerInterceptor(rpcsLog),
	)
	strmInterceptors = append(
		strmInterceptors, errorLogStreamServerInterceptor(rpcsLog),
	)

	// Get the listeners and server options to use for this rpc server.
	listeners, cleanup, err := getListeners()
	if err != nil {
		return nil, err
	}

	// If any interceptors have been set up, add them to the server options.
	if len(unaryInterceptors) != 0 && len(strmInterceptors) != 0 {
		chainedUnary := grpc_middleware.WithUnaryServerChain(
			unaryInterceptors...,
		)
		chainedStream := grpc_middleware.WithStreamServerChain(
			strmInterceptors...,
		)
		serverOpts = append(serverOpts, chainedUnary, chainedStream)
	}

	// Finally, with all the pre-set up complete,  we can create the main
	// gRPC server, and register the main lnrpc server along side.
	grpcServer := grpc.NewServer(serverOpts...)
	rootRPCServer := &rpcServer{
		restDialOpts:    restDialOpts,
		listeners:       listeners,
		listenerCleanUp: []func(){cleanup},
		restProxyDest:   restProxyDest,
		subServers:      subServers,
		tlsCfg:          tlsCfg,
		grpcServer:      grpcServer,
		server:          s,
		routerBackend:   routerBackend,
		chanPredicate:   chanPredicate,
		quit:            make(chan struct{}, 1),
		macService:      macService,
		selfNode:        selfNode.PubKeyBytes,
	}
	lnrpc.RegisterLightningServer(grpcServer, rootRPCServer)

	// Now the main RPC server has been registered, we'll iterate through
	// all the sub-RPC servers and register them to ensure that requests
	// are properly routed towards them.
	for _, subServer := range subServers {
		err := subServer.RegisterWithRootServer(grpcServer)
		if err != nil {
			return nil, fmt.Errorf("unable to register "+
				"sub-server %v with root: %v",
				subServer.Name(), err)
		}
	}

	return rootRPCServer, nil
}

// Start launches any helper goroutines required for the rpcServer to function.
func (r *rpcServer) Start() error {
	if atomic.AddInt32(&r.started, 1) != 1 {
		return nil
	}

	// First, we'll start all the sub-servers to ensure that they're ready
	// to take new requests in.
	//
	// TODO(roasbeef): some may require that the entire daemon be started
	// at that point
	for _, subServer := range r.subServers {
		rpcsLog.Debugf("Starting sub RPC server: %v", subServer.Name())

		if err := subServer.Start(); err != nil {
			return err
		}
	}

	// With all the sub-servers started, we'll spin up the listeners for
	// the main RPC server itself.
	for _, lis := range r.listeners {
		go func(lis *ListenerWithSignal) {
			rpcsLog.Infof("RPC server listening on %s", lis.Addr())

			// Close the ready chan to indicate we are listening.
			close(lis.Ready)
			r.grpcServer.Serve(lis)
		}(lis)
	}

	// If Prometheus monitoring is enabled, start the Prometheus exporter.
	if cfg.Prometheus.Enabled() {
		err := monitoring.ExportPrometheusMetrics(
			r.grpcServer, cfg.Prometheus,
		)
		if err != nil {
			return err
		}
	}

	// Finally, start the REST proxy for our gRPC server above. We'll ensure
	// we direct LND to connect to its loopback address rather than a
	// wildcard to prevent certificate issues when accessing the proxy
	// externally.
	//
	// TODO(roasbeef): eventually also allow the sub-servers to themselves
	// have a REST proxy.
	mux := proxy.NewServeMux()

	err := lnrpc.RegisterLightningHandlerFromEndpoint(
		context.Background(), mux, r.restProxyDest,
		r.restDialOpts,
	)
	if err != nil {
		return err
	}
	for _, restEndpoint := range cfg.RESTListeners {
		lis, err := lncfg.TLSListenOnAddress(restEndpoint, r.tlsCfg)
		if err != nil {
			ltndLog.Errorf(
				"gRPC proxy unable to listen on %s",
				restEndpoint,
			)
			return err
		}

		r.listenerCleanUp = append(r.listenerCleanUp, func() {
			lis.Close()
		})

		go func() {
			rpcsLog.Infof("gRPC proxy started at %s", lis.Addr())
			http.Serve(lis, mux)
		}()
	}

	return nil
}

// Stop signals any active goroutines for a graceful closure.
func (r *rpcServer) Stop() error {
	if atomic.AddInt32(&r.shutdown, 1) != 1 {
		return nil
	}

	rpcsLog.Infof("Stopping RPC Server")

	close(r.quit)

	// After we've signalled all of our active goroutines to exit, we'll
	// then do the same to signal a graceful shutdown of all the sub
	// servers.
	for _, subServer := range r.subServers {
		rpcsLog.Infof("Stopping %v Sub-RPC Server",
			subServer.Name())

		if err := subServer.Stop(); err != nil {
			rpcsLog.Errorf("unable to stop sub-server %v: %v",
				subServer.Name(), err)
			continue
		}
	}

	// Finally, we can clean up all the listening sockets to ensure that we
	// give the file descriptors back to the OS.
	for _, cleanUp := range r.listenerCleanUp {
		cleanUp()
	}

	return nil
}

// addrPairsToOutputs converts a map describing a set of outputs to be created,
// the outputs themselves. The passed map pairs up an address, to a desired
// output value amount. Each address is converted to its corresponding pkScript
// to be used within the constructed output(s).
func addrPairsToOutputs(addrPairs map[string]int64, netParams *chaincfg.Params) ([]*wire.TxOut, error) {
	outputs := make([]*wire.TxOut, 0, len(addrPairs))
	for addr, amt := range addrPairs {
		addr, err := dcrutil.DecodeAddress(addr, netParams)
		if err != nil {
			return nil, err
		}

		pkscript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, err
		}

		outputs = append(outputs, wire.NewTxOut(amt, pkscript))
	}

	return outputs, nil
}

// sendCoinsOnChain makes an on-chain transaction in or to send coins to one or
// more addresses specified in the passed payment map. The payment map maps an
// address to a specified output value to be sent to that address.
func (r *rpcServer) sendCoinsOnChain(paymentMap map[string]int64,
	feeRate chainfee.AtomPerKByte) (*chainhash.Hash, error) {

	outputs, err := addrPairsToOutputs(paymentMap, activeNetParams.Params)
	if err != nil {
		return nil, err
	}

	tx, err := r.server.cc.wallet.SendOutputs(outputs, feeRate)
	if err != nil {
		return nil, err
	}

	txHash := tx.TxHash()
	return &txHash, nil
}

// ListUnspent returns useful information about each unspent output owned by
// the wallet, as reported by the underlying `ListUnspentWitness`; the
// information returned is: outpoint, amount in atoms, address, address
// type, scriptPubKey in hex and number of confirmations.  The result is
// filtered to contain outputs whose number of confirmations is between a
// minimum and maximum number of confirmations specified by the user, with 0
// meaning unconfirmed.
func (r *rpcServer) ListUnspent(ctx context.Context,
	in *lnrpc.ListUnspentRequest) (*lnrpc.ListUnspentResponse, error) {

	minConfs := in.MinConfs
	maxConfs := in.MaxConfs

	switch {
	// Ensure that the user didn't attempt to specify a negative number of
	// confirmations, as that isn't possible.
	case minConfs < 0:
		return nil, fmt.Errorf("min confirmations must be >= 0")

	// We'll also ensure that the min number of confs is strictly less than
	// or equal to the max number of confs for sanity.
	case minConfs > maxConfs:
		return nil, fmt.Errorf("max confirmations must be >= min " +
			"confirmations")
	}

	// With our arguments validated, we'll query the internal wallet for
	// the set of UTXOs that match our query.
	utxos, err := r.server.cc.wallet.ListUnspentWitness(minConfs, maxConfs)
	if err != nil {
		return nil, err
	}

	resp := &lnrpc.ListUnspentResponse{
		Utxos: make([]*lnrpc.Utxo, 0, len(utxos)),
	}

	for _, utxo := range utxos {
		// Translate lnwallet address type to the proper gRPC proto
		// address type.
		var addrType lnrpc.AddressType
		switch utxo.AddressType {

		case lnwallet.PubKeyHash:
			addrType = lnrpc.AddressType_PUBKEY_HASH

		case lnwallet.UnknownAddressType:
			rpcsLog.Warnf("[listunspent] utxo with address of "+
				"unknown type ignored: %v",
				utxo.OutPoint.String())
			continue

		default:
			return nil, fmt.Errorf("invalid utxo address type")
		}

		// Now that we know we have a proper mapping to an address,
		// we'll convert the regular outpoint to an lnrpc variant.
		outpoint := &lnrpc.OutPoint{
			TxidBytes:   utxo.OutPoint.Hash[:],
			TxidStr:     utxo.OutPoint.Hash.String(),
			OutputIndex: utxo.OutPoint.Index,
		}

		utxoResp := lnrpc.Utxo{
			Type:          addrType,
			AmountAtoms:   int64(utxo.Value),
			PkScript:      hex.EncodeToString(utxo.PkScript),
			Outpoint:      outpoint,
			Confirmations: utxo.Confirmations,
		}

		// Finally, we'll attempt to extract the raw address from the
		// script so we can display a human friendly address to the end
		// user.
		// TODO(decred) version needs to come from utxo.
		_, outAddresses, _, err := txscript.ExtractPkScriptAddrs(
			scriptVersion, utxo.PkScript, activeNetParams.Params,
		)
		if err != nil {
			return nil, err
		}

		// If we can't properly locate a single address, then this was
		// an error in our mapping, and we'll return an error back to
		// the user.
		if len(outAddresses) != 1 {
			return nil, fmt.Errorf("an output was unexpectedly " +
				"multisig")
		}

		utxoResp.Address = outAddresses[0].String()

		resp.Utxos = append(resp.Utxos, &utxoResp)
	}

	maxStr := ""
	if maxConfs != math.MaxInt32 {
		maxStr = " max=" + fmt.Sprintf("%d", maxConfs)
	}

	rpcsLog.Debugf("[listunspent] min=%v%v, generated utxos: %v", minConfs,
		maxStr, utxos)

	return resp, nil
}

// EstimateFee handles a request for estimating the fee for sending a
// transaction spending to multiple specified outputs in parallel.
func (r *rpcServer) EstimateFee(ctx context.Context,
	in *lnrpc.EstimateFeeRequest) (*lnrpc.EstimateFeeResponse, error) {

	// Create the list of outputs we are spending to.
	outputs, err := addrPairsToOutputs(in.AddrToAmount, activeNetParams.Params)
	if err != nil {
		return nil, err
	}

	// Query the fee estimator for the fee rate for the given confirmation
	// target.
	target := in.TargetConf
	feePerKB, err := sweep.DetermineFeePerKB(
		r.server.cc.feeEstimator, sweep.FeePreference{
			ConfTarget: uint32(target),
		},
	)
	if err != nil {
		return nil, err
	}

	// We will ask the wallet to create a tx using this fee rate. We set
	// dryRun=true to avoid inflating the change addresses in the db.
	var tx *txauthor.AuthoredTx
	wallet := r.server.cc.wallet
	err = wallet.WithCoinSelectLock(func() error {
		tx, err = wallet.CreateSimpleTx(outputs, feePerKB, true)
		return err
	})
	if err != nil {
		return nil, err
	}

	// Use the created tx to calculate the total fee.
	totalOutput := int64(0)
	for _, out := range tx.Tx.TxOut {
		totalOutput += out.Value
	}
	totalFee := int64(tx.TotalInput) - totalOutput

	resp := &lnrpc.EstimateFeeResponse{
		FeeAtoms:            totalFee,
		FeerateAtomsPerByte: int64(feePerKB / 1000),
	}

	rpcsLog.Debugf("[estimatefee] fee estimate for conf target %d: %v",
		target, resp)

	return resp, nil
}

// SendCoins executes a request to send coins to a particular address. Unlike
// SendMany, this RPC call only allows creating a single output at a time.
func (r *rpcServer) SendCoins(ctx context.Context,
	in *lnrpc.SendCoinsRequest) (*lnrpc.SendCoinsResponse, error) {

	// Based on the passed fee related parameters, we'll determine an
	// appropriate fee rate for this transaction.
	atomsPerKB := chainfee.AtomPerKByte(in.AtomsPerByte * 1000)
	feePerKB, err := sweep.DetermineFeePerKB(
		r.server.cc.feeEstimator, sweep.FeePreference{
			ConfTarget: uint32(in.TargetConf),
			FeeRate:    atomsPerKB,
		},
	)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[sendcoins] addr=%v, amt=%v, atom/kb=%v, sweep_all=%v",
		in.Addr, dcrutil.Amount(in.Amount), int64(feePerKB),
		in.SendAll)

	// Decode the address receiving the coins, we need to check whether the
	// address is valid for this network.
	targetAddr, err := dcrutil.DecodeAddress(in.Addr, activeNetParams.Params)
	if err != nil {
		return nil, err
	}

	// If the destination address parses to a valid pubkey, we assume the
	// user accidentally tried to send funds to a bare pubkey address. This
	// check is here to prevent unintended transfers.
	decodedAddr, _ := hex.DecodeString(in.Addr)
	_, err = secp256k1.ParsePubKey(decodedAddr)
	if err == nil {
		return nil, fmt.Errorf("cannot send coins to pubkeys")
	}

	var txid *chainhash.Hash

	wallet := r.server.cc.wallet

	// If the send all flag is active, then we'll attempt to sweep all the
	// coins in the wallet in a single transaction (if possible),
	// otherwise, we'll respect the amount, and attempt a regular 2-output
	// send.
	if in.SendAll {
		// At this point, the amount shouldn't be set since we've been
		// instructed to sweep all the coins from the wallet.
		if in.Amount != 0 {
			return nil, fmt.Errorf("amount set while SendAll is " +
				"active")
		}

		_, bestHeight, err := r.server.cc.chainIO.GetBestBlock()
		if err != nil {
			return nil, err
		}

		// With the sweeper instance created, we can now generate a
		// transaction that will sweep ALL outputs from the wallet in a
		// single transaction. This will be generated in a concurrent
		// safe manner, so no need to worry about locking.
		sweepTxPkg, err := sweep.CraftSweepAllTx(
			feePerKB, uint32(bestHeight), targetAddr, wallet,
			wallet.WalletController, wallet.WalletController,
			r.server.cc.feeEstimator, r.server.cc.signer,
			activeNetParams.Params,
		)
		if err != nil {
			return nil, err
		}

		rpcsLog.Debugf("Sweeping all coins from wallet to addr=%v, "+
			"with tx=%v", in.Addr, spew.Sdump(sweepTxPkg.SweepTx))

		// As our sweep transaction was created, successfully, we'll
		// now attempt to publish it, cancelling the sweep pkg to
		// return all outputs if it fails.
		err = wallet.PublishTransaction(sweepTxPkg.SweepTx)
		if err != nil {
			sweepTxPkg.CancelSweepAttempt()

			return nil, fmt.Errorf("unable to broadcast sweep "+
				"transaction: %v", err)
		}

		sweepTXID := sweepTxPkg.SweepTx.TxHash()
		txid = &sweepTXID
	} else {

		// We'll now construct out payment map, and use the wallet's
		// coin selection synchronization method to ensure that no coin
		// selection (funding, sweep alls, other sends) can proceed
		// while we instruct the wallet to send this transaction.
		paymentMap := map[string]int64{targetAddr.String(): in.Amount}
		err := wallet.WithCoinSelectLock(func() error {
			newTXID, err := r.sendCoinsOnChain(paymentMap, feePerKB)
			if err != nil {
				return err
			}

			txid = newTXID

			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	rpcsLog.Infof("[sendcoins] spend generated txid: %v", txid.String())

	return &lnrpc.SendCoinsResponse{Txid: txid.String()}, nil
}

// SendMany handles a request for a transaction create multiple specified
// outputs in parallel.
func (r *rpcServer) SendMany(ctx context.Context,
	in *lnrpc.SendManyRequest) (*lnrpc.SendManyResponse, error) {

	// Based on the passed fee related parameters, we'll determine an
	// appropriate fee rate for this transaction.
	atomsPerKB := chainfee.AtomPerKByte(in.AtomsPerByte * 1000)
	feePerKB, err := sweep.DetermineFeePerKB(
		r.server.cc.feeEstimator, sweep.FeePreference{
			ConfTarget: uint32(in.TargetConf),
			FeeRate:    atomsPerKB,
		},
	)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[sendmany] outputs=%v, atom/kb=%v",
		spew.Sdump(in.AddrToAmount), int64(feePerKB))

	var txid *chainhash.Hash

	// We'll attempt to send to the target set of outputs, ensuring that we
	// synchronize with any other ongoing coin selection attempts which
	// happen to also be concurrently executing.
	wallet := r.server.cc.wallet
	err = wallet.WithCoinSelectLock(func() error {

		sendManyTXID, err := r.sendCoinsOnChain(
			in.AddrToAmount, feePerKB,
		)
		if err != nil {
			return err
		}

		txid = sendManyTXID

		return nil
	})
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[sendmany] spend generated txid: %v", txid.String())

	return &lnrpc.SendManyResponse{Txid: txid.String()}, nil
}

// NewAddress creates a new address under control of the local wallet.
func (r *rpcServer) NewAddress(ctx context.Context,
	in *lnrpc.NewAddressRequest) (*lnrpc.NewAddressResponse, error) {

	// Translate the gRPC proto address type to the wallet controller's
	// available address types.
	var (
		addr dcrutil.Address
		err  error
	)
	switch in.Type {
	case lnrpc.AddressType_PUBKEY_HASH:
		addr, err = r.server.cc.wallet.NewAddress(
			lnwallet.PubKeyHash, false,
		)
		if err != nil {
			return nil, err
		}
	case lnrpc.AddressType_UNUSED_PUBKEY_HASH:
		addr, err = r.server.cc.wallet.LastUnusedAddress(
			lnwallet.PubKeyHash,
		)
		if err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unsupported address type %s", in.Type)
	}

	rpcsLog.Debugf("[newaddress] type=%v addr=%v", in.Type, addr.String())
	return &lnrpc.NewAddressResponse{Address: addr.String()}, nil
}

var (
	// signedMsgPrefix is a special prefix that we'll prepend to any
	// messages we sign/verify. We do this to ensure that we don't
	// accidentally sign a sighash, or other sensitive material. By
	// prepending this fragment, we mind message signing to our particular
	// context.
	signedMsgPrefix = []byte("Lightning Signed Message:")
)

// SignMessage signs a message with the resident node's private key. The
// returned signature string is zbase32 encoded and pubkey recoverable, meaning
// that only the message digest and signature are needed for verification.
func (r *rpcServer) SignMessage(ctx context.Context,
	in *lnrpc.SignMessageRequest) (*lnrpc.SignMessageResponse, error) {

	if in.Msg == nil {
		return nil, fmt.Errorf("need a message to sign")
	}

	in.Msg = append(signedMsgPrefix, in.Msg...)
	sigBytes, err := r.server.nodeSigner.SignCompact(in.Msg)
	if err != nil {
		return nil, err
	}

	sig := zbase32.EncodeToString(sigBytes)
	return &lnrpc.SignMessageResponse{Signature: sig}, nil
}

// VerifyMessage verifies a signature over a msg. The signature must be zbase32
// encoded and signed by an active node in the resident node's channel
// database. In addition to returning the validity of the signature,
// VerifyMessage also returns the recovered pubkey from the signature.
func (r *rpcServer) VerifyMessage(ctx context.Context,
	in *lnrpc.VerifyMessageRequest) (*lnrpc.VerifyMessageResponse, error) {

	if in.Msg == nil {
		return nil, fmt.Errorf("need a message to verify")
	}

	// The signature should be zbase32 encoded
	sig, err := zbase32.DecodeString(in.Signature)
	if err != nil {
		return nil, fmt.Errorf("failed to decode signature: %v", err)
	}

	// The signature is over the double-sha256 hash of the message.
	in.Msg = append(signedMsgPrefix, in.Msg...)
	digest := chainhash.HashB(in.Msg)

	// RecoverCompact both recovers the pubkey and validates the signature.
	pubKey, _, err := secp256k1.RecoverCompact(sig, digest)
	if err != nil {
		return &lnrpc.VerifyMessageResponse{Valid: false}, nil
	}
	pubKeyHex := hex.EncodeToString(pubKey.SerializeCompressed())

	var pub [33]byte
	copy(pub[:], pubKey.SerializeCompressed())

	// Query the channel graph to ensure a node in the network with active
	// channels signed the message.
	//
	// TODO(phlip9): Require valid nodes to have capital in active channels.
	graph := r.server.chanDB.ChannelGraph()
	_, active, err := graph.HasLightningNode(pub)
	if err != nil {
		return nil, fmt.Errorf("failed to query graph: %v", err)
	}

	return &lnrpc.VerifyMessageResponse{
		Valid:  active,
		Pubkey: pubKeyHex,
	}, nil
}

// ConnectPeer attempts to establish a connection to a remote peer.
func (r *rpcServer) ConnectPeer(ctx context.Context,
	in *lnrpc.ConnectPeerRequest) (*lnrpc.ConnectPeerResponse, error) {

	// The server hasn't yet started, so it won't be able to service any of
	// our requests, so we'll bail early here.
	if !r.server.Started() {
		return nil, ErrServerNotActive
	}

	if in.Addr == nil {
		return nil, fmt.Errorf("need: lnc pubkeyhash@hostname")
	}

	pubkeyHex, err := hex.DecodeString(in.Addr.Pubkey)
	if err != nil {
		return nil, err
	}
	pubKey, err := secp256k1.ParsePubKey(pubkeyHex)
	if err != nil {
		return nil, err
	}

	// Connections to ourselves are disallowed for obvious reasons.
	if pubKey.IsEqual(r.server.identityPriv.PubKey()) {
		return nil, fmt.Errorf("cannot make connection to self")
	}

	addr, err := parseAddr(in.Addr.Host)
	if err != nil {
		return nil, err
	}

	peerAddr := &lnwire.NetAddress{
		IdentityKey: pubKey,
		Address:     addr,
		ChainNet:    activeNetParams.Net,
	}

	rpcsLog.Debugf("[connectpeer] requested connection to %x@%s",
		peerAddr.IdentityKey.SerializeCompressed(), peerAddr.Address)

	if err := r.server.ConnectToPeer(peerAddr, in.Perm); err != nil {
		rpcsLog.Errorf("[connectpeer]: error connecting to peer: %v", err)
		return nil, err
	}

	rpcsLog.Debugf("Connected to peer: %v", peerAddr.String())
	return &lnrpc.ConnectPeerResponse{}, nil
}

// DisconnectPeer attempts to disconnect one peer from another identified by a
// given pubKey. In the case that we currently have a pending or active channel
// with the target peer, this action will be disallowed.
func (r *rpcServer) DisconnectPeer(ctx context.Context,
	in *lnrpc.DisconnectPeerRequest) (*lnrpc.DisconnectPeerResponse, error) {

	rpcsLog.Debugf("[disconnectpeer] from peer(%s)", in.PubKey)

	if !r.server.Started() {
		return nil, ErrServerNotActive
	}

	// First we'll validate the string passed in within the request to
	// ensure that it's a valid hex-string, and also a valid compressed
	// public key.
	pubKeyBytes, err := hex.DecodeString(in.PubKey)
	if err != nil {
		return nil, fmt.Errorf("unable to decode pubkey bytes: %v", err)
	}
	peerPubKey, err := secp256k1.ParsePubKey(pubKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("unable to parse pubkey: %v", err)
	}

	// Next, we'll fetch the pending/active channels we have with a
	// particular peer.
	nodeChannels, err := r.server.chanDB.FetchOpenChannels(peerPubKey)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch channels for peer: %v", err)
	}

	// In order to avoid erroneously disconnecting from a peer that we have
	// an active channel with, if we have any channels active with this
	// peer, then we'll disallow disconnecting from them.
	if len(nodeChannels) > 0 && !cfg.UnsafeDisconnect {
		return nil, fmt.Errorf("cannot disconnect from peer(%x), "+
			"all active channels with the peer need to be closed "+
			"first", pubKeyBytes)
	}

	// With all initial validation complete, we'll now request that the
	// server disconnects from the peer.
	if err := r.server.DisconnectPeer(peerPubKey); err != nil {
		return nil, fmt.Errorf("unable to disconnect peer: %v", err)
	}

	return &lnrpc.DisconnectPeerResponse{}, nil
}

// extractOpenChannelMinConfs extracts the minimum number of confirmations from
// the OpenChannelRequest that each output used to fund the channel's funding
// transaction should satisfy.
func extractOpenChannelMinConfs(in *lnrpc.OpenChannelRequest) (int32, error) {
	switch {
	// Ensure that the MinConfs parameter is non-negative.
	case in.MinConfs < 0:
		return 0, errors.New("minimum number of confirmations must " +
			"be a non-negative number")

	// The funding transaction should not be funded with unconfirmed outputs
	// unless explicitly specified by SpendUnconfirmed. We do this to
	// provide sane defaults to the OpenChannel RPC, as otherwise, if the
	// MinConfs field isn't explicitly set by the caller, we'll use
	// unconfirmed outputs without the caller being aware.
	case in.MinConfs == 0 && !in.SpendUnconfirmed:
		return 1, nil

	// In the event that the caller set MinConfs > 0 and SpendUnconfirmed to
	// true, we'll return an error to indicate the conflict.
	case in.MinConfs > 0 && in.SpendUnconfirmed:
		return 0, errors.New("SpendUnconfirmed set to true with " +
			"MinConfs > 0")

	// The funding transaction of the new channel to be created can be
	// funded with unconfirmed outputs.
	case in.SpendUnconfirmed:
		return 0, nil

	// If none of the above cases matched, we'll return the value set
	// explicitly by the caller.
	default:
		return in.MinConfs, nil
	}
}

// OpenChannel attempts to open a singly funded channel specified in the
// request to a remote peer.
func (r *rpcServer) OpenChannel(in *lnrpc.OpenChannelRequest,
	updateStream lnrpc.Lightning_OpenChannelServer) error {

	rpcsLog.Tracef("[openchannel] request to NodeKey(%v) "+
		"allocation(us=%v, them=%v)", in.NodePubkeyString,
		in.LocalFundingAmount, in.PushAtoms)

	if !r.server.Started() {
		return ErrServerNotActive
	}

	localFundingAmt := dcrutil.Amount(in.LocalFundingAmount)
	remoteInitialBalance := dcrutil.Amount(in.PushAtoms)
	minHtlcIn := lnwire.MilliAtom(in.MinHtlcMAtoms)
	remoteCsvDelay := uint16(in.RemoteCsvDelay)

	// Ensure that the initial balance of the remote party (if pushing
	// atoms) does not exceed the amount the local party has requested
	// for funding.
	//
	// TODO(roasbeef): incorporate base fee?
	if remoteInitialBalance >= localFundingAmt {
		return fmt.Errorf("amount pushed to remote peer for initial " +
			"state must be below the local funding amount")
	}

	// Ensure that the user doesn't exceed the current soft-limit for
	// channel size. If the funding amount is above the soft-limit, then
	// we'll reject the request.
	if localFundingAmt > MaxFundingAmount {
		return fmt.Errorf("funding amount is too large, the max "+
			"channel size is: %v", MaxFundingAmount)
	}

	// Restrict the size of the channel we'll actually open. At a later
	// level, we'll ensure that the output we create after accounting for
	// fees that a dust output isn't created.
	if localFundingAmt < minChanFundingSize {
		return fmt.Errorf("channel is too small, the minimum channel "+
			"size is: %v Atoms", int64(minChanFundingSize))
	}

	// Then, we'll extract the minimum number of confirmations that each
	// output we use to fund the channel's funding transaction should
	// satisfy.
	minConfs, err := extractOpenChannelMinConfs(in)
	if err != nil {
		return err
	}

	var (
		nodePubKey      *secp256k1.PublicKey
		nodePubKeyBytes []byte
	)

	// TODO(roasbeef): also return channel ID?

	// Ensure that the NodePubKey is set before attempting to use it
	if len(in.NodePubkey) == 0 {
		return fmt.Errorf("NodePubKey is not set")
	}

	// Parse the raw bytes of the node key into a pubkey object so we
	// can easily manipulate it.
	nodePubKey, err = secp256k1.ParsePubKey(in.NodePubkey)
	if err != nil {
		return err
	}

	// Making a channel to ourselves wouldn't be of any use, so we
	// explicitly disallow them.
	if nodePubKey.IsEqual(r.server.identityPriv.PubKey()) {
		return fmt.Errorf("cannot open channel to self")
	}

	nodePubKeyBytes = nodePubKey.SerializeCompressed()

	// Based on the passed fee related parameters, we'll determine an
	// appropriate fee rate for the funding transaction.
	atomsPerKB := chainfee.AtomPerKByte(in.AtomsPerByte * 1000)
	feeRate, err := sweep.DetermineFeePerKB(
		r.server.cc.feeEstimator, sweep.FeePreference{
			ConfTarget: uint32(in.TargetConf),
			FeeRate:    atomsPerKB,
		},
	)
	if err != nil {
		return err
	}

	rpcsLog.Debugf("[openchannel]: using fee of %v atom/kB for funding tx",
		int64(feeRate))

	script, err := parseUpfrontShutdownAddress(in.CloseAddress)
	if err != nil {
		return fmt.Errorf("error parsing upfront shutdown: %v", err)
	}

	// Instruct the server to trigger the necessary events to attempt to
	// open a new channel. A stream is returned in place, this stream will
	// be used to consume updates of the state of the pending channel.
	req := &openChanReq{
		targetPubkey:    nodePubKey,
		chainHash:       activeNetParams.GenesisHash,
		localFundingAmt: localFundingAmt,
		pushAmt:         lnwire.NewMAtomsFromAtoms(remoteInitialBalance),
		minHtlcIn:       minHtlcIn,
		fundingFeePerKB: feeRate,
		private:         in.Private,
		remoteCsvDelay:  remoteCsvDelay,
		minConfs:        minConfs,
		shutdownScript:  script,
	}

	updateChan, errChan := r.server.OpenChannel(req)

	var outpoint wire.OutPoint
out:
	for {
		select {
		case err := <-errChan:
			rpcsLog.Errorf("unable to open channel to NodeKey(%x): %v",
				nodePubKeyBytes, err)
			return err
		case fundingUpdate := <-updateChan:
			rpcsLog.Tracef("[openchannel] sending update: %v",
				fundingUpdate)
			if err := updateStream.Send(fundingUpdate); err != nil {
				return err
			}

			// If a final channel open update is being sent, then
			// we can break out of our recv loop as we no longer
			// need to process any further updates.
			switch update := fundingUpdate.Update.(type) {
			case *lnrpc.OpenStatusUpdate_ChanOpen:
				chanPoint := update.ChanOpen.ChannelPoint
				txid, err := GetChanPointFundingTxid(chanPoint)
				if err != nil {
					return err
				}
				outpoint = wire.OutPoint{
					Hash:  *txid,
					Index: chanPoint.OutputIndex,
				}

				break out
			}
		case <-r.quit:
			return nil
		}
	}

	rpcsLog.Tracef("[openchannel] success NodeKey(%x), ChannelPoint(%v)",
		nodePubKeyBytes, outpoint)
	return nil
}

// OpenChannelSync is a synchronous version of the OpenChannel RPC call. This
// call is meant to be consumed by clients to the REST proxy. As with all other
// sync calls, all byte slices are instead to be populated as hex encoded
// strings.
func (r *rpcServer) OpenChannelSync(ctx context.Context,
	in *lnrpc.OpenChannelRequest) (*lnrpc.ChannelPoint, error) {

	rpcsLog.Tracef("[openchannel] request to NodeKey(%v) "+
		"allocation(us=%v, them=%v)", in.NodePubkeyString,
		in.LocalFundingAmount, in.PushAtoms)

	// We don't allow new channels to be open while the server is still
	// syncing, as otherwise we may not be able to obtain the relevant
	// notifications.
	if !r.server.Started() {
		return nil, ErrServerNotActive
	}

	// Creation of channels before the wallet syncs up is currently
	// disallowed.
	isSynced, _, err := r.server.cc.wallet.IsSynced()
	if err != nil {
		return nil, err
	}
	if !isSynced {
		return nil, errors.New("channels cannot be created before the " +
			"wallet is fully synced")
	}

	// Decode the provided target node's public key, parsing it into a pub
	// key object. For all sync call, byte slices are expected to be
	// encoded as hex strings.
	keyBytes, err := hex.DecodeString(in.NodePubkeyString)
	if err != nil {
		return nil, err
	}
	nodepubKey, err := secp256k1.ParsePubKey(keyBytes)
	if err != nil {
		return nil, err
	}

	localFundingAmt := dcrutil.Amount(in.LocalFundingAmount)
	remoteInitialBalance := dcrutil.Amount(in.PushAtoms)
	minHtlcIn := lnwire.MilliAtom(in.MinHtlcMAtoms)
	remoteCsvDelay := uint16(in.RemoteCsvDelay)

	// Ensure that the initial balance of the remote party (if pushing
	// atoms) does not exceed the amount the local party has requested
	// for funding.
	if remoteInitialBalance >= localFundingAmt {
		return nil, fmt.Errorf("amount pushed to remote peer for " +
			"initial state must be below the local funding amount")
	}

	// Restrict the size of the channel we'll actually open. At a later
	// level, we'll ensure that the output we create after accounting for
	// fees that a dust output isn't created.
	if localFundingAmt < minChanFundingSize {
		return nil, fmt.Errorf("channel is too small, the minimum channel "+
			"size is: %v Atoms", int64(minChanFundingSize))
	}

	// Then, we'll extract the minimum number of confirmations that each
	// output we use to fund the channel's funding transaction should
	// satisfy.
	minConfs, err := extractOpenChannelMinConfs(in)
	if err != nil {
		return nil, err
	}

	// Based on the passed fee related parameters, we'll determine an
	// appropriate fee rate for the funding transaction.
	atomsPerKB := chainfee.AtomPerKByte(in.AtomsPerByte * 1000)
	feeRate, err := sweep.DetermineFeePerKB(
		r.server.cc.feeEstimator, sweep.FeePreference{
			ConfTarget: uint32(in.TargetConf),
			FeeRate:    atomsPerKB,
		},
	)
	if err != nil {
		return nil, err
	}

	rpcsLog.Tracef("[openchannel] target atom/kB for funding tx: %v",
		int64(feeRate))

	script, err := parseUpfrontShutdownAddress(in.CloseAddress)
	if err != nil {
		return nil, fmt.Errorf("error parsing upfront shutdown: %v", err)
	}

	req := &openChanReq{
		targetPubkey:    nodepubKey,
		chainHash:       activeNetParams.GenesisHash,
		localFundingAmt: localFundingAmt,
		pushAmt:         lnwire.NewMAtomsFromAtoms(remoteInitialBalance),
		minHtlcIn:       minHtlcIn,
		fundingFeePerKB: feeRate,
		private:         in.Private,
		remoteCsvDelay:  remoteCsvDelay,
		minConfs:        minConfs,
		shutdownScript:  script,
	}

	updateChan, errChan := r.server.OpenChannel(req)
	select {
	// If an error occurs them immediately return the error to the client.
	case err := <-errChan:
		rpcsLog.Errorf("unable to open channel to NodeKey(%x): %v",
			nodepubKey, err)
		return nil, err

	// Otherwise, wait for the first channel update. The first update sent
	// is when the funding transaction is broadcast to the network.
	case fundingUpdate := <-updateChan:
		rpcsLog.Tracef("[openchannel] sending update: %v",
			fundingUpdate)

		// Parse out the txid of the pending funding transaction. The
		// sync client can use this to poll against the list of
		// PendingChannels.
		openUpdate := fundingUpdate.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
		chanUpdate := openUpdate.ChanPending

		return &lnrpc.ChannelPoint{
			FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
				FundingTxidBytes: chanUpdate.Txid,
			},
			OutputIndex: chanUpdate.OutputIndex,
		}, nil
	case <-r.quit:
		return nil, nil
	}
}

// parseUpfrontShutdownScript attempts to parse an upfront shutdown address.
// If the address is empty, it returns nil. If it successfully decoded the
// address, it returns a script that pays out to the address.
func parseUpfrontShutdownAddress(address string) (lnwire.DeliveryAddress, error) {
	if len(address) == 0 {
		return nil, nil
	}

	addr, err := dcrutil.DecodeAddress(
		address, activeNetParams.Params,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid address: %v", err)
	}

	return txscript.PayToAddrScript(addr)
}

// GetChanPointFundingTxid returns the given channel point's funding txid in
// raw bytes.
func GetChanPointFundingTxid(chanPoint *lnrpc.ChannelPoint) (*chainhash.Hash, error) {
	var txid []byte

	// A channel point's funding txid can be get/set as a byte slice or a
	// string. In the case it is a string, decode it.
	switch chanPoint.GetFundingTxid().(type) {
	case *lnrpc.ChannelPoint_FundingTxidBytes:
		txid = chanPoint.GetFundingTxidBytes()
	case *lnrpc.ChannelPoint_FundingTxidStr:
		s := chanPoint.GetFundingTxidStr()
		h, err := chainhash.NewHashFromStr(s)
		if err != nil {
			return nil, err
		}

		txid = h[:]
	}

	return chainhash.NewHash(txid)
}

// CloseChannel attempts to close an active channel identified by its channel
// point. The actions of this method can additionally be augmented to attempt
// a force close after a timeout period in the case of an inactive peer.
func (r *rpcServer) CloseChannel(in *lnrpc.CloseChannelRequest,
	updateStream lnrpc.Lightning_CloseChannelServer) error {

	if !r.server.Started() {
		return ErrServerNotActive
	}

	// If the user didn't specify a channel point, then we'll reject this
	// request all together.
	if in.GetChannelPoint() == nil {
		return fmt.Errorf("must specify channel point in close channel")
	}

	// If force closing a channel, the fee set in the commitment transaction
	// is used.
	if in.Force && (in.AtomsPerByte != 0 || in.TargetConf != 0) {
		return fmt.Errorf("force closing a channel uses a pre-defined fee")
	}

	force := in.Force
	index := in.ChannelPoint.OutputIndex
	txid, err := GetChanPointFundingTxid(in.GetChannelPoint())
	if err != nil {
		rpcsLog.Errorf("[closechannel] unable to get funding txid: %v", err)
		return err
	}
	chanPoint := wire.NewOutPoint(txid, index, wire.TxTreeRegular)

	rpcsLog.Tracef("[closechannel] request for ChannelPoint(%v), force=%v",
		chanPoint, force)

	var (
		updateChan chan interface{}
		errChan    chan error
	)

	// TODO(roasbeef): if force and peer online then don't force?

	// First, we'll fetch the channel as is, as we'll need to examine it
	// regardless of if this is a force close or not.
	channel, err := r.fetchActiveChannel(*chanPoint)
	if err != nil {
		return err
	}

	// If a force closure was requested, then we'll handle all the details
	// around the creation and broadcast of the unilateral closure
	// transaction here rather than going to the switch as we don't require
	// interaction from the peer.
	if force {
		_, bestHeight, err := r.server.cc.chainIO.GetBestBlock()
		if err != nil {
			return err
		}

		// As we're force closing this channel, as a precaution, we'll
		// ensure that the switch doesn't continue to see this channel
		// as eligible for forwarding HTLC's. If the peer is online,
		// then we'll also purge all of its indexes.
		remotePub := &channel.StateSnapshot().RemoteIdentity
		if peer, err := r.server.FindPeer(remotePub); err == nil {
			// TODO(roasbeef): actually get the active channel
			// instead too?
			//  * so only need to grab from database
			peer.WipeChannel(channel.ChannelPoint())
		} else {
			chanID := lnwire.NewChanIDFromOutPoint(channel.ChannelPoint())
			r.server.htlcSwitch.RemoveLink(chanID)
		}

		// With the necessary indexes cleaned up, we'll now force close
		// the channel.
		chainArbitrator := r.server.chainArb
		closingTx, err := chainArbitrator.ForceCloseContract(
			*chanPoint,
		)
		if err != nil {
			rpcsLog.Errorf("unable to force close transaction: %v", err)
			return err
		}

		closingTxid := closingTx.TxHash()

		// With the transaction broadcast, we send our first update to
		// the client.
		updateChan = make(chan interface{}, 2)
		updateChan <- &pendingUpdate{
			Txid: closingTxid[:],
		}

		errChan = make(chan error, 1)
		notifier := r.server.cc.chainNotifier
		go waitForChanToClose(uint32(bestHeight), notifier, errChan, chanPoint,
			&closingTxid, closingTx.TxOut[0].PkScript, func() {
				// Respond to the local subsystem which
				// requested the channel closure.
				updateChan <- &channelCloseUpdate{
					ClosingTxid: closingTxid[:],
					Success:     true,
				}
			})
	} else {
		// If the link is not known by the switch, we cannot gracefully close
		// the channel.
		channelID := lnwire.NewChanIDFromOutPoint(chanPoint)
		if _, err := r.server.htlcSwitch.GetLink(channelID); err != nil {
			rpcsLog.Debugf("Trying to non-force close offline channel with "+
				"chan_point=%v", chanPoint)
			return fmt.Errorf("unable to gracefully close channel while peer "+
				"is offline (try force closing it instead): %v", err)
		}

		// Based on the passed fee related parameters, we'll determine
		// an appropriate fee rate for the cooperative closure
		// transaction.
		atomsPerKB := chainfee.AtomPerKByte(in.AtomsPerByte * 1000)
		feeRate, err := sweep.DetermineFeePerKB(
			r.server.cc.feeEstimator, sweep.FeePreference{
				ConfTarget: uint32(in.TargetConf),
				FeeRate:    atomsPerKB,
			},
		)
		if err != nil {
			return err
		}

		rpcsLog.Debugf("Target atom/kB for closing transaction: %v",
			int64(feeRate))

		// Before we attempt the cooperative channel closure, we'll
		// examine the channel to ensure that it doesn't have a
		// lingering HTLC.
		if len(channel.ActiveHtlcs()) != 0 {
			return fmt.Errorf("cannot co-op close channel " +
				"with active htlcs")
		}

		// Otherwise, the caller has requested a regular interactive
		// cooperative channel closure. So we'll forward the request to
		// the htlc switch which will handle the negotiation and
		// broadcast details.

		var deliveryScript lnwire.DeliveryAddress

		// If a delivery address to close out to was specified, decode it.
		if len(in.DeliveryAddress) > 0 {
			// Decode the address provided.
			addr, err := dcrutil.DecodeAddress(
				in.DeliveryAddress, activeNetParams.Params,
			)
			if err != nil {
				return fmt.Errorf("invalid delivery address: %v", err)
			}

			// Create a script to pay out to the address provided.
			deliveryScript, err = txscript.PayToAddrScript(addr)
			if err != nil {
				return err
			}
		}

		updateChan, errChan = r.server.htlcSwitch.CloseLink(
			chanPoint, htlcswitch.CloseRegular, feeRate, deliveryScript,
		)
	}
out:
	for {
		select {
		case err := <-errChan:
			rpcsLog.Errorf("[closechannel] unable to close "+
				"ChannelPoint(%v): %v", chanPoint, err)
			return err
		case closingUpdate := <-updateChan:
			rpcClosingUpdate, err := createRPCCloseUpdate(
				closingUpdate,
			)
			if err != nil {
				return err
			}

			rpcsLog.Tracef("[closechannel] sending update: %v",
				rpcClosingUpdate)

			if err := updateStream.Send(rpcClosingUpdate); err != nil {
				return err
			}

			// If a final channel closing updates is being sent,
			// then we can break out of our dispatch loop as we no
			// longer need to process any further updates.
			switch closeUpdate := closingUpdate.(type) {
			case *channelCloseUpdate:
				h, _ := chainhash.NewHash(closeUpdate.ClosingTxid)
				rpcsLog.Infof("[closechannel] close completed: "+
					"txid(%v)", h)
				break out
			}
		case <-r.quit:
			return nil
		}
	}

	return nil
}

func createRPCCloseUpdate(update interface{}) (
	*lnrpc.CloseStatusUpdate, error) {

	switch u := update.(type) {
	case *channelCloseUpdate:
		return &lnrpc.CloseStatusUpdate{
			Update: &lnrpc.CloseStatusUpdate_ChanClose{
				ChanClose: &lnrpc.ChannelCloseUpdate{
					ClosingTxid: u.ClosingTxid,
				},
			},
		}, nil
	case *pendingUpdate:
		return &lnrpc.CloseStatusUpdate{
			Update: &lnrpc.CloseStatusUpdate_ClosePending{
				ClosePending: &lnrpc.PendingUpdate{
					Txid:        u.Txid,
					OutputIndex: u.OutputIndex,
				},
			},
		}, nil
	}

	return nil, errors.New("unknown close status update")
}

// abandonChanFromGraph attempts to remove a channel from the channel graph. If
// we can't find the chanID in the graph, then we assume it has already been
// removed, and will return a nop.
func abandonChanFromGraph(chanGraph *channeldb.ChannelGraph,
	chanPoint *wire.OutPoint) error {

	// First, we'll obtain the channel ID. If we can't locate this, then
	// it's the case that the channel may have already been removed from
	// the graph, so we'll return a nil error.
	chanID, err := chanGraph.ChannelID(chanPoint)
	switch {
	case err == channeldb.ErrEdgeNotFound:
		return nil
	case err != nil:
		return err
	}

	// If the channel ID is still in the graph, then that means the channel
	// is still open, so we'll now move to purge it from the graph.
	return chanGraph.DeleteChannelEdges(chanID)
}

// AbandonChannel removes all channel state from the database except for a
// close summary. This method can be used to get rid of permanently unusable
// channels due to bugs fixed in newer versions of lnd.
func (r *rpcServer) AbandonChannel(ctx context.Context,
	in *lnrpc.AbandonChannelRequest) (*lnrpc.AbandonChannelResponse, error) {

	// If this isn't the dev build, then we won't allow the RPC to be
	// executed, as it's an advanced feature and won't be activated in
	// regular production/release builds.
	if !build.IsDevBuild() {
		return nil, fmt.Errorf("AbandonChannel RPC call only " +
			"available in dev builds")
	}

	// We'll parse out the arguments to we can obtain the chanPoint of the
	// target channel.
	txid, err := GetChanPointFundingTxid(in.GetChannelPoint())
	if err != nil {
		return nil, err
	}
	index := in.ChannelPoint.OutputIndex
	chanPoint := wire.NewOutPoint(txid, index, wire.TxTreeRegular)

	// When we remove the channel from the database, we need to set a close
	// height, so we'll just use the current best known height.
	_, bestHeight, err := r.server.cc.chainIO.GetBestBlock()
	if err != nil {
		return nil, err
	}

	dbChan, err := r.server.chanDB.FetchChannel(*chanPoint)
	switch {
	// If the channel isn't found in the set of open channels, then we can
	// continue on as it can't be loaded into the link/peer.
	case err == channeldb.ErrChannelNotFound:
		break

	// If the channel is still known to be open, then before we modify any
	// on-disk state, we'll remove the channel from the switch and peer
	// state if it's been loaded in.
	case err == nil:
		// We'll mark the channel as borked before we remove the state
		// from the switch/peer so it won't be loaded back in if the
		// peer reconnects.
		if err := dbChan.MarkBorked(); err != nil {
			return nil, err
		}
		remotePub := dbChan.IdentityPub
		if peer, err := r.server.FindPeer(remotePub); err == nil {
			if err := peer.WipeChannel(chanPoint); err != nil {
				return nil, fmt.Errorf("unable to wipe "+
					"channel state: %v", err)
			}
		}

	default:
		return nil, err
	}

	// Abandoning a channel is a three step process: remove from the open
	// channel state, remove from the graph, remove from the contract
	// court. Between any step it's possible that the users restarts the
	// process all over again. As a result, each of the steps below are
	// intended to be idempotent.
	err = r.server.chanDB.AbandonChannel(chanPoint, uint32(bestHeight))
	if err != nil {
		return nil, err
	}
	err = abandonChanFromGraph(
		r.server.chanDB.ChannelGraph(), chanPoint,
	)
	if err != nil {
		return nil, err
	}
	err = r.server.chainArb.ResolveContract(*chanPoint)
	if err != nil {
		return nil, err
	}

	// If this channel was in the process of being closed, but didn't fully
	// close, then it's possible that the nursery is hanging on to some
	// state. To err on the side of caution, we'll now attempt to wipe any
	// state for this channel from the nursery.
	err = r.server.utxoNursery.cfg.Store.RemoveChannel(chanPoint)
	if err != nil && err != ErrContractNotFound {
		return nil, err
	}

	return &lnrpc.AbandonChannelResponse{}, nil
}

// fetchActiveChannel attempts to locate a channel identified by its channel
// point from the database's set of all currently opened channels and
// return it as a fully populated state machine
func (r *rpcServer) fetchActiveChannel(chanPoint wire.OutPoint) (
	*lnwallet.LightningChannel, error) {

	dbChan, err := r.server.chanDB.FetchChannel(chanPoint)
	if err != nil {
		return nil, err
	}

	// If the channel is successfully fetched from the database,
	// we create a fully populated channel state machine which
	// uses the db channel as backing storage.
	return lnwallet.NewLightningChannel(
		r.server.cc.wallet.Cfg.Signer, dbChan, nil,
		activeNetParams.Params,
	)
}

// GetInfo returns general information concerning the lightning node including
// its identity pubkey, alias, the chains it is connected to, and information
// concerning the number of open+pending channels.
func (r *rpcServer) GetInfo(ctx context.Context,
	in *lnrpc.GetInfoRequest) (*lnrpc.GetInfoResponse, error) {

	serverPeers := r.server.Peers()

	openChannels, err := r.server.chanDB.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}

	var activeChannels uint32
	for _, channel := range openChannels {
		chanID := lnwire.NewChanIDFromOutPoint(&channel.FundingOutpoint)
		if r.server.htlcSwitch.HasActiveLink(chanID) {
			activeChannels++
		}
	}

	inactiveChannels := uint32(len(openChannels)) - activeChannels

	pendingChannels, err := r.server.chanDB.FetchPendingChannels()
	if err != nil {
		return nil, fmt.Errorf("unable to get retrieve pending "+
			"channels: %v", err)
	}
	nPendingChannels := uint32(len(pendingChannels))

	idPub := r.server.identityPriv.PubKey().SerializeCompressed()
	encodedIDPub := hex.EncodeToString(idPub)

	bestHash, bestHeight, err := r.server.cc.chainIO.GetBestBlock()
	if err != nil {
		return nil, fmt.Errorf("unable to get best block info: %v", err)
	}

	isSynced, bestHeaderTimestamp, err := r.server.cc.wallet.IsSynced()
	if err != nil {
		return nil, fmt.Errorf("unable to sync PoV of the wallet "+
			"with current best block in the main chain: %v", err)
	}

	network := normalizeNetwork(activeNetParams.Name)
	activeChains := make([]*lnrpc.Chain, registeredChains.NumActiveChains())
	for i, chain := range registeredChains.ActiveChains() {
		activeChains[i] = &lnrpc.Chain{
			Chain:   chain.String(),
			Network: network,
		}

	}

	// Check if external IP addresses were provided to lnd and use them
	// to set the URIs.
	nodeAnn, err := r.server.genNodeAnnouncement(false)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve current fully signed "+
			"node announcement: %v", err)
	}
	addrs := nodeAnn.Addresses
	uris := make([]string, len(addrs))
	for i, addr := range addrs {
		uris[i] = fmt.Sprintf("%s@%s", encodedIDPub, addr.String())
	}

	isGraphSynced := r.server.authGossiper.SyncManager().IsGraphSynced()

	features := make(map[uint32]*lnrpc.Feature)
	sets := r.server.featureMgr.ListSets()

	for _, set := range sets {
		// Get the a list of lnrpc features for each set we support.
		featureVector := r.server.featureMgr.Get(set)
		rpcFeatures := invoicesrpc.CreateRPCFeatures(featureVector)

		// Add the features to our map of features, allowing over writing of
		// existing values because features in different sets with the same bit
		// are duplicated across sets.
		for bit, feature := range rpcFeatures {
			features[bit] = feature
		}
	}

	// TODO(roasbeef): add synced height n stuff
	return &lnrpc.GetInfoResponse{
		IdentityPubkey:      encodedIDPub,
		NumPendingChannels:  nPendingChannels,
		NumActiveChannels:   activeChannels,
		NumInactiveChannels: inactiveChannels,
		NumPeers:            uint32(len(serverPeers)),
		BlockHeight:         uint32(bestHeight),
		BlockHash:           bestHash.String(),
		SyncedToChain:       isSynced,
		Testnet:             isTestnet(&activeNetParams),
		Chains:              activeChains,
		Uris:                uris,
		Alias:               nodeAnn.Alias.String(),
		Color:               routing.EncodeHexColor(nodeAnn.RGBColor),
		BestHeaderTimestamp: bestHeaderTimestamp,
		Version:             build.Version(),
		SyncedToGraph:       isGraphSynced,
		Features:            features,
	}, nil
}

// ListPeers returns a verbose listing of all currently active peers.
func (r *rpcServer) ListPeers(ctx context.Context,
	in *lnrpc.ListPeersRequest) (*lnrpc.ListPeersResponse, error) {

	rpcsLog.Tracef("[listpeers] request")

	serverPeers := r.server.Peers()
	resp := &lnrpc.ListPeersResponse{
		Peers: make([]*lnrpc.Peer, 0, len(serverPeers)),
	}

	for _, serverPeer := range serverPeers {
		var (
			atomsSent int64
			atomsRecv int64
		)

		// In order to display the total number of atoms of outbound
		// (sent) and inbound (recv'd) atoms that have been
		// transported through this peer, we'll sum up the sent/recv'd
		// values for each of the active channels we have with the
		// peer.
		chans := serverPeer.ChannelSnapshots()
		for _, c := range chans {
			atomsSent += int64(c.TotalMAtomsSent.ToAtoms())
			atomsRecv += int64(c.TotalMAtomsReceived.ToAtoms())
		}

		nodePub := serverPeer.PubKey()

		// Retrieve the peer's sync type. If we don't currently have a
		// syncer for the peer, then we'll default to a passive sync.
		// This can happen if the RPC is called while a peer is
		// initializing.
		syncer, ok := r.server.authGossiper.SyncManager().GossipSyncer(
			nodePub,
		)

		var lnrpcSyncType lnrpc.Peer_SyncType
		if !ok {
			rpcsLog.Warnf("Gossip syncer for peer=%x not found",
				nodePub)
			lnrpcSyncType = lnrpc.Peer_UNKNOWN_SYNC
		} else {
			syncType := syncer.SyncType()
			switch syncType {
			case discovery.ActiveSync:
				lnrpcSyncType = lnrpc.Peer_ACTIVE_SYNC
			case discovery.PassiveSync:
				lnrpcSyncType = lnrpc.Peer_PASSIVE_SYNC
			default:
				return nil, fmt.Errorf("unhandled sync type %v",
					syncType)
			}
		}

		features := invoicesrpc.CreateRPCFeatures(
			serverPeer.RemoteFeatures(),
		)

		peer := &lnrpc.Peer{
			PubKey:    hex.EncodeToString(nodePub[:]),
			Address:   serverPeer.conn.RemoteAddr().String(),
			Inbound:   serverPeer.inbound,
			BytesRecv: atomic.LoadUint64(&serverPeer.bytesReceived),
			BytesSent: atomic.LoadUint64(&serverPeer.bytesSent),
			AtomsSent: atomsSent,
			AtomsRecv: atomsRecv,
			PingTime:  serverPeer.PingTime(),
			SyncType:  lnrpcSyncType,
			Features:  features,
		}

		resp.Peers = append(resp.Peers, peer)
	}

	rpcsLog.Debugf("[listpeers] yielded %v peers", serverPeers)

	return resp, nil
}

// SubscribePeerEvents returns a uni-directional stream (server -> client)
// for notifying the client of peer online and offline events.
func (r *rpcServer) SubscribePeerEvents(req *lnrpc.PeerEventSubscription,
	eventStream lnrpc.Lightning_SubscribePeerEventsServer) error {

	peerEventSub, err := r.server.peerNotifier.SubscribePeerEvents()
	if err != nil {
		return err
	}
	defer peerEventSub.Cancel()

	for {
		select {
		// A new update has been sent by the peer notifier, we'll
		// marshal it into the form expected by the gRPC client, then
		// send it off to the client.
		case e := <-peerEventSub.Updates():
			var event *lnrpc.PeerEvent

			switch peerEvent := e.(type) {
			case peernotifier.PeerOfflineEvent:
				event = &lnrpc.PeerEvent{
					PubKey: hex.EncodeToString(peerEvent.PubKey[:]),
					Type:   lnrpc.PeerEvent_PEER_OFFLINE,
				}

			case peernotifier.PeerOnlineEvent:
				event = &lnrpc.PeerEvent{
					PubKey: hex.EncodeToString(peerEvent.PubKey[:]),
					Type:   lnrpc.PeerEvent_PEER_ONLINE,
				}

			default:
				return fmt.Errorf("unexpected peer event: %v", event)
			}

			if err := eventStream.Send(event); err != nil {
				return err
			}
		case <-r.quit:
			return nil
		}
	}
}

// WalletBalance returns total unspent outputs(confirmed and unconfirmed), all
// confirmed unspent outputs and all unconfirmed unspent outputs under control
// by the wallet. This method can be modified by having the request specify
// only witness outputs should be factored into the final output sum.
// TODO(roasbeef): add async hooks into wallet balance changes
func (r *rpcServer) WalletBalance(ctx context.Context,
	in *lnrpc.WalletBalanceRequest) (*lnrpc.WalletBalanceResponse, error) {

	// Get total balance, from txs that have >= 0 confirmations.
	totalBal, err := r.server.cc.wallet.ConfirmedBalance(0)
	if err != nil {
		return nil, err
	}

	// Get confirmed balance, from txs that have >= 1 confirmations.
	// TODO(halseth): get both unconfirmed and confirmed balance in one
	// call, as this is racy.
	confirmedBal, err := r.server.cc.wallet.ConfirmedBalance(1)
	if err != nil {
		return nil, err
	}

	// Get unconfirmed balance, from txs with 0 confirmations.
	unconfirmedBal := totalBal - confirmedBal

	rpcsLog.Debugf("[walletbalance] Total balance=%v (confirmed=%v, "+
		"unconfirmed=%v)", totalBal, confirmedBal, unconfirmedBal)

	return &lnrpc.WalletBalanceResponse{
		TotalBalance:       int64(totalBal),
		ConfirmedBalance:   int64(confirmedBal),
		UnconfirmedBalance: int64(unconfirmedBal),
	}, nil
}

// ChannelBalance returns the total available channel flow across all open
// channels in atoms.
func (r *rpcServer) ChannelBalance(ctx context.Context,
	in *lnrpc.ChannelBalanceRequest) (*lnrpc.ChannelBalanceResponse, error) {

	openChannels, err := r.server.chanDB.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}

	var balance dcrutil.Amount
	var maxInbound dcrutil.Amount
	var maxOutbound dcrutil.Amount
	for _, channel := range openChannels {
		local := channel.LocalCommitment.LocalBalance.ToAtoms()
		localReserve := channel.LocalChanCfg.ChannelConstraints.ChanReserve
		remote := channel.RemoteCommitment.RemoteBalance.ToAtoms()
		remoteReserve := channel.RemoteChanCfg.ChannelConstraints.ChanReserve

		balance += local

		// The maximum amount we can receive from this channel is however much
		// the remote node has, minus its required channel reserve.
		if remote > remoteReserve {
			maxInbound += remote - remoteReserve
		}

		// The maximum amount we can send accoss this channel is however much
		// the local node has, minus what the remote node requires us to
		// reserve.
		if local > localReserve {
			maxOutbound += local - localReserve
		}

	}

	pendingChannels, err := r.server.chanDB.FetchPendingChannels()
	if err != nil {
		return nil, err
	}

	var pendingOpenBalance dcrutil.Amount
	for _, channel := range pendingChannels {
		pendingOpenBalance += channel.LocalCommitment.LocalBalance.ToAtoms()
	}

	rpcsLog.Debugf("[channelbalance] balance=%v pending-open=%v",
		balance, pendingOpenBalance)

	return &lnrpc.ChannelBalanceResponse{
		Balance:            int64(balance),
		PendingOpenBalance: int64(pendingOpenBalance),
		MaxInboundAmount:   int64(maxInbound),
		MaxOutboundAmount:  int64(maxOutbound),
	}, nil
}

// PendingChannels returns a list of all the channels that are currently
// considered "pending". A channel is pending if it has finished the funding
// workflow and is waiting for confirmations for the funding txn, or is in the
// process of closure, either initiated cooperatively or non-cooperatively.
func (r *rpcServer) PendingChannels(ctx context.Context,
	in *lnrpc.PendingChannelsRequest) (*lnrpc.PendingChannelsResponse, error) {

	rpcsLog.Debugf("[pendingchannels]")

	resp := &lnrpc.PendingChannelsResponse{}

	// First, we'll populate the response with all the channels that are
	// soon to be opened. We can easily fetch this data from the database
	// and map the db struct to the proto response.
	pendingOpenChannels, err := r.server.chanDB.FetchPendingChannels()
	if err != nil {
		rpcsLog.Errorf("unable to fetch pending channels: %v", err)
		return nil, err
	}
	resp.PendingOpenChannels = make([]*lnrpc.PendingChannelsResponse_PendingOpenChannel,
		len(pendingOpenChannels))
	for i, pendingChan := range pendingOpenChannels {
		pub := pendingChan.IdentityPub.SerializeCompressed()

		// As this is required for display purposes, we'll calculate
		// the size of the commitment transaction. We also add on the
		// estimated size of the witness to calculate the size of the
		// transaction if it were to be immediately unilaterally
		// broadcast.
		// TODO(roasbeef): query for funding tx from wallet, display
		// that also?
		localCommitment := pendingChan.LocalCommitment
		utx := localCommitment.CommitTx
		commitBaseSize := int64(utx.SerializeSize())
		commitSize := commitBaseSize + 1 + input.FundingOutputSigScriptSize

		resp.PendingOpenChannels[i] = &lnrpc.PendingChannelsResponse_PendingOpenChannel{
			Channel: &lnrpc.PendingChannelsResponse_PendingChannel{
				RemoteNodePub:          hex.EncodeToString(pub),
				ChannelPoint:           pendingChan.FundingOutpoint.String(),
				Capacity:               int64(pendingChan.Capacity),
				LocalBalance:           int64(localCommitment.LocalBalance.ToAtoms()),
				RemoteBalance:          int64(localCommitment.RemoteBalance.ToAtoms()),
				LocalChanReserveAtoms:  int64(pendingChan.LocalChanCfg.ChanReserve),
				RemoteChanReserveAtoms: int64(pendingChan.RemoteChanCfg.ChanReserve),
			},
			CommitSize: commitSize,
			CommitFee:  int64(localCommitment.CommitFee),
			FeePerKb:   int64(localCommitment.FeePerKB),
			// TODO(roasbeef): need to track confirmation height
		}
	}

	_, currentHeight, err := r.server.cc.chainIO.GetBestBlock()
	if err != nil {
		return nil, err
	}

	// Next, we'll examine the channels that are soon to be closed so we
	// can populate these fields within the response.
	pendingCloseChannels, err := r.server.chanDB.FetchClosedChannels(true)
	if err != nil {
		rpcsLog.Errorf("unable to fetch closed channels: %v", err)
		return nil, err
	}
	for _, pendingClose := range pendingCloseChannels {
		// First construct the channel struct itself, this will be
		// needed regardless of how this channel was closed.
		pub := pendingClose.RemotePub.SerializeCompressed()
		chanPoint := pendingClose.ChanPoint
		channel := &lnrpc.PendingChannelsResponse_PendingChannel{
			RemoteNodePub: hex.EncodeToString(pub),
			ChannelPoint:  chanPoint.String(),
			Capacity:      int64(pendingClose.Capacity),
			LocalBalance:  int64(pendingClose.SettledBalance),
		}

		closeTXID := pendingClose.ClosingTXID.String()

		switch pendingClose.CloseType {

		// If the channel was closed cooperatively, then we'll only
		// need to tack on the closing txid.
		// TODO(halseth): remove. After recent changes, a coop closed
		// channel should never be in the "pending close" state.
		// Keeping for now to let someone that upgraded in the middle
		// of a close let their closing tx confirm.
		case channeldb.CooperativeClose:
			resp.PendingClosingChannels = append(
				resp.PendingClosingChannels,
				&lnrpc.PendingChannelsResponse_ClosedChannel{
					Channel:     channel,
					ClosingTxid: closeTXID,
				},
			)

			resp.TotalLimboBalance += channel.LocalBalance

		// If the channel was force closed, then we'll need to query
		// the utxoNursery for additional information.
		// TODO(halseth): distinguish remote and local case?
		case channeldb.LocalForceClose, channeldb.RemoteForceClose:
			forceClose := &lnrpc.PendingChannelsResponse_ForceClosedChannel{
				Channel:     channel,
				ClosingTxid: closeTXID,
			}

			// Fetch reports from both nursery and resolvers. At the
			// moment this is not an atomic snapshot. This is
			// planned to be resolved when the nursery is removed
			// and channel arbitrator will be the single source for
			// these kind of reports.
			err := r.nurseryPopulateForceCloseResp(
				&chanPoint, currentHeight, forceClose,
			)
			if err != nil {
				return nil, err
			}

			err = r.arbitratorPopulateForceCloseResp(
				&chanPoint, currentHeight, forceClose,
			)
			if err != nil {
				return nil, err
			}

			resp.TotalLimboBalance += forceClose.LimboBalance

			resp.PendingForceClosingChannels = append(
				resp.PendingForceClosingChannels,
				forceClose,
			)
		}
	}

	// We'll also fetch all channels that are open, but have had their
	// commitment broadcasted, meaning they are waiting for the closing
	// transaction to confirm.
	waitingCloseChans, err := r.server.chanDB.FetchWaitingCloseChannels()
	if err != nil {
		rpcsLog.Errorf("unable to fetch channels waiting close: %v",
			err)
		return nil, err
	}

	for _, waitingClose := range waitingCloseChans {
		pub := waitingClose.IdentityPub.SerializeCompressed()
		chanPoint := waitingClose.FundingOutpoint
		channel := &lnrpc.PendingChannelsResponse_PendingChannel{
			RemoteNodePub: hex.EncodeToString(pub),
			ChannelPoint:  chanPoint.String(),
			Capacity:      int64(waitingClose.Capacity),
			LocalBalance:  int64(waitingClose.LocalCommitment.LocalBalance.ToAtoms()),
		}

		// A close tx has been broadcasted, all our balance will be in
		// limbo until it confirms.
		resp.WaitingCloseChannels = append(
			resp.WaitingCloseChannels,
			&lnrpc.PendingChannelsResponse_WaitingCloseChannel{
				Channel:      channel,
				LimboBalance: channel.LocalBalance,
			},
		)

		resp.TotalLimboBalance += channel.LocalBalance
	}

	return resp, nil
}

// arbitratorPopulateForceCloseResp populates the pending channels response
// message with channel resolution information from the contract resolvers.
func (r *rpcServer) arbitratorPopulateForceCloseResp(chanPoint *wire.OutPoint,
	currentHeight int32,
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel) error {

	// Query for contract resolvers state.
	arbitrator, err := r.server.chainArb.GetChannelArbitrator(*chanPoint)
	if err != nil {
		return err
	}
	reports := arbitrator.Report()

	for _, report := range reports {
		switch report.Type {

		// For a direct output, populate/update the top level
		// response properties.
		case contractcourt.ReportOutputUnencumbered:
			// Populate the maturity height fields for the direct
			// commitment output to us.
			forceClose.MaturityHeight = report.MaturityHeight

			// If the transaction has been confirmed, then we can
			// compute how many blocks it has left.
			if forceClose.MaturityHeight != 0 {
				forceClose.BlocksTilMaturity =
					int32(forceClose.MaturityHeight) -
						currentHeight
			}

		// Add htlcs to the PendingHtlcs response property.
		case contractcourt.ReportOutputIncomingHtlc,
			contractcourt.ReportOutputOutgoingHtlc:

			incoming := report.Type == contractcourt.ReportOutputIncomingHtlc
			htlc := &lnrpc.PendingHTLC{
				Incoming:       incoming,
				Amount:         int64(report.Amount),
				Outpoint:       report.Outpoint.String(),
				MaturityHeight: report.MaturityHeight,
				Stage:          report.Stage,
			}

			if htlc.MaturityHeight != 0 {
				htlc.BlocksTilMaturity =
					int32(htlc.MaturityHeight) - currentHeight
			}

			forceClose.PendingHtlcs = append(forceClose.PendingHtlcs, htlc)

		default:
			return fmt.Errorf("unknown report output type: %v",
				report.Type)
		}

		forceClose.LimboBalance += int64(report.LimboBalance)
		forceClose.RecoveredBalance += int64(report.RecoveredBalance)

	}

	return nil
}

// nurseryPopulateForceCloseResp populates the pending channels response
// message with contract resolution information from utxonursery.
func (r *rpcServer) nurseryPopulateForceCloseResp(chanPoint *wire.OutPoint,
	currentHeight int32,
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel) error {

	// Query for the maturity state for this force closed channel. If we
	// didn't have any time-locked outputs, then the nursery may not know of
	// the contract.
	nurseryInfo, err := r.server.utxoNursery.NurseryReport(chanPoint)
	if err == ErrContractNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("unable to obtain "+
			"nursery report for ChannelPoint(%v): %v",
			chanPoint, err)
	}

	// If the nursery knows of this channel, then we can populate
	// information detailing exactly how much funds are time locked and also
	// the height in which we can ultimately sweep the funds into the
	// wallet.
	forceClose.LimboBalance = int64(nurseryInfo.limboBalance)
	forceClose.RecoveredBalance = int64(nurseryInfo.recoveredBalance)

	for _, htlcReport := range nurseryInfo.htlcs {
		// TODO(conner) set incoming flag appropriately after handling
		// incoming incubation
		htlc := &lnrpc.PendingHTLC{
			Incoming:       false,
			Amount:         int64(htlcReport.amount),
			Outpoint:       htlcReport.outpoint.String(),
			MaturityHeight: htlcReport.maturityHeight,
			Stage:          htlcReport.stage,
		}

		if htlc.MaturityHeight != 0 {
			htlc.BlocksTilMaturity =
				int32(htlc.MaturityHeight) -
					currentHeight
		}

		forceClose.PendingHtlcs = append(forceClose.PendingHtlcs,
			htlc)
	}

	return nil
}

// ClosedChannels returns a list of all the channels have been closed.
// This does not include channels that are still in the process of closing.
func (r *rpcServer) ClosedChannels(ctx context.Context,
	in *lnrpc.ClosedChannelsRequest) (*lnrpc.ClosedChannelsResponse,
	error) {

	// Show all channels when no filter flags are set.
	filterResults := in.Cooperative || in.LocalForce ||
		in.RemoteForce || in.Breach || in.FundingCanceled ||
		in.Abandoned

	resp := &lnrpc.ClosedChannelsResponse{}

	dbChannels, err := r.server.chanDB.FetchClosedChannels(false)
	if err != nil {
		return nil, err
	}

	// In order to make the response easier to parse for clients, we'll
	// sort the set of closed channels by their closing height before
	// serializing the proto response.
	sort.Slice(dbChannels, func(i, j int) bool {
		return dbChannels[i].CloseHeight < dbChannels[j].CloseHeight
	})

	for _, dbChannel := range dbChannels {
		if dbChannel.IsPending {
			continue
		}

		switch dbChannel.CloseType {
		case channeldb.CooperativeClose:
			if filterResults && !in.Cooperative {
				continue
			}
		case channeldb.LocalForceClose:
			if filterResults && !in.LocalForce {
				continue
			}
		case channeldb.RemoteForceClose:
			if filterResults && !in.RemoteForce {
				continue
			}
		case channeldb.BreachClose:
			if filterResults && !in.Breach {
				continue
			}
		case channeldb.FundingCanceled:
			if filterResults && !in.FundingCanceled {
				continue
			}
		case channeldb.Abandoned:
			if filterResults && !in.Abandoned {
				continue
			}
		}

		channel := createRPCClosedChannel(dbChannel)
		resp.Channels = append(resp.Channels, channel)
	}

	return resp, nil
}

// ListChannels returns a description of all the open channels that this node
// is a participant in.
func (r *rpcServer) ListChannels(ctx context.Context,
	in *lnrpc.ListChannelsRequest) (*lnrpc.ListChannelsResponse, error) {

	if in.ActiveOnly && in.InactiveOnly {
		return nil, fmt.Errorf("either `active_only` or " +
			"`inactive_only` can be set, but not both")
	}

	if in.PublicOnly && in.PrivateOnly {
		return nil, fmt.Errorf("either `public_only` or " +
			"`private_only` can be set, but not both")
	}

	resp := &lnrpc.ListChannelsResponse{}

	graph := r.server.chanDB.ChannelGraph()

	dbChannels, err := r.server.chanDB.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}

	rpcsLog.Debugf("[listchannels] fetched %v channels from DB",
		len(dbChannels))

	for _, dbChannel := range dbChannels {
		nodePub := dbChannel.IdentityPub
		chanPoint := dbChannel.FundingOutpoint

		var peerOnline bool
		if _, err := r.server.FindPeer(nodePub); err == nil {
			peerOnline = true
		}

		channelID := lnwire.NewChanIDFromOutPoint(&chanPoint)
		var linkActive bool
		if link, err := r.server.htlcSwitch.GetLink(channelID); err == nil {
			// A channel is only considered active if it is known
			// by the switch *and* able to forward
			// incoming/outgoing payments.
			linkActive = link.EligibleToForward()
		}

		// Next, we'll determine whether we should add this channel to
		// our list depending on the type of channels requested to us.
		isActive := peerOnline && linkActive
		channel, err := createRPCOpenChannel(r, graph, dbChannel, isActive)
		if err != nil {
			return nil, err
		}

		// We'll only skip returning this channel if we were requested
		// for a specific kind and this channel doesn't satisfy it.
		switch {
		case in.ActiveOnly && !isActive:
			continue
		case in.InactiveOnly && isActive:
			continue
		case in.PublicOnly && channel.Private:
			continue
		case in.PrivateOnly && !channel.Private:
			continue
		}

		resp.Channels = append(resp.Channels, channel)
	}

	return resp, nil
}

// createRPCOpenChannel creates an *lnrpc.Channel from the *channeldb.Channel.
func createRPCOpenChannel(r *rpcServer, graph *channeldb.ChannelGraph,
	dbChannel *channeldb.OpenChannel, isActive bool) (*lnrpc.Channel, error) {

	nodePub := dbChannel.IdentityPub
	nodeID := hex.EncodeToString(nodePub.SerializeCompressed())
	chanPoint := dbChannel.FundingOutpoint

	// Next, we'll determine whether the channel is public or not.
	isPublic := dbChannel.ChannelFlags&lnwire.FFAnnounceChannel != 0

	// As this is required for display purposes, we'll calculate
	// the size of the commitment transaction. We also add on the
	// estimated size of the witness to calculate the size of the
	// transaction if it were to be immediately unilaterally
	// broadcast.
	localCommit := dbChannel.LocalCommitment
	utx := localCommit.CommitTx
	commitBaseSize := int64(utx.SerializeSize())
	commitSize := commitBaseSize + 1 + input.FundingOutputSigScriptSize

	localBalance := localCommit.LocalBalance
	remoteBalance := localCommit.RemoteBalance

	// As an artifact of our usage of milli-atoms internally, either party
	// may end up in a state where they're holding a fractional
	// amount of atoms which can't be expressed within the
	// actual commitment output. Since we round down when going
	// from milli-atoms -> Atoms, we may at any point be adding an
	// additional Atoms to miners fees. As a result, we display a
	// commitment fee that accounts for this externally.
	var sumOutputs dcrutil.Amount
	for _, txOut := range localCommit.CommitTx.TxOut {
		sumOutputs += dcrutil.Amount(txOut.Value)
	}
	externalCommitFee := dbChannel.Capacity - sumOutputs

	channel := &lnrpc.Channel{
		Active:                 isActive,
		Private:                !isPublic,
		RemotePubkey:           nodeID,
		ChannelPoint:           chanPoint.String(),
		ChanId:                 dbChannel.ShortChannelID.ToUint64(),
		Capacity:               int64(dbChannel.Capacity),
		LocalBalance:           int64(localBalance.ToAtoms()),
		RemoteBalance:          int64(remoteBalance.ToAtoms()),
		CommitFee:              int64(externalCommitFee),
		CommitSize:             commitSize,
		FeePerKb:               int64(localCommit.FeePerKB),
		TotalAtomsSent:         int64(dbChannel.TotalMAtomsSent.ToAtoms()),
		TotalAtomsReceived:     int64(dbChannel.TotalMAtomsReceived.ToAtoms()),
		NumUpdates:             localCommit.CommitHeight,
		PendingHtlcs:           make([]*lnrpc.HTLC, len(localCommit.Htlcs)),
		CsvDelay:               uint32(dbChannel.LocalChanCfg.CsvDelay),
		Initiator:              dbChannel.IsInitiator,
		ChanStatusFlags:        dbChannel.ChanStatus().String(),
		LocalChanReserveAtoms:  int64(dbChannel.LocalChanCfg.ChanReserve),
		RemoteChanReserveAtoms: int64(dbChannel.RemoteChanCfg.ChanReserve),
		StaticRemoteKey:        dbChannel.ChanType.IsTweakless(),
	}

	for i, htlc := range localCommit.Htlcs {
		var rHash [32]byte
		copy(rHash[:], htlc.RHash[:])
		channel.PendingHtlcs[i] = &lnrpc.HTLC{
			Incoming:         htlc.Incoming,
			Amount:           int64(htlc.Amt.ToAtoms()),
			HashLock:         rHash[:],
			ExpirationHeight: htlc.RefundTimeout,
		}
		channel.UnsettledBalance += channel.PendingHtlcs[i].Amount
	}

	outpoint := dbChannel.FundingOutpoint

	// Get the lifespan observed by the channel event store. If the channel is
	// not known to the channel event store, return early because we cannot
	// calculate any further uptime information.
	startTime, endTime, err := r.server.chanEventStore.GetLifespan(outpoint)
	switch err {
	case chanfitness.ErrChannelNotFound:
		rpcsLog.Infof("channel: %v not found by channel event store",
			outpoint)

		return channel, nil
	case nil:
		// If there is no error getting lifespan, continue to uptime
		// calculation.
	default:
		return nil, err
	}

	// If endTime is zero, the channel is still open, progress endTime to
	// the present so we can calculate lifetime.
	if endTime.IsZero() {
		endTime = time.Now()
	}
	channel.Lifetime = int64(endTime.Sub(startTime).Seconds())

	// Once we have successfully obtained channel lifespan, we know that the
	// channel is known to the event store, so we can return any non-nil error
	// that occurs.
	uptime, err := r.server.chanEventStore.GetUptime(
		outpoint, startTime, endTime,
	)
	if err != nil {
		return nil, err
	}
	channel.Uptime = int64(uptime.Seconds())

	if len(dbChannel.LocalShutdownScript) > 0 {
		// TODO(decred): Store version along with LocalShutdownScript?
		scriptVersion := uint16(0)
		_, addresses, _, err := txscript.ExtractPkScriptAddrs(
			scriptVersion, dbChannel.LocalShutdownScript, activeNetParams.Params,
		)
		if err != nil {
			return nil, err
		}

		// We only expect one upfront shutdown address for a channel. If
		// LocalShutdownScript is non-zero, there should be one payout address
		// set.
		if len(addresses) != 1 {
			return nil, fmt.Errorf("expected one upfront shutdown address, "+
				"got: %v", len(addresses))
		}

		channel.CloseAddress = addresses[0].String()
	}

	return channel, nil
}

// createRPCClosedChannel creates an *lnrpc.ClosedChannelSummary from a
// *channeldb.ChannelCloseSummary.
func createRPCClosedChannel(
	dbChannel *channeldb.ChannelCloseSummary) *lnrpc.ChannelCloseSummary {

	nodePub := dbChannel.RemotePub
	nodeID := hex.EncodeToString(nodePub.SerializeCompressed())

	var closeType lnrpc.ChannelCloseSummary_ClosureType
	switch dbChannel.CloseType {
	case channeldb.CooperativeClose:
		closeType = lnrpc.ChannelCloseSummary_COOPERATIVE_CLOSE
	case channeldb.LocalForceClose:
		closeType = lnrpc.ChannelCloseSummary_LOCAL_FORCE_CLOSE
	case channeldb.RemoteForceClose:
		closeType = lnrpc.ChannelCloseSummary_REMOTE_FORCE_CLOSE
	case channeldb.BreachClose:
		closeType = lnrpc.ChannelCloseSummary_BREACH_CLOSE
	case channeldb.FundingCanceled:
		closeType = lnrpc.ChannelCloseSummary_FUNDING_CANCELED
	case channeldb.Abandoned:
		closeType = lnrpc.ChannelCloseSummary_ABANDONED
	}

	return &lnrpc.ChannelCloseSummary{
		Capacity:          int64(dbChannel.Capacity),
		RemotePubkey:      nodeID,
		CloseHeight:       dbChannel.CloseHeight,
		CloseType:         closeType,
		ChannelPoint:      dbChannel.ChanPoint.String(),
		ChanId:            dbChannel.ShortChanID.ToUint64(),
		SettledBalance:    int64(dbChannel.SettledBalance),
		TimeLockedBalance: int64(dbChannel.TimeLockedBalance),
		ChainHash:         dbChannel.ChainHash.String(),
		ClosingTxHash:     dbChannel.ClosingTXID.String(),
	}
}

// SubscribeChannelEvents returns a uni-directional stream (server -> client)
// for notifying the client of newly active, inactive or closed channels.
func (r *rpcServer) SubscribeChannelEvents(req *lnrpc.ChannelEventSubscription,
	updateStream lnrpc.Lightning_SubscribeChannelEventsServer) error {

	channelEventSub, err := r.server.channelNotifier.SubscribeChannelEvents()
	if err != nil {
		return err
	}

	// Ensure that the resources for the client is cleaned up once either
	// the server, or client exits.
	defer channelEventSub.Cancel()

	graph := r.server.chanDB.ChannelGraph()

	for {
		select {
		// A new update has been sent by the channel router, we'll
		// marshal it into the form expected by the gRPC client, then
		// send it off to the client(s).
		case e := <-channelEventSub.Updates():
			var update *lnrpc.ChannelEventUpdate
			switch event := e.(type) {
			case channelnotifier.OpenChannelEvent:
				channel, err := createRPCOpenChannel(r, graph,
					event.Channel, true)
				if err != nil {
					return err
				}

				update = &lnrpc.ChannelEventUpdate{
					Type: lnrpc.ChannelEventUpdate_OPEN_CHANNEL,
					Channel: &lnrpc.ChannelEventUpdate_OpenChannel{
						OpenChannel: channel,
					},
				}

			case channelnotifier.ClosedChannelEvent:
				closedChannel := createRPCClosedChannel(event.CloseSummary)
				update = &lnrpc.ChannelEventUpdate{
					Type: lnrpc.ChannelEventUpdate_CLOSED_CHANNEL,
					Channel: &lnrpc.ChannelEventUpdate_ClosedChannel{
						ClosedChannel: closedChannel,
					},
				}

			case channelnotifier.ActiveChannelEvent:
				update = &lnrpc.ChannelEventUpdate{
					Type: lnrpc.ChannelEventUpdate_ACTIVE_CHANNEL,
					Channel: &lnrpc.ChannelEventUpdate_ActiveChannel{
						ActiveChannel: &lnrpc.ChannelPoint{
							FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
								FundingTxidBytes: event.ChannelPoint.Hash[:],
							},
							OutputIndex: event.ChannelPoint.Index,
						},
					},
				}

			case channelnotifier.InactiveChannelEvent:
				update = &lnrpc.ChannelEventUpdate{
					Type: lnrpc.ChannelEventUpdate_INACTIVE_CHANNEL,
					Channel: &lnrpc.ChannelEventUpdate_InactiveChannel{
						InactiveChannel: &lnrpc.ChannelPoint{
							FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
								FundingTxidBytes: event.ChannelPoint.Hash[:],
							},
							OutputIndex: event.ChannelPoint.Index,
						},
					},
				}

			default:
				return fmt.Errorf("unexpected channel event update: %v", event)
			}

			if err := updateStream.Send(update); err != nil {
				return err
			}
		case <-r.quit:
			return nil
		}
	}
}

// paymentStream enables different types of payment streams, such as:
// lnrpc.Lightning_SendPaymentServer and lnrpc.Lightning_SendToRouteServer to
// execute sendPayment. We use this struct as a sort of bridge to enable code
// re-use between SendPayment and SendToRoute.
type paymentStream struct {
	recv func() (*rpcPaymentRequest, error)
	send func(*lnrpc.SendResponse) error
}

// rpcPaymentRequest wraps lnrpc.SendRequest so that routes from
// lnrpc.SendToRouteRequest can be passed to sendPayment.
type rpcPaymentRequest struct {
	*lnrpc.SendRequest
	route *route.Route
}

// SendPayment dispatches a bi-directional streaming RPC for sending payments
// through the Lightning Network. A single RPC invocation creates a persistent
// bi-directional stream allowing clients to rapidly send payments through the
// Lightning Network with a single persistent connection.
func (r *rpcServer) SendPayment(stream lnrpc.Lightning_SendPaymentServer) error {
	var lock sync.Mutex

	return r.sendPayment(&paymentStream{
		recv: func() (*rpcPaymentRequest, error) {
			req, err := stream.Recv()
			if err != nil {
				return nil, err
			}

			return &rpcPaymentRequest{
				SendRequest: req,
			}, nil
		},
		send: func(r *lnrpc.SendResponse) error {
			// Calling stream.Send concurrently is not safe.
			lock.Lock()
			defer lock.Unlock()
			return stream.Send(r)
		},
	})
}

// SendToRoute dispatches a bi-directional streaming RPC for sending payments
// through the Lightning Network via predefined routes passed in. A single RPC
// invocation creates a persistent bi-directional stream allowing clients to
// rapidly send payments through the Lightning Network with a single persistent
// connection.
func (r *rpcServer) SendToRoute(stream lnrpc.Lightning_SendToRouteServer) error {
	var lock sync.Mutex

	return r.sendPayment(&paymentStream{
		recv: func() (*rpcPaymentRequest, error) {
			req, err := stream.Recv()
			if err != nil {
				return nil, err
			}

			return r.unmarshallSendToRouteRequest(req)
		},
		send: func(r *lnrpc.SendResponse) error {
			// Calling stream.Send concurrently is not safe.
			lock.Lock()
			defer lock.Unlock()
			return stream.Send(r)
		},
	})
}

// unmarshallSendToRouteRequest unmarshalls an rpc sendtoroute request
func (r *rpcServer) unmarshallSendToRouteRequest(
	req *lnrpc.SendToRouteRequest) (*rpcPaymentRequest, error) {

	if req.Route == nil {
		return nil, fmt.Errorf("unable to send, no route provided")
	}

	route, err := r.routerBackend.UnmarshallRoute(req.Route)
	if err != nil {
		return nil, err
	}

	return &rpcPaymentRequest{
		SendRequest: &lnrpc.SendRequest{
			PaymentHash:       req.PaymentHash,
			PaymentHashString: req.PaymentHashString,
		},
		route: route,
	}, nil
}

// rpcPaymentIntent is a small wrapper struct around the of values we can
// receive from a client over RPC if they wish to send a payment. We'll either
// extract these fields from a payment request (which may include routing
// hints), or we'll get a fully populated route from the user that we'll pass
// directly to the channel router for dispatching.
type rpcPaymentIntent struct {
	mat                  lnwire.MilliAtom
	feeLimit             lnwire.MilliAtom
	cltvLimit            uint32
	dest                 route.Vertex
	rHash                [32]byte
	cltvDelta            uint16
	routeHints           [][]zpay32.HopHint
	outgoingChannelID    *uint64
	lastHop              *route.Vertex
	ignoreMaxOutboundAmt bool
	destFeatures         *lnwire.FeatureVector
	paymentAddr          *[32]byte
	payReq               []byte

	destCustomRecords record.CustomSet

	route *route.Route
}

// extractPaymentIntent attempts to parse the complete details required to
// dispatch a client from the information presented by an RPC client. There are
// three ways a client can specify their payment details: a payment request,
// via manual details, or via a complete route.
func (r *rpcServer) extractPaymentIntent(rpcPayReq *rpcPaymentRequest) (rpcPaymentIntent, error) {
	payIntent := rpcPaymentIntent{
		ignoreMaxOutboundAmt: rpcPayReq.IgnoreMaxOutboundAmt,
	}

	// If a route was specified, then we can use that directly.
	if rpcPayReq.route != nil {
		// If the user is using the REST interface, then they'll be
		// passing the payment hash as a hex encoded string.
		if rpcPayReq.PaymentHashString != "" {
			paymentHash, err := hex.DecodeString(
				rpcPayReq.PaymentHashString,
			)
			if err != nil {
				return payIntent, err
			}

			copy(payIntent.rHash[:], paymentHash)
		} else {
			copy(payIntent.rHash[:], rpcPayReq.PaymentHash)
		}

		payIntent.route = rpcPayReq.route
		return payIntent, nil
	}

	// If there are no routes specified, pass along a outgoing channel
	// restriction if specified.
	if rpcPayReq.OutgoingChanId != 0 {
		payIntent.outgoingChannelID = &rpcPayReq.OutgoingChanId
	}

	// Pass along a last hop restriction if specified.
	if len(rpcPayReq.LastHopPubkey) > 0 {
		lastHop, err := route.NewVertexFromBytes(
			rpcPayReq.LastHopPubkey,
		)
		if err != nil {
			return payIntent, err
		}
		payIntent.lastHop = &lastHop
	}

	// Take the CLTV limit from the request if set, otherwise use the max.
	cltvLimit, err := routerrpc.ValidateCLTVLimit(
		rpcPayReq.CltvLimit, cfg.MaxOutgoingCltvExpiry,
	)
	if err != nil {
		return payIntent, err
	}
	payIntent.cltvLimit = cltvLimit

	customRecords := record.CustomSet(rpcPayReq.DestCustomRecords)
	if err := customRecords.Validate(); err != nil {
		return payIntent, err
	}
	payIntent.destCustomRecords = customRecords

	validateDest := func(dest route.Vertex) error {
		if rpcPayReq.AllowSelfPayment {
			return nil
		}

		if dest == r.selfNode {
			return errors.New("self-payments not allowed")
		}

		return nil
	}

	// If the payment request field isn't blank, then the details of the
	// invoice are encoded entirely within the encoded payReq.  So we'll
	// attempt to decode it, populating the payment accordingly.
	if rpcPayReq.PaymentRequest != "" {
		payReq, err := zpay32.Decode(
			rpcPayReq.PaymentRequest, activeNetParams.Params,
		)
		if err != nil {
			return payIntent, err
		}

		// Copy the decoded payment hash so that callers can identify
		// the original payreq in case of errors.
		copy(payIntent.rHash[:], payReq.PaymentHash[:])

		// Next, we'll ensure that this payreq hasn't already expired.
		err = routerrpc.ValidatePayReqExpiry(payReq)
		if err != nil {
			return payIntent, err
		}

		// If the amount was not included in the invoice, then we let
		// the payee specify the amount of atoms they wish to send.
		// We override the amount to pay with the amount provided from
		// the payment request.
		if payReq.MilliAt == nil {
			amt, err := lnrpc.UnmarshallAmt(
				rpcPayReq.Amt, rpcPayReq.AmtMAtoms,
			)
			if err != nil {
				return payIntent, err
			}
			if amt == 0 {
				return payIntent, errors.New("amount must be " +
					"specified when paying a zero amount " +
					"invoice")
			}

			payIntent.mat = amt
		} else {
			payIntent.mat = *payReq.MilliAt
		}

		// Calculate the fee limit that should be used for this payment.
		payIntent.feeLimit = lnrpc.CalculateFeeLimit(
			rpcPayReq.FeeLimit, payIntent.mat,
		)

		copy(payIntent.rHash[:], payReq.PaymentHash[:])
		destKey := payReq.Destination.SerializeCompressed()
		copy(payIntent.dest[:], destKey)
		payIntent.cltvDelta = uint16(payReq.MinFinalCLTVExpiry())
		payIntent.routeHints = payReq.RouteHints
		payIntent.payReq = []byte(rpcPayReq.PaymentRequest)
		payIntent.destFeatures = payReq.Features
		payIntent.paymentAddr = payReq.PaymentAddr

		if err := validateDest(payIntent.dest); err != nil {
			return payIntent, err
		}

		return payIntent, nil
	}

	// At this point, a destination MUST be specified, so we'll convert it
	// into the proper representation now. The destination will either be
	// encoded as raw bytes, or via a hex string.
	var pubBytes []byte
	if len(rpcPayReq.Dest) != 0 {
		pubBytes = rpcPayReq.Dest
	} else {
		var err error
		pubBytes, err = hex.DecodeString(rpcPayReq.DestString)
		if err != nil {
			return payIntent, err
		}
	}
	if len(pubBytes) != 33 {
		return payIntent, errors.New("invalid key length")
	}
	copy(payIntent.dest[:], pubBytes)

	if err := validateDest(payIntent.dest); err != nil {
		return payIntent, err
	}

	// Otherwise, If the payment request field was not specified
	// (and a custom route wasn't specified), construct the payment
	// from the other fields.
	payIntent.mat, err = lnrpc.UnmarshallAmt(
		rpcPayReq.Amt, rpcPayReq.AmtMAtoms,
	)
	if err != nil {
		return payIntent, err
	}

	// Calculate the fee limit that should be used for this payment.
	payIntent.feeLimit = lnrpc.CalculateFeeLimit(
		rpcPayReq.FeeLimit, payIntent.mat,
	)

	if rpcPayReq.FinalCltvDelta != 0 {
		payIntent.cltvDelta = uint16(rpcPayReq.FinalCltvDelta)
	} else {
		payIntent.cltvDelta = zpay32.DefaultFinalCLTVDelta
	}

	// If the user is manually specifying payment details, then the payment
	// hash may be encoded as a string.
	switch {
	case rpcPayReq.PaymentHashString != "":
		paymentHash, err := hex.DecodeString(
			rpcPayReq.PaymentHashString,
		)
		if err != nil {
			return payIntent, err
		}

		copy(payIntent.rHash[:], paymentHash)

	default:
		copy(payIntent.rHash[:], rpcPayReq.PaymentHash)
	}

	// Unmarshal any custom destination features.
	payIntent.destFeatures, err = routerrpc.UnmarshalFeatures(
		rpcPayReq.DestFeatures,
	)
	if err != nil {
		return payIntent, err
	}

	// Currently, within the bootstrap phase of the network, we limit the
	// largest payment size allotted to (2^32) - 1 milli-atoms or 4.29
	// million atoms.
	if payIntent.mat > MaxPaymentMAtoms {
		// In this case, we'll send an error to the caller, but
		// continue our loop for the next payment.
		return payIntent, fmt.Errorf("payment of %v is too large, "+
			"max payment allowed is %v", payIntent.mat,
			MaxPaymentMAtoms)

	}

	return payIntent, nil
}

type paymentIntentResponse struct {
	Route    *route.Route
	Preimage [32]byte
	Err      error
}

// checkCanSendPayment verifies whether the minimum conditions for sending the
// given payment from this node are met, such as having an open channel with a
// live peer with enough outbound bandwidth for sending it.
func (r *rpcServer) checkCanSendPayment(payIntent *rpcPaymentIntent) error {
	// Return early if we've been instructed to ignore the available
	// inbound bandwidth.
	if payIntent.ignoreMaxOutboundAmt {
		return nil
	}

	// Verify whether there is at least one channel with enough outbound
	// capacity (after accounting for channel reserves) to receive the
	// payment from this invoice.
	openChannels, err := r.server.chanDB.FetchAllOpenChannels()
	if err != nil {
		return err
	}

	// If the node has no open channels, it can't possibly send payment for
	// this.
	if len(openChannels) == 0 {
		return errors.New("no open channels")
	}

	// Determine how much we're likely to pay as tx fee for adding a new
	// htlc. We use the minimum relay fee since this is just a quick
	// estimate on whether we'll be able to fulfill the payment.
	relayFee := r.server.cc.feeEstimator.RelayFeePerKB()
	htlcFee := relayFee.FeeForSize(input.HTLCOutputSize)

	// Convert the payment amount to atoms, since we can't have an open
	// channel with less than 1 atom and milliatom payments might not alter
	// the channel balances.
	amt := payIntent.mat.ToAtoms() + htlcFee
	graph := r.server.chanDB.ChannelGraph()

	// Loop through all available channels, check for liveliness and
	// capacity.
	var maxChanCap dcrutil.Amount
	var maxChanID uint64
	for _, channel := range openChannels {
		// Ensure the channel is active and the remote peer is online,
		// which is required to send to this channel.
		chanPoint := &channel.FundingOutpoint
		if _, err := r.server.FindPeer(channel.IdentityPub); err != nil {
			// We're not connected to the peer, therefore can't
			// send htlcs to it.
			continue
		}

		// Try to retrieve a the link from the htlc switch to verify we
		// can currently use this channel for routing.
		channelID := lnwire.NewChanIDFromOutPoint(chanPoint)
		var link htlcswitch.ChannelLink
		if link, err = r.server.htlcSwitch.GetLink(channelID); err != nil {
			continue
		}

		// If this link isn' eligible for htcl forwarding, it means we
		// can't send to it.
		if !link.EligibleToForward() {
			continue
		}

		// We have now verified the channel is online and can route
		// htlcs through it. Verifiy if it has enough outbound capacity
		// for this new invoice.
		//
		// Outbound capacity for a channel is how much the local node
		// currently has minus what the remote node requires us to
		// maintain at all times (chan_reserve).
		capacity := channel.LocalCommitment.LocalBalance.ToAtoms() -
			channel.LocalChanCfg.ChannelConstraints.ChanReserve

		if capacity >= amt {
			// Found an online channel with enough capacity. Signal
			// success.
			return nil
		}

		// Not yet enough capacity. Store the largest channel to
		// present a better error msg.
		if capacity > maxChanCap {
			maxChanCap = capacity
			maxChanID, _ = graph.ChannelID(chanPoint)
		}
	}

	if maxChanID == 0 {
		return errors.New("no online channels found")
	}

	missingCap := amt - maxChanCap
	return fmt.Errorf("not enough outbound capacity (missing %d atoms "+
		"in channel %d)", missingCap, maxChanID)
}

// dispatchPaymentIntent attempts to fully dispatch an RPC payment intent.
// We'll either pass the payment as a whole to the channel router, or give it a
// pre-built route. The first error this method returns denotes if we were
// unable to save the payment. The second error returned denotes if the payment
// didn't succeed.
func (r *rpcServer) dispatchPaymentIntent(
	payIntent *rpcPaymentIntent) (*paymentIntentResponse, error) {

	// Perform a pre-flight check for sending this payment.
	if err := r.checkCanSendPayment(payIntent); err != nil {
		return &paymentIntentResponse{
			Err: err,
		}, nil
	}

	// Construct a payment request to send to the channel router. If the
	// payment is successful, the route chosen will be returned. Otherwise,
	// we'll get a non-nil error.
	var (
		preImage  [32]byte
		route     *route.Route
		routerErr error
	)

	// If a route was specified, then we'll pass the route directly to the
	// router, otherwise we'll create a payment session to execute it.
	if payIntent.route == nil {
		payment := &routing.LightningPayment{
			Target:            payIntent.dest,
			Amount:            payIntent.mat,
			FinalCLTVDelta:    payIntent.cltvDelta,
			FeeLimit:          payIntent.feeLimit,
			CltvLimit:         payIntent.cltvLimit,
			PaymentHash:       payIntent.rHash,
			RouteHints:        payIntent.routeHints,
			OutgoingChannelID: payIntent.outgoingChannelID,
			LastHop:           payIntent.lastHop,
			PaymentRequest:    payIntent.payReq,
			PayAttemptTimeout: routing.DefaultPayAttemptTimeout,
			DestCustomRecords: payIntent.destCustomRecords,
			DestFeatures:      payIntent.destFeatures,
			PaymentAddr:       payIntent.paymentAddr,
		}

		preImage, route, routerErr = r.server.chanRouter.SendPayment(
			payment,
		)
	} else {
		preImage, routerErr = r.server.chanRouter.SendToRoute(
			payIntent.rHash, payIntent.route,
		)

		route = payIntent.route
	}

	// If the route failed, then we'll return a nil save err, but a non-nil
	// routing err.
	if routerErr != nil {
		rpcsLog.Warnf("Unable to send payment: %v", routerErr)

		return &paymentIntentResponse{
			Err: routerErr,
		}, nil
	}

	return &paymentIntentResponse{
		Route:    route,
		Preimage: preImage,
	}, nil
}

// sendPayment takes a paymentStream (a source of pre-built routes or payment
// requests) and continually attempt to dispatch payment requests written to
// the write end of the stream. Responses will also be streamed back to the
// client via the write end of the stream. This method is by both SendToRoute
// and SendPayment as the logic is virtually identical.
func (r *rpcServer) sendPayment(stream *paymentStream) error {
	payChan := make(chan *rpcPaymentIntent)
	errChan := make(chan error, 1)

	// We don't allow payments to be sent while the daemon itself is still
	// syncing as we may be trying to sent a payment over a "stale"
	// channel.
	if !r.server.Started() {
		return ErrServerNotActive
	}

	// TODO(roasbeef): check payment filter to see if already used?

	// In order to limit the level of concurrency and prevent a client from
	// attempting to OOM the server, we'll set up a semaphore to create an
	// upper ceiling on the number of outstanding payments.
	const numOutstandingPayments = 2000
	htlcSema := make(chan struct{}, numOutstandingPayments)
	for i := 0; i < numOutstandingPayments; i++ {
		htlcSema <- struct{}{}
	}

	// Launch a new goroutine to handle reading new payment requests from
	// the client. This way we can handle errors independently of blocking
	// and waiting for the next payment request to come through.
	reqQuit := make(chan struct{})
	defer func() {
		close(reqQuit)
	}()

	// TODO(joostjager): Callers expect result to come in in the same order
	// as the request were sent, but this is far from guarantueed in the
	// code below.
	go func() {
		for {
			select {
			case <-reqQuit:
				return
			case <-r.quit:
				errChan <- nil
				return
			default:
				// Receive the next pending payment within the
				// stream sent by the client. If we read the
				// EOF sentinel, then the client has closed the
				// stream, and we can exit normally.
				nextPayment, err := stream.recv()
				if err == io.EOF {
					errChan <- nil
					return
				} else if err != nil {
					select {
					case errChan <- err:
					case <-reqQuit:
						return
					}
					return
				}

				// Populate the next payment, either from the
				// payment request, or from the explicitly set
				// fields. If the payment proto wasn't well
				// formed, then we'll send an error reply and
				// wait for the next payment.
				payIntent, err := r.extractPaymentIntent(
					nextPayment,
				)
				if err != nil {
					if err := stream.send(&lnrpc.SendResponse{
						PaymentError: err.Error(),
						PaymentHash:  payIntent.rHash[:],
					}); err != nil {
						select {
						case errChan <- err:
						case <-reqQuit:
							return
						}
					}
					continue
				}

				// If the payment was well formed, then we'll
				// send to the dispatch goroutine, or exit,
				// which ever comes first
				select {
				case payChan <- &payIntent:
				case <-reqQuit:
					return
				}
			}
		}
	}()

	for {
		select {
		case err := <-errChan:
			return err

		case payIntent := <-payChan:
			// We launch a new goroutine to execute the current
			// payment so we can continue to serve requests while
			// this payment is being dispatched.
			go func() {
				// Attempt to grab a free semaphore slot, using
				// a defer to eventually release the slot
				// regardless of payment success.
				<-htlcSema
				defer func() {
					htlcSema <- struct{}{}
				}()

				resp, saveErr := r.dispatchPaymentIntent(
					payIntent,
				)

				switch {
				// If we were unable to save the state of the
				// payment, then we'll return the error to the
				// user, and terminate.
				case saveErr != nil:
					errChan <- saveErr
					return

				// If we receive payment error than, instead of
				// terminating the stream, send error response
				// to the user.
				case resp.Err != nil:
					err := stream.send(&lnrpc.SendResponse{
						PaymentError: resp.Err.Error(),
						PaymentHash:  payIntent.rHash[:],
					})
					if err != nil {
						errChan <- err
					}
					return
				}

				backend := r.routerBackend
				marshalledRouted, err := backend.MarshallRoute(
					resp.Route,
				)
				if err != nil {
					errChan <- err
					return
				}

				err = stream.send(&lnrpc.SendResponse{
					PaymentHash:     payIntent.rHash[:],
					PaymentPreimage: resp.Preimage[:],
					PaymentRoute:    marshalledRouted,
				})
				if err != nil {
					errChan <- err
					return
				}
			}()
		}
	}
}

// SendPaymentSync is the synchronous non-streaming version of SendPayment.
// This RPC is intended to be consumed by clients of the REST proxy.
// Additionally, this RPC expects the destination's public key and the payment
// hash (if any) to be encoded as hex strings.
func (r *rpcServer) SendPaymentSync(ctx context.Context,
	nextPayment *lnrpc.SendRequest) (*lnrpc.SendResponse, error) {

	return r.sendPaymentSync(ctx, &rpcPaymentRequest{
		SendRequest: nextPayment,
	})
}

// SendToRouteSync is the synchronous non-streaming version of SendToRoute.
// This RPC is intended to be consumed by clients of the REST proxy.
// Additionally, this RPC expects the payment hash (if any) to be encoded as
// hex strings.
func (r *rpcServer) SendToRouteSync(ctx context.Context,
	req *lnrpc.SendToRouteRequest) (*lnrpc.SendResponse, error) {

	if req.Route == nil {
		return nil, fmt.Errorf("unable to send, no routes provided")
	}

	paymentRequest, err := r.unmarshallSendToRouteRequest(req)
	if err != nil {
		return nil, err
	}

	return r.sendPaymentSync(ctx, paymentRequest)
}

// sendPaymentSync is the synchronous variant of sendPayment. It will block and
// wait until the payment has been fully completed.
func (r *rpcServer) sendPaymentSync(ctx context.Context,
	nextPayment *rpcPaymentRequest) (*lnrpc.SendResponse, error) {

	// We don't allow payments to be sent while the daemon itself is still
	// syncing as we may be trying to sent a payment over a "stale"
	// channel.
	if !r.server.Started() {
		return nil, ErrServerNotActive
	}

	// First we'll attempt to map the proto describing the next payment to
	// an intent that we can pass to local sub-systems.
	payIntent, err := r.extractPaymentIntent(nextPayment)
	if err != nil {
		return nil, err
	}

	// With the payment validated, we'll now attempt to dispatch the
	// payment.
	resp, saveErr := r.dispatchPaymentIntent(&payIntent)
	switch {
	case saveErr != nil:
		return nil, saveErr

	case resp.Err != nil:
		return &lnrpc.SendResponse{
			PaymentError: resp.Err.Error(),
			PaymentHash:  payIntent.rHash[:],
		}, nil
	}

	rpcRoute, err := r.routerBackend.MarshallRoute(resp.Route)
	if err != nil {
		return nil, err
	}

	return &lnrpc.SendResponse{
		PaymentHash:     payIntent.rHash[:],
		PaymentPreimage: resp.Preimage[:],
		PaymentRoute:    rpcRoute,
	}, nil
}

// checkCanReceiveInvoice performs a check on available inbound capacity from
// directly connected channels to ensure the passed invoice can be settled.
//
// It returns nil if there is enough capacity to potentially settle the invoice
// or an error otherwise.
func (r *rpcServer) checkCanReceiveInvoice(ctx context.Context,
	invoice *lnrpc.Invoice) error {

	// Return early if we've been instructed to ignore the available inbound
	// bandwidth.
	if invoice.IgnoreMaxInboundAmt {
		return nil
	}

	// Verify whether there is at least one channel with enough inbound
	// capacity (after accounting for channel reserves) to receive the payment
	// from this invoice.
	openChannels, err := r.server.chanDB.FetchAllOpenChannels()
	if err != nil {
		return err
	}

	// If the node has no open channels, it can't possibly receive payment for
	// this.
	if len(openChannels) == 0 {
		return errors.New("no open channels")
	}

	amt := dcrutil.Amount(invoice.Value)
	graph := r.server.chanDB.ChannelGraph()

	// Loop through all available channels, check for liveliness and capacity.
	var maxChanCap dcrutil.Amount
	var maxChanID uint64
	for _, channel := range openChannels {
		// Ensure the channel is active and the remote peer is online, which is
		// required to receive from this channel.
		chanPoint := &channel.FundingOutpoint
		if _, err := r.server.FindPeer(channel.IdentityPub); err != nil {
			// We're not connected to the peer, therefore can't receive htlcs
			// from it.
			continue
		}

		// Try to retrieve a the link from the htlc switch to verify we can
		// currently use this channel for routing.
		channelID := lnwire.NewChanIDFromOutPoint(chanPoint)
		var link htlcswitch.ChannelLink
		if link, err = r.server.htlcSwitch.GetLink(channelID); err != nil {
			continue
		}

		// If this link isn' eligible for htcl forwarding, it means we can't
		// receive from it.
		if !link.EligibleToForward() {
			continue
		}

		// We have now verified the channel is online and can route htlcs
		// through it. Verifiy if it has enough inbound capacity for this new
		// invoice.
		//
		// Inbound capacity for a channel is how much the remote node currently
		// has (the remote_balance from our pov) minus what we require the
		// remote node to maintain at all times (chan_reserve).
		capacity := channel.RemoteCommitment.RemoteBalance.ToAtoms() -
			channel.RemoteChanCfg.ChannelConstraints.ChanReserve

		if capacity >= amt {
			// Found an online channel with enough capacity. Signal success.
			return nil
		}

		// Not yet enough capacity. Store the largest channel to present a
		// better error msg.
		if capacity > maxChanCap {
			maxChanCap = capacity
			maxChanID, _ = graph.ChannelID(chanPoint)
		}
	}

	if maxChanID == 0 {
		return errors.New("no online channels found")
	}

	missingCap := amt - maxChanCap
	return fmt.Errorf("not enough inbound capacity (missing %d atoms "+
		"in channel %d)", missingCap, maxChanID)
}

// AddInvoice attempts to add a new invoice to the invoice database. Any
// duplicated invoices are rejected, therefore all invoices *must* have a
// unique payment preimage.
func (r *rpcServer) AddInvoice(ctx context.Context,
	invoice *lnrpc.Invoice) (*lnrpc.AddInvoiceResponse, error) {

	if err := r.checkCanReceiveInvoice(ctx, invoice); err != nil {
		return nil, err
	}

	defaultDelta := cfg.TimeLockDelta

	addInvoiceCfg := &invoicesrpc.AddInvoiceConfig{
		AddInvoice:        r.server.invoices.AddInvoice,
		IsChannelActive:   r.server.htlcSwitch.HasActiveLink,
		ChainParams:       activeNetParams.Params,
		NodeSigner:        r.server.nodeSigner,
		MaxPaymentMAtoms:  MaxPaymentMAtoms,
		DefaultCLTVExpiry: defaultDelta,
		ChanDB:            r.server.chanDB,
		GenInvoiceFeatures: func() *lnwire.FeatureVector {
			return r.server.featureMgr.Get(feature.SetInvoice)
		},
	}

	value, err := lnrpc.UnmarshallAmt(invoice.Value, invoice.ValueMAtoms)
	if err != nil {
		return nil, err
	}

	addInvoiceData := &invoicesrpc.AddInvoiceData{
		Memo:            invoice.Memo,
		Value:           value,
		DescriptionHash: invoice.DescriptionHash,
		Expiry:          invoice.Expiry,
		FallbackAddr:    invoice.FallbackAddr,
		CltvExpiry:      invoice.CltvExpiry,
		Private:         invoice.Private,
	}

	if invoice.RPreimage != nil {
		preimage, err := lntypes.MakePreimage(invoice.RPreimage)
		if err != nil {
			return nil, err
		}
		addInvoiceData.Preimage = &preimage
	}

	hash, dbInvoice, err := invoicesrpc.AddInvoice(
		ctx, addInvoiceCfg, addInvoiceData,
	)
	if err != nil {
		return nil, err
	}

	return &lnrpc.AddInvoiceResponse{
		AddIndex:       dbInvoice.AddIndex,
		PaymentRequest: string(dbInvoice.PaymentRequest),
		RHash:          hash[:],
	}, nil
}

// LookupInvoice attempts to look up an invoice according to its payment hash.
// The passed payment hash *must* be exactly 32 bytes, if not an error is
// returned.
func (r *rpcServer) LookupInvoice(ctx context.Context,
	req *lnrpc.PaymentHash) (*lnrpc.Invoice, error) {

	var (
		payHash [32]byte
		rHash   []byte
		err     error
	)

	// If the RHash as a raw string was provided, then decode that and use
	// that directly. Otherwise, we use the raw bytes provided.
	if req.RHashStr != "" {
		rHash, err = hex.DecodeString(req.RHashStr)
		if err != nil {
			return nil, err
		}
	} else {
		rHash = req.RHash
	}

	// Ensure that the payment hash is *exactly* 32-bytes.
	if len(rHash) != 0 && len(rHash) != 32 {
		return nil, fmt.Errorf("payment hash must be exactly "+
			"32 bytes, is instead %v", len(rHash))
	}
	copy(payHash[:], rHash)

	rpcsLog.Tracef("[lookupinvoice] searching for invoice %x", payHash[:])

	invoice, err := r.server.invoices.LookupInvoice(payHash)
	if err != nil {
		return nil, err
	}

	rpcsLog.Tracef("[lookupinvoice] located invoice %v",
		newLogClosure(func() string {
			return spew.Sdump(invoice)
		}))

	rpcInvoice, err := invoicesrpc.CreateRPCInvoice(
		&invoice, activeNetParams.Params,
	)
	if err != nil {
		return nil, err
	}

	return rpcInvoice, nil
}

// ListInvoices returns a list of all the invoices currently stored within the
// database. Any active debug invoices are ignored.
func (r *rpcServer) ListInvoices(ctx context.Context,
	req *lnrpc.ListInvoiceRequest) (*lnrpc.ListInvoiceResponse, error) {

	// If the number of invoices was not specified, then we'll default to
	// returning the latest 100 invoices.
	if req.NumMaxInvoices == 0 {
		req.NumMaxInvoices = 100
	}

	// Next, we'll map the proto request into a format that is understood by
	// the database.
	q := channeldb.InvoiceQuery{
		IndexOffset:    req.IndexOffset,
		NumMaxInvoices: req.NumMaxInvoices,
		PendingOnly:    req.PendingOnly,
		Reversed:       req.Reversed,
	}
	invoiceSlice, err := r.server.chanDB.QueryInvoices(q)
	if err != nil {
		return nil, fmt.Errorf("unable to query invoices: %v", err)
	}

	// Before returning the response, we'll need to convert each invoice
	// into it's proto representation.
	resp := &lnrpc.ListInvoiceResponse{
		Invoices:         make([]*lnrpc.Invoice, len(invoiceSlice.Invoices)),
		FirstIndexOffset: invoiceSlice.FirstIndexOffset,
		LastIndexOffset:  invoiceSlice.LastIndexOffset,
	}
	for i, invoice := range invoiceSlice.Invoices {
		resp.Invoices[i], err = invoicesrpc.CreateRPCInvoice(
			&invoice, activeNetParams.Params,
		)
		if err != nil {
			// Instead of failing and returning an error, encode
			// the error message into the payment request field
			// (along with the original payment request stored in
			// the source db invoice) so that we can keep listing
			// the rest of the invoices even if a single invoice
			// was encoded in an otherwise invalid state.
			resp.Invoices[i] = &lnrpc.Invoice{
				PaymentRequest: fmt.Sprintf("[ERROR] %s (%s)",
					err.Error(), invoice.PaymentRequest),
			}
		}
	}

	return resp, nil
}

// SubscribeInvoices returns a uni-directional stream (server -> client) for
// notifying the client of newly added/settled invoices.
func (r *rpcServer) SubscribeInvoices(req *lnrpc.InvoiceSubscription,
	updateStream lnrpc.Lightning_SubscribeInvoicesServer) error {

	invoiceClient := r.server.invoices.SubscribeNotifications(
		req.AddIndex, req.SettleIndex,
	)
	defer invoiceClient.Cancel()

	for {
		select {
		case newInvoice := <-invoiceClient.NewInvoices:
			rpcInvoice, err := invoicesrpc.CreateRPCInvoice(
				newInvoice, activeNetParams.Params,
			)
			if err != nil {
				return err
			}

			if err := updateStream.Send(rpcInvoice); err != nil {
				return err
			}

		case settledInvoice := <-invoiceClient.SettledInvoices:
			rpcInvoice, err := invoicesrpc.CreateRPCInvoice(
				settledInvoice, activeNetParams.Params,
			)
			if err != nil {
				return err
			}

			if err := updateStream.Send(rpcInvoice); err != nil {
				return err
			}

		case <-r.quit:
			return nil
		}
	}
}

// SubscribeTransactions creates a uni-directional stream (server -> client) in
// which any newly discovered transactions relevant to the wallet are sent
// over.
func (r *rpcServer) SubscribeTransactions(req *lnrpc.GetTransactionsRequest,
	updateStream lnrpc.Lightning_SubscribeTransactionsServer) error {

	txClient, err := r.server.cc.wallet.SubscribeTransactions()
	if err != nil {
		return err
	}
	defer txClient.Cancel()

	for {
		select {
		case tx := <-txClient.ConfirmedTransactions():
			destAddresses := make([]string, 0, len(tx.DestAddresses))
			for _, destAddress := range tx.DestAddresses {
				destAddresses = append(destAddresses, destAddress.Address())
			}
			detail := &lnrpc.Transaction{
				TxHash:           tx.Hash.String(),
				Amount:           int64(tx.Value),
				NumConfirmations: tx.NumConfirmations,
				BlockHash:        tx.BlockHash.String(),
				BlockHeight:      tx.BlockHeight,
				TimeStamp:        tx.Timestamp,
				TotalFees:        tx.TotalFees,
				DestAddresses:    destAddresses,
				RawTxHex:         hex.EncodeToString(tx.RawTx),
			}
			if err := updateStream.Send(detail); err != nil {
				return err
			}

		case tx := <-txClient.UnconfirmedTransactions():
			var destAddresses []string
			for _, destAddress := range tx.DestAddresses {
				destAddresses = append(destAddresses, destAddress.Address())
			}
			detail := &lnrpc.Transaction{
				TxHash:        tx.Hash.String(),
				Amount:        int64(tx.Value),
				TimeStamp:     tx.Timestamp,
				TotalFees:     tx.TotalFees,
				DestAddresses: destAddresses,
				RawTxHex:      hex.EncodeToString(tx.RawTx),
			}
			if err := updateStream.Send(detail); err != nil {
				return err
			}

		case <-r.quit:
			return nil
		}
	}
}

// GetTransactions returns a list of describing all the known transactions
// relevant to the wallet.
func (r *rpcServer) GetTransactions(ctx context.Context,
	_ *lnrpc.GetTransactionsRequest) (*lnrpc.TransactionDetails, error) {

	// TODO(roasbeef): add pagination support
	transactions, err := r.server.cc.wallet.ListTransactionDetails()
	if err != nil {
		return nil, err
	}

	txDetails := &lnrpc.TransactionDetails{
		Transactions: make([]*lnrpc.Transaction, len(transactions)),
	}
	for i, tx := range transactions {
		var destAddresses []string
		for _, destAddress := range tx.DestAddresses {
			destAddresses = append(destAddresses, destAddress.Address())
		}

		// We also get unconfirmed transactions, so BlockHash can be
		// nil.
		blockHash := ""
		if tx.BlockHash != nil {
			blockHash = tx.BlockHash.String()
		}

		txDetails.Transactions[i] = &lnrpc.Transaction{
			TxHash:           tx.Hash.String(),
			Amount:           int64(tx.Value),
			NumConfirmations: tx.NumConfirmations,
			BlockHash:        blockHash,
			BlockHeight:      tx.BlockHeight,
			TimeStamp:        tx.Timestamp,
			TotalFees:        tx.TotalFees,
			DestAddresses:    destAddresses,
			RawTxHex:         hex.EncodeToString(tx.RawTx),
		}
	}

	return txDetails, nil
}

// DescribeGraph returns a description of the latest graph state from the PoV
// of the node. The graph information is partitioned into two components: all
// the nodes/vertexes, and all the edges that connect the vertexes themselves.
// As this is a directed graph, the edges also contain the node directional
// specific routing policy which includes: the time lock delta, fee
// information, etc.
func (r *rpcServer) DescribeGraph(ctx context.Context,
	req *lnrpc.ChannelGraphRequest) (*lnrpc.ChannelGraph, error) {

	resp := &lnrpc.ChannelGraph{}
	includeUnannounced := req.IncludeUnannounced

	// Obtain the pointer to the global singleton channel graph, this will
	// provide a consistent view of the graph due to bolt db's
	// transactional model.
	graph := r.server.chanDB.ChannelGraph()

	// First iterate through all the known nodes (connected or unconnected
	// within the graph), collating their current state into the RPC
	// response.
	err := graph.ForEachNode(nil, func(_ *bolt.Tx, node *channeldb.LightningNode) error {
		nodeAddrs := make([]*lnrpc.NodeAddress, 0)
		for _, addr := range node.Addresses {
			nodeAddr := &lnrpc.NodeAddress{
				Network: addr.Network(),
				Addr:    addr.String(),
			}
			nodeAddrs = append(nodeAddrs, nodeAddr)
		}

		resp.Nodes = append(resp.Nodes, &lnrpc.LightningNode{
			LastUpdate: uint32(node.LastUpdate.Unix()),
			PubKey:     hex.EncodeToString(node.PubKeyBytes[:]),
			Addresses:  nodeAddrs,
			Alias:      node.Alias,
			Color:      routing.EncodeHexColor(node.Color),
			Features:   invoicesrpc.CreateRPCFeatures(node.Features),
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Next, for each active channel we know of within the graph, create a
	// similar response which details both the edge information as well as
	// the routing policies of th nodes connecting the two edges.
	err = graph.ForEachChannel(func(edgeInfo *channeldb.ChannelEdgeInfo,
		c1, c2 *channeldb.ChannelEdgePolicy) error {

		// Do not include unannounced channels unless specifically
		// requested. Unannounced channels include both private channels as
		// well as public channels whose authentication proof were not
		// confirmed yet, hence were not announced.
		if !includeUnannounced && edgeInfo.AuthProof == nil {
			return nil
		}

		edge := marshalDbEdge(edgeInfo, c1, c2)
		resp.Edges = append(resp.Edges, edge)

		return nil
	})
	if err != nil && err != channeldb.ErrGraphNoEdgesFound {
		return nil, err
	}

	return resp, nil
}

func marshalDbEdge(edgeInfo *channeldb.ChannelEdgeInfo,
	c1, c2 *channeldb.ChannelEdgePolicy) *lnrpc.ChannelEdge {

	// Order the edges by increasing pubkey.
	if bytes.Compare(edgeInfo.NodeKey2Bytes[:],
		edgeInfo.NodeKey1Bytes[:]) < 0 {

		c2, c1 = c1, c2
	}

	var lastUpdate int64
	if c1 != nil {
		lastUpdate = c1.LastUpdate.Unix()
	}
	if c2 != nil && c2.LastUpdate.Unix() > lastUpdate {
		lastUpdate = c2.LastUpdate.Unix()
	}

	edge := &lnrpc.ChannelEdge{
		ChannelId: edgeInfo.ChannelID,
		ChanPoint: edgeInfo.ChannelPoint.String(),
		// TODO(roasbeef): update should be on edge info itself
		LastUpdate: uint32(lastUpdate),
		Node1Pub:   hex.EncodeToString(edgeInfo.NodeKey1Bytes[:]),
		Node2Pub:   hex.EncodeToString(edgeInfo.NodeKey2Bytes[:]),
		Capacity:   int64(edgeInfo.Capacity),
	}

	if c1 != nil {
		edge.Node1Policy = &lnrpc.RoutingPolicy{
			TimeLockDelta:      uint32(c1.TimeLockDelta),
			MinHtlc:            int64(c1.MinHTLC),
			MaxHtlcMAtoms:      uint64(c1.MaxHTLC),
			FeeBaseMAtoms:      int64(c1.FeeBaseMAtoms),
			FeeRateMilliMAtoms: int64(c1.FeeProportionalMillionths),
			Disabled:           c1.ChannelFlags&lnwire.ChanUpdateDisabled != 0,
			LastUpdate:         uint32(c1.LastUpdate.Unix()),
		}
	}

	if c2 != nil {
		edge.Node2Policy = &lnrpc.RoutingPolicy{
			TimeLockDelta:      uint32(c2.TimeLockDelta),
			MinHtlc:            int64(c2.MinHTLC),
			MaxHtlcMAtoms:      uint64(c2.MaxHTLC),
			FeeBaseMAtoms:      int64(c2.FeeBaseMAtoms),
			FeeRateMilliMAtoms: int64(c2.FeeProportionalMillionths),
			Disabled:           c2.ChannelFlags&lnwire.ChanUpdateDisabled != 0,
			LastUpdate:         uint32(c2.LastUpdate.Unix()),
		}
	}

	return edge
}

// GetChanInfo returns the latest authenticated network announcement for the
// given channel identified by its channel ID: an 8-byte integer which uniquely
// identifies the location of transaction's funding output within the block
// chain.
func (r *rpcServer) GetChanInfo(ctx context.Context,
	in *lnrpc.ChanInfoRequest) (*lnrpc.ChannelEdge, error) {

	graph := r.server.chanDB.ChannelGraph()

	edgeInfo, edge1, edge2, err := graph.FetchChannelEdgesByID(in.ChanId)
	if err != nil {
		return nil, err
	}

	// Convert the database's edge format into the network/RPC edge format
	// which couples the edge itself along with the directional node
	// routing policies of each node involved within the channel.
	channelEdge := marshalDbEdge(edgeInfo, edge1, edge2)

	return channelEdge, nil
}

// GetNodeInfo returns the latest advertised and aggregate authenticated
// channel information for the specified node identified by its public key.
func (r *rpcServer) GetNodeInfo(ctx context.Context,
	in *lnrpc.NodeInfoRequest) (*lnrpc.NodeInfo, error) {

	graph := r.server.chanDB.ChannelGraph()

	// First, parse the hex-encoded public key into a full in-memory public
	// key object we can work with for querying.
	pubKey, err := route.NewVertexFromStr(in.PubKey)
	if err != nil {
		return nil, err
	}

	// With the public key decoded, attempt to fetch the node corresponding
	// to this public key. If the node cannot be found, then an error will
	// be returned.
	node, err := graph.FetchLightningNode(pubKey)
	if err != nil {
		return nil, err
	}

	// With the node obtained, we'll now iterate through all its out going
	// edges to gather some basic statistics about its out going channels.
	var (
		numChannels   uint32
		totalCapacity dcrutil.Amount
		channels      []*lnrpc.ChannelEdge
	)

	if err := node.ForEachChannel(nil, func(_ *bolt.Tx,
		edge *channeldb.ChannelEdgeInfo,
		c1, c2 *channeldb.ChannelEdgePolicy) error {

		numChannels++
		totalCapacity += edge.Capacity

		// Only populate the node's channels if the user requested them.
		if in.IncludeChannels {
			// Do not include unannounced channels - private
			// channels or public channels whose authentication
			// proof were not confirmed yet.
			if edge.AuthProof == nil {
				return nil
			}

			// Convert the database's edge format into the
			// network/RPC edge format.
			channelEdge := marshalDbEdge(edge, c1, c2)
			channels = append(channels, channelEdge)
		}

		return nil
	}); err != nil {
		return nil, err
	}

	nodeAddrs := make([]*lnrpc.NodeAddress, 0)
	for _, addr := range node.Addresses {
		nodeAddr := &lnrpc.NodeAddress{
			Network: addr.Network(),
			Addr:    addr.String(),
		}
		nodeAddrs = append(nodeAddrs, nodeAddr)
	}

	features := invoicesrpc.CreateRPCFeatures(node.Features)

	return &lnrpc.NodeInfo{
		Node: &lnrpc.LightningNode{
			LastUpdate: uint32(node.LastUpdate.Unix()),
			PubKey:     in.PubKey,
			Addresses:  nodeAddrs,
			Alias:      node.Alias,
			Color:      routing.EncodeHexColor(node.Color),
			Features:   features,
		},
		NumChannels:   numChannels,
		TotalCapacity: int64(totalCapacity),
		Channels:      channels,
	}, nil
}

// QueryRoutes attempts to query the daemons' Channel Router for a possible
// route to a target destination capable of carrying a specific amount of
// atoms within the route's flow. The retuned route contains the full
// details required to craft and send an HTLC, also including the necessary
// information that should be present within the Sphinx packet encapsulated
// within the HTLC.
//
// TODO(roasbeef): should return a slice of routes in reality
//  * create separate PR to send based on well formatted route
func (r *rpcServer) QueryRoutes(ctx context.Context,
	in *lnrpc.QueryRoutesRequest) (*lnrpc.QueryRoutesResponse, error) {

	return r.routerBackend.QueryRoutes(ctx, in)
}

// GetNetworkInfo returns some basic stats about the known channel graph from
// the PoV of the node.
func (r *rpcServer) GetNetworkInfo(ctx context.Context,
	_ *lnrpc.NetworkInfoRequest) (*lnrpc.NetworkInfo, error) {

	graph := r.server.chanDB.ChannelGraph()

	var (
		numNodes             uint32
		numChannels          uint32
		maxChanOut           uint32
		totalNetworkCapacity dcrutil.Amount
		minChannelSize       dcrutil.Amount = math.MaxInt64
		maxChannelSize       dcrutil.Amount
		medianChanSize       dcrutil.Amount
	)

	// We'll use this map to de-duplicate channels during our traversal.
	// This is needed since channels are directional, so there will be two
	// edges for each channel within the graph.
	seenChans := make(map[uint64]struct{})

	// We also keep a list of all encountered capacities, in order to
	// calculate the median channel size.
	var allChans []dcrutil.Amount

	// We'll run through all the known nodes in the within our view of the
	// network, tallying up the total number of nodes, and also gathering
	// each node so we can measure the graph diameter and degree stats
	// below.
	if err := graph.ForEachNode(nil, func(tx *bolt.Tx, node *channeldb.LightningNode) error {
		// Increment the total number of nodes with each iteration.
		numNodes++

		// For each channel we'll compute the out degree of each node,
		// and also update our running tallies of the min/max channel
		// capacity, as well as the total channel capacity. We pass
		// through the db transaction from the outer view so we can
		// re-use it within this inner view.
		var outDegree uint32
		if err := node.ForEachChannel(tx, func(_ *bolt.Tx,
			edge *channeldb.ChannelEdgeInfo, _, _ *channeldb.ChannelEdgePolicy) error {

			// Bump up the out degree for this node for each
			// channel encountered.
			outDegree++

			// If we've already seen this channel, then we'll
			// return early to ensure that we don't double-count
			// stats.
			if _, ok := seenChans[edge.ChannelID]; ok {
				return nil
			}

			// Compare the capacity of this channel against the
			// running min/max to see if we should update the
			// extrema.
			chanCapacity := edge.Capacity
			if chanCapacity < minChannelSize {
				minChannelSize = chanCapacity
			}
			if chanCapacity > maxChannelSize {
				maxChannelSize = chanCapacity
			}

			// Accumulate the total capacity of this channel to the
			// network wide-capacity.
			totalNetworkCapacity += chanCapacity

			numChannels++

			seenChans[edge.ChannelID] = struct{}{}
			allChans = append(allChans, edge.Capacity)
			return nil
		}); err != nil {
			return err
		}

		// Finally, if the out degree of this node is greater than what
		// we've seen so far, update the maxChanOut variable.
		if outDegree > maxChanOut {
			maxChanOut = outDegree
		}

		return nil
	}); err != nil {
		return nil, err
	}

	// Query the graph for the current number of zombie channels.
	numZombies, err := graph.NumZombies()
	if err != nil {
		return nil, err
	}

	// Find the median.
	medianChanSize = autopilot.Median(allChans)

	// If we don't have any channels, then reset the minChannelSize to zero
	// to avoid outputting NaN in encoded JSON.
	if numChannels == 0 {
		minChannelSize = 0
	}

	// TODO(roasbeef): graph diameter

	// TODO(roasbeef): also add oldest channel?
	netInfo := &lnrpc.NetworkInfo{
		MaxOutDegree:         maxChanOut,
		AvgOutDegree:         float64(2*numChannels) / float64(numNodes),
		NumNodes:             numNodes,
		NumChannels:          numChannels,
		TotalNetworkCapacity: int64(totalNetworkCapacity),
		AvgChannelSize:       float64(totalNetworkCapacity) / float64(numChannels),

		MinChannelSize:       int64(minChannelSize),
		MaxChannelSize:       int64(maxChannelSize),
		MedianChannelSizeSat: int64(medianChanSize),
		NumZombieChans:       numZombies,
	}

	// Similarly, if we don't have any channels, then we'll also set the
	// average channel size to zero in order to avoid weird JSON encoding
	// outputs.
	if numChannels == 0 {
		netInfo.AvgChannelSize = 0
	}

	return netInfo, nil
}

// StopDaemon will send a shutdown request to the interrupt handler, triggering
// a graceful shutdown of the daemon.
func (r *rpcServer) StopDaemon(ctx context.Context,
	_ *lnrpc.StopRequest) (*lnrpc.StopResponse, error) {

	signal.RequestShutdown()
	return &lnrpc.StopResponse{}, nil
}

// SubscribeChannelGraph launches a streaming RPC that allows the caller to
// receive notifications upon any changes the channel graph topology from the
// review of the responding node. Events notified include: new nodes coming
// online, nodes updating their authenticated attributes, new channels being
// advertised, updates in the routing policy for a directional channel edge,
// and finally when prior channels are closed on-chain.
func (r *rpcServer) SubscribeChannelGraph(req *lnrpc.GraphTopologySubscription,
	updateStream lnrpc.Lightning_SubscribeChannelGraphServer) error {

	// First, we start by subscribing to a new intent to receive
	// notifications from the channel router.
	client, err := r.server.chanRouter.SubscribeTopology()
	if err != nil {
		return err
	}

	// Ensure that the resources for the topology update client is cleaned
	// up once either the server, or client exists.
	defer client.Cancel()

	for {
		select {

		// A new update has been sent by the channel router, we'll
		// marshal it into the form expected by the gRPC client, then
		// send it off.
		case topChange, ok := <-client.TopologyChanges:
			// If the second value from the channel read is nil,
			// then this means that the channel router is exiting
			// or the notification client was canceled. So we'll
			// exit early.
			if !ok {
				return errors.New("server shutting down")
			}

			// Convert the struct from the channel router into the
			// form expected by the gRPC service then send it off
			// to the client.
			graphUpdate := marshallTopologyChange(topChange)
			if err := updateStream.Send(graphUpdate); err != nil {
				return err
			}

		// The server is quitting, so we'll exit immediately. Returning
		// nil will close the clients read end of the stream.
		case <-r.quit:
			return nil
		}
	}
}

// marshallTopologyChange performs a mapping from the topology change struct
// returned by the router to the form of notifications expected by the current
// gRPC service.
func marshallTopologyChange(topChange *routing.TopologyChange) *lnrpc.GraphTopologyUpdate {

	// encodeKey is a simple helper function that converts a live public
	// key into a hex-encoded version of the compressed serialization for
	// the public key.
	encodeKey := func(k *secp256k1.PublicKey) string {
		return hex.EncodeToString(k.SerializeCompressed())
	}

	nodeUpdates := make([]*lnrpc.NodeUpdate, len(topChange.NodeUpdates))
	for i, nodeUpdate := range topChange.NodeUpdates {
		addrs := make([]string, len(nodeUpdate.Addresses))
		for i, addr := range nodeUpdate.Addresses {
			addrs[i] = addr.String()
		}

		nodeUpdates[i] = &lnrpc.NodeUpdate{
			Addresses:      addrs,
			IdentityKey:    encodeKey(nodeUpdate.IdentityKey),
			GlobalFeatures: nodeUpdate.GlobalFeatures,
			Alias:          nodeUpdate.Alias,
			Color:          nodeUpdate.Color,
		}
	}

	channelUpdates := make([]*lnrpc.ChannelEdgeUpdate, len(topChange.ChannelEdgeUpdates))
	for i, channelUpdate := range topChange.ChannelEdgeUpdates {
		channelUpdates[i] = &lnrpc.ChannelEdgeUpdate{
			ChanId: channelUpdate.ChanID,
			ChanPoint: &lnrpc.ChannelPoint{
				FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
					FundingTxidBytes: channelUpdate.ChanPoint.Hash[:],
				},
				OutputIndex: channelUpdate.ChanPoint.Index,
			},
			Capacity: int64(channelUpdate.Capacity),
			RoutingPolicy: &lnrpc.RoutingPolicy{
				TimeLockDelta:      uint32(channelUpdate.TimeLockDelta),
				MinHtlc:            int64(channelUpdate.MinHTLC),
				MaxHtlcMAtoms:      uint64(channelUpdate.MaxHTLC),
				FeeBaseMAtoms:      int64(channelUpdate.BaseFee),
				FeeRateMilliMAtoms: int64(channelUpdate.FeeRate),
				Disabled:           channelUpdate.Disabled,
			},
			AdvertisingNode: encodeKey(channelUpdate.AdvertisingNode),
			ConnectingNode:  encodeKey(channelUpdate.ConnectingNode),
		}
	}

	closedChans := make([]*lnrpc.ClosedChannelUpdate, len(topChange.ClosedChannels))
	for i, closedChan := range topChange.ClosedChannels {
		closedChans[i] = &lnrpc.ClosedChannelUpdate{
			ChanId:       closedChan.ChanID,
			Capacity:     int64(closedChan.Capacity),
			ClosedHeight: closedChan.ClosedHeight,
			ChanPoint: &lnrpc.ChannelPoint{
				FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
					FundingTxidBytes: closedChan.ChanPoint.Hash[:],
				},
				OutputIndex: closedChan.ChanPoint.Index,
			},
		}
	}

	return &lnrpc.GraphTopologyUpdate{
		NodeUpdates:    nodeUpdates,
		ChannelUpdates: channelUpdates,
		ClosedChans:    closedChans,
	}
}

// ListPayments returns a list of all outgoing payments.
func (r *rpcServer) ListPayments(ctx context.Context,
	req *lnrpc.ListPaymentsRequest) (*lnrpc.ListPaymentsResponse, error) {

	rpcsLog.Debugf("[ListPayments]")

	payments, err := r.server.chanDB.FetchPayments()
	if err != nil {
		return nil, err
	}

	paymentsResp := &lnrpc.ListPaymentsResponse{}
	for _, payment := range payments {
		// To keep compatibility with the old API, we only return
		// non-suceeded payments if requested.
		if payment.Status != channeldb.StatusSucceeded &&
			!req.IncludeIncomplete {
			continue
		}

		// Fetch the payment's route and preimage. If no HTLC was
		// successful, an empty route and preimage will be used.
		var (
			route    route.Route
			preimage lntypes.Preimage
		)
		for _, htlc := range payment.HTLCs {
			// Display the last route attempted.
			route = htlc.Route

			// If any of the htlcs have settled, extract a valid
			// preimage.
			if htlc.Settle != nil {
				preimage = htlc.Settle.Preimage
			}
		}

		// Encode the hops from the successful route, if any.
		path := make([]string, len(route.Hops))
		for i, hop := range route.Hops {
			path[i] = hex.EncodeToString(hop.PubKeyBytes[:])
		}

		mAtomsValue := int64(payment.Info.Value)
		atomsValue := int64(payment.Info.Value.ToAtoms())

		status, err := convertPaymentStatus(payment.Status)
		if err != nil {
			return nil, err
		}

		htlcs := make([]*lnrpc.HTLCAttempt, 0, len(payment.HTLCs))
		for _, dbHTLC := range payment.HTLCs {
			htlc, err := r.routerBackend.MarshalHTLCAttempt(dbHTLC)
			if err != nil {
				return nil, err
			}

			htlcs = append(htlcs, htlc)
		}

		paymentHash := payment.Info.PaymentHash
		creationTimeNS := routerrpc.MarshalTimeNano(payment.Info.CreationTime)
		paymentsResp.Payments = append(paymentsResp.Payments, &lnrpc.Payment{
			PaymentHash:     hex.EncodeToString(paymentHash[:]),
			Value:           atomsValue,
			ValueMAtoms:     mAtomsValue,
			ValueAtoms:      atomsValue,
			CreationDate:    payment.Info.CreationTime.Unix(),
			CreationTimeNs:  creationTimeNS,
			Path:            path,
			Fee:             int64(route.TotalFees().ToAtoms()),
			FeeAtoms:        int64(route.TotalFees().ToAtoms()),
			FeeMAtoms:       int64(route.TotalFees()),
			PaymentPreimage: hex.EncodeToString(preimage[:]),
			PaymentRequest:  string(payment.Info.PaymentRequest),
			Status:          status,
			Htlcs:           htlcs,
		})
	}

	return paymentsResp, nil
}

// convertPaymentStatus converts a channeldb.PaymentStatus to the type expected
// by the RPC.
func convertPaymentStatus(dbStatus channeldb.PaymentStatus) (
	lnrpc.Payment_PaymentStatus, error) {

	switch dbStatus {
	case channeldb.StatusUnknown:
		return lnrpc.Payment_UNKNOWN, nil

	case channeldb.StatusInFlight:
		return lnrpc.Payment_IN_FLIGHT, nil

	case channeldb.StatusSucceeded:
		return lnrpc.Payment_SUCCEEDED, nil

	case channeldb.StatusFailed:
		return lnrpc.Payment_FAILED, nil

	default:
		return 0, fmt.Errorf("unhandled payment status %v", dbStatus)
	}
}

// DeleteAllPayments deletes all outgoing payments from DB.
func (r *rpcServer) DeleteAllPayments(ctx context.Context,
	_ *lnrpc.DeleteAllPaymentsRequest) (*lnrpc.DeleteAllPaymentsResponse, error) {

	rpcsLog.Debugf("[DeleteAllPayments]")

	if err := r.server.chanDB.DeletePayments(); err != nil {
		return nil, err
	}

	return &lnrpc.DeleteAllPaymentsResponse{}, nil
}

// DebugLevel allows a caller to programmatically set the logging verbosity of
// lnd. The logging can be targeted according to a coarse daemon-wide logging
// level, or in a granular fashion to specify the logging for a target
// sub-system.
func (r *rpcServer) DebugLevel(ctx context.Context,
	req *lnrpc.DebugLevelRequest) (*lnrpc.DebugLevelResponse, error) {

	// If show is set, then we simply print out the list of available
	// sub-systems.
	if req.Show {
		return &lnrpc.DebugLevelResponse{
			SubSystems: strings.Join(
				logWriter.SupportedSubsystems(), " ",
			),
		}, nil
	}

	rpcsLog.Infof("[debuglevel] changing debug level to: %v", req.LevelSpec)

	// Otherwise, we'll attempt to set the logging level using the
	// specified level spec.
	err := build.ParseAndSetDebugLevels(req.LevelSpec, logWriter)
	if err != nil {
		return nil, err
	}

	return &lnrpc.DebugLevelResponse{}, nil
}

// DecodePayReq takes an encoded payment request string and attempts to decode
// it, returning a full description of the conditions encoded within the
// payment request.
func (r *rpcServer) DecodePayReq(ctx context.Context,
	req *lnrpc.PayReqString) (*lnrpc.PayReq, error) {

	rpcsLog.Tracef("[decodepayreq] decoding: %v", req.PayReq)

	// Fist we'll attempt to decode the payment request string, if the
	// request is invalid or the checksum doesn't match, then we'll exit
	// here with an error.
	payReq, err := zpay32.Decode(req.PayReq, activeNetParams.Params)
	if err != nil {
		return nil, err
	}

	// Let the fields default to empty strings.
	desc := ""
	if payReq.Description != nil {
		desc = *payReq.Description
	}

	descHash := []byte("")
	if payReq.DescriptionHash != nil {
		descHash = payReq.DescriptionHash[:]
	}

	fallbackAddr := ""
	if payReq.FallbackAddr != nil {
		fallbackAddr = payReq.FallbackAddr.String()
	}

	// Expiry time will default to 3600 seconds if not specified
	// explicitly.
	expiry := int64(payReq.Expiry().Seconds())

	// Convert between the `lnrpc` and `routing` types.
	routeHints := invoicesrpc.CreateRPCRouteHints(payReq.RouteHints)

	var amtAtoms, amtMAtoms int64
	if payReq.MilliAt != nil {
		amtAtoms = int64(payReq.MilliAt.ToAtoms())
		amtMAtoms = int64(*payReq.MilliAt)
	}

	// Extract the payment address from the payment request, if present.
	var paymentAddr []byte
	if payReq.PaymentAddr != nil {
		paymentAddr = payReq.PaymentAddr[:]
	}

	dest := payReq.Destination.SerializeCompressed()
	return &lnrpc.PayReq{
		Destination:     hex.EncodeToString(dest),
		PaymentHash:     hex.EncodeToString(payReq.PaymentHash[:]),
		NumAtoms:        amtAtoms,
		NumMAtoms:       amtMAtoms,
		Timestamp:       payReq.Timestamp.Unix(),
		Description:     desc,
		DescriptionHash: hex.EncodeToString(descHash),
		FallbackAddr:    fallbackAddr,
		Expiry:          expiry,
		CltvExpiry:      int64(payReq.MinFinalCLTVExpiry()),
		RouteHints:      routeHints,
		PaymentAddr:     paymentAddr,
		Features:        invoicesrpc.CreateRPCFeatures(payReq.Features),
	}, nil
}

// feeBase is the fixed point that fee rate computation are performed over.
// Nodes on the network advertise their fee rate using this point as a base.
// This means that the minimal possible fee rate if 1e-6, or 0.000001, or
// 0.0001%.
const feeBase = 1000000

// FeeReport allows the caller to obtain a report detailing the current fee
// schedule enforced by the node globally for each channel.
func (r *rpcServer) FeeReport(ctx context.Context,
	_ *lnrpc.FeeReportRequest) (*lnrpc.FeeReportResponse, error) {

	// TODO(roasbeef): use UnaryInterceptor to add automated logging

	rpcsLog.Debugf("[feereport]")

	channelGraph := r.server.chanDB.ChannelGraph()
	selfNode, err := channelGraph.SourceNode()
	if err != nil {
		return nil, err
	}

	var feeReports []*lnrpc.ChannelFeeReport
	err = selfNode.ForEachChannel(nil, func(_ *bolt.Tx, chanInfo *channeldb.ChannelEdgeInfo,
		edgePolicy, _ *channeldb.ChannelEdgePolicy) error {

		// Self node should always have policies for its channels.
		if edgePolicy == nil {
			return fmt.Errorf("no policy for outgoing channel %v ",
				chanInfo.ChannelID)
		}

		// We'll compute the effective fee rate by converting from a
		// fixed point fee rate to a floating point fee rate. The fee
		// rate field in the database the amount of milli-atoms charged per
		// 1mil milli-atoms sent, so will divide by this to get the proper fee
		// rate.
		feeRateFixedPoint := edgePolicy.FeeProportionalMillionths
		feeRate := float64(feeRateFixedPoint) / float64(feeBase)

		// TODO(roasbeef): also add stats for revenue for each channel
		feeReports = append(feeReports, &lnrpc.ChannelFeeReport{
			ChanPoint:     chanInfo.ChannelPoint.String(),
			BaseFeeMAtoms: int64(edgePolicy.FeeBaseMAtoms),
			FeePerMil:     int64(feeRateFixedPoint),
			FeeRate:       feeRate,
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	fwdEventLog := r.server.chanDB.ForwardingLog()

	// computeFeeSum is a helper function that computes the total fees for
	// a particular time slice described by a forwarding event query.
	computeFeeSum := func(query channeldb.ForwardingEventQuery) (lnwire.MilliAtom, error) {

		var totalFees lnwire.MilliAtom

		// We'll continue to fetch the next query and accumulate the
		// fees until the next query returns no events.
		for {
			timeSlice, err := fwdEventLog.Query(query)
			if err != nil {
				return 0, err
			}

			// If the timeslice is empty, then we'll return as
			// we've retrieved all the entries in this range.
			if len(timeSlice.ForwardingEvents) == 0 {
				break
			}

			// Otherwise, we'll tally up an accumulate the total
			// fees for this time slice.
			for _, event := range timeSlice.ForwardingEvents {
				fee := event.AmtIn - event.AmtOut
				totalFees += fee
			}

			// We'll now take the last offset index returned as
			// part of this response, and modify our query to start
			// at this index. This has a pagination effect in the
			// case that our query bounds has more than 100k
			// entries.
			query.IndexOffset = timeSlice.LastIndexOffset
		}

		return totalFees, nil
	}

	now := time.Now()

	// Before we perform the queries below, we'll instruct the switch to
	// flush any pending events to disk. This ensure we get a complete
	// snapshot at this particular time.
	if err := r.server.htlcSwitch.FlushForwardingEvents(); err != nil {
		return nil, fmt.Errorf("unable to flush forwarding "+
			"events: %v", err)
	}

	// In addition to returning the current fee schedule for each channel.
	// We'll also perform a series of queries to obtain the total fees
	// earned over the past day, week, and month.
	dayQuery := channeldb.ForwardingEventQuery{
		StartTime:    now.Add(-time.Hour * 24),
		EndTime:      now,
		NumMaxEvents: 1000,
	}
	dayFees, err := computeFeeSum(dayQuery)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve day fees: %v", err)
	}

	weekQuery := channeldb.ForwardingEventQuery{
		StartTime:    now.Add(-time.Hour * 24 * 7),
		EndTime:      now,
		NumMaxEvents: 1000,
	}
	weekFees, err := computeFeeSum(weekQuery)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve day fees: %v", err)
	}

	monthQuery := channeldb.ForwardingEventQuery{
		StartTime:    now.Add(-time.Hour * 24 * 30),
		EndTime:      now,
		NumMaxEvents: 1000,
	}
	monthFees, err := computeFeeSum(monthQuery)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve day fees: %v", err)
	}

	return &lnrpc.FeeReportResponse{
		ChannelFees: feeReports,
		DayFeeSum:   uint64(dayFees.ToAtoms()),
		WeekFeeSum:  uint64(weekFees.ToAtoms()),
		MonthFeeSum: uint64(monthFees.ToAtoms()),
	}, nil
}

// minFeeRate is the smallest permitted fee rate within the network. This is
// derived by the fact that fee rates are computed using a fixed point of
// 1,000,000. As a result, the smallest representable fee rate is 1e-6, or
// 0.000001, or 0.0001%.
const minFeeRate = 1e-6

// UpdateChannelPolicy allows the caller to update the channel forwarding policy
// for all channels globally, or a particular channel.
func (r *rpcServer) UpdateChannelPolicy(ctx context.Context,
	req *lnrpc.PolicyUpdateRequest) (*lnrpc.PolicyUpdateResponse, error) {

	var targetChans []wire.OutPoint
	switch scope := req.Scope.(type) {
	// If the request is targeting all active channels, then we don't need
	// target any channels by their channel point.
	case *lnrpc.PolicyUpdateRequest_Global:

	// Otherwise, we're targeting an individual channel by its channel
	// point.
	case *lnrpc.PolicyUpdateRequest_ChanPoint:
		txid, err := GetChanPointFundingTxid(scope.ChanPoint)
		if err != nil {
			return nil, err
		}
		targetChans = append(targetChans, wire.OutPoint{
			Hash:  *txid,
			Index: scope.ChanPoint.OutputIndex,
		})
	default:
		return nil, fmt.Errorf("unknown scope: %v", scope)
	}

	switch {
	// As a sanity check, if the fee isn't zero, we'll ensure that the
	// passed fee rate is below 1e-6, or the lowest allowed non-zero fee
	// rate expressible within the protocol.
	case req.FeeRate != 0 && req.FeeRate < minFeeRate:
		return nil, fmt.Errorf("fee rate of %v is too small, min fee "+
			"rate is %v", req.FeeRate, minFeeRate)

	// We'll also ensure that the user isn't setting a CLTV delta that
	// won't give outgoing HTLCs enough time to fully resolve if needed.
	case req.TimeLockDelta < minTimeLockDelta:
		return nil, fmt.Errorf("time lock delta of %v is too small, "+
			"minimum supported is %v", req.TimeLockDelta,
			minTimeLockDelta)
	}

	// We'll also need to convert the floating point fee rate we accept
	// over RPC to the fixed point rate that we use within the protocol. We
	// do this by multiplying the passed fee rate by the fee base. This
	// gives us the fixed point, scaled by 1 million that's used within the
	// protocol.
	feeRateFixed := uint32(req.FeeRate * feeBase)
	baseFeeMAtoms := lnwire.MilliAtom(req.BaseFeeMAtoms)
	feeSchema := routing.FeeSchema{
		BaseFee: baseFeeMAtoms,
		FeeRate: feeRateFixed,
	}

	maxHtlc := lnwire.MilliAtom(req.MaxHtlcMAtoms)
	var minHtlc *lnwire.MilliAtom
	if req.MinHtlcMAtomsSpecified {
		min := lnwire.MilliAtom(req.MinHtlcMAtoms)
		minHtlc = &min
	}

	chanPolicy := routing.ChannelPolicy{
		FeeSchema:     feeSchema,
		TimeLockDelta: req.TimeLockDelta,
		MaxHTLC:       maxHtlc,
		MinHTLC:       minHtlc,
	}

	rpcsLog.Debugf("[updatechanpolicy] updating channel policy base_fee=%v, "+
		"rate_float=%v, rate_fixed=%v, time_lock_delta: %v, "+
		"min_htlc=%v, max_htlc=%v, targets=%v",
		req.BaseFeeMAtoms, req.FeeRate, feeRateFixed, req.TimeLockDelta,
		minHtlc, maxHtlc,
		spew.Sdump(targetChans))

	// With the scope resolved, we'll now send this to the local channel
	// manager so it can propagate the new policy for our target channel(s).
	err := r.server.localChanMgr.UpdatePolicy(chanPolicy, targetChans...)
	if err != nil {
		return nil, err
	}

	return &lnrpc.PolicyUpdateResponse{}, nil
}

// ForwardingHistory allows the caller to query the htlcswitch for a record of
// all HTLC's forwarded within the target time range, and integer offset within
// that time range. If no time-range is specified, then the first chunk of the
// past 24 hrs of forwarding history are returned.

// A list of forwarding events are returned. The size of each forwarding event
// is 40 bytes, and the max message size able to be returned in gRPC is 4 MiB.
// In order to safely stay under this max limit, we'll return 50k events per
// response.  Each response has the index offset of the last entry. The index
// offset can be provided to the request to allow the caller to skip a series
// of records.
func (r *rpcServer) ForwardingHistory(ctx context.Context,
	req *lnrpc.ForwardingHistoryRequest) (*lnrpc.ForwardingHistoryResponse, error) {

	rpcsLog.Debugf("[forwardinghistory]")

	// Before we perform the queries below, we'll instruct the switch to
	// flush any pending events to disk. This ensure we get a complete
	// snapshot at this particular time.
	if err := r.server.htlcSwitch.FlushForwardingEvents(); err != nil {
		return nil, fmt.Errorf("unable to flush forwarding "+
			"events: %v", err)
	}

	var (
		startTime, endTime time.Time

		numEvents uint32
	)

	// startTime defaults to the Unix epoch (0 unixtime, or midnight 01-01-1970).
	startTime = time.Unix(int64(req.StartTime), 0)

	// If the end time wasn't specified, assume a default end time of now.
	if req.EndTime == 0 {
		now := time.Now()
		endTime = now
	} else {
		endTime = time.Unix(int64(req.EndTime), 0)
	}

	// If the number of events wasn't specified, then we'll default to
	// returning the last 100 events.
	numEvents = req.NumMaxEvents
	if numEvents == 0 {
		numEvents = 100
	}

	// Next, we'll map the proto request into a format that is understood by
	// the forwarding log.
	eventQuery := channeldb.ForwardingEventQuery{
		StartTime:    startTime,
		EndTime:      endTime,
		IndexOffset:  req.IndexOffset,
		NumMaxEvents: numEvents,
	}
	timeSlice, err := r.server.chanDB.ForwardingLog().Query(eventQuery)
	if err != nil {
		return nil, fmt.Errorf("unable to query forwarding log: %v", err)
	}

	// TODO(roasbeef): add settlement latency?
	//  * use FPE on all records?

	// With the events retrieved, we'll now map them into the proper proto
	// response.
	//
	// TODO(roasbeef): show in ns for the outside?
	resp := &lnrpc.ForwardingHistoryResponse{
		ForwardingEvents: make([]*lnrpc.ForwardingEvent, len(timeSlice.ForwardingEvents)),
		LastOffsetIndex:  timeSlice.LastIndexOffset,
	}
	for i, event := range timeSlice.ForwardingEvents {
		amtInMAtoms := event.AmtIn
		amtOutMAtoms := event.AmtOut
		feeMAtoms := event.AmtIn - event.AmtOut

		resp.ForwardingEvents[i] = &lnrpc.ForwardingEvent{
			Timestamp:    uint64(event.Timestamp.Unix()),
			ChanIdIn:     event.IncomingChanID.ToUint64(),
			ChanIdOut:    event.OutgoingChanID.ToUint64(),
			AmtIn:        uint64(amtInMAtoms.ToAtoms()),
			AmtOut:       uint64(amtOutMAtoms.ToAtoms()),
			Fee:          uint64(feeMAtoms.ToAtoms()),
			FeeMAtoms:    uint64(feeMAtoms),
			AmtInMAtoms:  uint64(amtInMAtoms),
			AmtOutMAtoms: uint64(amtOutMAtoms),
		}
	}

	return resp, nil
}

// ExportChannelBackup attempts to return an encrypted static channel backup
// for the target channel identified by it channel point. The backup is
// encrypted with a key generated from the aezeed seed of the user. The
// returned backup can either be restored using the RestoreChannelBackup method
// once lnd is running, or via the InitWallet and UnlockWallet methods from the
// WalletUnlocker service.
func (r *rpcServer) ExportChannelBackup(ctx context.Context,
	in *lnrpc.ExportChannelBackupRequest) (*lnrpc.ChannelBackup, error) {

	// First, we'll convert the lnrpc channel point into a wire.OutPoint
	// that we can manipulate.
	txid, err := GetChanPointFundingTxid(in.ChanPoint)
	if err != nil {
		return nil, err
	}
	chanPoint := wire.OutPoint{
		Hash:  *txid,
		Index: in.ChanPoint.OutputIndex,
	}

	// Next, we'll attempt to fetch a channel backup for this channel from
	// the database. If this channel has been closed, or the outpoint is
	// unknown, then we'll return an error
	unpackedBackup, err := chanbackup.FetchBackupForChan(
		chanPoint, r.server.chanDB,
	)
	if err != nil {
		return nil, err
	}

	// At this point, we have an unpacked backup (plaintext) so we'll now
	// attempt to serialize and encrypt it in order to create a packed
	// backup.
	packedBackups, err := chanbackup.PackStaticChanBackups(
		[]chanbackup.Single{*unpackedBackup},
		r.server.cc.keyRing,
	)
	if err != nil {
		return nil, fmt.Errorf("packing of back ups failed: %v", err)
	}

	// Before we proceed, we'll ensure that we received a backup for this
	// channel, otherwise, we'll bail out.
	packedBackup, ok := packedBackups[chanPoint]
	if !ok {
		return nil, fmt.Errorf("expected single backup for "+
			"ChannelPoint(%v), got %v", chanPoint,
			len(packedBackup))
	}

	return &lnrpc.ChannelBackup{
		ChanPoint:  in.ChanPoint,
		ChanBackup: packedBackup,
	}, nil
}

// VerifyChanBackup allows a caller to verify the integrity of a channel backup
// snapshot. This method will accept both either a packed Single or a packed
// Multi. Specifying both will result in an error.
func (r *rpcServer) VerifyChanBackup(ctx context.Context,
	in *lnrpc.ChanBackupSnapshot) (*lnrpc.VerifyChanBackupResponse, error) {

	switch {
	// If neither a Single or Multi has been specified, then we have nothing
	// to verify.
	case in.GetSingleChanBackups() == nil && in.GetMultiChanBackup() == nil:
		return nil, errors.New("either a Single or Multi channel " +
			"backup must be specified")

	// Either a Single or a Multi must be specified, but not both.
	case in.GetSingleChanBackups() != nil && in.GetMultiChanBackup() != nil:
		return nil, errors.New("either a Single or Multi channel " +
			"backup must be specified, but not both")

	// If a Single is specified then we'll only accept one of them to allow
	// the caller to map the valid/invalid state for each individual Single.
	case in.GetSingleChanBackups() != nil:
		chanBackupsProtos := in.GetSingleChanBackups().ChanBackups
		if len(chanBackupsProtos) != 1 {
			return nil, errors.New("only one Single is accepted " +
				"at a time")
		}

		// First, we'll convert the raw byte slice into a type we can
		// work with a bit better.
		chanBackup := chanbackup.PackedSingles(
			[][]byte{chanBackupsProtos[0].ChanBackup},
		)

		// With our PackedSingles created, we'll attempt to unpack the
		// backup. If this fails, then we know the backup is invalid for
		// some reason.
		_, err := chanBackup.Unpack(r.server.cc.keyRing)
		if err != nil {
			return nil, fmt.Errorf("invalid single channel "+
				"backup: %v", err)
		}

	case in.GetMultiChanBackup() != nil:
		// We'll convert the raw byte slice into a PackedMulti that we
		// can easily work with.
		packedMultiBackup := in.GetMultiChanBackup().MultiChanBackup
		packedMulti := chanbackup.PackedMulti(packedMultiBackup)

		// We'll now attempt to unpack the Multi. If this fails, then we
		// know it's invalid.
		_, err := packedMulti.Unpack(r.server.cc.keyRing)
		if err != nil {
			return nil, fmt.Errorf("invalid multi channel backup: "+
				"%v", err)
		}
	}

	return &lnrpc.VerifyChanBackupResponse{}, nil
}

// createBackupSnapshot converts the passed Single backup into a snapshot which
// contains individual packed single backups, as well as a single packed multi
// backup.
func (r *rpcServer) createBackupSnapshot(backups []chanbackup.Single) (
	*lnrpc.ChanBackupSnapshot, error) {

	// Once we have the set of back ups, we'll attempt to pack them all
	// into a series of single channel backups.
	singleChanPackedBackups, err := chanbackup.PackStaticChanBackups(
		backups, r.server.cc.keyRing,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to pack set of chan "+
			"backups: %v", err)
	}

	// Now that we have our set of single packed backups, we'll morph that
	// into a form that the proto response requires.
	numBackups := len(singleChanPackedBackups)
	singleBackupResp := &lnrpc.ChannelBackups{
		ChanBackups: make([]*lnrpc.ChannelBackup, 0, numBackups),
	}
	for chanPoint, singlePackedBackup := range singleChanPackedBackups {
		txid := chanPoint.Hash
		rpcChanPoint := &lnrpc.ChannelPoint{
			FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
				FundingTxidBytes: txid[:],
			},
			OutputIndex: chanPoint.Index,
		}

		singleBackupResp.ChanBackups = append(
			singleBackupResp.ChanBackups,
			&lnrpc.ChannelBackup{
				ChanPoint:  rpcChanPoint,
				ChanBackup: singlePackedBackup,
			},
		)
	}

	// In addition, to the set of single chan backups, we'll also create a
	// single multi-channel backup which can be serialized into a single
	// file for safe storage.
	var b bytes.Buffer
	unpackedMultiBackup := chanbackup.Multi{
		StaticBackups: backups,
	}
	err = unpackedMultiBackup.PackToWriter(&b, r.server.cc.keyRing)
	if err != nil {
		return nil, fmt.Errorf("unable to multi-pack backups: %v", err)
	}

	multiBackupResp := &lnrpc.MultiChanBackup{
		MultiChanBackup: b.Bytes(),
	}
	for _, singleBackup := range singleBackupResp.ChanBackups {
		multiBackupResp.ChanPoints = append(
			multiBackupResp.ChanPoints, singleBackup.ChanPoint,
		)
	}

	return &lnrpc.ChanBackupSnapshot{
		SingleChanBackups: singleBackupResp,
		MultiChanBackup:   multiBackupResp,
	}, nil
}

// ExportAllChannelBackups returns static channel backups for all existing
// channels known to lnd. A set of regular singular static channel backups for
// each channel are returned. Additionally, a multi-channel backup is returned
// as well, which contains a single encrypted blob containing the backups of
// each channel.
func (r *rpcServer) ExportAllChannelBackups(ctx context.Context,
	in *lnrpc.ChanBackupExportRequest) (*lnrpc.ChanBackupSnapshot, error) {

	// First, we'll attempt to read back ups for ALL currently opened
	// channels from disk.
	allUnpackedBackups, err := chanbackup.FetchStaticChanBackups(
		r.server.chanDB,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch all static chan "+
			"backups: %v", err)
	}

	// With the backups assembled, we'll create a full snapshot.
	return r.createBackupSnapshot(allUnpackedBackups)
}

// RestoreChannelBackups accepts a set of singular channel backups, or a single
// encrypted multi-chan backup and attempts to recover any funds remaining
// within the channel. If we're able to unpack the backup, then the new channel
// will be shown under listchannels, as well as pending channels.
func (r *rpcServer) RestoreChannelBackups(ctx context.Context,
	in *lnrpc.RestoreChanBackupRequest) (*lnrpc.RestoreBackupResponse, error) {

	// First, we'll make our implementation of the
	// chanbackup.ChannelRestorer interface which we'll use to properly
	// restore either a set of chanbackup.Single or chanbackup.Multi
	// backups.
	chanRestorer := &chanDBRestorer{
		db:         r.server.chanDB,
		secretKeys: r.server.cc.keyRing,
		chainArb:   r.server.chainArb,
	}

	// We'll accept either a list of Single backups, or a single Multi
	// backup which contains several single backups.
	switch {
	case in.GetChanBackups() != nil:
		chanBackupsProtos := in.GetChanBackups()

		// Now that we know what type of backup we're working with,
		// we'll parse them all out into a more suitable format.
		packedBackups := make([][]byte, 0, len(chanBackupsProtos.ChanBackups))
		for _, chanBackup := range chanBackupsProtos.ChanBackups {
			packedBackups = append(
				packedBackups, chanBackup.ChanBackup,
			)
		}

		// With our backups obtained, we'll now restore them which will
		// write the new backups to disk, and then attempt to connect
		// out to any peers that we know of which were our prior
		// channel peers.
		err := chanbackup.UnpackAndRecoverSingles(
			chanbackup.PackedSingles(packedBackups),
			r.server.cc.keyRing, chanRestorer, r.server,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to unpack single "+
				"backups: %v", err)
		}

	case in.GetMultiChanBackup() != nil:
		packedMultiBackup := in.GetMultiChanBackup()

		// With our backups obtained, we'll now restore them which will
		// write the new backups to disk, and then attempt to connect
		// out to any peers that we know of which were our prior
		// channel peers.
		packedMulti := chanbackup.PackedMulti(packedMultiBackup)
		err := chanbackup.UnpackAndRecoverMulti(
			packedMulti, r.server.cc.keyRing, chanRestorer,
			r.server,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to unpack chan "+
				"backup: %v", err)
		}
	}

	return &lnrpc.RestoreBackupResponse{}, nil
}

// SubscribeChannelBackups allows a client to sub-subscribe to the most up to
// date information concerning the state of all channel back ups. Each time a
// new channel is added, we return the new set of channels, along with a
// multi-chan backup containing the backup info for all channels. Each time a
// channel is closed, we send a new update, which contains new new chan back
// ups, but the updated set of encrypted multi-chan backups with the closed
// channel(s) removed.
func (r *rpcServer) SubscribeChannelBackups(req *lnrpc.ChannelBackupSubscription,
	updateStream lnrpc.Lightning_SubscribeChannelBackupsServer) error {

	// First, we'll subscribe to the primary channel notifier so we can
	// obtain events for new opened/closed channels.
	chanSubscription, err := r.server.channelNotifier.SubscribeChannelEvents()
	if err != nil {
		return err
	}

	defer chanSubscription.Cancel()
	for {
		select {
		// A new event has been sent by the channel notifier, we'll
		// assemble, then sling out a new event to the client.
		case e := <-chanSubscription.Updates():
			// TODO(roasbeef): batch dispatch ntnfs

			switch e.(type) {

			// We only care about new/closed channels, so we'll
			// skip any events for active/inactive channels.
			case channelnotifier.ActiveChannelEvent:
				continue
			case channelnotifier.InactiveChannelEvent:
				continue
			}

			// Now that we know the channel state has changed,
			// we'll obtains the current set of single channel
			// backups from disk.
			chanBackups, err := chanbackup.FetchStaticChanBackups(
				r.server.chanDB,
			)
			if err != nil {
				return fmt.Errorf("unable to fetch all "+
					"static chan backups: %v", err)
			}

			// With our backups obtained, we'll pack them into a
			// snapshot and send them back to the client.
			backupSnapshot, err := r.createBackupSnapshot(
				chanBackups,
			)
			if err != nil {
				return err
			}
			err = updateStream.Send(backupSnapshot)
			if err != nil {
				return err
			}

		case <-r.quit:
			return nil
		}
	}
}

// chanAcceptInfo is used in the ChannelAcceptor bidirectional stream and
// encapsulates the request information sent from the RPCAcceptor to the
// RPCServer.
type chanAcceptInfo struct {
	chanReq      *chanacceptor.ChannelAcceptRequest
	responseChan chan bool
}

// ChannelAcceptor dispatches a bi-directional streaming RPC in which
// OpenChannel requests are sent to the client and the client responds with
// a boolean that tells LND whether or not to accept the channel. This allows
// node operators to specify their own criteria for accepting inbound channels
// through a single persistent connection.
func (r *rpcServer) ChannelAcceptor(stream lnrpc.Lightning_ChannelAcceptorServer) error {
	chainedAcceptor := r.chanPredicate

	// Create two channels to handle requests and responses respectively.
	newRequests := make(chan *chanAcceptInfo)
	responses := make(chan lnrpc.ChannelAcceptResponse)

	// Define a quit channel that will be used to signal to the RPCAcceptor's
	// closure whether the stream still exists.
	quit := make(chan struct{})
	defer close(quit)

	// demultiplexReq is a closure that will be passed to the RPCAcceptor and
	// acts as an intermediary between the RPCAcceptor and the RPCServer.
	demultiplexReq := func(req *chanacceptor.ChannelAcceptRequest) bool {
		respChan := make(chan bool, 1)

		newRequest := &chanAcceptInfo{
			chanReq:      req,
			responseChan: respChan,
		}

		// timeout is the time after which ChannelAcceptRequests expire.
		timeout := time.After(defaultAcceptorTimeout)

		// Send the request to the newRequests channel.
		select {
		case newRequests <- newRequest:
		case <-timeout:
			rpcsLog.Errorf("RPCAcceptor returned false - reached timeout of %d",
				defaultAcceptorTimeout)
			return false
		case <-quit:
			return false
		case <-r.quit:
			return false
		}

		// Receive the response and return it. If no response has been received
		// in defaultAcceptorTimeout, then return false.
		select {
		case resp := <-respChan:
			return resp
		case <-timeout:
			rpcsLog.Errorf("RPCAcceptor returned false - reached timeout of %d",
				defaultAcceptorTimeout)
			return false
		case <-quit:
			return false
		case <-r.quit:
			return false
		}
	}

	// Create a new RPCAcceptor via the NewRPCAcceptor method.
	rpcAcceptor := chanacceptor.NewRPCAcceptor(demultiplexReq)

	// Add the RPCAcceptor to the ChainedAcceptor and defer its removal.
	id := chainedAcceptor.AddAcceptor(rpcAcceptor)
	defer chainedAcceptor.RemoveAcceptor(id)

	// errChan is used by the receive loop to signal any errors that occur
	// during reading from the stream. This is primarily used to shutdown the
	// send loop in the case of an RPC client disconnecting.
	errChan := make(chan error, 1)

	// We need to have the stream.Recv() in a goroutine since the call is
	// blocking and would prevent us from sending more ChannelAcceptRequests to
	// the RPC client.
	go func() {
		for {
			resp, err := stream.Recv()
			if err != nil {
				errChan <- err
				return
			}

			var pendingID [32]byte
			copy(pendingID[:], resp.PendingChanId)

			openChanResp := lnrpc.ChannelAcceptResponse{
				Accept:        resp.Accept,
				PendingChanId: pendingID[:],
			}

			// Now that we have the response from the RPC client, send it to
			// the responses chan.
			select {
			case responses <- openChanResp:
			case <-quit:
				return
			case <-r.quit:
				return
			}
		}
	}()

	acceptRequests := make(map[[32]byte]chan bool)

	for {
		select {
		case newRequest := <-newRequests:

			req := newRequest.chanReq
			pendingChanID := req.OpenChanMsg.PendingChannelID

			acceptRequests[pendingChanID] = newRequest.responseChan

			// A ChannelAcceptRequest has been received, send it to the client.
			chanAcceptReq := &lnrpc.ChannelAcceptRequest{
				NodePubkey:       req.Node.SerializeCompressed(),
				ChainHash:        req.OpenChanMsg.ChainHash[:],
				PendingChanId:    req.OpenChanMsg.PendingChannelID[:],
				FundingAmt:       uint64(req.OpenChanMsg.FundingAmount),
				PushAmt:          uint64(req.OpenChanMsg.PushAmount),
				DustLimit:        uint64(req.OpenChanMsg.DustLimit),
				MaxValueInFlight: uint64(req.OpenChanMsg.MaxValueInFlight),
				ChannelReserve:   uint64(req.OpenChanMsg.ChannelReserve),
				MinHtlc:          uint64(req.OpenChanMsg.HtlcMinimum),
				FeePerKb:         uint64(req.OpenChanMsg.FeePerKiloByte),
				CsvDelay:         uint32(req.OpenChanMsg.CsvDelay),
				MaxAcceptedHtlcs: uint32(req.OpenChanMsg.MaxAcceptedHTLCs),
				ChannelFlags:     uint32(req.OpenChanMsg.ChannelFlags),
			}

			if err := stream.Send(chanAcceptReq); err != nil {
				return err
			}
		case resp := <-responses:
			// Look up the appropriate channel to send on given the pending ID.
			// If a channel is found, send the response over it.
			var pendingID [32]byte
			copy(pendingID[:], resp.PendingChanId)
			respChan, ok := acceptRequests[pendingID]
			if !ok {
				continue
			}

			// Send the response boolean over the buffered response channel.
			respChan <- resp.Accept

			// Delete the channel from the acceptRequests map.
			delete(acceptRequests, pendingID)
		case err := <-errChan:
			rpcsLog.Errorf("Received an error: %v, shutting down", err)
			return err
		case <-r.quit:
			return fmt.Errorf("RPC server is shutting down")
		}
	}
}

// BakeMacaroon allows the creation of a new macaroon with custom read and write
// permissions. No first-party caveats are added since this can be done offline.
func (r *rpcServer) BakeMacaroon(ctx context.Context,
	req *lnrpc.BakeMacaroonRequest) (*lnrpc.BakeMacaroonResponse, error) {

	rpcsLog.Debugf("[bakemacaroon]")

	// If the --no-macaroons flag is used to start lnd, the macaroon service
	// is not initialized. Therefore we can't bake new macaroons.
	if r.macService == nil {
		return nil, fmt.Errorf("macaroon authentication disabled, " +
			"remove --no-macaroons flag to enable")
	}

	helpMsg := fmt.Sprintf("supported actions are %v, supported entities "+
		"are %v", validActions, validEntities)

	// Don't allow empty permission list as it doesn't make sense to have
	// a macaroon that is not allowed to access any RPC.
	if len(req.Permissions) == 0 {
		return nil, fmt.Errorf("permission list cannot be empty. "+
			"specify at least one action/entity pair. %s", helpMsg)
	}

	// Validate and map permission struct used by gRPC to the one used by
	// the bakery.
	requestedPermissions := make([]bakery.Op, len(req.Permissions))
	for idx, op := range req.Permissions {
		if !stringInSlice(op.Action, validActions) {
			return nil, fmt.Errorf("invalid permission action. %s",
				helpMsg)
		}
		if !stringInSlice(op.Entity, validEntities) {
			return nil, fmt.Errorf("invalid permission entity. %s",
				helpMsg)
		}

		requestedPermissions[idx] = bakery.Op{
			Entity: op.Entity,
			Action: op.Action,
		}
	}

	// Bake new macaroon with the given permissions and send it binary
	// serialized and hex encoded to the client.
	newMac, err := r.macService.Oven.NewMacaroon(
		ctx, bakery.LatestVersion, nil, requestedPermissions...,
	)
	if err != nil {
		return nil, err
	}
	newMacBytes, err := newMac.M().MarshalBinary()
	if err != nil {
		return nil, err
	}
	resp := &lnrpc.BakeMacaroonResponse{}
	resp.Macaroon = hex.EncodeToString(newMacBytes)

	return resp, nil
}
