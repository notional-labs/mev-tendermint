package v0

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/fortytw2/leaktest"
	"github.com/go-kit/log/term"
	"github.com/gogo/protobuf/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tendermint/tendermint/abci/example/kvstore"
	abci "github.com/tendermint/tendermint/abci/types"
	cfg "github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/libs/log"
	tmrand "github.com/tendermint/tendermint/libs/rand"
	"github.com/tendermint/tendermint/mempool"
	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/p2p/mock"
	memproto "github.com/tendermint/tendermint/proto/tendermint/mempool"
	"github.com/tendermint/tendermint/proxy"
	"github.com/tendermint/tendermint/types"
)

const (
	numTxs  = 1000
	timeout = 120 * time.Second // ridiculously high because CircleCI is slow
)

type peerState struct {
	height int64
}

func (ps peerState) GetHeight() int64 {
	return ps.height
}

// Send a bunch of txs to the first reactor's mempool and wait for them all to
// be received in the others.
func TestReactorBroadcastTxsMessage(t *testing.T) {
	config := cfg.TestConfig()
	// if there were more than two reactors, the order of transactions could not be
	// asserted in waitForTxsOnReactors (due to transactions gossiping). If we
	// replace Connect2Switches (full mesh) with a func, which connects first
	// reactor to others and nothing else, this test should also pass with >2 reactors.
	const N = 2
	reactors := makeAndConnectReactors(config, N)
	defer func() {
		for _, r := range reactors {
			if err := r.Stop(); err != nil {
				assert.NoError(t, err)
			}
		}
	}()
	for _, r := range reactors {
		for _, peer := range r.Switch.Peers().List() {
			peer.Set(types.PeerStateKey, peerState{1})
		}
	}

	txs := checkTxs(t, reactors[0].mempool, numTxs, mempool.UnknownPeerID)
	waitForTxsOnReactors(t, txs, reactors, false)
}

func TestReactorBroadcastSidecarOnly(t *testing.T) {
	config := cfg.TestConfig()
	const N = 8
	reactors := makeAndConnectReactorsEvensSidecar(config, N)
	defer func() {
		for _, r := range reactors {
			if err := r.Stop(); err != nil {
				assert.NoError(t, err)
			}
		}
	}()
	for _, r := range reactors {
		for _, peer := range r.Switch.Peers().List() {
			peer.Set(types.PeerStateKey, peerState{1})
		}
	}
	txs := addNumBundlesToSidecar(t, reactors[0].sidecar, 5, 10, mempool.UnknownPeerID)
	time.Sleep(2000)
	reactors[0].sidecar.PrettyPrintBundles()
	waitForTxsOnReactors(t, txs, reactors[2:3], true)
	waitForTxsOnReactors(t, txs, reactors[4:5], true)
	waitForTxsOnReactors(t, txs, reactors[6:7], true)
	assert.Equal(t, 0, reactors[1].sidecar.Size())
	assert.Equal(t, 0, reactors[5].sidecar.Size())
	assert.Equal(t, 0, reactors[7].sidecar.Size())
	assert.Equal(t, 0, reactors[3].sidecar.Size())
}

// Send a bunch of txs to the first reactor's sidecar and wait for them all to
// be received in the others, IN THE RIGHT ORDER
func TestReactorBroadcastSidecarTxsMessage(t *testing.T) {
	config := cfg.TestConfig()
	const N = 2
	reactors := makeAndConnectReactors(config, N)
	defer func() {
		for _, r := range reactors {
			if err := r.Stop(); err != nil {
				assert.NoError(t, err)
			}
		}
	}()
	for _, r := range reactors {
		for _, peer := range r.Switch.Peers().List() {
			peer.Set(types.PeerStateKey, peerState{1})
		}
	}
	txs := addNumBundlesToSidecar(t, reactors[0].sidecar, 5, 10, mempool.UnknownPeerID)
	time.Sleep(2000)
	reactors[0].sidecar.PrettyPrintBundles()
	waitForTxsOnReactors(t, txs, reactors, true)
	reactors[1].sidecar.PrettyPrintBundles()
}

func TestReactorInsertOutOfOrderThenReap(t *testing.T) {
	config := cfg.TestConfig()
	const N = 2
	reactors := makeAndConnectReactors(config, N)
	defer func() {
		for _, r := range reactors {
			if err := r.Stop(); err != nil {
				assert.NoError(t, err)
			}
		}
	}()
	for _, r := range reactors {
		for _, peer := range r.Switch.Peers().List() {
			peer.Set(types.PeerStateKey, peerState{1})
		}
	}
	txs := addNumBundlesToSidecar(t, reactors[0].sidecar, 5, 10, mempool.UnknownPeerID)
	time.Sleep(2000)
	reactors[0].sidecar.PrettyPrintBundles()
	waitForTxsOnReactors(t, txs, reactors, true)
	reactors[1].sidecar.PrettyPrintBundles()
}

// regression test for https://github.com/tendermint/tendermint/issues/5408
func TestReactorConcurrency(t *testing.T) {
	config := cfg.TestConfig()
	const N = 2
	reactors := makeAndConnectReactors(config, N)
	defer func() {
		for _, r := range reactors {
			if err := r.Stop(); err != nil {
				assert.NoError(t, err)
			}
		}
	}()
	for _, r := range reactors {
		for _, peer := range r.Switch.Peers().List() {
			peer.Set(types.PeerStateKey, peerState{1})
		}
	}
	var wg sync.WaitGroup

	const numTxs = 5

	for i := 0; i < 1000; i++ {
		wg.Add(2)

		// 1. submit a bunch of txs
		// 2. update the whole mempool
		txs := checkTxs(t, reactors[0].mempool, numTxs, mempool.UnknownPeerID)
		go func() {
			defer wg.Done()

			reactors[0].mempool.Lock()
			defer reactors[0].mempool.Unlock()

			deliverTxResponses := make([]*abci.ResponseDeliverTx, len(txs))
			for i := range txs {
				deliverTxResponses[i] = &abci.ResponseDeliverTx{Code: 0}
			}
			err := reactors[0].mempool.Update(1, txs, deliverTxResponses, nil, nil)
			assert.NoError(t, err)
		}()

		// 1. submit a bunch of txs
		// 2. update none
		_ = checkTxs(t, reactors[1].mempool, numTxs, mempool.UnknownPeerID)
		go func() {
			defer wg.Done()

			reactors[1].mempool.Lock()
			defer reactors[1].mempool.Unlock()
			err := reactors[1].mempool.Update(1, []types.Tx{}, make([]*abci.ResponseDeliverTx, 0), nil, nil)
			assert.NoError(t, err)
		}()

		// 1. flush the mempool
		reactors[1].mempool.Flush()
	}

	wg.Wait()
}

// Send a bunch of txs to the first reactor's mempool, claiming it came from peer
// ensure peer gets no txs.
func TestReactorNoBroadcastToSender(t *testing.T) {
	config := cfg.TestConfig()
	const N = 2
	reactors := makeAndConnectReactors(config, N)
	defer func() {
		for _, r := range reactors {
			if err := r.Stop(); err != nil {
				assert.NoError(t, err)
			}
		}
	}()
	for _, r := range reactors {
		for _, peer := range r.Switch.Peers().List() {
			peer.Set(types.PeerStateKey, peerState{1})
		}
	}

	const peerID = 1
	checkTxs(t, reactors[0].mempool, numTxs, peerID)
	ensureNoTxs(t, reactors[peerID], 100*time.Millisecond)
}

func TestReactor_MaxTxBytes(t *testing.T) {
	config := cfg.TestConfig()

	const N = 2
	reactors := makeAndConnectReactors(config, N)
	defer func() {
		for _, r := range reactors {
			if err := r.Stop(); err != nil {
				assert.NoError(t, err)
			}
		}
	}()
	for _, r := range reactors {
		for _, peer := range r.Switch.Peers().List() {
			peer.Set(types.PeerStateKey, peerState{1})
		}
	}

	// Broadcast a tx, which has the max size
	// => ensure it's received by the second reactor.
	tx1 := tmrand.Bytes(config.Mempool.MaxTxBytes)
	err := reactors[0].mempool.CheckTx(tx1, nil, mempool.TxInfo{SenderID: mempool.UnknownPeerID})
	require.NoError(t, err)
	waitForTxsOnReactors(t, []types.Tx{tx1}, reactors, false)

	reactors[0].mempool.Flush()
	reactors[1].mempool.Flush()

	// Broadcast a tx, which is beyond the max size
	// => ensure it's not sent
	tx2 := tmrand.Bytes(config.Mempool.MaxTxBytes + 1)
	err = reactors[0].mempool.CheckTx(tx2, nil, mempool.TxInfo{SenderID: mempool.UnknownPeerID})
	require.Error(t, err)
}

func TestBroadcastTxForPeerStopsWhenPeerStops(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	config := cfg.TestConfig()
	const N = 2
	reactors := makeAndConnectReactors(config, N)
	defer func() {
		for _, r := range reactors {
			if err := r.Stop(); err != nil {
				assert.NoError(t, err)
			}
		}
	}()

	// stop peer
	sw := reactors[1].Switch
	sw.StopPeerForError(sw.Peers().List()[0], errors.New("some reason"))

	// check that we are not leaking any go-routines
	// i.e. broadcastTxRoutine finishes when peer is stopped
	leaktest.CheckTimeout(t, 10*time.Second)()
}

func TestBroadcastTxForPeerStopsWhenReactorStops(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	config := cfg.TestConfig()
	const N = 2
	reactors := makeAndConnectReactors(config, N)

	// stop reactors
	for _, r := range reactors {
		if err := r.Stop(); err != nil {
			assert.NoError(t, err)
		}
	}

	// check that we are not leaking any go-routines
	// i.e. broadcastTxRoutine finishes when reactor is stopped
	leaktest.CheckTimeout(t, 10*time.Second)()
}

func TestMempoolIDsBasic(t *testing.T) {
	ids := newMempoolIDs()

	peer := mock.NewPeer(net.IP{127, 0, 0, 1})

	ids.ReserveForPeer(peer)
	assert.EqualValues(t, 1, ids.GetForPeer(peer))
	ids.Reclaim(peer)

	ids.ReserveForPeer(peer)
	assert.EqualValues(t, 2, ids.GetForPeer(peer))
	ids.Reclaim(peer)
}

func TestMempoolIDsPanicsIfNodeRequestsOvermaxActiveIDs(t *testing.T) {
	if testing.Short() {
		return
	}

	// 0 is already reserved for UnknownPeerID
	ids := newMempoolIDs()

	for i := 0; i < mempool.MaxActiveIDs-1; i++ {
		peer := mock.NewPeer(net.IP{127, 0, 0, 1})
		ids.ReserveForPeer(peer)
	}

	assert.Panics(t, func() {
		peer := mock.NewPeer(net.IP{127, 0, 0, 1})
		ids.ReserveForPeer(peer)
	})
}

// TODO: This test tests that we don't panic and are able to generate new
// PeerIDs for each peer we add. It seems as though we should be able to test
// this in a much more direct way.
// https://github.com/tendermint/tendermint/issues/9639
func TestDontExhaustMaxActiveIDs(t *testing.T) {
	config := cfg.TestConfig()
	const N = 1
	reactors := makeAndConnectReactors(config, N)
	defer func() {
		for _, r := range reactors {
			if err := r.Stop(); err != nil {
				assert.NoError(t, err)
			}
		}
	}()
	reactor := reactors[0]

	for i := 0; i < mempool.MaxActiveIDs+1; i++ {
		peer := mock.NewPeer(nil)
		reactor.ReceiveEnvelope(p2p.Envelope{
			ChannelID: mempool.MempoolChannel,
			Src:       peer,
			Message:   &memproto.Message{}, // This uses the wrong message type on purpose to stop the peer as in an error state in the reactor.
		},
		)
		reactor.AddPeer(peer)
	}
}

func TestLegacyReactorReceiveBasic(t *testing.T) {
	config := cfg.TestConfig()
	const N = 1
	reactors := makeAndConnectReactors(config, N)
	var (
		reactor = reactors[0]
		peer    = mock.NewPeer(nil)
	)
	defer func() {
		err := reactor.Stop()
		assert.NoError(t, err)
	}()

	reactor.InitPeer(peer)
	reactor.AddPeer(peer)
	m := &memproto.Txs{}
	wm := m.Wrap()
	msg, err := proto.Marshal(wm)
	assert.NoError(t, err)

	assert.NotPanics(t, func() {
		reactor.Receive(mempool.MempoolChannel, peer, msg)
	})
}

func TestLegacyReactorReceiveSidecarMEVTxs(t *testing.T) {
	config := cfg.TestConfig()
	const N = 1
	reactors := makeAndConnectReactors(config, N)
	var (
		reactor = reactors[0]
		peer    = mock.NewPeer(nil)
	)
	defer func() {
		err := reactor.Stop()
		assert.NoError(t, err)
	}()

	reactor.InitPeer(peer)
	reactor.AddPeer(peer)
	txBytes := make([]byte, 20)
	m := &memproto.MEVTxs{
		Txs:           [][]byte{txBytes},
		DesiredHeight: 1,
		BundleId:      0,
		BundleOrder:   0,
		BundleSize:    1,
	}
	wm := m.Wrap()
	msg, err := proto.Marshal(wm)
	assert.NoError(t, err)
	assert.NotPanics(t, func() {
		reactor.Receive(mempool.SidecarLegacyChannel, peer, msg)
		reactor.Receive(mempool.SidecarChannel, peer, msg)
		waitForSidecarTxsOnReactor(t, []types.Tx{txBytes}, reactor, 0)
	})
}

func TestReactorReceiveSidecarMEVTxs(t *testing.T) {
	config := cfg.TestConfig()
	const N = 1
	reactors := makeAndConnectReactors(config, N)
	var (
		reactor = reactors[0]
		peer    = mock.NewPeer(nil)
	)
	defer func() {
		err := reactor.Stop()
		assert.NoError(t, err)
	}()

	reactor.InitPeer(peer)
	reactor.AddPeer(peer)
	txBytes := make([]byte, 20)
	m := &memproto.MEVTxs{
		Txs:           [][]byte{txBytes},
		DesiredHeight: 1,
		BundleId:      0,
		BundleOrder:   0,
		BundleSize:    1,
	}
	assert.NotPanics(t, func() {
		reactor.ReceiveEnvelope(p2p.Envelope{
			ChannelID: mempool.SidecarChannel,
			Src:       peer,
			Message:   m,
		})
		reactor.ReceiveEnvelope(p2p.Envelope{
			ChannelID: mempool.SidecarLegacyChannel,
			Src:       peer,
			Message:   m,
		})
		waitForSidecarTxsOnReactor(t, []types.Tx{txBytes}, reactor, 0)
	})
}

func TestReactorReceiveSidecarMEVMessage(t *testing.T) {
	config := cfg.TestConfig()
	const N = 1
	reactors := makeAndConnectReactors(config, N)
	var (
		reactor = reactors[0]
		peer    = mock.NewPeer(nil)
	)
	defer func() {
		err := reactor.Stop()
		assert.NoError(t, err)
	}()

	reactor.InitPeer(peer)
	reactor.AddPeer(peer)
	txBytes := make([]byte, 20)
	msg := &memproto.MEVMessage{
		Sum: &memproto.MEVMessage_Txs{
			Txs: &memproto.Txs{Txs: [][]byte{txBytes}},
		},
		DesiredHeight: 1,
		BundleId:      0,
		BundleOrder:   0,
		BundleSize:    1,
	}

	assert.NotPanics(t, func() {
		reactor.ReceiveEnvelope(p2p.Envelope{
			ChannelID: mempool.SidecarChannel,
			Src:       peer,
			Message:   msg,
		})
		reactor.ReceiveEnvelope(p2p.Envelope{
			ChannelID: mempool.SidecarLegacyChannel,
			Src:       peer,
			Message:   msg,
		})
		waitForSidecarTxsOnReactor(t, []types.Tx{txBytes}, reactor, 0)
	})
}

func TestLegacyReactorReceiveSidecarMEVMessage(t *testing.T) {
	config := cfg.TestConfig()
	const N = 1
	reactors := makeAndConnectReactors(config, N)
	var (
		reactor = reactors[0]
		peer    = mock.NewPeer(nil)
	)
	defer func() {
		err := reactor.Stop()
		assert.NoError(t, err)
	}()

	reactor.InitPeer(peer)
	reactor.AddPeer(peer)
	txBytes := make([]byte, 20)
	msg := &memproto.MEVMessage{
		Sum: &memproto.MEVMessage_Txs{
			Txs: &memproto.Txs{Txs: [][]byte{txBytes}},
		},
		DesiredHeight: 1,
		BundleId:      0,
		BundleOrder:   0,
		BundleSize:    1,
	}

	mm, err := proto.Marshal(msg)
	assert.NoError(t, err)
	assert.NotPanics(t, func() {
		reactor.Receive(mempool.SidecarLegacyChannel, peer, mm)
		reactor.Receive(mempool.SidecarChannel, peer, mm)
		fmt.Println(reactor.sidecar.Size())
		waitForSidecarTxsOnReactor(t, []types.Tx{txBytes}, reactor, 0)
	})
}

// mempoolLogger is a TestingLogger which uses a different
// color for each validator ("validator" key must exist).
func mempoolLogger() log.Logger {
	return log.TestingLoggerWithColorFn(func(keyvals ...interface{}) term.FgBgColor {
		for i := 0; i < len(keyvals)-1; i += 2 {
			if keyvals[i] == "validator" {
				return term.FgBgColor{Fg: term.Color(uint8(keyvals[i+1].(int) + 1))}
			}
		}
		return term.FgBgColor{}
	})
}

// connect N mempool reactors through N switches
// can add additional logic to set which ones should be treated as sidecar
// peers in p2p.Connect2Switches, including based on index
func makeAndConnectReactorsEvensSidecar(config *cfg.Config, n int) []*Reactor {
	reactors := make([]*Reactor, n)
	logger := mempoolLogger()
	for i := 0; i < n; i++ {
		app := kvstore.NewApplication()
		cc := proxy.NewLocalClientCreator(app)
		sidecar := mempool.NewCListSidecar(0, log.NewNopLogger(), mempool.NopMetrics())
		mempool, cleanup := newMempoolWithApp(cc)
		defer cleanup()

		reactors[i] = NewReactor(config.Mempool, mempool, sidecar) // so we dont start the consensus states
		reactors[i].SetLogger(logger.With("validator", i))
	}

	p2p.MakeConnectedSwitches(config.P2P, n, func(i int, s *p2p.Switch) *p2p.Switch {
		s.AddReactor("MEMPOOL", reactors[i])
		return s

	}, p2p.Connect2SwitchesEvensSidecar)
	return reactors
}

// connect N mempool reactors through N switches
func makeAndConnectReactors(config *cfg.Config, n int) []*Reactor {
	reactors := make([]*Reactor, n)
	logger := mempoolLogger()
	for i := 0; i < n; i++ {
		app := kvstore.NewApplication()
		cc := proxy.NewLocalClientCreator(app)
		sidecar := mempool.NewCListSidecar(0, log.NewNopLogger(), mempool.NopMetrics())
		mempool, cleanup := newMempoolWithApp(cc)
		defer cleanup()

		reactors[i] = NewReactor(config.Mempool, mempool, sidecar) // so we dont start the consensus states
		reactors[i].SetLogger(logger.With("validator", i))
	}

	p2p.MakeConnectedSwitches(config.P2P, n, func(i int, s *p2p.Switch) *p2p.Switch {
		s.AddReactor("MEMPOOL", reactors[i])
		return s

	}, p2p.Connect2Switches)
	return reactors
}

func waitForTxsOnReactors(t *testing.T, txs types.Txs, reactors []*Reactor, useSidecar bool) {
	// wait for the txs in all mempools
	wg := new(sync.WaitGroup)
	for i, reactor := range reactors {
		wg.Add(1)
		go func(r *Reactor, reactorIndex int) {
			defer wg.Done()
			if useSidecar {
				waitForSidecarTxsOnReactor(t, txs, r, reactorIndex)
			} else {
				waitForTxsOnReactor(t, txs, r, reactorIndex)
			}
		}(reactor, i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	timer := time.After(timeout)
	select {
	case <-timer:
		t.Fatal("Timed out waiting for txs")
	case <-done:
	}
}

func waitForTxsOnReactor(t *testing.T, txs types.Txs, reactor *Reactor, reactorIndex int) {
	mempool := reactor.mempool
	for mempool.Size() < len(txs) {
		time.Sleep(time.Millisecond * 100)
	}

	reapedTxs := mempool.ReapMaxTxs(len(txs))
	for i, tx := range txs {
		assert.Equalf(t, tx, reapedTxs[i],
			"txs at index %d on reactor %d don't match: %v vs %v", i, reactorIndex, tx, reapedTxs[i])
	}
}

func waitForSidecarTxsOnReactor(t *testing.T, txs types.Txs, reactor *Reactor, reactorIndex int) {
	sidecar := reactor.sidecar
	for sidecar.Size() < len(txs) {
		time.Sleep(time.Millisecond * 100)
	}

	reapedTxs := sidecar.ReapMaxTxs()
	var i int
	for _, scTx := range reapedTxs.Txs {
		assert.Equalf(t, txs[i], scTx,
			"txs at index %d on reactor %d don't match: %s vs %s", i, reactorIndex, txs[i], scTx)
		i++
	}
}

// ensure no txs on reactor after some timeout
func ensureNoTxs(t *testing.T, reactor *Reactor, timeout time.Duration) {
	time.Sleep(timeout) // wait for the txs in all mempools
	assert.Zero(t, reactor.mempool.Size())
}

func TestMempoolVectors(t *testing.T) {
	testCases := []struct {
		testName string
		tx       []byte
		expBytes string
	}{
		{"tx 1", []byte{123}, "0a030a017b"},
		{"tx 2", []byte("proto encoding in mempool"), "0a1b0a1970726f746f20656e636f64696e6720696e206d656d706f6f6c"},
	}

	for _, tc := range testCases {
		tc := tc

		msg := memproto.Message{
			Sum: &memproto.Message_Txs{
				Txs: &memproto.Txs{Txs: [][]byte{tc.tx}},
			},
		}
		bz, err := msg.Marshal()
		require.NoError(t, err, tc.testName)

		require.Equal(t, tc.expBytes, hex.EncodeToString(bz), tc.testName)
	}
}

// Sidecar testing utils

type testBundleInfo struct {
	BundleSize    int64
	DesiredHeight int64
	BundleID      int64
	PeerID        uint16
}

func addNumBundlesToSidecar(t *testing.T, sidecar mempool.PriorityTxSidecar, numBundles int, bundleSize int64, peerID uint16) types.Txs {
	totalTxsCount := 0
	txs := make(types.Txs, 0)
	for i := 0; i < numBundles; i++ {
		totalTxsCount += int(bundleSize)
		newTxs := createSidecarBundleAndTxs(t, sidecar, testBundleInfo{BundleSize: bundleSize,
			PeerID: mempool.UnknownPeerID, DesiredHeight: sidecar.HeightForFiringAuction(), BundleID: int64(i)})
		txs = append(txs, newTxs...)
	}
	return txs
}

func createSidecarBundleAndTxs(t *testing.T, sidecar mempool.PriorityTxSidecar, bInfo testBundleInfo) types.Txs {
	txs := make(types.Txs, bInfo.BundleSize)
	for i := 0; i < int(bInfo.BundleSize); i++ {
		txBytes := addTxToSidecar(t, sidecar, bInfo, int64(i))
		txs[i] = txBytes
	}
	return txs
}

func addTxToSidecar(t *testing.T, sidecar mempool.PriorityTxSidecar, bInfo testBundleInfo, bundleOrder int64) types.Tx {
	txInfo := mempool.TxInfo{SenderID: bInfo.PeerID, BundleSize: bInfo.BundleSize,
		BundleID: bInfo.BundleID, DesiredHeight: bInfo.DesiredHeight, BundleOrder: bundleOrder}
	txBytes := make([]byte, 20)
	_, err := rand.Read(txBytes)
	if err != nil {
		t.Error(err)
	}
	if err := sidecar.AddTx(txBytes, txInfo); err != nil {
		fmt.Println("Ignoring error in AddTx:", err)
	}
	return txBytes
}