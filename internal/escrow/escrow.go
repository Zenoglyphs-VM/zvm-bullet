// Package escrow is the bot's side of TelegramRussianRoulette and BULLET: the
// reads that decide whether a player can join, and the three calls the owner
// makes. Every call is the contract's own; nothing here moves tokens that the
// contract would not.
package escrow

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Zenoglyphs-VM/zvm-bullet/internal/chain"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/contracts"
)

type Escrow struct {
	C      *chain.Client
	Token  common.Address
	Escrow common.Address
}

func (e *Escrow) call(ctx context.Context, to common.Address, a *contracts.Artifact, method string, args ...any) ([]any, error) {
	out, err := e.C.Call(ctx, to, a.Pack(method, args...))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	return a.ABI.Unpack(method, out)
}

// Holdings is what the escrow could take from a player right now.
func (e *Escrow) Holdings(ctx context.Context, player common.Address) (balance, allowance *big.Int, err error) {
	b, err := e.call(ctx, e.Token, contracts.Token, "balanceOf", player)
	if err != nil {
		return nil, nil, err
	}
	a, err := e.call(ctx, e.Token, contracts.Token, "allowance", player, e.Escrow)
	if err != nil {
		return nil, nil, err
	}
	return b[0].(*big.Int), a[0].(*big.Int), nil
}

func (e *Escrow) InProgress(ctx context.Context, chat int64) (bool, error) {
	v, err := e.call(ctx, e.Escrow, contracts.Escrow, "isGameInProgress", chat)
	if err != nil {
		return false, err
	}
	return v[0].(bool), nil
}

func (e *Escrow) MinimumBet(ctx context.Context) (*big.Int, error) {
	v, err := e.call(ctx, e.Escrow, contracts.Escrow, "minimumBet")
	if err != nil {
		return nil, err
	}
	return v[0].(*big.Int), nil
}

// Started is a newGame that landed.
type Started struct {
	Tx   common.Hash
	Bets []*big.Int // after the contract's cap, from its Bet events, in seat order
}

// NewGame collects the stakes. The contract caps each bet at the sum of the
// others' so nobody can lose more than the table can pay; the capped amounts
// come back in Bet events, which are what was actually taken.
func (e *Escrow) NewGame(ctx context.Context, chat int64, size int, minBet *big.Int, commit [32]byte, players []common.Address, bets []*big.Int) (*Started, error) {
	to := e.Escrow
	r, err := e.C.Send(ctx, &to, nil, contracts.Escrow.Pack("newGame", chat, big.NewInt(int64(size)), minBet, commit, players, bets))
	if err != nil {
		return nil, err
	}
	s := &Started{Tx: r.TxHash, Bets: make([]*big.Int, len(players))}
	for _, ev := range events(r, e.Escrow, "Bet") {
		idx := int(ev["playerIndex"].(uint16))
		if idx >= len(players) || ev["player"].(common.Address) != players[idx] {
			return nil, fmt.Errorf("newGame %s: a Bet event for seat %d does not match the player list", r.TxHash, idx)
		}
		s.Bets[idx] = ev["amount"].(*big.Int)
	}
	for i, b := range s.Bets {
		if b == nil {
			return nil, fmt.Errorf("newGame %s: no Bet event for seat %d", r.TxHash, i)
		}
	}
	return s, nil
}

// Ended is an endGame that landed.
type Ended struct {
	Tx       common.Hash
	Winnings map[int]*big.Int // seat -> winnings on top of the returned stake
	Burned   *big.Int
	Revenue  *big.Int
}

func (e *Escrow) EndGame(ctx context.Context, chat int64, loser int, reveal []string) (*Ended, error) {
	to := e.Escrow
	r, err := e.C.Send(ctx, &to, nil, contracts.Escrow.Pack("endGame", chat, uint16(loser), reveal))
	if err != nil {
		return nil, err
	}
	out := &Ended{Tx: r.TxHash, Winnings: map[int]*big.Int{}}
	for _, ev := range events(r, e.Escrow, "Win") {
		out.Winnings[int(ev["playerIndex"].(uint16))] = ev["amount"].(*big.Int)
	}
	if b := events(r, e.Escrow, "Burn"); len(b) == 1 {
		out.Burned = b[0]["amount"].(*big.Int)
	}
	if v := events(r, e.Escrow, "Revenue"); len(v) == 1 {
		out.Revenue = v[0]["amount"].(*big.Int)
	}
	if out.Burned == nil || out.Revenue == nil {
		return nil, fmt.Errorf("endGame %s: no Burn or Revenue event", r.TxHash)
	}
	return out, nil
}

// AbortGame refunds every stake at par. Mainnet's operator used it after a
// bot crash; so does this one.
func (e *Escrow) AbortGame(ctx context.Context, chat int64) (common.Hash, error) {
	to := e.Escrow
	r, err := e.C.Send(ctx, &to, nil, contracts.Escrow.Pack("abortGame", chat))
	if err != nil {
		return common.Hash{}, err
	}
	return r.TxHash, nil
}

// events decodes the logs of one event from one contract in a receipt.
func events(r *types.Receipt, addr common.Address, name string) []map[string]any {
	ev := contracts.Escrow.ABI.Events[name]
	var out []map[string]any
	for _, l := range r.Logs {
		if l.Address != addr || len(l.Topics) == 0 || l.Topics[0] != ev.ID {
			continue
		}
		m := map[string]any{}
		if err := contracts.Escrow.ABI.UnpackIntoMap(m, name, l.Data); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

// ErrNotConnect is a transaction that is not a connectAndApprove on BULLET.
var ErrNotConnect = errors.New("not a connectAndApprove call on the BULLET contract")

// Connect is a confirmed connectAndApprove: who sent it and the secret it
// carried.
type Connect struct {
	Owner  common.Address
	Secret uint32
	Block  uint64
}

// ReadConnect checks a transaction hash a player sent the bot. The owner is
// taken from the Approval the call emitted rather than from the transaction's
// signature, because a Zenon account calling the token through the native
// lane has no EVM signature; the Approval's owner is msg.sender either way.
func (e *Escrow) ReadConnect(ctx context.Context, hash common.Hash) (*Connect, error) {
	tx, pending, err := e.C.Eth.TransactionByHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	if pending {
		return nil, errors.New("that transaction has not landed yet")
	}
	if tx.To() == nil || *tx.To() != e.Token {
		return nil, ErrNotConnect
	}
	method, err := contracts.Token.ABI.MethodById(tx.Data())
	if err != nil || method.Name != "connectAndApprove" {
		return nil, ErrNotConnect
	}
	args, err := method.Inputs.Unpack(tx.Data()[4:])
	if err != nil {
		return nil, ErrNotConnect
	}
	r, err := e.C.Eth.TransactionReceipt(ctx, hash)
	if err != nil {
		return nil, err
	}
	if r.Status != types.ReceiptStatusSuccessful {
		return nil, errors.New("that transaction failed")
	}
	approval := contracts.Token.ABI.Events["Approval"]
	for _, l := range r.Logs {
		if l.Address != e.Token || len(l.Topics) != 3 || l.Topics[0] != approval.ID {
			continue
		}
		if common.BytesToAddress(l.Topics[2].Bytes()) != e.Escrow {
			continue
		}
		return &Connect{Owner: common.BytesToAddress(l.Topics[1].Bytes()), Secret: args[0].(uint32), Block: r.BlockNumber.Uint64()}, nil
	}
	return nil, errors.New("that transaction approved nothing for the escrow")
}
