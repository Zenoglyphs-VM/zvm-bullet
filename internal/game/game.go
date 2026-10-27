// Package game runs Bullet Game's tables: one per Telegram group, from
// /roulette to the payout, following the rules the original bot documented.
//
//   - A game waits for players until the revolver is full or the join window
//     closes; it starts with two or more, and the most players is the number
//     of chambers.
//   - At the start the bot draws the chamber (package fair), commits to it,
//     and collects every stake into the escrow with newGame.
//   - The gun goes round in join order. The holder must /pull at least once
//     before they may /pass. Each pull fires the next chamber.
//   - A holder who goes quiet is pulled for, again and again, until they come
//     back or die.
//   - The death ends the game: endGame names the loser, pays the survivors in
//     proportion to their stakes, burns 1% and sends 9% to revenue.
//   - Every player voting /stop ends the game with each stake refunded.
//
// The chain is the arbiter of money and a table never pays anyone itself.
package game

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Zenoglyphs-VM/zvm-bullet/internal/chain"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/escrow"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/fair"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/store"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/vrf"
)

// Chain is what a table needs of the escrow; *escrow.Escrow is the real one.
type Chain interface {
	Holdings(ctx context.Context, player common.Address) (balance, allowance *big.Int, err error)
	InProgress(ctx context.Context, chat int64) (bool, error)
	NewGame(ctx context.Context, chat int64, size int, minBet *big.Int, commit [32]byte, players []common.Address, bets []*big.Int) (*escrow.Started, error)
	EndGame(ctx context.Context, chat int64, loser int, reveal []string) (*escrow.Ended, error)
	AbortGame(ctx context.Context, chat int64) (common.Hash, error)
}

// Out is where a table speaks: the group's chat.
type Out interface {
	Send(ctx context.Context, chat int64, html string) error
}

type Config struct {
	Chain  Chain
	Out    Out
	Store  *store.Store
	VRF    *vrf.PrivateKey
	Links  Links
	MinBet *big.Int // the escrow's minimumBet

	// House addresses may never sit at a table: the bot knows the chamber from
	// the moment a game starts.
	House map[common.Address]bool
	// Admins may /abort a game, which refunds every stake.
	Admins map[int64]bool

	MaxSize    int           // largest revolver /roulette accepts
	JoinWindow time.Duration // how long a table waits for players
	AFK        time.Duration // how long a holder has before the bot pulls for them
	EndRetries int           // endGame attempts before falling back to abortGame

	Now func() time.Time
	Log *slog.Logger
}

func (c *Config) defaults() {
	if c.MaxSize == 0 {
		c.MaxSize = 12
	}
	if c.JoinWindow == 0 {
		c.JoinWindow = 2 * time.Minute
	}
	if c.AFK == 0 {
		c.AFK = 30 * time.Second
	}
	if c.EndRetries == 0 {
		c.EndRetries = 3
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.MinBet == nil {
		c.MinBet = big.NewInt(1)
	}
}

// Player is who sent a command.
type Player struct {
	ID   int64
	Name string
}

type Manager struct {
	cfg    Config
	ctx    context.Context
	mu     sync.Mutex
	tables map[int64]*table
	wg     sync.WaitGroup
}

func NewManager(ctx context.Context, cfg Config) *Manager {
	cfg.defaults()
	return &Manager{cfg: cfg, ctx: ctx, tables: map[int64]*table{}}
}

// Wait blocks until every table has finished.
func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) say(chat int64, html string) {
	if err := m.cfg.Out.Send(m.ctx, chat, html); err != nil {
		m.cfg.Log.Warn("send", "chat", chat, "err", err)
	}
}

// Command handles one group command. cmd is without the slash or @bot suffix.
func (m *Manager) Command(chat int64, from Player, cmd string, args []string) {
	switch cmd {
	case "roulette":
		m.roulette(chat, from, args)
	case "join", "pull", "pass", "stop", "game", "abort":
		m.mu.Lock()
		t := m.tables[chat]
		if t == nil {
			m.mu.Unlock()
			if cmd == "abort" && m.cfg.Admins[from.ID] {
				m.abortOrphan(chat)
				return
			}
			if cmd == "join" || cmd == "game" {
				m.say(chat, "No game is waiting for players. Start one with <code>/roulette &lt;revolver size&gt; &lt;minimum bet&gt;</code>, for example <code>/roulette 6 50</code>.")
			}
			return
		}
		select {
		case t.events <- event{cmd: cmd, from: from, args: args}:
		default:
			m.say(chat, "The table is busy; try again in a moment.")
		}
		m.mu.Unlock()
	case "leaderboard":
		m.leaderboard(chat)
	}
}

func (m *Manager) roulette(chat int64, from Player, args []string) {
	usage := "Usage: <code>/roulette &lt;revolver size&gt; &lt;minimum bet&gt;</code>, for example <code>/roulette 6 50</code> for a six-shot revolver and 50 BULLET to join."
	if len(args) != 2 {
		m.say(chat, usage)
		return
	}
	size, err := strconv.Atoi(args[0])
	if err != nil || size < 2 || size > m.cfg.MaxSize {
		m.say(chat, fmt.Sprintf("The revolver needs 2 to %d chambers. %s", m.cfg.MaxSize, usage))
		return
	}
	minBet, err := ParseBullets(args[1])
	if err != nil {
		m.say(chat, esc(err.Error())+". "+usage)
		return
	}
	if minBet.Cmp(m.cfg.MinBet) < 0 {
		m.say(chat, fmt.Sprintf("The escrow's minimum bet is %s BULLET.", Bullets(m.cfg.MinBet)))
		return
	}

	m.mu.Lock()
	if m.tables[chat] != nil {
		m.mu.Unlock()
		m.say(chat, "A game is already running here. One at a time per group: /join it, or wait for it to finish.")
		return
	}
	t := &table{m: m, chat: chat, size: size, minBet: minBet, creator: from, events: make(chan event, 64), stopVotes: map[int64]bool{}}
	m.tables[chat] = t
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		t.run()
	}()
}

// finish removes a table and answers whatever was queued for it after it
// stopped reading.
func (m *Manager) finish(t *table) {
	m.mu.Lock()
	if m.tables[t.chat] == t {
		delete(m.tables, t.chat)
	}
	m.mu.Unlock()
	for {
		select {
		case <-t.events:
		default:
			return
		}
	}
}

// abortOrphan refunds a game the escrow holds that no table is running, which
// is what a crash between newGame and endGame leaves.
func (m *Manager) abortOrphan(chat int64) {
	in, err := m.cfg.Chain.InProgress(m.ctx, chat)
	if err != nil {
		m.say(chat, "Could not read the escrow: "+esc(err.Error()))
		return
	}
	if !in {
		m.say(chat, "Nothing to abort: the escrow holds no game for this group.")
		return
	}
	m.say(chat, "Aborting the escrow's game for this group and refunding every stake…")
	tx, err := m.cfg.Chain.AbortGame(m.ctx, chat)
	if err != nil {
		m.say(chat, "abortGame failed: "+esc(err.Error()))
		return
	}
	m.say(chat, "Every stake refunded: "+m.cfg.Links.Tx(tx))
}

func (m *Manager) leaderboard(chat int64) {
	rows, err := m.cfg.Store.Leaderboard(m.ctx, chat, 10)
	if err != nil {
		m.say(chat, "Could not read the leaderboard: "+esc(err.Error()))
		return
	}
	if len(rows) == 0 {
		m.say(chat, "No games have finished in this group yet.")
		return
	}
	var b strings.Builder
	b.WriteString("<b>Leaderboard</b>, by BULLET won or lost\n")
	for i, r := range rows {
		sign := ""
		if r.Net.Sign() > 0 {
			sign = "+"
		}
		fmt.Fprintf(&b, "%d. %s  %s%s  (%d games, %d deaths)\n", i+1, esc(r.Name), sign, Bullets(r.Net), r.Games, r.Deaths)
	}
	m.say(chat, b.String())
}

// Recover settles what a previous run left behind. A table in play cannot be
// resumed honestly because its turn clock is gone. Uncertain prior outcomes
// remain unfinished and stop startup until the operator reconciles them.
func (m *Manager) Recover() error {
	left, err := m.cfg.Store.Unfinished(m.ctx)
	if err != nil {
		return err
	}
	for _, g := range left {
		now := m.cfg.Now()
		if g.State == "joining" {
			if err := m.cfg.Store.Cancelled(m.ctx, g.ID, "the bot restarted before the game started", now); err != nil {
				return fmt.Errorf("recording restart cancellation for game %d: %w", g.ID, err)
			}
			m.say(g.Chat, fmt.Sprintf("The bot restarted, so game #%d is cancelled. No bets were collected.", g.ID))
			continue
		}
		in, err := m.cfg.Chain.InProgress(m.ctx, g.Chat)
		if err != nil {
			return fmt.Errorf("reading restart escrow for game %d: %w", g.ID, err)
		}
		if !in {
			return fmt.Errorf("game %d remains %s but the current escrow is inactive; reconcile its original transaction outcome before restarting", g.ID, g.State)
		}
		tx, err := m.cfg.Chain.AbortGame(m.ctx, g.Chat)
		if err != nil {
			m.cfg.Log.Error("recover: abortGame", "game", g.ID, "chat", g.Chat, "err", err)
			m.say(g.Chat, fmt.Sprintf("The bot restarted during game #%d and could not confirm its refund. Recovery has stopped and requires operator reconciliation.", g.ID))
			return fmt.Errorf("refunding restart game %d: %w", g.ID, err)
		}
		if err := m.cfg.Store.Aborted(m.ctx, g.ID, tx.Hex(), "the bot restarted during the game", now); err != nil {
			return fmt.Errorf("recording restart refund for game %d, transaction %s: %w", g.ID, tx.Hex(), err)
		}
		m.say(g.Chat, fmt.Sprintf("The bot restarted during game #%d, so it is aborted and every stake refunded: %s", g.ID, m.cfg.Links.Tx(tx)))
	}
	return nil
}

// --- a table ---

type event struct {
	cmd  string
	from Player
	args []string
}

type seat struct {
	Player
	addr      common.Address
	requested *big.Int
	bet       *big.Int
}

type table struct {
	m       *Manager
	id      int64
	chat    int64
	size    int
	minBet  *big.Int
	creator Player
	events  chan event

	seats     []*seat
	stopVotes map[int64]bool

	// Play.
	commit  *fair.Commit
	holder  int
	pulls   int  // pulls so far; the next pull fires chamber `pulls`
	pulled  bool // the holder has pulled during this turn
	seq     int
	playing bool
}

func (t *table) cfg() *Config            { return &t.m.cfg }
func (t *table) ctx() context.Context    { return t.m.ctx }
func (t *table) say(html string)         { t.m.say(t.chat, html) }
func (t *table) sayf(f string, a ...any) { t.say(fmt.Sprintf(f, a...)) }

func (t *table) run() {
	defer t.m.finish(t)
	c := t.cfg()
	id, err := c.Store.NewGame(t.ctx(), t.chat, t.size, t.minBet, t.creator.ID, c.Now())
	if err != nil {
		t.say("Could not open a game: " + esc(err.Error()))
		return
	}
	t.id = id

	// The creator joins at the minimum bet; a creator who cannot play
	// cannot open a table.
	if msg := t.join(t.creator, nil); msg != "" {
		_ = c.Store.Cancelled(t.ctx(), t.id, "the creator could not join", c.Now())
		t.say(msg)
		return
	}
	t.sayf("🔫 <b>Game #%d</b>: a %d-shot revolver, %s BULLET to join.\n%s\n\n/join to take a seat (or <code>/join &lt;bet&gt;</code> to stake more). The game starts when all %d seats are taken or in %s, with two players or more.",
		t.id, t.size, Bullets(t.minBet), t.roster(), t.size, human(c.JoinWindow))

	if !t.joining() {
		return
	}
	if !t.start() {
		return
	}
	t.play()
}

// joining runs until the table is full or the window closes. It reports
// whether the game should start.
func (t *table) joining() bool {
	c := t.cfg()
	window := time.NewTimer(c.JoinWindow)
	defer window.Stop()
	for len(t.seats) < t.size {
		select {
		case <-t.ctx().Done():
			return false
		case <-window.C:
			if len(t.seats) < 2 {
				_ = c.Store.Cancelled(t.ctx(), t.id, "not enough players", c.Now())
				t.sayf("Game #%d is cancelled: nobody else joined. Nothing was collected.", t.id)
				return false
			}
			return true
		case ev := <-t.events:
			switch ev.cmd {
			case "join":
				if msg := t.join(ev.from, ev.args); msg != "" {
					t.say(msg)
					continue
				}
				s := t.seats[len(t.seats)-1]
				t.sayf("%s joins with %s BULLET. %d of %d seats taken.", esc(s.Name), Bullets(s.requested), len(t.seats), t.size)
			case "stop":
				if t.vote(ev.from) {
					_ = c.Store.Cancelled(t.ctx(), t.id, "every player voted to stop", c.Now())
					t.sayf("Every player voted to stop. Game #%d is cancelled; nothing was collected.", t.id)
					return false
				}
			case "abort":
				if t.isAdmin(ev.from) {
					_ = c.Store.Cancelled(t.ctx(), t.id, "cancelled by an admin", c.Now())
					t.sayf("Game #%d cancelled by an admin. Nothing was collected.", t.id)
					return false
				}
			case "game":
				t.sayf("Game #%d is waiting for players: %d-shot revolver, %s BULLET to join.\n%s", t.id, t.size, Bullets(t.minBet), t.roster())
			case "pull", "pass":
				t.say("The game has not started yet.")
			}
		}
	}
	return true
}

// join seats a player, or returns why it cannot. Everything the escrow would
// refuse at newGame is checked here, so a refusal is explained to the one
// player it concerns rather than cancelling the game for everyone.
func (t *table) join(p Player, args []string) string {
	c := t.cfg()
	for _, s := range t.seats {
		if s.ID == p.ID {
			return esc(p.Name) + ", you already have a seat."
		}
	}
	if len(t.seats) >= t.size {
		return "Every seat is taken."
	}
	bet := new(big.Int).Set(t.minBet)
	if len(args) > 0 {
		b, err := ParseBullets(args[0])
		if err != nil {
			return esc(err.Error()) + "."
		}
		if b.Cmp(t.minBet) < 0 {
			return fmt.Sprintf("The minimum bet here is %s BULLET.", Bullets(t.minBet))
		}
		bet = b
	}
	u, err := c.Store.User(t.ctx(), p.ID)
	if err != nil || u.Address == "" {
		return esc(p.Name) + ", connect a wallet first: send me /connect in a direct message."
	}
	addr := common.HexToAddress(u.Address)
	if c.House[addr] {
		return "The house does not play: this address belongs to the bot's operator."
	}
	bal, allowance, err := c.Chain.Holdings(t.ctx(), addr)
	if err != nil {
		return "Could not read your balance: " + esc(err.Error())
	}
	if bal.Cmp(bet) < 0 {
		return fmt.Sprintf("%s, your wallet holds %s BULLET and the bet is %s. <a href=\"%s\">Buy BULLET</a>.", esc(p.Name), Bullets(bal), Bullets(bet), c.Links.Buy())
	}
	if allowance.Cmp(bet) < 0 {
		return fmt.Sprintf("%s, the escrow is not approved to take %s BULLET from your wallet. Send me /connect in a direct message to approve it again.", esc(p.Name), Bullets(bet))
	}
	s := &seat{Player: p, addr: addr, requested: bet}
	if err := c.Store.AddSeat(t.ctx(), t.id, store.Seat{Seat: len(t.seats), TgID: p.ID, Name: p.Name, Address: addr.Hex(), Requested: bet}); err != nil {
		return "Could not record your seat: " + esc(err.Error())
	}
	t.seats = append(t.seats, s)
	return ""
}

func (t *table) roster() string {
	var b strings.Builder
	for i, s := range t.seats {
		amt := s.requested
		if s.bet != nil {
			amt = s.bet
		}
		fmt.Fprintf(&b, "%d. %s, %s BULLET\n", i+1, esc(s.Name), Bullets(amt))
	}
	return strings.TrimRight(b.String(), "\n")
}

// vote records a /stop and reports whether every player has now voted.
func (t *table) vote(p Player) bool {
	seated := false
	for _, s := range t.seats {
		if s.ID == p.ID {
			seated = true
		}
	}
	if !seated {
		return false
	}
	t.stopVotes[p.ID] = true
	if len(t.stopVotes) == len(t.seats) {
		return true
	}
	t.sayf("%s votes to stop. %d of %d votes; the game stops if every player votes.", esc(p.Name), len(t.stopVotes), len(t.seats))
	return false
}

func (t *table) isAdmin(p Player) bool { return t.cfg().Admins[p.ID] }

// start draws the chamber, commits to it and collects the stakes.
func (t *table) start() bool {
	c := t.cfg()
	players := make([]common.Address, len(t.seats))
	bets := make([]*big.Int, len(t.seats))
	for i, s := range t.seats {
		players[i], bets[i] = s.addr, s.requested
	}
	commit, err := fair.New(c.VRF, t.size, bets, c.Now())
	if err != nil {
		t.fail("drawing the chamber: " + err.Error())
		return false
	}
	t.commit = commit
	reveal := strings.Join(commit.Reveal(c.VRF.Public()), "\n")
	if err := c.Store.Starting(t.ctx(), t.id, commit.Chamber, fmt.Sprintf("0x%x", commit.Hash), reveal); err != nil {
		t.fail("recording the commitment: " + err.Error())
		return false
	}
	t.sayf("Seats closed. Loading one bullet into the %d-shot revolver and collecting the bets into escrow…", t.size)

	// A game the escrow holds for this group with no table running it is a
	// leftover from a crash; newGame would refuse, so refund it first.
	if in, err := c.Chain.InProgress(t.ctx(), t.chat); err == nil && in {
		if tx, err := c.Chain.AbortGame(t.ctx(), t.chat); err == nil {
			t.say("Refunded a game the escrow still held for this group from before a restart: " + c.Links.Tx(tx))
		}
	}

	st, err := c.Chain.NewGame(t.ctx(), t.chat, t.size, t.minBet, commit.Hash, players, bets)
	if err != nil {
		reason := err.Error()
		var rev *chain.RevertError
		if errors.As(err, &rev) {
			reason = rev.Reason
		}
		_ = c.Store.Cancelled(t.ctx(), t.id, "newGame: "+reason, c.Now())
		t.sayf("Game #%d is cancelled: the escrow refused the bets (%s). Nothing was collected.", t.id, esc(reason))
		return false
	}
	for i, b := range st.Bets {
		t.seats[i].bet = b
	}
	if err := c.Store.Started(t.ctx(), t.id, st.Tx.Hex(), st.Bets, c.Now()); err != nil {
		c.Log.Error("recording the start", "game", t.id, "err", err)
	}
	capped := ""
	for i, s := range t.seats {
		if s.bet.Cmp(s.requested) != 0 {
			capped += fmt.Sprintf("\n%s's bet was capped at %s BULLET: nobody can lose more than the rest of the table staked.", esc(t.seats[i].Name), Bullets(s.bet))
		}
	}
	t.sayf("The bullet is loaded and the bets are in escrow: %s\nCommitment to the chamber: <code>0x%x</code>%s\n\nOrder of play:\n%s",
		c.Links.Tx(st.Tx), commit.Hash, capped, t.roster())
	t.playing = true
	return true
}

// play runs turns until someone dies or everyone stops.
func (t *table) play() {
	c := t.cfg()
	t.stopVotes = map[int64]bool{}
	t.announceHolder()
	afk := time.NewTimer(c.AFK)
	defer afk.Stop()
	reset := func() {
		if !afk.Stop() {
			select {
			case <-afk.C:
			default:
			}
		}
		afk.Reset(c.AFK)
	}
	for {
		select {
		case <-t.ctx().Done():
			return
		case <-afk.C:
			h := t.seats[t.holder]
			t.sayf("%s has not moved in %s, so the bot pulls for them.", esc(h.Name), human(c.AFK))
			if t.fire(true) {
				return
			}
			reset()
		case ev := <-t.events:
			h := t.seats[t.holder]
			switch ev.cmd {
			case "pull":
				if ev.from.ID != h.ID {
					t.sayf("It is %s's turn.", esc(h.Name))
					continue
				}
				if t.fire(false) {
					return
				}
				reset()
			case "pass":
				if ev.from.ID != h.ID {
					t.sayf("It is %s's turn.", esc(h.Name))
					continue
				}
				if !t.pulled {
					t.sayf("%s, pull the trigger at least once before you pass the gun.", esc(h.Name))
					continue
				}
				t.record("pass", false, false)
				t.holder = (t.holder + 1) % len(t.seats)
				t.pulled = false
				t.announceHolder()
				reset()
			case "stop":
				if t.vote(ev.from) {
					t.abort("every player voted to stop")
					return
				}
			case "abort":
				if t.isAdmin(ev.from) {
					t.abort("an admin aborted it")
					return
				}
			case "join":
				t.say("This game has started. Wait for the next one.")
			case "game":
				t.sayf("Game #%d: %d pulls so far on a %d-shot revolver. %s has the gun.\n%s", t.id, t.pulls, t.size, esc(h.Name), t.roster())
			}
		}
	}
}

func (t *table) announceHolder() {
	h := t.seats[t.holder]
	t.sayf("%s, you have the gun. /pull the trigger.", Mention(h.ID, h.Name))
}

func (t *table) record(action string, auto, died bool) {
	c := t.cfg()
	if err := c.Store.Turn(t.ctx(), t.id, t.seq, t.holder, action, auto, died, c.Now()); err != nil {
		c.Log.Error("recording a turn", "game", t.id, "err", err)
	}
	t.seq++
}

// fire pulls the trigger for the holder and reports whether the game ended.
func (t *table) fire(auto bool) bool {
	h := t.seats[t.holder]
	chamber := t.pulls
	t.pulls++
	died := chamber == t.commit.Chamber
	t.record("pull", auto, died)
	if !died {
		t.pulled = true
		t.sayf("<i>click</i>. %s survives pull %d of %d. /pull again or /pass the gun.", esc(h.Name), t.pulls, t.size)
		return false
	}
	t.sayf("💥 <b>BANG.</b> %s is dead, on pull %d of %d.", esc(h.Name), t.pulls, t.size)
	t.settle(t.holder)
	return true
}

// settle sends endGame, retrying a few times: the loser is decided and only
// the transaction is outstanding. If it cannot land, the stakes are refunded
// rather than left in escrow.
func (t *table) settle(loser int) {
	c := t.cfg()
	t.say("Paying the survivors…")
	reveal := t.commit.Reveal(c.VRF.Public())
	var end *escrow.Ended
	var err error
	for attempt := 1; attempt <= c.EndRetries; attempt++ {
		if end, err = c.Chain.EndGame(t.ctx(), t.chat, loser, reveal); err == nil {
			break
		}
		c.Log.Warn("endGame", "game", t.id, "attempt", attempt, "err", err)
		var rev *chain.RevertError
		if errors.As(err, &rev) || attempt == c.EndRetries {
			break // a revert will not change on retry, and after the last try there is nothing to wait for
		}
		select {
		case <-t.ctx().Done():
			return
		case <-time.After(time.Duration(attempt) * 5 * time.Second):
		}
	}
	if err != nil {
		t.sayf("endGame failed (%s), so the game is aborted instead.", esc(err.Error()))
		t.abort("endGame failed: " + err.Error())
		return
	}
	if err := c.Store.Ended(t.ctx(), t.id, loser, end.Tx.Hex(), end.Winnings, c.Now()); err != nil {
		c.Log.Error("recording the end", "game", t.id, "err", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<b>Game #%d is over.</b> %s lost %s BULLET.\n", t.id, esc(t.seats[loser].Name), Bullets(t.seats[loser].bet))
	for i, s := range t.seats {
		if i == loser {
			continue
		}
		fmt.Fprintf(&b, "%s wins %s BULLET and gets their %s back.\n", esc(s.Name), Bullets(end.Winnings[i]), Bullets(s.bet))
	}
	fmt.Fprintf(&b, "%s BULLET burned, %s to the revenue share.\nPaid out: %s\nThe bullet was in chamber %d of %d; the proof is in that transaction.",
		Bullets(end.Burned), Bullets(end.Revenue), c.Links.Tx(end.Tx), t.commit.Chamber+1, t.size)
	t.say(b.String())
}

func (t *table) abort(why string) {
	c := t.cfg()
	t.say("Refunding every stake…")
	tx, err := c.Chain.AbortGame(t.ctx(), t.chat)
	if err != nil {
		c.Log.Error("abortGame", "game", t.id, "err", err)
		t.sayf("abortGame failed too (%s). The stakes are still in escrow; an admin can /abort once the chain answers.", esc(err.Error()))
		return
	}
	_ = c.Store.Aborted(t.ctx(), t.id, tx.Hex(), why, c.Now())
	t.sayf("Game #%d is aborted (%s). Every stake refunded: %s", t.id, esc(why), c.Links.Tx(tx))
}

func (t *table) fail(why string) {
	c := t.cfg()
	_ = c.Store.Cancelled(t.ctx(), t.id, why, c.Now())
	t.sayf("Game #%d is cancelled: %s. Nothing was collected.", t.id, esc(why))
}

func human(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		if d == time.Minute {
			return "a minute"
		}
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	}
	return fmt.Sprintf("%d seconds", int(d.Round(time.Second)/time.Second))
}
