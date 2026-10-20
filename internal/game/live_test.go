package game

// A whole game through the real engine and the real escrow, with players who
// bought BULLET through the router and connected with connectAndApprove. Set
// BULLET_LIVE_RPC, BULLET_LIVE_RECORD and BULLET_LIVE_KEYS as for the escrow's
// live test. With BULLET_LIVE_TG_CHAT and BULLET_TELEGRAM_TOKEN set as well,
// everything the bot says is also posted to that chat, which is how the
// messages were checked against Telegram's HTML parser.

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Zenoglyphs-VM/zvm-bullet/internal/chain"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/contracts"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/deploy"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/escrow"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/fair"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/keys"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/store"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/tg"
)

// teeOut hands every message to the test and, when configured, to Telegram.
type teeOut struct {
	ch   chan string
	tg   *tg.Client
	chat int64
	t    *testing.T
}

func (o *teeOut) Send(ctx context.Context, _ int64, html string) error {
	if o.tg != nil {
		if err := o.tg.Send(ctx, o.chat, html); err != nil {
			o.t.Errorf("telegram refused a message: %v\n%s", err, html)
		}
	}
	o.ch <- html
	return nil
}

func TestLiveGameThroughTheEngine(t *testing.T) {
	rpc, recPath, keyPath := os.Getenv("BULLET_LIVE_RPC"), os.Getenv("BULLET_LIVE_RECORD"), os.Getenv("BULLET_LIVE_KEYS")
	if rpc == "" || recPath == "" || keyPath == "" {
		t.Skip("BULLET_LIVE_RPC, BULLET_LIVE_RECORD and BULLET_LIVE_KEYS are not set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
	esc := &escrow.Escrow{C: bot, Token: rec.Token, Escrow: rec.Escrow}
	st, err := store.Open(filepath.Join(t.TempDir(), "bullet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	out := &teeOut{ch: make(chan string, 256), t: t}
	chat := -time.Now().UnixNano() % 1_000_000_000_000
	if id := os.Getenv("BULLET_LIVE_TG_CHAT"); id != "" {
		out.chat, _ = strconv.ParseInt(id, 10, 64)
		out.tg = tg.New(os.Getenv("BULLET_TELEGRAM_TOKEN"), nil)
		chat = out.chat
	}
	links := Links{Explorer: "https://devnet.example.org/explorer", Swap: "https://devnet.example.org/glyphswap", Token: rec.Token}
	conn := &Connector{Escrow: esc, Store: st, Links: links}

	// Three players who do what a real one does: buy on the DEX, /connect,
	// call connectAndApprove with the secret, send the hash.
	router := mustRouterABI()
	var players []Player
	for i := int64(1); i <= 3; i++ {
		key, _ := crypto.GenerateKey()
		addr := crypto.PubkeyToAddress(key.PublicKey)
		pc, err := chain.Dial(ctx, rpc, key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := funder.Send(ctx, &addr, new(big.Int).Mul(big.NewInt(2), big.NewInt(1e18)), nil); err != nil {
			t.Fatal(err)
		}
		data, _ := router.Pack("swapExactETHForTokensSupportingFeeOnTransferTokens", big.NewInt(0),
			[]common.Address{rec.WETH, rec.Token}, addr, big.NewInt(time.Now().Add(time.Hour).Unix()))
		r := rec.Router
		if _, err := pc.Send(ctx, &r, big.NewInt(5e17), data); err != nil {
			t.Fatalf("buying: %v", err)
		}
		// The player's Telegram id: a test user, the bot never messages them.
		id := 7_000_000_000 + i
		reply := conn.Connect(ctx, id)
		secret := secretIn(t, reply)
		connectTx := connectAndApprove(t, ctx, pc, rec.Token, secret)
		if got := conn.Hash(ctx, id, "tester "+strconv.FormatInt(i, 10), connectTx.Hex()); !strings.HasPrefix(got, "Connected") {
			t.Fatalf("connect: %s", got)
		}
		// Sending the same hash again is refused.
		if got := conn.Hash(ctx, id, "tester", connectTx.Hex()); strings.HasPrefix(got, "Connected") {
			t.Fatal("a connect transaction linked twice")
		}
		players = append(players, Player{ID: id, Name: "tester " + strconv.FormatInt(i, 10)})
	}

	m := NewManager(ctx, Config{
		Chain: esc, Out: out, Store: st, VRF: k.VRF, Links: links, MinBet: big.NewInt(contracts.EscrowMinimumBet),
		House: map[common.Address]bool{rec.Bot: true}, JoinWindow: time.Minute, AFK: 3 * time.Minute,
	})
	h := &harness{t: t, out: &fakeOut{ch: out.ch}, st: st, m: m, chat: chat, wait: 5 * time.Minute}
	byID := map[int64]Player{}
	for _, p := range players {
		byID[p.ID] = p
	}

	h.cmd(players[0], "roulette", "4", "100")
	h.expect(`Game #1</b>`)
	h.cmd(players[1], "join", "150")
	h.expect(`joins with 150`)
	h.cmd(players[2], "join")
	h.expect(`joins with 100`)
	h.cmd(players[0], "join")
	// The window is a minute; the fourth seat stays empty, so it starts then.
	h.expect(`bets are in escrow`)
	holder := holderOf(h.expect(`you have the gun`))
	for {
		h.cmd(byID[holder], "pull")
		if strings.Contains(h.expect(`<i>click</i>|BANG`), "BANG") {
			break
		}
		h.cmd(byID[holder], "pass")
		holder = holderOf(h.expect(`you have the gun`))
	}
	end := h.expect(`Game #1 is over`)
	m.Wait()
	t.Log(end)

	g, err := st.Game(ctx, 1)
	if err != nil || g.State != "ended" {
		t.Fatalf("game %+v %v", g, err)
	}
	// The reveal on chain checks out, from chain data alone.
	etx, _, err := bot.Eth.TransactionByHash(ctx, common.HexToHash(g.EndTx))
	if err != nil {
		t.Fatal(err)
	}
	in, _ := contracts.Escrow.ABI.Methods["endGame"].Inputs.Unpack(etx.Data()[4:])
	ntx, _, _ := bot.Eth.TransactionByHash(ctx, common.HexToHash(g.NewTx))
	nin, _ := contracts.Escrow.ABI.Methods["newGame"].Inputs.Unpack(ntx.Data()[4:])
	checked, err := fair.Verify(4, nin[3].([32]byte), in[2].([]string))
	if err != nil {
		t.Fatalf("on-chain reveal: %v", err)
	}
	if checked.Chamber != int(g.Chamber.Int64) {
		t.Fatalf("chain says chamber %d, the bot recorded %d", checked.Chamber, g.Chamber.Int64)
	}
	if in, _ := esc.InProgress(ctx, chat); in {
		t.Fatal("the escrow still holds the game")
	}
}

func secretIn(t *testing.T, reply string) uint32 {
	t.Helper()
	i := strings.Index(reply, "<code>")
	j := strings.Index(reply, "</code>")
	n, err := strconv.ParseUint(reply[i+6:j], 10, 32)
	if err != nil {
		t.Fatalf("no secret in %q", reply)
	}
	return uint32(n)
}

func connectAndApprove(t *testing.T, ctx context.Context, c *chain.Client, token common.Address, secret uint32) common.Hash {
	t.Helper()
	r, err := c.Send(ctx, &token, nil, contracts.Token.Pack("connectAndApprove", secret))
	if err != nil {
		t.Fatal(err)
	}
	return r.TxHash
}

func mustRouterABI() abi.ABI {
	a, err := abi.JSON(strings.NewReader(`[{"type":"function","name":"swapExactETHForTokensSupportingFeeOnTransferTokens","stateMutability":"payable",
	 "inputs":[{"name":"amountOutMin","type":"uint256"},{"name":"path","type":"address[]"},{"name":"to","type":"address"},{"name":"deadline","type":"uint256"}],"outputs":[]}]`))
	if err != nil {
		panic(err)
	}
	return a
}
