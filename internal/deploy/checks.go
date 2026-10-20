package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"

	"github.com/Zenoglyphs-VM/zvm-bullet/internal/chain"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/contracts"
)

var routerABI = mustABI(`[
 {"type":"function","name":"factory","inputs":[],"outputs":[{"type":"address"}],"stateMutability":"view"},
 {"type":"function","name":"WETH","inputs":[],"outputs":[{"type":"address"}],"stateMutability":"view"}]`)

var factoryABI = mustABI(`[
 {"type":"function","name":"getPair","inputs":[{"type":"address"},{"type":"address"}],"outputs":[{"type":"address"}],"stateMutability":"view"}]`)

var pairABI = mustABI(`[
 {"type":"function","name":"getReserves","inputs":[],"outputs":[{"type":"uint112"},{"type":"uint112"},{"type":"uint32"}],"stateMutability":"view"}]`)

func mustABI(s string) *abi.ABI {
	a, err := abi.JSON(strings.NewReader(s))
	if err != nil {
		panic(err)
	}
	return &a
}

func read(ctx context.Context, c *chain.Client, to common.Address, a *abi.ABI, method string, args ...any) ([]any, error) {
	data, err := a.Pack(method, args...)
	if err != nil {
		return nil, err
	}
	out, err := c.Call(ctx, to, data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	return a.Unpack(method, out)
}

func readAddress(ctx context.Context, c *chain.Client, to common.Address, a *abi.ABI, method string, args ...any) (common.Address, error) {
	v, err := read(ctx, c, to, a, method, args...)
	if err != nil {
		return common.Address{}, err
	}
	return v[0].(common.Address), nil
}

func readBool(ctx context.Context, c *chain.Client, token common.Address, method string) (bool, error) {
	v, err := read(ctx, c, token, &contracts.Token.ABI, method)
	if err != nil {
		return false, err
	}
	return v[0].(bool), nil
}

func readUint(ctx context.Context, c *chain.Client, to common.Address, a *abi.ABI, method string, args ...any) (*big.Int, error) {
	v, err := read(ctx, c, to, a, method, args...)
	if err != nil {
		return nil, err
	}
	return v[0].(*big.Int), nil
}

// checkAll is everything that has to be true before ownership goes, read off
// the chain rather than taken from the record. It is also Check's body, so the
// same list can be run against a finished deployment at any time.
func (x *run) checkAll(ctx context.Context) error {
	return Check(ctx, x.bot, x.rec)
}

// Check reads a deployment back and compares it with what mainnet's launch
// left behind, adjusted only for the router.
func Check(ctx context.Context, c *chain.Client, rec *Record) error {
	var problems []string
	fail := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }
	tok, esc := &contracts.Token.ABI, &contracts.Escrow.ABI

	addr := func(to common.Address, a *abi.ABI, m string, want common.Address) {
		got, err := readAddress(ctx, c, to, a, m)
		if err != nil {
			fail("%s: %v", m, err)
		} else if got != want {
			fail("%s is %s, want %s", m, got, want)
		}
	}
	num := func(to common.Address, a *abi.ABI, m string, want *big.Int, args ...any) {
		got, err := readUint(ctx, c, to, a, m, args...)
		if err != nil {
			fail("%s: %v", m, err)
		} else if got.Cmp(want) != 0 {
			fail("%s is %s, want %s", m, got, want)
		}
	}
	flag := func(m string, want bool) {
		got, err := readBool(ctx, c, rec.Token, m)
		if err != nil {
			fail("%s: %v", m, err)
		} else if got != want {
			fail("%s is %v, want %v", m, got, want)
		}
	}

	// The token.
	addr(rec.Token, tok, "router", rec.Router)
	addr(rec.Token, tok, "factory", rec.Factory)
	addr(rec.Token, tok, "myWallet", rec.Bot)
	addr(rec.Token, tok, "rouletteContract", rec.Escrow)
	addr(rec.Token, tok, "marketingWallet", rec.Marketing)
	addr(rec.Token, tok, "revenueWallet", rec.Revenue)
	flag("isLaunched", true)
	flag("engagedOnce", true)
	flag("disengagedOnce", true)
	flag("isTestnet", false)
	num(rec.Token, tok, "buyTaxBps", big.NewInt(500))
	num(rec.Token, tok, "sellTaxBps", big.NewInt(500))
	supply := new(big.Int).Mul(big.NewInt(10_000_000), big.NewInt(100_000_000)) // 10M BULLET, 8 decimals
	num(rec.Token, tok, "totalSupply", supply)
	num(rec.Token, tok, "balanceOf", new(big.Int).Div(supply, big.NewInt(10)), rec.Marketing)
	if v, err := read(ctx, c, rec.Token, tok, "decimals"); err != nil || v[0].(uint8) != contracts.Decimals {
		fail("decimals is not %d", contracts.Decimals)
	}
	if v, err := read(ctx, c, rec.Token, tok, "symbol"); err != nil || v[0].(string) != "BULLET" {
		fail("symbol is not BULLET")
	}

	// The pair: the one the factory knows, holding the launch's liquidity.
	pair, err := readAddress(ctx, c, rec.Factory, factoryABI, "getPair", rec.Token, rec.WETH)
	if err != nil {
		fail("getPair: %v", err)
	} else {
		addr(rec.Token, tok, "pair", pair)
		if pair == (common.Address{}) {
			fail("the factory has no BULLET/WETH pair")
		} else if v, err := read(ctx, c, pair, pairABI, "getReserves"); err != nil {
			fail("getReserves: %v", err)
		} else if v[0].(*big.Int).Sign() == 0 || v[1].(*big.Int).Sign() == 0 {
			fail("the pair is empty")
		}
		// 90% of supply went into the pool, all of it.
		num(rec.Token, tok, "balanceOf", new(big.Int).Div(new(big.Int).Mul(supply, big.NewInt(9)), big.NewInt(10)), pair)
	}

	// The escrow, with mainnet's constructor arguments.
	addr(rec.Escrow, esc, "owner", rec.Bot)
	addr(rec.Escrow, esc, "bettingToken", rec.Token)
	addr(rec.Escrow, esc, "revenueWallet", rec.Revenue)
	num(rec.Escrow, esc, "minimumBet", big.NewInt(contracts.EscrowMinimumBet))
	num(rec.Escrow, esc, "revenueBps", big.NewInt(contracts.EscrowRevenueBps))
	num(rec.Escrow, esc, "burnBps", big.NewInt(contracts.EscrowBurnBps))

	// The runtime code is what was compiled, apart from the immutables the
	// constructor writes into it.
	for _, x := range []struct {
		name string
		a    common.Address
		art  *contracts.Artifact
	}{{"token", rec.Token, contracts.Token}, {"escrow", rec.Escrow, contracts.Escrow}} {
		code, err := c.Eth.CodeAt(ctx, x.a, nil)
		if err != nil {
			fail("%s code: %v", x.name, err)
		} else if len(code) != len(x.art.DeployedBytecode) {
			fail("%s runtime code is %d bytes, compiled %d", x.name, len(code), len(x.art.DeployedBytecode))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// verifyContracts submits both contracts to the node's verifier with the
// standard JSON input they were built from, so the explorer shows their
// source and, which is what the game needs, a Write tab for connectAndApprove.
func (x *run) verifyContracts(ctx context.Context) error {
	if x.rec.Verified == nil {
		x.rec.Verified = map[string]string{}
	}
	base := strings.TrimRight(x.cfg.API, "/")
	for _, v := range []struct {
		addr common.Address
		art  *contracts.Artifact
	}{{x.rec.Token, contracts.Token}, {x.rec.Escrow, contracts.Escrow}} {
		body, err := json.Marshal(map[string]any{
			"address":  v.addr.Hex(),
			"compiler": v.art.CompilerVersion,
			"contract": v.art.SourceName + ":" + v.art.ContractName,
			"input":    v.art.StandardJSONInput,
		})
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/verify", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		var job struct {
			ID, Status, Message string
			Match               string `json:"match"`
			Error               string `json:"error"`
		}
		err = json.NewDecoder(resp.Body).Decode(&job)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("%s: %s", v.art.ContractName, resp.Status)
		}
		if job.ID == "" {
			// Already verified is not a failure; anything else is.
			if strings.Contains(strings.ToLower(job.Error+job.Message), "already verified") {
				x.rec.Verified[v.art.ContractName] = "already verified"
				continue
			}
			return fmt.Errorf("%s: %s %s", v.art.ContractName, resp.Status, job.Error+job.Message)
		}
		deadline := time.Now().Add(10 * time.Minute)
		for job.Status == "queued" || job.Status == "running" {
			if time.Now().After(deadline) {
				return fmt.Errorf("%s: verification job %s still %s after 10 minutes", v.art.ContractName, job.ID, job.Status)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			if err := getJSON(ctx, base+"/api/verify/"+job.ID, &job); err != nil {
				return err
			}
		}
		if job.Status != "verified" {
			return fmt.Errorf("%s: %s: %s", v.art.ContractName, job.Status, job.Message)
		}
		x.rec.Verified[v.art.ContractName] = job.Match
		x.log.Info("verified", "contract", v.art.ContractName, "address", v.addr, "match", job.Match)
	}
	return nil
}
