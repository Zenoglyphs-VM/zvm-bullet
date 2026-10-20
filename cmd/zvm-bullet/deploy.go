package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"

	"github.com/Zenoglyphs-VM/zvm-bullet/internal/chain"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/deploy"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/keys"
)

func cmdKeys(args []string) error {
	fs := flag.NewFlagSet("keys", flag.ExitOnError)
	out := fs.String("out", "", "where to write the secrets file (refuses to overwrite)")
	_ = fs.Parse(args)
	if *out == "" {
		return errors.New("-out is required")
	}
	k, err := keys.Generate(*out)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s\n  bot        %s\n  marketing  %s\n  revenue    %s\n  vrf key    %x\n",
		*out, k.BotAddress(), k.MarketingAddress(), k.RevenueAddress(), k.VRF.Public())
	return nil
}

// wholeZNN parses a decimal ZNN amount into wei (18 decimals on the ZVM).
func wholeZNN(s string) (*big.Int, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok || r.Sign() < 0 {
		return nil, fmt.Errorf("not an amount: %q", s)
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)))
	if !r.IsInt() {
		return nil, fmt.Errorf("%q has more than 18 decimals", s)
	}
	return r.Num(), nil
}

func cmdDeploy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	rpc := fs.String("rpc", os.Getenv("ZVM_RPC"), "ZVM JSON-RPC ($ZVM_RPC)")
	api := fs.String("api", os.Getenv("ZVM_NODE"), "the zvm node's API, for the router cross-check and verification; \"\" skips both ($ZVM_NODE)")
	keyFile := fs.String("keys", "", "the operator's secrets file")
	record := fs.String("record", "", "the deployment record to write, and to resume from")
	funder := fs.Uint("funder", 900, "zvm devnet EVM key index that provisions everything (900 is the devnet deployer)")
	liquidity := fs.String("liquidity", "250", "ZNN the launch pairs with 9,000,000 BULLET (mainnet: 2.5 ETH)")
	botGas := fs.String("bot-gas", "50", "ZNN the bot keeps for gas")
	walletGas := fs.String("wallet-gas", "5", "ZNN given to the marketing and revenue wallets for gas")
	_ = fs.Parse(args)
	if err := need("rpc", *rpc, "ZVM_RPC"); err != nil {
		return err
	}
	if *keyFile == "" || *record == "" {
		return errors.New("-keys and -record are required")
	}
	k, err := keys.Load(*keyFile)
	if err != nil {
		return err
	}
	liq, err := wholeZNN(*liquidity)
	if err != nil {
		return err
	}
	bg, err := wholeZNN(*botGas)
	if err != nil {
		return err
	}
	wg, err := wholeZNN(*walletGas)
	if err != nil {
		return err
	}
	rec, err := deploy.Run(ctx, deploy.Config{
		RPC: *rpc, API: *api, Keys: k, Funder: keys.DevEVMKey(uint32(*funder)), Record: *record,
		LiquidityWei: liq, BotGasWei: bg, WalletGasWei: wg,
	})
	if rec != nil {
		fmt.Printf("token   %s\nescrow  %s\npair    %s\nrecord  %s\n", rec.Token, rec.Escrow, rec.Pair, *record)
	}
	return err
}

func cmdCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	rpc := fs.String("rpc", os.Getenv("ZVM_RPC"), "ZVM JSON-RPC ($ZVM_RPC)")
	record := fs.String("record", "", "the deployment record")
	_ = fs.Parse(args)
	if err := need("rpc", *rpc, "ZVM_RPC"); err != nil {
		return err
	}
	rec, err := deploy.LoadRecord(*record)
	if err != nil {
		return err
	}
	c, err := chain.Dial(ctx, *rpc, nil)
	if err != nil {
		return err
	}
	if err := deploy.Check(ctx, c, rec); err != nil {
		return err
	}
	fmt.Println("ok: the token and escrow match mainnet's launch state")
	return nil
}
