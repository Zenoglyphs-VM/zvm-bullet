package escrow

// A live test: it runs against a deployed game and spends real (devnet) ZNN.
// It is skipped unless BULLET_LIVE_RPC, BULLET_LIVE_RECORD and
// BULLET_LIVE_KEYS name a node, a deployment record and its secrets file, so
// it can be pointed at an anvil fork of chain 69 first and at chain 69 after.
//
//	BULLET_LIVE_RPC=http://127.0.0.1:48645 BULLET_LIVE_RECORD=... BULLET_LIVE_KEYS=... go test ./internal/escrow -run Live -v

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Zenoglyphs-VM/zvm-bullet/internal/chain"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/contracts"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/deploy"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/fair"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/keys"
)

var routerSwapABI = func() abi.ABI {
	a, err := abi.JSON(strings.NewReader(`[{"type":"function","name":"swapExactETHForTokensSupportingFeeOnTransferTokens","stateMutability":"payable",
	 "inputs":[{"name":"amountOutMin","type":"uint256"},{"name":"path","type":"address[]"},{"name":"to","type":"address"},{"name":"deadline","type":"uint256"}],"outputs":[]}]`))
	if err != nil {
		panic(err)
	}
	return a
}()

type live struct {
	t      *testing.T
	ctx    context.Context
	rpc    string
	rec    *deploy.Record
	k      *keys.Keys
	e      *Escrow
	funder *chain.Client
}

func setupLive(t *testing.T) *live {
	rpc, recPath, keyPath := os.Getenv("BULLET_LIVE_RPC"), os.Getenv("BULLET_LIVE_RECORD"), os.Getenv("BULLET_LIVE_KEYS")
	if rpc == "" || recPath == "" || keyPath == "" {
		t.Skip("BULLET_LIVE_RPC, BULLET_LIVE_RECORD and BULLET_LIVE_KEYS are not set")
	}
	ctx := context.Background()
	rec, err := deploy.LoadRecord(recPath)
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.Load(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	bot, err := chain.Dial(ctx, rpc, k.Bot)
	if err != nil {
		t.Fatal(err)
	}
	funder, err := chain.Dial(ctx, rpc, keys.DevEVMKey(900))
	if err != nil {
		t.Fatal(err)
	}
	return &live{t: t, ctx: ctx, rpc: rpc, rec: rec, k: k, e: &Escrow{C: bot, Token: rec.Token, Escrow: rec.Escrow}, funder: funder}
}

type player struct {
	key  *ecdsa.PrivateKey
	addr common.Address
	c    *chain.Client
}

// newPlayer funds a fresh wallet from the devnet wallet, buys BULLET through
// the router the way a player would, and connects it with a secret.
func (l *live) newPlayer(znn int64, buyZNN int64, secret uint32) *player {
	l.t.Helper()
	key, _ := crypto.GenerateKey()
	p := &player{key: key, addr: crypto.PubkeyToAddress(key.PublicKey)}
	var err error
	if p.c, err = chain.Dial(l.ctx, l.rpc, key); err != nil {
		l.t.Fatal(err)
	}
	wei := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }
	if _, err := l.funder.Send(l.ctx, &p.addr, wei(znn), nil); err != nil {
		l.t.Fatalf("funding: %v", err)
	}
	data, err := routerSwapABI.Pack("swapExactETHForTokensSupportingFeeOnTransferTokens", big.NewInt(0),
		[]common.Address{l.rec.WETH, l.rec.Token}, p.addr, big.NewInt(time.Now().Add(time.Hour).Unix()))
	if err != nil {
		l.t.Fatal(err)
	}
	router := l.rec.Router
	if _, err := p.c.Send(l.ctx, &router, wei(buyZNN), data); err != nil {
		l.t.Fatalf("buying BULLET: %v", err)
	}
	token := l.rec.Token
	r, err := p.c.Send(l.ctx, &token, nil, contracts.Token.Pack("connectAndApprove", secret))
	if err != nil {
		l.t.Fatalf("connectAndApprove: %v", err)
	}
	got, err := l.e.ReadConnect(l.ctx, r.TxHash)
	if err != nil {
		l.t.Fatalf("ReadConnect: %v", err)
	}
	if got.Owner != p.addr || got.Secret != secret {
		l.t.Fatalf("ReadConnect gave %s/%d, want %s/%d", got.Owner, got.Secret, p.addr, secret)
	}
	return p
}

func (l *live) balance(a common.Address) *big.Int {
	b, _, err := l.e.Holdings(l.ctx, a)
	if err != nil {
		l.t.Fatal(err)
	}
	return b
}

func (l *live) supply() *big.Int {
	out, err := l.e.C.Call(l.ctx, l.rec.Token, contracts.Token.Pack("totalSupply"))
	if err != nil {
		l.t.Fatal(err)
	}
	v, _ := contracts.Token.ABI.Unpack("totalSupply", out)
	return v[0].(*big.Int)
}

func TestLiveGame(t *testing.T) {
	l := setupLive(t)
	unit := big.NewInt(100_000_000) // one BULLET
	bullets := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), unit) }

	a := l.newPlayer(3, 1, 111)
	b := l.newPlayer(3, 1, 222)
	c := l.newPlayer(3, 1, 333)
	for _, p := range []*player{a, b, c} {
		bal, allow, err := l.e.Holdings(l.ctx, p.addr)
		if err != nil {
			t.Fatal(err)
		}
		if bal.Sign() == 0 {
			t.Fatalf("%s bought no BULLET", p.addr)
		}
		if allow.Cmp(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))) != 0 {
			t.Fatalf("%s: allowance %s, want max", p.addr, allow)
		}
	}

	chat := -time.Now().UnixNano() % 1_000_000_000_000
	// Seat 2 bets more than the other two together: the contract caps it.
	req := []*big.Int{bullets(50), bullets(60), bullets(500)}
	commit, err := fair.New(l.k.VRF, 6, req, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	players := []common.Address{a.addr, b.addr, c.addr}
	before := []*big.Int{l.balance(a.addr), l.balance(b.addr), l.balance(c.addr)}
	revBefore, supplyBefore := l.balance(l.rec.Revenue), l.supply()

	st, err := l.e.NewGame(l.ctx, chat, 6, bullets(50), commit.Hash, players, req)
	if err != nil {
		t.Fatalf("newGame: %v", err)
	}
	wantBets := []*big.Int{bullets(50), bullets(60), bullets(110)}
	for i := range wantBets {
		if st.Bets[i].Cmp(wantBets[i]) != 0 {
			t.Fatalf("seat %d staked %s, want %s", i, st.Bets[i], wantBets[i])
		}
	}
	if in, _ := l.e.InProgress(l.ctx, chat); !in {
		t.Fatal("no game in progress after newGame")
	}

	// Seat 1 loses 60. The contract's arithmetic, mirrored.
	const loser = 1
	lost := wantBets[loser]
	burn := new(big.Int).Div(new(big.Int).Mul(lost, big.NewInt(100)), big.NewInt(10_000))
	approxRev := new(big.Int).Div(new(big.Int).Mul(lost, big.NewInt(900)), big.NewInt(10_000))
	pot := new(big.Int).Sub(new(big.Int).Sub(lost, burn), approxRev)
	winTotal := new(big.Int).Add(wantBets[0], wantBets[2])
	win := map[int]*big.Int{
		0: new(big.Int).Div(new(big.Int).Mul(pot, wantBets[0]), winTotal),
		2: new(big.Int).Div(new(big.Int).Mul(pot, wantBets[2]), winTotal),
	}
	realRev := new(big.Int).Sub(new(big.Int).Sub(lost, burn), new(big.Int).Add(win[0], win[2]))

	end, err := l.e.EndGame(l.ctx, chat, loser, commit.Reveal(l.k.VRF.Public()))
	if err != nil {
		t.Fatalf("endGame: %v", err)
	}
	for seat, w := range win {
		if end.Winnings[seat] == nil || end.Winnings[seat].Cmp(w) != 0 {
			t.Fatalf("seat %d won %v, want %s", seat, end.Winnings[seat], w)
		}
	}
	if end.Burned.Cmp(burn) != 0 || end.Revenue.Cmp(realRev) != 0 {
		t.Fatalf("burned %s revenue %s, want %s and %s", end.Burned, end.Revenue, burn, realRev)
	}
	after := []*big.Int{l.balance(a.addr), l.balance(b.addr), l.balance(c.addr)}
	if d := new(big.Int).Sub(after[0], before[0]); d.Cmp(win[0]) != 0 {
		t.Fatalf("seat 0 balance moved %s, want +%s", d, win[0])
	}
	if d := new(big.Int).Sub(before[1], after[1]); d.Cmp(lost) != 0 {
		t.Fatalf("seat 1 balance moved -%s, want -%s", d, lost)
	}
	if d := new(big.Int).Sub(after[2], before[2]); d.Cmp(win[2]) != 0 {
		t.Fatalf("seat 2 balance moved %s, want +%s (its stake beyond the cap was never taken)", d, win[2])
	}
	if d := new(big.Int).Sub(l.balance(l.rec.Revenue), revBefore); d.Cmp(realRev) != 0 {
		t.Fatalf("revenue wallet moved %s, want %s", d, realRev)
	}
	if d := new(big.Int).Sub(supplyBefore, l.supply()); d.Cmp(burn) != 0 {
		t.Fatalf("supply fell %s, want %s", d, burn)
	}
	if in, _ := l.e.InProgress(l.ctx, chat); in {
		t.Fatal("game still in progress after endGame")
	}
	// The reveal on chain verifies against the commitment.
	if got, err := fair.Verify(6, commit.Hash, commit.Reveal(l.k.VRF.Public())); err != nil || got.Chamber != commit.Chamber {
		t.Fatalf("reveal: %v", err)
	}

	// An aborted game refunds at par.
	chat2 := chat - 1
	commit2, _ := fair.New(l.k.VRF, 2, []*big.Int{bullets(10), bullets(10)}, time.Now())
	mid := []*big.Int{l.balance(a.addr), l.balance(c.addr)}
	if _, err := l.e.NewGame(l.ctx, chat2, 2, bullets(10), commit2.Hash, []common.Address{a.addr, c.addr}, []*big.Int{bullets(10), bullets(10)}); err != nil {
		t.Fatalf("second newGame: %v", err)
	}
	if _, err := l.e.AbortGame(l.ctx, chat2); err != nil {
		t.Fatalf("abortGame: %v", err)
	}
	if l.balance(a.addr).Cmp(mid[0]) != 0 || l.balance(c.addr).Cmp(mid[1]) != 0 {
		t.Fatal("abortGame did not refund at par")
	}

	// A player who revoked the approval makes newGame revert, and it is caught
	// before anything is sent.
	token := l.rec.Token
	if _, err := b.c.Send(l.ctx, &token, nil, contracts.Token.Pack("approve", l.rec.Escrow, big.NewInt(0))); err != nil {
		t.Fatal(err)
	}
	_, err = l.e.NewGame(l.ctx, chat2, 2, bullets(10), commit2.Hash, []common.Address{a.addr, b.addr}, []*big.Int{bullets(10), bullets(10)})
	var rev *chain.RevertError
	if err == nil || !asRevert(err, &rev) || !strings.Contains(rev.Reason, "Not enough allowance") {
		t.Fatalf("newGame with a revoked approval: %v", err)
	}
}

func asRevert(err error, out **chain.RevertError) bool {
	r, ok := err.(*chain.RevertError)
	if ok {
		*out = r
	}
	return ok
}
