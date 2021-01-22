package node

import (
	"bufio"
	"context"
	"fmt"
	"github.com/dennis-tra/pcp/pkg/commons"
	"github.com/libp2p/go-libp2p/p2p/discovery"
	"github.com/multiformats/go-multiaddr"
	"sync"
	"time"

	p2p "github.com/dennis-tra/pcp/pkg/pb"
	"github.com/golang/protobuf/proto"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/multiformats/go-varint"
)

// Node represents the construct to send messages to the
// receiving peer. Stream is a bufio.ReadWriter because
// in order to read an urvarint we need an io.ByteReader.
type Node struct {
	host.Host
	stream   *bufio.ReadWriter
	mdnsServ discovery.Service
}

// InitSending creates a new node connected to the given peer.
// Every subsequent calls to send will transmit messages to
// this peer.
func InitSending(ctx context.Context, pi peer.AddrInfo) (*Node, error) {

	priv, _, err := crypto.GenerateKeyPair(crypto.Secp256k1, 256)
	if err != nil {
		return nil, err
	}

	h, err := libp2p.New(ctx, libp2p.Identity(priv))
	if err != nil {
		return nil, err
	}

	err = h.Connect(ctx, pi)
	if err != nil {
		return nil, err
	}

	s, err := h.NewStream(ctx, pi.ID, commons.ServiceTag)
	if err != nil {
		return nil, err
	}

	rw := bufio.NewReadWriter(bufio.NewReader(s), bufio.NewWriter(s))

	node := &Node{
		Host:   h,
		stream: rw,
	}

	return node, nil
}

func InitReceiving(ctx context.Context, port int64) (*Node, error) {

	sourceAddr, err := multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", port))
	if err != nil {
		return nil, err
	}

	priv, _, err := crypto.GenerateKeyPair(crypto.Secp256k1, 256)
	if err != nil {
		return nil, err
	}

	h, err := libp2p.New(ctx, libp2p.ListenAddrs(sourceAddr), libp2p.Identity(priv))
	if err != nil {
		return nil, err
	}

	ser, err := discovery.NewMdnsService(ctx, h, time.Second, commons.ServiceTag)
	if err != nil {
		return nil, err
	}

	node := &Node{
		Host:     h,
		mdnsServ: ser,
	}

	return node, nil
}

func (n *Node) WaitForConnection() {
	var wg sync.WaitGroup
	wg.Add(1)

	n.SetStreamHandler(commons.ServiceTag, func(stream network.Stream) {
		rw := bufio.NewReadWriter(bufio.NewReader(stream), bufio.NewWriter(stream))
		n.stream = rw
		wg.Done()
	})

	wg.Wait()

	n.RemoveStreamHandler(commons.ServiceTag)
}

func (n *Node) Shutdown() error {
	err := n.Close()
	if err != nil {
		fmt.Println(err)
	}

	if n.mdnsServ != nil {
		err = n.mdnsServ.Close()
		if err != nil {
			fmt.Println(err)
		}
	}

	return nil
}

// Send takes the given proto message, marshals it to its binary
// format and transmits it to the peer that was given in the
// node initialization step. Since we have a streaming connection
// to our peer we need to make sure the messages are properly
// delimited. Here the size of the payload is transmitted first
// as a uvarint. This is read from the peer to determine how much
// data will follow.
func (n *Node) Send(msg *p2p.MessageData) error {

	// Transform msg to binary to calculate the signature.
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}

	key := n.Peerstore().PrivKey(n.ID())
	res, err := key.Sign(data)
	if err != nil {
		return err
	}
	msg.Signature = res

	// Transform msg + signature to binary.
	data, err = proto.Marshal(msg)
	if err != nil {
		return err
	}

	// First transmit the payload length.
	size := uint64(len(data))
	sizePayload := make([]byte, varint.UvarintSize(size))
	varint.PutUvarint(sizePayload, size)
	_, err = n.stream.Write(sizePayload)
	if err != nil {
		return err
	}

	// Then transmit the data.
	_, err = n.stream.Write(data)
	if err != nil {
		return err
	}

	err = n.stream.Flush()
	if err != nil {
		return err
	}

	return nil
}

//func (n *Node) SendBytes(data io.Reader) error{
//	_, err := io.Copy(n.stream, data)
//	return err
//}
//
//func (n *Node) ReceiveBytes() error {
//	data, err := ioutil.ReadAll(n.stream)
//}

// Receive blocks until the node has received a message from its peer.
// The first bytes it reads represent the expected message length encoded
// as an uvarint.
func (n *Node) Receive() (*p2p.MessageData, error) {
	size, err := varint.ReadUvarint(n.stream)
	if err != nil {
		return nil, err
	}

	payload := make([]byte, size)
	_, err = n.stream.Read(payload)
	if err != nil {
		return nil, err
	}

	var msg p2p.MessageData
	err = proto.Unmarshal(payload, &msg)
	if err != nil {
		return nil, err
	}

	isAuthentic, err := n.authenticateMessage(&msg)
	if err != nil {
		return nil, err
	} else if !isAuthentic {
		return nil, fmt.Errorf("message authenticity could not be verified")
	}

	return &msg, nil
}

func (n *Node) NewMessageData() (*p2p.MessageData, error) {
	// Add protobufs bin data for message author public key
	// this is useful for authenticating  messages forwarded by a node authored by another node
	nodePubKey, err := n.Peerstore().PubKey(n.ID()).Bytes()
	if err != nil {
		return nil, err
	}

	return &p2p.MessageData{
		NodeId:     peer.Encode(n.ID()),
		NodePubKey: nodePubKey,
		Timestamp:  time.Now().Unix(),
	}, nil
}

func (n *Node) authenticateMessage(data *p2p.MessageData) (bool, error) {
	// store a temp ref to signature and remove it from message data
	// sign is a string to allow easy reset to zero-value (empty string)
	signature := data.Signature
	data.Signature = nil

	// marshall data without the signature to protobufs3 binary format
	bin, err := proto.Marshal(data)
	if err != nil {
		return false, err
	}

	// restore sig in message data (for possible future use)
	data.Signature = signature

	// restore peer id binary format from base58 encoded node id data
	peerId, err := peer.Decode(data.NodeId)
	if err != nil {
		return false, err
	}

	key, err := crypto.UnmarshalPublicKey(data.NodePubKey)
	if err != nil {
		return false, fmt.Errorf("failed to extract key from message key data")
	}

	// extract node id from the provided public key
	idFromKey, err := peer.IDFromPublicKey(key)

	if err != nil {
		return false, fmt.Errorf("failed to extract peer id from public key")
	}

	// verify that message author node id matches the provided node public key
	if idFromKey != peerId {
		return false, fmt.Errorf("node id and provided public key mismatch")
	}

	return key.Verify(bin, signature)
}