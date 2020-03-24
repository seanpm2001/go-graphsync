package graphsync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	ipldfree "github.com/ipld/go-ipld-prime/impl/free"

	cidlink "github.com/ipld/go-ipld-prime/linking/cid"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-datastore"
	dss "github.com/ipfs/go-datastore/sync"
	bstore "github.com/ipfs/go-ipfs-blockstore"
	chunker "github.com/ipfs/go-ipfs-chunker"
	offline "github.com/ipfs/go-ipfs-exchange-offline"
	files "github.com/ipfs/go-ipfs-files"
	ipldformat "github.com/ipfs/go-ipld-format"
	"github.com/ipfs/go-merkledag"
	unixfile "github.com/ipfs/go-unixfs/file"
	"github.com/ipfs/go-unixfs/importer/balanced"
	ihelper "github.com/ipfs/go-unixfs/importer/helpers"

	"github.com/ipfs/go-graphsync"

	"github.com/ipfs/go-graphsync/ipldutil"
	gsmsg "github.com/ipfs/go-graphsync/message"
	gsnet "github.com/ipfs/go-graphsync/network"
	"github.com/ipfs/go-graphsync/testutil"
	ipld "github.com/ipld/go-ipld-prime"
	ipldselector "github.com/ipld/go-ipld-prime/traversal/selector"
	"github.com/ipld/go-ipld-prime/traversal/selector/builder"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	mocknet "github.com/libp2p/go-libp2p/p2p/net/mock"
)

func TestMakeRequestToNetwork(t *testing.T) {
	// create network
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	td := newGsTestData(ctx, t)
	r := &receiver{
		messageReceived: make(chan receivedMessage),
	}
	td.gsnet2.SetDelegate(r)
	graphSync := td.GraphSyncHost1()

	blockChainLength := 100
	blockChain := testutil.SetupBlockChain(ctx, t, td.loader1, td.storer1, 100, blockChainLength)

	requestCtx, requestCancel := context.WithCancel(ctx)
	defer requestCancel()
	graphSync.Request(requestCtx, td.host2.ID(), blockChain.TipLink, blockChain.Selector(), td.extension)

	var message receivedMessage
	select {
	case <-ctx.Done():
		t.Fatal("did not receive message sent")
	case message = <-r.messageReceived:
	}

	sender := message.sender
	if sender != td.host1.ID() {
		t.Fatal("received message from wrong node")
	}

	received := message.message
	receivedRequests := received.Requests()
	if len(receivedRequests) != 1 {
		t.Fatal("Did not add request to received message")
	}
	receivedRequest := receivedRequests[0]
	receivedSpec := receivedRequest.Selector()
	if !reflect.DeepEqual(blockChain.Selector(), receivedSpec) {
		t.Fatal("did not transmit selector spec correctly")
	}
	_, err := ipldutil.ParseSelector(receivedSpec)
	if err != nil {
		t.Fatal("did not receive parsible selector on other side")
	}

	returnedData, found := receivedRequest.Extension(td.extensionName)
	if !found || !reflect.DeepEqual(td.extensionData, returnedData) {
		t.Fatal("Failed to encode extension")
	}
}

func TestSendResponseToIncomingRequest(t *testing.T) {
	// create network
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	td := newGsTestData(ctx, t)
	r := &receiver{
		messageReceived: make(chan receivedMessage),
	}
	td.gsnet1.SetDelegate(r)

	var receivedRequestData []byte
	// initialize graphsync on second node to response to requests
	gsnet := td.GraphSyncHost2()
	err := gsnet.RegisterRequestReceivedHook(
		func(p peer.ID, requestData graphsync.RequestData, hookActions graphsync.RequestReceivedHookActions) {
			var has bool
			receivedRequestData, has = requestData.Extension(td.extensionName)
			if !has {
				t.Fatal("did not have expected extension")
			}
			hookActions.SendExtensionData(td.extensionResponse)
		},
	)
	if err != nil {
		t.Fatal("error registering extension")
	}

	blockChainLength := 100
	blockChain := testutil.SetupBlockChain(ctx, t, td.loader2, td.storer2, 100, blockChainLength)

	requestID := graphsync.RequestID(rand.Int31())

	message := gsmsg.New()
	message.AddRequest(gsmsg.NewRequest(requestID, blockChain.TipLink.(cidlink.Link).Cid, blockChain.Selector(), graphsync.Priority(math.MaxInt32), td.extension))
	// send request across network
	td.gsnet1.SendMessage(ctx, td.host2.ID(), message)
	// read the values sent back to requestor
	var received gsmsg.GraphSyncMessage
	var receivedBlocks []blocks.Block
	var receivedExtensions [][]byte
readAllMessages:
	for {
		select {
		case <-ctx.Done():
			t.Fatal("did not receive complete response")
		case message := <-r.messageReceived:
			sender := message.sender
			if sender != td.host2.ID() {
				t.Fatal("received message from wrong node")
			}

			received = message.message
			receivedBlocks = append(receivedBlocks, received.Blocks()...)
			receivedResponses := received.Responses()
			receivedExtension, found := receivedResponses[0].Extension(td.extensionName)
			if found {
				receivedExtensions = append(receivedExtensions, receivedExtension)
			}
			if len(receivedResponses) != 1 {
				t.Fatal("Did not receive response")
			}
			if receivedResponses[0].RequestID() != requestID {
				t.Fatal("Sent response for incorrect request id")
			}
			if receivedResponses[0].Status() != graphsync.PartialResponse {
				break readAllMessages
			}
		}
	}

	if len(receivedBlocks) != blockChainLength {
		t.Fatal("Send incorrect number of blocks or there were duplicate blocks")
	}

	if !reflect.DeepEqual(td.extensionData, receivedRequestData) {
		t.Fatal("did not receive correct request extension data")
	}

	if len(receivedExtensions) != 1 {
		t.Fatal("should have sent extension responses but didn't")
	}

	if !reflect.DeepEqual(receivedExtensions[0], td.extensionResponseData) {
		t.Fatal("did not return correct extension data")
	}
}

func TestGraphsyncRoundTrip(t *testing.T) {
	// create network
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	td := newGsTestData(ctx, t)

	// initialize graphsync on first node to make requests
	requestor := td.GraphSyncHost1()

	// setup receiving peer to just record message coming in
	blockChainLength := 100
	blockChain := testutil.SetupBlockChain(ctx, t, td.loader2, td.storer2, 100, blockChainLength)

	// initialize graphsync on second node to response to requests
	responder := td.GraphSyncHost2()

	var receivedResponseData []byte
	var receivedRequestData []byte

	err := requestor.RegisterResponseReceivedHook(
		func(p peer.ID, responseData graphsync.ResponseData) error {
			data, has := responseData.Extension(td.extensionName)
			if has {
				receivedResponseData = data
			}
			return nil
		})
	if err != nil {
		t.Fatal("Error setting up extension")
	}

	err = responder.RegisterRequestReceivedHook(func(p peer.ID, requestData graphsync.RequestData, hookActions graphsync.RequestReceivedHookActions) {
		var has bool
		receivedRequestData, has = requestData.Extension(td.extensionName)
		if !has {
			hookActions.TerminateWithError(errors.New("Missing extension"))
		} else {
			hookActions.SendExtensionData(td.extensionResponse)
		}
	})

	if err != nil {
		t.Fatal("Error setting up extension")
	}

	progressChan, errChan := requestor.Request(ctx, td.host2.ID(), blockChain.TipLink, blockChain.Selector(), td.extension)

	responses := testutil.CollectResponses(ctx, t, progressChan)
	errs := testutil.CollectErrors(ctx, t, errChan)

	if len(responses) != blockChainLength*2 {
		t.Fatal("did not traverse all nodes")
	}
	if len(errs) != 0 {
		t.Fatal("errors during traverse")
	}
	if len(td.blockStore1) != blockChainLength {
		t.Fatal("did not store all blocks")
	}

	expectedPath := ""
	for i, response := range responses {
		if response.Path.String() != expectedPath {
			t.Fatal("incorrect path")
		}
		if i%2 == 0 {
			if expectedPath == "" {
				expectedPath = "Parents"
			} else {
				expectedPath = expectedPath + "/Parents"
			}
		} else {
			expectedPath = expectedPath + "/0"
		}
	}

	// verify extension roundtrip
	if !reflect.DeepEqual(receivedRequestData, td.extensionData) {
		t.Fatal("did not receive correct extension request data")
	}

	if !reflect.DeepEqual(receivedResponseData, td.extensionResponseData) {
		t.Fatal("did not receive correct extension response data")
	}
}

// TestRoundTripLargeBlocksSlowNetwork test verifies graphsync continues to work
// under a specific of adverse conditions:
// -- large blocks being returned by a query
// -- slow network connection
// It verifies that Graphsync will properly break up network message packets
// so they can still be decoded on the client side, instead of building up a huge
// backlog of blocks and then sending them in one giant network packet that can't
// be decoded on the client side
func TestRoundTripLargeBlocksSlowNetwork(t *testing.T) {
	// create network
	if testing.Short() {
		t.Skip()
	}
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	td := newGsTestData(ctx, t)
	td.mn.SetLinkDefaults(mocknet.LinkOptions{Latency: 100 * time.Millisecond, Bandwidth: 3000000})

	// initialize graphsync on first node to make requests
	requestor := td.GraphSyncHost1()

	// setup receiving peer to just record message coming in
	blockChainLength := 40
	blockChain := testutil.SetupBlockChain(ctx, t, td.loader1, td.storer2, 200000, blockChainLength)

	// initialize graphsync on second node to response to requests
	td.GraphSyncHost2()

	progressChan, errChan := requestor.Request(ctx, td.host2.ID(), blockChain.TipLink, blockChain.Selector())

	responses := testutil.CollectResponses(ctx, t, progressChan)
	errs := testutil.CollectErrors(ctx, t, errChan)

	if len(responses) != blockChainLength*2 {
		t.Fatal("did not traverse all nodes")
	}
	if len(errs) != 0 {
		t.Fatal("errors during traverse")
	}
}

// What this test does:
// - Construct a blockstore + dag service
// - Import a file to UnixFS v1
// - setup a graphsync request from one node to the other
// for the file
// - Load the file from the new block store on the other node
// using the
// existing UnixFS v1 file reader
// - Verify the bytes match the original
func TestUnixFSFetch(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	const unixfsChunkSize uint64 = 1 << 10
	const unixfsLinksPerLevel = 1024

	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	makeLoader := func(bs bstore.Blockstore) ipld.Loader {
		return func(lnk ipld.Link, lnkCtx ipld.LinkContext) (io.Reader, error) {
			c, ok := lnk.(cidlink.Link)
			if !ok {
				return nil, errors.New("Incorrect Link Type")
			}
			// read block from one store
			block, err := bs.Get(c.Cid)
			if err != nil {
				return nil, err
			}
			return bytes.NewReader(block.RawData()), nil
		}
	}

	makeStorer := func(bs bstore.Blockstore) ipld.Storer {
		return func(lnkCtx ipld.LinkContext) (io.Writer, ipld.StoreCommitter, error) {
			var buf bytes.Buffer
			var committer ipld.StoreCommitter = func(lnk ipld.Link) error {
				c, ok := lnk.(cidlink.Link)
				if !ok {
					return errors.New("Incorrect Link Type")
				}
				block, err := blocks.NewBlockWithCid(buf.Bytes(), c.Cid)
				if err != nil {
					return err
				}
				return bs.Put(block)
			}
			return &buf, committer, nil
		}
	}
	// make a blockstore and dag service
	bs1 := bstore.NewBlockstore(dss.MutexWrap(datastore.NewMapDatastore()))

	// make a second blockstore
	bs2 := bstore.NewBlockstore(dss.MutexWrap(datastore.NewMapDatastore()))
	dagService2 := merkledag.NewDAGService(blockservice.New(bs2, offline.Exchange(bs2)))

	// read in a fixture file
	path, err := filepath.Abs(filepath.Join("fixtures", "lorem.txt"))
	if err != nil {
		t.Fatal("unable to create path for fixture file")
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal("unable to open fixture file")
	}
	var buf bytes.Buffer
	tr := io.TeeReader(f, &buf)
	file := files.NewReaderFile(tr)

	// import to UnixFS
	bufferedDS := ipldformat.NewBufferedDAG(ctx, dagService2)

	params := ihelper.DagBuilderParams{
		Maxlinks:   unixfsLinksPerLevel,
		RawLeaves:  true,
		CidBuilder: nil,
		Dagserv:    bufferedDS,
	}

	db, err := params.New(chunker.NewSizeSplitter(file, int64(unixfsChunkSize)))
	if err != nil {
		t.Fatal("unable to setup dag builder")
	}
	nd, err := balanced.Layout(db)
	if err != nil {
		t.Fatal("unable to create unix fs node")
	}
	err = bufferedDS.Commit()
	if err != nil {
		t.Fatal("unable to commit unix fs node")
	}

	// save the original files bytes
	origBytes := buf.Bytes()

	// setup an IPLD loader/storer for blockstore 1
	loader1 := makeLoader(bs1)
	storer1 := makeStorer(bs1)

	// setup an IPLD loader/storer for blockstore 2
	loader2 := makeLoader(bs2)
	storer2 := makeStorer(bs2)

	td := newGsTestData(ctx, t)
	requestor := New(ctx, td.gsnet1, loader1, storer1)
	responder := New(ctx, td.gsnet2, loader2, storer2)
	extensionName := graphsync.ExtensionName("Free for all")
	responder.RegisterRequestReceivedHook(func(p peer.ID, requestData graphsync.RequestData, hookActions graphsync.RequestReceivedHookActions) {
		hookActions.ValidateRequest()
		hookActions.SendExtensionData(graphsync.ExtensionData{
			Name: extensionName,
			Data: nil,
		})
	})
	// make a go-ipld-prime link for the root UnixFS node
	clink := cidlink.Link{Cid: nd.Cid()}

	// create a selector for the whole UnixFS dag
	ssb := builder.NewSelectorSpecBuilder(ipldfree.NodeBuilder())

	allSelector := ssb.ExploreRecursive(ipldselector.RecursionLimitNone(),
		ssb.ExploreAll(ssb.ExploreRecursiveEdge())).Node()

	// execute the traversal
	progressChan, errChan := requestor.Request(ctx, td.host2.ID(), clink, allSelector,
		graphsync.ExtensionData{
			Name: extensionName,
			Data: nil,
		})

	_ = testutil.CollectResponses(ctx, t, progressChan)
	responseErrors := testutil.CollectErrors(ctx, t, errChan)

	// verify traversal was successful
	if len(responseErrors) != 0 {
		t.Fatal("Response should be successful but wasn't")
	}

	// setup a DagService for the second block store
	dagService1 := merkledag.NewDAGService(blockservice.New(bs1, offline.Exchange(bs1)))

	// load the root of the UnixFS DAG from the new blockstore
	otherNode, err := dagService1.Get(ctx, nd.Cid())
	if err != nil {
		t.Fatal("should have been able to read received root node but didn't")
	}

	// Setup a UnixFS file reader
	n, err := unixfile.NewUnixfsFile(ctx, dagService1, otherNode)
	if err != nil {
		t.Fatal("should have been able to setup UnixFS file but wasn't")
	}

	fn, ok := n.(files.File)
	if !ok {
		t.Fatal("file should be a regular file, but wasn't")
	}

	// Read the bytes for the UnixFS File
	finalBytes, err := ioutil.ReadAll(fn)
	if err != nil {
		t.Fatal("should have been able to read all of unix FS file but wasn't")
	}

	// verify original bytes match final bytes!
	if !reflect.DeepEqual(origBytes, finalBytes) {
		t.Fatal("should have gotten same bytes written as read but didn't")
	}

}

type gsTestData struct {
	mn                       mocknet.Mocknet
	ctx                      context.Context
	host1                    host.Host
	host2                    host.Host
	gsnet1                   gsnet.GraphSyncNetwork
	gsnet2                   gsnet.GraphSyncNetwork
	blockStore1, blockStore2 map[ipld.Link][]byte
	loader1, loader2         ipld.Loader
	storer1, storer2         ipld.Storer
	extensionData            []byte
	extensionName            graphsync.ExtensionName
	extension                graphsync.ExtensionData
	extensionResponseData    []byte
	extensionResponse        graphsync.ExtensionData
}

func newGsTestData(ctx context.Context, t *testing.T) *gsTestData {
	td := &gsTestData{ctx: ctx}
	td.mn = mocknet.New(ctx)
	var err error
	// setup network
	td.host1, err = td.mn.GenPeer()
	if err != nil {
		t.Fatal("error generating host")
	}
	td.host2, err = td.mn.GenPeer()
	if err != nil {
		t.Fatal("error generating host")
	}
	err = td.mn.LinkAll()
	if err != nil {
		t.Fatal("error linking hosts")
	}

	td.gsnet1 = gsnet.NewFromLibp2pHost(td.host1)
	td.gsnet2 = gsnet.NewFromLibp2pHost(td.host2)
	td.blockStore1 = make(map[ipld.Link][]byte)
	td.loader1, td.storer1 = testutil.NewTestStore(td.blockStore1)
	td.blockStore2 = make(map[ipld.Link][]byte)
	td.loader2, td.storer2 = testutil.NewTestStore(td.blockStore2)
	// setup extension handlers
	td.extensionData = testutil.RandomBytes(100)
	td.extensionName = graphsync.ExtensionName("AppleSauce/McGee")
	td.extension = graphsync.ExtensionData{
		Name: td.extensionName,
		Data: td.extensionData,
	}
	td.extensionResponseData = testutil.RandomBytes(100)
	td.extensionResponse = graphsync.ExtensionData{
		Name: td.extensionName,
		Data: td.extensionResponseData,
	}

	return td
}

func (td *gsTestData) GraphSyncHost1() graphsync.GraphExchange {
	return New(td.ctx, td.gsnet1, td.loader1, td.storer1)
}

func (td *gsTestData) GraphSyncHost2() graphsync.GraphExchange {

	return New(td.ctx, td.gsnet2, td.loader2, td.storer2)
}

type receivedMessage struct {
	message gsmsg.GraphSyncMessage
	sender  peer.ID
}

// Receiver is an interface for receiving messages from the GraphSyncNetwork.
type receiver struct {
	messageReceived chan receivedMessage
}

func (r *receiver) ReceiveMessage(
	ctx context.Context,
	sender peer.ID,
	incoming gsmsg.GraphSyncMessage) {

	select {
	case <-ctx.Done():
	case r.messageReceived <- receivedMessage{incoming, sender}:
	}
}

func (r *receiver) ReceiveError(err error) {
}

func (r *receiver) Connected(p peer.ID) {
}

func (r *receiver) Disconnected(p peer.ID) {
}