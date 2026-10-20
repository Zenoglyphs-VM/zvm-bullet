package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/zns"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Zenoglyphs-VM/zvm-bullet/internal/chain"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/deploy"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/escrow"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/game"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/keys"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/store"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/tg"
)

func cmdBot(ctx context.Context, args []string) error {
	// Every setting defaults from the environment, so the whole deployment is
	// zvm-bullet/.env and the command line is just "bot". A flag still wins,
	// for a run by hand.
	join0, err := envDuration("BULLET_JOIN_WINDOW", 2*time.Minute)
	if err != nil {
		return err
	}
	afk0, err := envDuration("BULLET_AFK", 30*time.Second)
	if err != nil {
		return err
	}
	max0, err := envInt("BULLET_MAX_SIZE", 12)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("bot", flag.ExitOnError)
	rpc := fs.String("rpc", envOr("BULLET_RPC", os.Getenv("ZVM_RPC")), "ZVM JSON-RPC ($BULLET_RPC, else $ZVM_RPC)")
	keyFile := fs.String("keys", os.Getenv("BULLET_KEYS"), "the operator's secrets file ($BULLET_KEYS)")
	record := fs.String("record", os.Getenv("BULLET_RECORD"), "the deployment record ($BULLET_RECORD)")
	dbPath := fs.String("db", envOr("BULLET_DB", "bullet.db"), "SQLite database ($BULLET_DB)")
	chats := fs.String("chats", os.Getenv("BULLET_CHATS"), "comma-separated Telegram group ids the bot hosts games in, empty for any group it is in ($BULLET_CHATS)")
	admins := fs.String("admins", os.Getenv("BULLET_ADMINS"), "comma-separated Telegram user ids allowed to /abort ($BULLET_ADMINS)")
	public := fs.String("public", envOr("BULLET_PUBLIC", os.Getenv("ZVM_GATEWAY")), "the public origin the explorer, swap page and docs are under ($BULLET_PUBLIC, else $ZVM_GATEWAY)")
	znsRegistry := fs.String("zns-registry", os.Getenv("ZNS_REGISTRY"), "ZNS's ENSRegistry on this chain, for names in messages; empty for addresses only ($ZNS_REGISTRY)")
	join := fs.Duration("join-window", join0, "how long a game waits for players ($BULLET_JOIN_WINDOW)")
	afk := fs.Duration("afk", afk0, "how long a player has before the bot pulls for them ($BULLET_AFK)")
	maxSize := fs.Int("max-size", max0, "largest revolver /roulette accepts ($BULLET_MAX_SIZE)")
	_ = fs.Parse(args)
	if err := need("rpc", *rpc, "BULLET_RPC or ZVM_RPC"); err != nil {
		return err
	}
	if err := need("public", *public, "BULLET_PUBLIC or ZVM_GATEWAY"); err != nil {
		return err
	}

	token := os.Getenv("BULLET_TELEGRAM_TOKEN")
	if token == "" {
		return errors.New("set BULLET_TELEGRAM_TOKEN")
	}
	if *keyFile == "" || *record == "" {
		return errors.New("set BULLET_KEYS and BULLET_RECORD, or pass -keys and -record")
	}
	k, err := keys.Load(*keyFile)
	if err != nil {
		return err
	}
	rec, err := deploy.LoadRecord(*record)
	if err != nil {
		return err
	}
	if rec.Bot != k.BotAddress() {
		return fmt.Errorf("%s names %s as the bot, and these keys are %s", *record, rec.Bot, k.BotAddress())
	}
	allowed, err := idSet(*chats)
	if err != nil {
		return fmt.Errorf("-chats: %w", err)
	}
	adminIDs, err := idSet(*admins)
	if err != nil {
		return fmt.Errorf("-admins: %w", err)
	}

	c, err := chain.Dial(ctx, *rpc, k.Bot)
	if err != nil {
		return err
	}
	if c.ChainID.Uint64() != rec.ChainID {
		return fmt.Errorf("%s is chain %d, the deployment is on %d", *rpc, c.ChainID, rec.ChainID)
	}
	esc := &escrow.Escrow{C: c, Token: rec.Token, Escrow: rec.Escrow}
	minBet, err := esc.MinimumBet(ctx)
	if err != nil {
		return err
	}
	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	log := slog.Default()
	bot := tg.New(token, log)
	me, err := bot.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("telegram: %w", err)
	}
	log.Info("bot", "username", me.Username, "address", c.From, "token", rec.Token, "escrow", rec.Escrow)

	origin := strings.TrimRight(*public, "/")
	links := game.Links{Explorer: origin + "/explorer", Swap: origin + "/glyphswap", Token: rec.Token, Names: zns.New(c.Eth, common.HexToAddress(*znsRegistry))}
	house := map[common.Address]bool{rec.Bot: true, rec.Marketing: true, rec.Revenue: true, rec.Token: true, rec.Escrow: true, rec.Pair: true}

	mgr := game.NewManager(ctx, game.Config{
		Chain: esc, Out: bot, Store: db, VRF: k.VRF, Links: links, MinBet: minBet,
		House: house, Admins: adminIDs, MaxSize: *maxSize, JoinWindow: *join, AFK: *afk, Log: log,
	})
	conn := &game.Connector{Escrow: esc, Store: db, Links: links, House: house}
	if err := mgr.Recover(); err != nil {
		return fmt.Errorf("recovering unfinished games: %w", err)
	}
	if err := registerCommands(ctx, bot); err != nil {
		log.Warn("setMyCommands", "err", err)
	}

	var offset int64
	for ctx.Err() == nil {
		updates, err := bot.Updates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Warn("getUpdates", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			if u.Message == nil || u.Message.From == nil || u.Message.From.IsBot {
				continue
			}
			handle(ctx, bot, me.Username, mgr, conn, allowed, u.Message)
		}
	}
	mgr.Wait()
	return nil
}

func handle(ctx context.Context, bot *tg.Client, username string, mgr *game.Manager, conn *game.Connector, allowed map[int64]bool, m *tg.Message) {
	text := strings.TrimSpace(m.Text)
	cmd, args := parseCommand(text, username)
	from := game.Player{ID: m.From.ID, Name: m.From.Name()}
	reply := func(html string) {
		if html == "" {
			return
		}
		if err := bot.Send(ctx, m.Chat.ID, html); err != nil {
			slog.Warn("send", "chat", m.Chat.ID, "err", err)
		}
	}

	if m.Chat.Type == "private" {
		switch cmd {
		case "start", "help":
			reply(conn.Help())
		case "connect":
			reply(conn.Connect(ctx, from.ID))
		case "wallet", "balance":
			reply(conn.Wallet(ctx, from.ID))
		case "":
			if r := conn.Hash(ctx, from.ID, from.Name, text); r != "" {
				reply(r)
			}
		default:
			reply("Games are played in groups. Here: /connect, /wallet, /help.")
		}
		return
	}
	if m.Chat.Type != "group" && m.Chat.Type != "supergroup" {
		return
	}
	if cmd == "" {
		return
	}
	if len(allowed) > 0 && !allowed[m.Chat.ID] {
		if cmd == "roulette" {
			reply("This bot hosts games in its own group only.")
		}
		return
	}
	switch cmd {
	case "help", "start":
		reply("<code>/roulette 6 50</code> starts a six-shot game at 50 BULLET. /join takes a seat, /pull and /pass play your turn, /stop votes to call the game off, /leaderboard ranks the group. To connect a wallet, send me /connect in a direct message.")
	case "connect", "wallet":
		reply("Send me that in a direct message.")
	default:
		mgr.Command(m.Chat.ID, from, cmd, args)
	}
}

// parseCommand splits "/join@bot 50" into ("join", ["50"]). A command
// addressed to another bot comes back empty.
func parseCommand(text, username string) (string, []string) {
	if !strings.HasPrefix(text, "/") {
		return "", nil
	}
	fields := strings.Fields(text)
	name := strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	if at := strings.IndexByte(name, '@'); at >= 0 {
		if !strings.EqualFold(name[at+1:], username) {
			return "", nil
		}
		name = name[:at]
	}
	return name, fields[1:]
}

func idSet(s string) (map[int64]bool, error) {
	out := map[int64]bool{}
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, nil
}

func registerCommands(ctx context.Context, bot *tg.Client) error {
	if err := bot.SetCommands(ctx, "all_private_chats", []tg.Command{
		{Command: "connect", Description: "connect a wallet"},
		{Command: "wallet", Description: "the wallet you have connected"},
		{Command: "help", Description: "how to play"},
	}); err != nil {
		return err
	}
	return bot.SetCommands(ctx, "all_group_chats", []tg.Command{
		{Command: "roulette", Description: "start a game: /roulette <revolver size> <minimum bet>"},
		{Command: "join", Description: "take a seat: /join [bet]"},
		{Command: "pull", Description: "pull the trigger"},
		{Command: "pass", Description: "pass the gun, after pulling at least once"},
		{Command: "stop", Description: "vote to call the game off, refunding every stake"},
		{Command: "game", Description: "the game in progress"},
		{Command: "leaderboard", Description: "who has won and lost the most"},
	})
}

// need reports a node or origin left unset, and what sets it. None is built
// in: .env.example names a local zvm-devnet's.
func need(flagName, value, env string) error {
	if value == "" {
		return fmt.Errorf("-%s is not set: pass it, or set %s", flagName, env)
	}
	return nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// A setting that is present but unreadable is an error, not the default: a
// typo in .env should stop the bot rather than run it with a value nobody set.
func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}

func envInt(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}
