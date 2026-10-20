package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/Zenoglyphs-VM/zvm-bullet/internal/chain"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/contracts"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/deploy"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/fair"
)

// cmdVerify checks one game from chain data alone: the endGame transaction's
// reveal against the commitment its newGame carried. It needs nothing from
// the bot, which is the point.
func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	rpc := fs.String("rpc", os.Getenv("ZVM_RPC"), "ZVM JSON-RPC ($ZVM_RPC)")
	record := fs.String("record", "", "the deployment record, for the escrow's address and the published VRF key")
	_ = fs.Parse(args)
	if err := need("rpc", *rpc, "ZVM_RPC"); err != nil {
		return err
	}
	if *record == "" || fs.NArg() != 1 {
		return errors.New("usage: zvm-bullet verify -record FILE <endGame transaction hash>")
	}
	rec, err := deploy.LoadRecord(*record)
	if err != nil {
		return err
	}
	c, err := chain.Dial(ctx, *rpc, nil)
	if err != nil {
		return err
	}
	endHash := common.HexToHash(fs.Arg(0))
	tx, _, err := c.Eth.TransactionByHash(ctx, endHash)
	if err != nil {
		return err
	}
	if tx.To() == nil || *tx.To() != rec.Escrow {
		return errors.New("that transaction is not to the escrow")
	}
	abi := &contracts.Escrow.ABI
	m, err := abi.MethodById(tx.Data())
	if err != nil || m.Name != "endGame" {
		return errors.New("that transaction is not an endGame")
	}
	in, err := m.Inputs.Unpack(tx.Data()[4:])
	if err != nil {
		return err
	}
	chat, loser, reveal := in[0].(int64), in[1].(uint16), in[2].([]string)
	endRcpt, err := c.Eth.TransactionReceipt(ctx, endHash)
	if err != nil {
		return err
	}

	// The game's newGame is the latest one for this chat before the endGame:
	// the escrow holds one game per chat at a time. Its Bet events carry the
	// chat id, which is how it is found without an index.
	newHash, err := findNewGame(ctx, c, rec, chat, endRcpt.BlockNumber.Uint64(), endRcpt.TransactionIndex)
	if err != nil {
		return err
	}
	ntx, _, err := c.Eth.TransactionByHash(ctx, newHash)
	if err != nil {
		return err
	}
	nin, err := abi.Methods["newGame"].Inputs.Unpack(ntx.Data()[4:])
	if err != nil {
		return err
	}
	size := int(nin[1].(*big.Int).Int64())
	commit := nin[3].([32]byte)
	players := nin[4].([]common.Address)

	fmt.Printf("game in chat %d\n  newGame  %s\n  endGame  %s\n  revolver %d chambers, %d players, loser seat %d (%s)\n  commit   0x%x\n",
		chat, newHash.Hex(), endHash.Hex(), size, len(players), loser, players[loser], commit)
	got, err := fair.Verify(size, commit, reveal)
	if err != nil {
		return fmt.Errorf("the reveal does not verify: %w", err)
	}
	fmt.Printf("  alpha    %q\n  chamber  %d (the %s pull fires it)\n", got.Alpha, got.Chamber, ordinal(got.Chamber+1))
	pub, _ := hexutil.Decode(rec.VRFPublicKey)
	if !bytes.Equal(got.PublicKey, pub) {
		return fmt.Errorf("the proof is valid for key 0x%x, which is not the operator's published key %s", got.PublicKey, rec.VRFPublicKey)
	}
	fmt.Println("ok: the VRF proof verifies under the operator's key, and the chamber it gives hashes to the commitment newGame carried.")
	fmt.Println("   Which seat that chamber reached depends on the /pull and /pass order in the group, which is not on chain.")
	return nil
}

func findNewGame(ctx context.Context, c *chain.Client, rec *deploy.Record, chat int64, endBlock uint64, endIndex uint) (common.Hash, error) {
	bet := contracts.Escrow.ABI.Events["Bet"]
	const step = 5000
	for hi := endBlock; ; {
		lo := rec.EscrowBlock
		if hi > lo+step {
			lo = hi - step
		}
		logs, err := c.Eth.FilterLogs(ctx, ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(lo), ToBlock: new(big.Int).SetUint64(hi),
			Addresses: []common.Address{rec.Escrow}, Topics: [][]common.Hash{{bet.ID}},
		})
		if err != nil {
			return common.Hash{}, err
		}
		for i := len(logs) - 1; i >= 0; i-- {
			l := logs[i]
			if l.BlockNumber == endBlock && l.TxIndex >= endIndex {
				continue
			}
			v, err := bet.Inputs.Unpack(l.Data)
			if err != nil || v[0].(int64) != chat {
				continue
			}
			return l.TxHash, nil
		}
		if lo <= rec.EscrowBlock {
			return common.Hash{}, fmt.Errorf("no newGame for chat %d before block %d", chat, endBlock)
		}
		hi = lo - 1
	}
}

func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return fmt.Sprintf("%d%s", n, suffix)
}
