// Package deploy launches Bullet Game on the ZVM the way it was launched on
// Ethereum, step for step, from the transactions mainnet's deployer sent on
// 2023-08-02 (block 17,823,772 onward):
//
//  1. deploy the token          6. setRevenueWallet(revenue)
//  2. deploy the escrow         7. enableAntiBotMode()      tax 10/10
//  3. setMyWallet(bot)          8. stealthLaunch{2.5 ETH}   mints, adds the LP
//  4. setRouletteContract(esc)  9. disableAntiBotMode()     tax back to 5/5
//  5. setMarketingWallet(mkt)  10. renounceOwnership()
//
// Every step reads the chain before it acts and is skipped when the chain
// already says it happened, so a deploy that stops halfway is finished by
// running it again rather than repeated. Step 10 cannot be undone, which is
// why everything the token and escrow hold is checked before it is sent.
package deploy

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Zenoglyphs-VM/zvm-bullet/internal/chain"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/contracts"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/keys"
)

// Record is what a deploy leaves behind, and what the bot reads its contract
// addresses from. It holds no secrets and is meant to be committed.
type Record struct {
	ChainID      uint64            `json:"chainId"`
	Token        common.Address    `json:"token"`
	Escrow       common.Address    `json:"escrow"`
	Pair         common.Address    `json:"pair"`
	Router       common.Address    `json:"router"`
	Factory      common.Address    `json:"factory"`
	WETH         common.Address    `json:"weth"`
	Bot          common.Address    `json:"bot"`
	Marketing    common.Address    `json:"marketing"`
	Revenue      common.Address    `json:"revenue"`
	VRFPublicKey string            `json:"vrfPublicKey"`
	LiquidityWei string            `json:"liquidityWei"`
	TokenBlock   uint64            `json:"tokenBlock"`
	EscrowBlock  uint64            `json:"escrowBlock"`
	Txs          map[string]string `json:"txs"`
	Verified     map[string]string `json:"verified,omitempty"`
	DeployedAt   time.Time         `json:"deployedAt"`
}

func LoadRecord(path string) (*Record, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

func (r *Record) save(path string) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type Config struct {
	RPC    string // JSON-RPC, e.g. http://localhost:49000/rpc
	API    string // the zvm node's API base, e.g. http://localhost:49000; "" skips the router cross-check and verification
	Keys   *keys.Keys
	Funder *ecdsa.PrivateKey // the devnet wallet that pays for everything
	Record string            // path of the deployment record

	LiquidityWei *big.Int // stealthLaunch's msg.value: mainnet sent 2.5 ETH
	BotGasWei    *big.Int // what the bot keeps for gas once the liquidity is spent
	WalletGasWei *big.Int // what the marketing and revenue wallets are given for gas

	Log *slog.Logger
}

type run struct {
	cfg    Config
	rec    *Record
	bot    *chain.Client
	funder *chain.Client
	log    *slog.Logger
}

func Run(ctx context.Context, cfg Config) (*Record, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	bot, err := chain.Dial(ctx, cfg.RPC, cfg.Keys.Bot)
	if err != nil {
		return nil, err
	}
	funder, err := chain.Dial(ctx, cfg.RPC, cfg.Funder)
	if err != nil {
		return nil, err
	}
	rec := &Record{Txs: map[string]string{}}
	if old, err := LoadRecord(cfg.Record); err == nil {
		rec = old
		if rec.Txs == nil {
			rec.Txs = map[string]string{}
		}
		if rec.Bot != (common.Address{}) && rec.Bot != cfg.Keys.BotAddress() {
			return nil, fmt.Errorf("%s was deployed by %s, and these keys are %s", cfg.Record, rec.Bot, cfg.Keys.BotAddress())
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	x := &run{cfg: cfg, rec: rec, bot: bot, funder: funder, log: log}
	if err := x.steps(ctx); err != nil {
		_ = rec.save(cfg.Record)
		return rec, err
	}
	return rec, rec.save(cfg.Record)
}

func (x *run) steps(ctx context.Context) error {
	k := x.cfg.Keys
	x.rec.ChainID = x.bot.ChainID.Uint64()
	x.rec.Bot, x.rec.Marketing, x.rec.Revenue = k.BotAddress(), k.MarketingAddress(), k.RevenueAddress()
	x.rec.VRFPublicKey = "0x" + common.Bytes2Hex(k.VRF.Public())
	x.rec.LiquidityWei = x.cfg.LiquidityWei.String()

	if err := x.preflight(ctx); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	if err := x.fund(ctx); err != nil {
		return fmt.Errorf("funding: %w", err)
	}
	if err := x.deployContracts(ctx); err != nil {
		return err
	}
	if err := x.rec.save(x.cfg.Record); err != nil {
		return err
	}
	if err := x.configure(ctx); err != nil {
		return err
	}
	if err := x.launch(ctx); err != nil {
		return err
	}
	if err := x.checkAll(ctx); err != nil {
		return fmt.Errorf("not renouncing, the launch state is wrong: %w", err)
	}
	if err := x.renounce(ctx); err != nil {
		return err
	}
	if x.cfg.API != "" {
		if err := x.verifyContracts(ctx); err != nil {
			return fmt.Errorf("verifying on the explorer: %w", err)
		}
	}
	x.rec.DeployedAt = time.Now().UTC()
	return nil
}

// preflight refuses any chain the token's constructor would refuse, and any
// router other than the one compiled into it.
func (x *run) preflight(ctx context.Context) error {
	if x.bot.ChainID.Uint64() != contracts.ChainID {
		return fmt.Errorf("chain id %d, but the token only deploys on %d", x.bot.ChainID, contracts.ChainID)
	}
	code, err := x.bot.Eth.CodeAt(ctx, contracts.Router, nil)
	if err != nil {
		return err
	}
	if len(code) == 0 {
		return fmt.Errorf("no router at %s", contracts.Router)
	}
	factory, err := readAddress(ctx, x.bot, contracts.Router, routerABI, "factory")
	if err != nil {
		return err
	}
	weth, err := readAddress(ctx, x.bot, contracts.Router, routerABI, "WETH")
	if err != nil {
		return err
	}
	x.rec.Router, x.rec.Factory, x.rec.WETH = contracts.Router, factory, weth
	if x.cfg.API != "" {
		var info struct {
			UniswapV2 struct {
				Router, Factory, WETH common.Address
			} `json:"uniswapV2"`
		}
		if err := getJSON(ctx, strings.TrimRight(x.cfg.API, "/")+"/api/info", &info); err != nil {
			return err
		}
		u := info.UniswapV2
		if u.Router != contracts.Router || u.Factory != factory || u.WETH != weth {
			return fmt.Errorf("the node's Uniswap V2 is router %s factory %s weth %s; the token is built for router %s (factory %s, weth %s)",
				u.Router, u.Factory, u.WETH, contracts.Router, factory, weth)
		}
	}
	x.log.Info("preflight", "chain", x.bot.ChainID, "router", contracts.Router, "factory", factory, "weth", weth)
	return nil
}

// fund tops the three wallets up from the devnet wallet. It tops up rather
// than sends, so a rerun pays only what is missing.
func (x *run) fund(ctx context.Context) error {
	launched := false
	if x.rec.Token != (common.Address{}) {
		if v, err := readBool(ctx, x.bot, x.rec.Token, "isLaunched"); err == nil {
			launched = v
		}
	}
	botTarget := new(big.Int).Set(x.cfg.BotGasWei)
	if !launched {
		botTarget.Add(botTarget, x.cfg.LiquidityWei)
	}
	for _, t := range []struct {
		name   string
		to     common.Address
		target *big.Int
	}{
		{"bot", x.rec.Bot, botTarget},
		{"marketing", x.rec.Marketing, x.cfg.WalletGasWei},
		{"revenue", x.rec.Revenue, x.cfg.WalletGasWei},
	} {
		have, err := x.bot.Eth.BalanceAt(ctx, t.to, nil)
		if err != nil {
			return err
		}
		if have.Cmp(t.target) >= 0 {
			continue
		}
		need := new(big.Int).Sub(t.target, have)
		x.log.Info("funding from the devnet wallet", "wallet", t.name, "address", t.to, "wei", need, "from", x.funder.From)
		to := t.to
		r, err := x.funder.Send(ctx, &to, need, nil)
		if err != nil {
			return fmt.Errorf("%s: %w", t.name, err)
		}
		x.rec.Txs["fund-"+t.name+"-"+r.BlockNumber.String()] = r.TxHash.Hex()
	}
	return nil
}

func (x *run) deployContracts(ctx context.Context) error {
	if !x.hasCode(ctx, x.rec.Token) {
		data, err := contracts.Token.DeployData()
		if err != nil {
			return err
		}
		x.log.Info("deploying BulletGame", "bytes", len(data))
		r, err := x.bot.Send(ctx, nil, nil, data)
		if err != nil {
			return fmt.Errorf("deploying the token: %w", err)
		}
		x.rec.Token, x.rec.TokenBlock = r.ContractAddress, r.BlockNumber.Uint64()
		x.rec.Txs["deploy-token"] = r.TxHash.Hex()
		x.log.Info("token deployed", "address", r.ContractAddress, "tx", r.TxHash)
	}
	if !x.hasCode(ctx, x.rec.Escrow) {
		data, err := contracts.Escrow.DeployData(x.rec.Token, big.NewInt(contracts.EscrowMinimumBet),
			big.NewInt(contracts.EscrowRevenueBps), big.NewInt(contracts.EscrowBurnBps), x.rec.Revenue)
		if err != nil {
			return err
		}
		x.log.Info("deploying TelegramRussianRoulette", "bytes", len(data))
		r, err := x.bot.Send(ctx, nil, nil, data)
		if err != nil {
			return fmt.Errorf("deploying the escrow: %w", err)
		}
		x.rec.Escrow, x.rec.EscrowBlock = r.ContractAddress, r.BlockNumber.Uint64()
		x.rec.Txs["deploy-escrow"] = r.TxHash.Hex()
		x.log.Info("escrow deployed", "address", r.ContractAddress, "tx", r.TxHash)
	}
	return nil
}

func (x *run) configure(ctx context.Context) error {
	for _, s := range []struct {
		getter, setter string
		want           common.Address
	}{
		{"myWallet", "setMyWallet", x.rec.Bot},
		{"rouletteContract", "setRouletteContract", x.rec.Escrow},
		{"marketingWallet", "setMarketingWallet", x.rec.Marketing},
		{"revenueWallet", "setRevenueWallet", x.rec.Revenue},
	} {
		have, err := readAddress(ctx, x.bot, x.rec.Token, &contracts.Token.ABI, s.getter)
		if err != nil {
			return err
		}
		if have == s.want {
			continue
		}
		if err := x.tokenTx(ctx, s.setter, nil, s.want); err != nil {
			return err
		}
	}
	return nil
}

func (x *run) launch(ctx context.Context) error {
	launched, err := readBool(ctx, x.bot, x.rec.Token, "isLaunched")
	if err != nil {
		return err
	}
	engaged, err := readBool(ctx, x.bot, x.rec.Token, "engagedOnce")
	if err != nil {
		return err
	}
	if !launched && !engaged {
		if err := x.tokenTx(ctx, "enableAntiBotMode", nil); err != nil {
			return err
		}
	}
	if !launched {
		if err := x.tokenTx(ctx, "stealthLaunch", x.cfg.LiquidityWei); err != nil {
			return err
		}
	}
	disengaged, err := readBool(ctx, x.bot, x.rec.Token, "disengagedOnce")
	if err != nil {
		return err
	}
	if !disengaged {
		if err := x.tokenTx(ctx, "disableAntiBotMode", nil); err != nil {
			return err
		}
	}
	pair, err := readAddress(ctx, x.bot, x.rec.Token, &contracts.Token.ABI, "pair")
	if err != nil {
		return err
	}
	x.rec.Pair = pair
	return nil
}

func (x *run) renounce(ctx context.Context) error {
	owner, err := readAddress(ctx, x.bot, x.rec.Token, &contracts.Token.ABI, "owner")
	if err != nil {
		return err
	}
	if owner == (common.Address{}) {
		return nil
	}
	return x.tokenTx(ctx, "renounceOwnership", nil)
}

func (x *run) tokenTx(ctx context.Context, method string, value *big.Int, args ...any) error {
	x.log.Info("token."+method, "args", args, "value", value)
	to := x.rec.Token
	r, err := x.bot.Send(ctx, &to, value, contracts.Token.Pack(method, args...))
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	x.rec.Txs[method] = r.TxHash.Hex()
	return x.rec.save(x.cfg.Record)
}

func (x *run) hasCode(ctx context.Context, a common.Address) bool {
	if a == (common.Address{}) {
		return false
	}
	code, err := x.bot.Eth.CodeAt(ctx, a, nil)
	return err == nil && len(code) > 0
}

func getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
