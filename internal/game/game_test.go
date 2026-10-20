package game

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Zenoglyphs-VM/zvm-bullet/internal/chain"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/escrow"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/fair"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/store"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/vrf"
)

// fakeChain is an escrow that does what the contract does to its books,
// without a chain.
type fakeChain struct {
	mu        sync.Mutex
	balance   map[common.Address]*big.Int
	allowance map[common.Address]*big.Int
	inGame    map[int64]bool
	newGames  []newGameCall
	ends      []endCall
	aborts    []int64
	endErr    error
	newErr    error
}

type newGameCall struct {
	chat    int64
	size    int
	commit  [32]byte
	players []common.Address
	bets    []*big.Int
}

type endCall struct {
	chat   int64
	loser  int
	reveal []string
}

func newFakeChain() *fakeChain {
	return &fakeChain{balance: map[common.Address]*big.Int{}, allowance: map[common.Address]*big.Int{}, inGame: map[int64]bool{}}
}

func (f *fakeChain) Holdings(_ context.Context, p common.Address) (*big.Int, *big.Int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, a := f.balance[p], f.allowance[p]
	if b == nil {
		b = new(big.Int)
	}
	if a == nil {
		a = new(big.Int)
	}
	return b, a, nil
}

func (f *fakeChain) InProgress(_ context.Context, chat int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inGame[chat], nil
}

// NewGame caps bets the way the contract does.
func (f *fakeChain) NewGame(_ context.Context, chat int64, size int, _ *big.Int, commit [32]byte, players []common.Address, bets []*big.Int) (*escrow.Started, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newErr != nil {
		return nil, f.newErr
	}
	capped := make([]*big.Int, len(bets))
	total := new(big.Int)
	for _, b := range bets {
		total.Add(total, b)
	}
	for i, b := range bets {
		total.Sub(total, b)
		capped[i] = new(big.Int).Set(b)
		if capped[i].Cmp(total) > 0 {
			capped[i].Set(total)
		}
		total.Add(total, capped[i])
	}
	f.newGames = append(f.newGames, newGameCall{chat, size, commit, players, capped})
	f.inGame[chat] = true
	return &escrow.Started{Tx: common.HexToHash("0x01"), Bets: capped}, nil
}

func (f *fakeChain) EndGame(_ context.Context, chat int64, loser int, reveal []string) (*escrow.Ended, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.endErr != nil {
		return nil, f.endErr
	}
	f.ends = append(f.ends, endCall{chat, loser, reveal})
	f.inGame[chat] = false
	g := f.newGames[len(f.newGames)-1]
	w := map[int]*big.Int{}
	for i := range g.players {
		if i != loser {
			w[i] = big.NewInt(1)
		}
	}
	return &escrow.Ended{Tx: common.HexToHash("0x02"), Winnings: w, Burned: big.NewInt(1), Revenue: big.NewInt(9)}, nil
}

func (f *fakeChain) AbortGame(_ context.Context, chat int64) (common.Hash, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborts = append(f.aborts, chat)
	f.inGame[chat] = false
	return common.HexToHash("0x03"), nil
}

// fakeOut collects what the bot says.
type fakeOut struct{ ch chan string }

func (o *fakeOut) Send(_ context.Context, _ int64, html string) error {
	o.ch <- html
	return nil
}

type harness struct {
	t     *testing.T
	chain *fakeChain
	out   *fakeOut
	st    *store.Store
	m     *Manager
	key   *vrf.PrivateKey
	chat  int64
	wait  time.Duration // how long expect waits; 5 s when zero
}

var one = big.NewInt(100_000_000)

func bullets(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), one) }

func newHarness(t *testing.T, tweak func(*Config)) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "bullet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	key, _ := vrf.NewKey(bytes.Repeat([]byte{9}, 32))
	h := &harness{t: t, chain: newFakeChain(), out: &fakeOut{ch: make(chan string, 256)}, st: st, key: key, chat: -100}
	cfg := Config{
		Chain: h.chain, Out: h.out, Store: st, VRF: key, MinBet: big.NewInt(1000),
		House: map[common.Address]bool{common.HexToAddress("0xb07"): true}, Admins: map[int64]bool{99: true},
		JoinWindow: 300 * time.Millisecond, AFK: time.Hour, Links: Links{Explorer: "https://x/explorer"},
	}
	if tweak != nil {
		tweak(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.m = NewManager(ctx, cfg)
	return h
}

// player links a Telegram user to a funded, approved address.
func (h *harness) player(id int64, bal int64) Player {
	addr := common.BigToAddress(big.NewInt(id))
	if err := h.st.Link(context.Background(), id, fmt.Sprintf("p%d", id), addr.Hex(), fmt.Sprintf("0x%064x", id), time.Now()); err != nil {
		h.t.Fatal(err)
	}
	h.chain.balance[addr] = bullets(bal)
	h.chain.allowance[addr] = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	return Player{ID: id, Name: fmt.Sprintf("p%d", id)}
}

func (h *harness) cmd(p Player, c string, args ...string) { h.m.Command(h.chat, p, c, args) }

// expect waits for a message matching re and returns it.
func (h *harness) expect(re string) string {
	h.t.Helper()
	rx := regexp.MustCompile(re)
	wait := h.wait
	if wait == 0 {
		wait = 5 * time.Second
	}
	deadline := time.After(wait)
	for {
		select {
		case msg := <-h.out.ch:
			if rx.MatchString(msg) {
				return msg
			}
		case <-deadline:
			h.t.Fatalf("no message matching %q", re)
		}
	}
}

var holderRx = regexp.MustCompile(`tg://user\?id=(\d+)">[^<]*</a>, you have the gun`)

func holderOf(msg string) int64 {
	m := holderRx.FindStringSubmatch(msg)
	id, _ := strconv.ParseInt(m[1], 10, 64)
	return id
}

func TestFullGame(t *testing.T) {
	h := newHarness(t, nil)
	a, b, c := h.player(1, 100), h.player(2, 100), h.player(3, 1000)
	players := map[int64]Player{1: a, 2: b, 3: c}

	h.cmd(a, "roulette", "3", "50")
	h.expect(`Game #1</b>: a 3-shot revolver, 50 BULLET to join`)
	h.cmd(b, "join")
	h.expect(`p2 joins with 50 BULLET. 2 of 3`)
	h.cmd(c, "join", "400") // more than the table: capped at 100
	h.expect(`bets are in escrow`)
	msg := h.expect(`you have the gun`)

	if len(h.chain.newGames) != 1 {
		t.Fatal("newGame not called once")
	}
	g := h.chain.newGames[0]
	if g.size != 3 || len(g.players) != 3 || g.bets[2].Cmp(bullets(100)) != 0 {
		t.Fatalf("newGame %+v", g)
	}

	// Play: whoever holds the gun pulls once, then passes, until someone dies.
	// Passing before pulling is refused.
	holder := holderOf(msg)
	h.cmd(players[holder], "pass")
	h.expect(`pull the trigger at least once before you pass`)
	// Someone out of turn is told whose turn it is.
	other := players[holder%3+1]
	h.cmd(other, "pull")
	h.expect(`It is p\d's turn`)

	var dead int64
	for pulls := 0; pulls < 3 && dead == 0; pulls++ {
		h.cmd(players[holder], "pull")
		m := h.expect(`<i>click</i>|BANG`)
		if strings.Contains(m, "BANG") {
			dead = holder
			break
		}
		h.cmd(players[holder], "pass")
		holder = holderOf(h.expect(`you have the gun`))
	}
	if dead == 0 {
		t.Fatal("nobody died in three pulls of a three-shot revolver")
	}
	h.expect(`Game #1 is over`)
	h.m.Wait()

	if len(h.chain.ends) != 1 {
		t.Fatal("endGame not called once")
	}
	e := h.chain.ends[0]
	if g.players[e.loser] != common.BigToAddress(big.NewInt(dead)) {
		t.Fatalf("endGame named seat %d, but player %d died", e.loser, dead)
	}
	checked, err := fair.Verify(3, g.commit, e.reveal)
	if err != nil {
		t.Fatalf("the reveal does not verify: %v", err)
	}
	// The death came on the pull that fires the committed chamber.
	gm, _ := h.st.Game(context.Background(), 1)
	if gm.State != "ended" || int(gm.Chamber.Int64) != checked.Chamber {
		t.Fatalf("stored game %+v, chamber %d", gm, checked.Chamber)
	}
	lb, _ := h.st.Leaderboard(context.Background(), h.chat, 10)
	if len(lb) != 3 {
		t.Fatalf("leaderboard has %d rows", len(lb))
	}
}

func TestAFKIsPulledForUntilDeath(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AFK = 20 * time.Millisecond })
	a, b := h.player(1, 100), h.player(2, 100)
	h.cmd(a, "roulette", "2", "10")
	h.expect(`Game #1`)
	h.cmd(b, "join")
	first := holderOf(h.expect(`you have the gun`))
	h.expect(`BANG`)
	h.m.Wait()
	if len(h.chain.ends) != 1 {
		t.Fatal("no endGame")
	}
	// Nobody passed, so the first holder took every pull and died.
	if h.chain.newGames[0].players[h.chain.ends[0].loser] != common.BigToAddress(big.NewInt(first)) {
		t.Fatal("the AFK holder did not die holding the gun")
	}
}

func TestStopVoteRefunds(t *testing.T) {
	h := newHarness(t, nil)
	a, b := h.player(1, 100), h.player(2, 100)
	h.cmd(a, "roulette", "2", "10")
	h.expect(`Game #1`)
	h.cmd(b, "join")
	h.expect(`you have the gun`)
	h.cmd(a, "stop")
	h.expect(`p1 votes to stop. 1 of 2`)
	h.cmd(Player{ID: 50, Name: "bystander"}, "stop") // not seated: ignored
	h.cmd(b, "stop")
	h.expect(`Every stake refunded`)
	h.m.Wait()
	if len(h.chain.aborts) != 1 || len(h.chain.ends) != 0 {
		t.Fatalf("aborts %v ends %d", h.chain.aborts, len(h.chain.ends))
	}
}

func TestJoinRefusals(t *testing.T) {
	h := newHarness(t, nil)
	a := h.player(1, 100)
	poor := h.player(2, 5)
	unapproved := h.player(3, 100)
	h.chain.allowance[common.BigToAddress(big.NewInt(3))] = big.NewInt(0)
	house := Player{ID: 4, Name: "house"}
	if err := h.st.Link(context.Background(), 4, "house", common.HexToAddress("0xb07").Hex(), "0x"+strings.Repeat("4", 64), time.Now()); err != nil {
		t.Fatal(err)
	}

	h.cmd(Player{ID: 9, Name: "stranger"}, "roulette", "6", "10")
	h.expect(`connect a wallet first`)
	h.cmd(a, "roulette", "6", "10")
	h.expect(`Game #\d+</b>`)
	h.cmd(a, "join")
	h.expect(`already have a seat`)
	h.cmd(poor, "join")
	h.expect(`holds 5 BULLET and the bet is 10`)
	h.cmd(unapproved, "join")
	h.expect(`not approved`)
	h.cmd(house, "join")
	h.expect(`house does not play`)
	h.cmd(poor, "join", "1")
	h.expect(`minimum bet here is 10`)
	h.cmd(a, "roulette", "6", "10")
	h.expect(`already running`)
	// Alone when the window closes: cancelled, nothing sent to the chain.
	h.expect(`cancelled: nobody else joined`)
	h.m.Wait()
	if len(h.chain.newGames) != 0 {
		t.Fatal("newGame sent for a one-player table")
	}
}

func TestRouletteArguments(t *testing.T) {
	h := newHarness(t, nil)
	a := h.player(1, 100)
	h.cmd(a, "roulette")
	h.expect(`Usage`)
	h.cmd(a, "roulette", "1", "10")
	h.expect(`2 to 12 chambers`)
	h.cmd(a, "roulette", "6", "0.000001")
	h.expect(`minimum bet is 0.00001`)
	h.cmd(a, "roulette", "6", "abc")
	h.expect(`not an amount`)
}

func TestNewGameRefusedCancels(t *testing.T) {
	h := newHarness(t, nil)
	h.chain.newErr = &chain.RevertError{Reason: "Not enough allowance"}
	a, b := h.player(1, 100), h.player(2, 100)
	h.cmd(a, "roulette", "2", "10")
	h.cmd(b, "join")
	h.expect(`escrow refused the bets \(Not enough allowance\)`)
	h.m.Wait()
	g, _ := h.st.Game(context.Background(), 1)
	if g.State != "cancelled" {
		t.Fatalf("state %s", g.State)
	}
}

func TestEndGameFailureAborts(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AFK = 10 * time.Millisecond; c.EndRetries = 1 })
	h.chain.endErr = errors.New("relayer down")
	a, b := h.player(1, 100), h.player(2, 100)
	h.cmd(a, "roulette", "2", "10")
	h.cmd(b, "join")
	h.expect(`BANG`)
	h.expect(`aborted instead`)
	h.expect(`Every stake refunded`)
	h.m.Wait()
	if len(h.chain.aborts) != 1 {
		t.Fatal("no abortGame after endGame failed")
	}
}

func TestRecover(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	// A game that was playing when the bot died, and one still joining.
	playing, _ := h.st.NewGame(ctx, -1, 6, bullets(1), 1, time.Now())
	_ = h.st.Started(ctx, playing, "0x1", nil, time.Now())
	joining, _ := h.st.NewGame(ctx, -2, 6, bullets(1), 1, time.Now())
	h.chain.inGame[-1] = true

	if err := h.m.Recover(); err != nil {
		t.Fatal(err)
	}
	if len(h.chain.aborts) != 1 || h.chain.aborts[0] != -1 {
		t.Fatalf("aborts %v", h.chain.aborts)
	}
	for id, want := range map[int64]string{playing: "aborted", joining: "cancelled"} {
		g, _ := h.st.Game(ctx, id)
		if g.State != want {
			t.Fatalf("game %d is %s, want %s", id, g.State, want)
		}
	}
}

func TestAdminAbortsOrphan(t *testing.T) {
	h := newHarness(t, nil)
	h.chain.inGame[h.chat] = true
	h.cmd(Player{ID: 5, Name: "not admin"}, "abort")
	h.cmd(Player{ID: 99, Name: "admin"}, "abort")
	h.expect(`Every stake refunded`)
	if len(h.chain.aborts) != 1 {
		t.Fatalf("aborts %v", h.chain.aborts)
	}
}

func TestBullets(t *testing.T) {
	for in, want := range map[string]string{"50": "50", "12.5": "12.5", "1000000": "1,000,000", "0.00001": "0.00001", "1,234.5": "1,234.5"} {
		v, err := ParseBullets(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := Bullets(v); got != want {
			t.Fatalf("%s -> %s, want %s", in, got, want)
		}
	}
	for _, bad := range []string{"0", "-1", "1.123456789", "x"} {
		if _, err := ParseBullets(bad); err == nil {
			t.Fatalf("parsed %q", bad)
		}
	}
	if Bullets(big.NewInt(-150_000_000)) != "-1.5" {
		t.Fatal("negative")
	}
}
