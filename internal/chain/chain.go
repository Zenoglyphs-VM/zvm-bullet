// Package chain is the bot's view of the ZVM: reads through eth_call, and
// transactions from one key, one at a time.
//
// One at a time is deliberate. The bot is the escrow's only owner and every
// game action is a transaction from that one address, so two in flight would
// have to agree on nonces and on which lands first. Mainnet's bot had the same
// shape, and a game takes minutes while a transaction takes a momentum or two.
package chain

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// TipWei is the priority fee every transaction offers: zvm's own senders'
// default (rollup.DefaultTipWei). The relayer refuses anything under 50 gwei.
var TipWei = big.NewInt(100_000_000_000)

// ReceiptTimeout bounds the wait for one transaction. A relayed transaction
// lands one or two momentums after it is posted, about 15 s each on this
// devnet; five minutes covers a relayer that is catching up.
var ReceiptTimeout = 5 * time.Minute

type Client struct {
	Eth     *ethclient.Client
	RPC     *rpc.Client
	ChainID *big.Int

	key  *ecdsa.PrivateKey
	From common.Address
	mu   sync.Mutex
}

// Dial connects to a JSON-RPC endpoint. key may be nil for a read-only client.
func Dial(ctx context.Context, url string, key *ecdsa.PrivateKey) (*Client, error) {
	r, err := rpc.DialContext(ctx, url)
	if err != nil {
		return nil, err
	}
	eth := ethclient.NewClient(r)
	id, err := eth.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("chain id from %s: %w", url, err)
	}
	c := &Client{Eth: eth, RPC: r, ChainID: id, key: key}
	if key != nil {
		c.From = crypto.PubkeyToAddress(key.PublicKey)
	}
	return c, nil
}

// Call runs a read.
func (c *Client) Call(ctx context.Context, to common.Address, data []byte) ([]byte, error) {
	return c.Eth.CallContract(ctx, ethereum.CallMsg{From: c.From, To: &to, Data: data}, nil)
}

// RevertError is a transaction that estimation or execution refused, with the
// reason the contract gave when there is one.
type RevertError struct {
	Reason string
	Tx     common.Hash // zero when it failed at estimation, before sending
}

func (e *RevertError) Error() string {
	if e.Tx != (common.Hash{}) {
		return fmt.Sprintf("transaction %s reverted: %s", e.Tx, e.Reason)
	}
	return "reverted: " + e.Reason
}

// Send signs and sends one transaction and waits for its receipt. A
// transaction that would revert is caught at estimation and never sent, so a
// refused newGame costs nothing and its reason reaches the caller intact.
func (c *Client) Send(ctx context.Context, to *common.Address, value *big.Int, data []byte) (*types.Receipt, error) {
	if c.key == nil {
		return nil, errors.New("read-only client")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if value == nil {
		value = new(big.Int)
	}
	msg := ethereum.CallMsg{From: c.From, To: to, Value: value, Data: data}
	gas, err := c.Eth.EstimateGas(ctx, msg)
	if err != nil {
		return nil, asRevert(err, common.Hash{})
	}
	gas = gas * 13 / 10

	head, err := c.Eth.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, err
	}
	feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), TipWei)

	// The pending nonce comes from the relayer through the node, which knows
	// about transactions it holds that the execution client has not seen.
	nonce, err := c.Eth.PendingNonceAt(ctx, c.From)
	if err != nil {
		return nil, err
	}
	tx, err := types.SignNewTx(c.key, types.LatestSignerForChainID(c.ChainID), &types.DynamicFeeTx{
		ChainID: c.ChainID, Nonce: nonce, GasTipCap: TipWei, GasFeeCap: feeCap,
		Gas: gas, To: to, Value: value, Data: data,
	})
	if err != nil {
		return nil, err
	}
	if err := c.Eth.SendTransaction(ctx, tx); err != nil {
		return nil, fmt.Errorf("sending: %w", err)
	}
	r, err := c.Wait(ctx, tx.Hash())
	if err != nil {
		return r, err
	}
	if r.Status != types.ReceiptStatusSuccessful {
		reason := "no reason given"
		// Replaying the call at the block before says why, when the reason
		// does not depend on something earlier in the same block.
		if _, err := c.Eth.CallContract(ctx, msg, new(big.Int).Sub(r.BlockNumber, big.NewInt(1))); err != nil {
			reason = revertReason(err)
		}
		return r, &RevertError{Reason: reason, Tx: tx.Hash()}
	}
	return r, nil
}

// Wait polls for a receipt.
func (c *Client) Wait(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	ctx, cancel := context.WithTimeout(ctx, ReceiptTimeout)
	defer cancel()
	for {
		r, err := c.Eth.TransactionReceipt(ctx, hash)
		// NotFound means not yet; any other error is a transient RPC
		// failure, not the transaction failing, so both wait for the deadline.
		if err == nil {
			return r, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("no receipt for %s within %s", hash, ReceiptTimeout)
		case <-time.After(2 * time.Second):
		}
	}
}

func asRevert(err error, tx common.Hash) error {
	var de rpc.DataError
	if errors.As(err, &de) || strings.Contains(err.Error(), "revert") {
		return &RevertError{Reason: revertReason(err), Tx: tx}
	}
	return err
}

// revertReason pulls Error(string) out of a JSON-RPC error when it is there.
func revertReason(err error) string {
	var de rpc.DataError
	if errors.As(err, &de) {
		if s, ok := de.ErrorData().(string); ok {
			if b, derr := hexutil.Decode(s); derr == nil {
				if reason, uerr := abi.UnpackRevert(b); uerr == nil {
					return reason
				}
			}
		}
	}
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "execution reverted: ")
	return msg
}
