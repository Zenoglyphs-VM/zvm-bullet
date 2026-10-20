package game

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Zenoglyphs-VM/zvm-bullet/internal/escrow"
	"github.com/Zenoglyphs-VM/zvm-bullet/internal/store"
)

// SecretTTL is how long a /connect secret stays good.
const SecretTTL = time.Hour

var txHash = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)

// Connector is the direct-message side of the bot: linking a wallet the way
// mainnet's did. /connect hands out a secret; the player calls
// connectAndApprove(secret) on the BULLET contract, which approves the escrow
// and carries the secret on chain; the player sends the transaction hash
// back; the bot reads the call and links the sender's address.
type Connector struct {
	Escrow *escrow.Escrow
	Store  *store.Store
	Links  Links
	House  map[common.Address]bool
	Now    func() time.Time
}

// Help is the direct-message introduction.
func (c *Connector) Help() string {
	return fmt.Sprintf(`<b>Bullet Game</b> is Russian Roulette in Telegram, played for BULLET on the ZVM.

1. Send /connect here. You get a secret number.
2. On the <a href="%s">BULLET contract</a>, open Write and call <code>connectAndApprove</code> with that number, from MetaMask, Rabby or a Zenon account.
3. Send me the transaction hash.
4. <a href="%s">Buy BULLET</a>.
5. In a group where I host games, <code>/roulette 6 50</code> starts a six-shot game at 50 BULLET, and /join takes a seat.

/wallet shows the wallet you have connected.`, c.Links.Contract(), c.Links.Buy())
}

func (c *Connector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Connect issues a fresh secret.
func (c *Connector) Connect(ctx context.Context, userID int64) string {
	var secret uint32
	var err error
	for range 5 {
		var b [4]byte
		if _, err = rand.Read(b[:]); err != nil {
			break
		}
		secret = binary.BigEndian.Uint32(b[:])
		if secret == 0 {
			continue
		}
		if err = c.Store.SetConnect(ctx, userID, secret, c.now()); err == nil {
			break
		}
	}
	if err != nil {
		return "Could not issue a secret: " + esc(err.Error())
	}
	return fmt.Sprintf(`Your secret is <code>%d</code>. It is good for an hour.

On the <a href="%s">BULLET contract</a>, open <b>Write</b>, connect your wallet and call <code>connectAndApprove</code> with <code>%d</code> as the secret. That approves the escrow to take your bets when a game starts, and proves the wallet is yours.

Then send me the transaction hash (0x…).`, secret, c.Links.Contract(), secret)
}

// Hash handles a transaction hash sent in a DM.
func (c *Connector) Hash(ctx context.Context, userID int64, name, text string) string {
	if !txHash.MatchString(text) {
		return ""
	}
	secret, issued, err := c.Store.PendingConnect(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return "Send /connect first: I need to give you a secret to put in the transaction."
	}
	if err != nil {
		return "Could not read your secret: " + esc(err.Error())
	}
	if c.now().Sub(issued) > SecretTTL {
		return "Your secret has expired. Send /connect for a new one."
	}
	got, err := c.Escrow.ReadConnect(ctx, common.HexToHash(text))
	if err != nil {
		return "That transaction does not connect a wallet: " + esc(err.Error()) + "."
	}
	if got.Secret != secret {
		return fmt.Sprintf("That transaction carries the secret %d, and yours is %d.", got.Secret, secret)
	}
	if c.House[got.Owner] {
		return "That address belongs to the bot's operator and cannot play."
	}
	if err := c.Store.Link(ctx, userID, name, got.Owner.Hex(), text, c.now()); err != nil {
		if errors.Is(err, store.ErrTxUsed) {
			return "That transaction has already been used to connect a wallet."
		}
		return "Could not save the link: " + esc(err.Error())
	}
	return fmt.Sprintf("Connected: %s is yours, and the escrow is approved to take your bets.\n\n%s", c.Links.Address(got.Owner), c.holdings(ctx, got.Owner))
}

// Wallet shows the connected wallet.
func (c *Connector) Wallet(ctx context.Context, userID int64) string {
	u, err := c.Store.User(ctx, userID)
	if err != nil || u.Address == "" {
		return "No wallet connected. Send /connect to connect one."
	}
	a := common.HexToAddress(u.Address)
	return fmt.Sprintf("Your wallet: %s\n%s", c.Links.Address(a), c.holdings(ctx, a))
}

func (c *Connector) holdings(ctx context.Context, a common.Address) string {
	bal, allowance, err := c.Escrow.Holdings(ctx, a)
	if err != nil {
		return "Could not read the balance: " + esc(err.Error())
	}
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	approved := "unlimited"
	if allowance.Cmp(max) != 0 {
		approved = Bullets(allowance) + " BULLET"
	}
	s := fmt.Sprintf("Balance: %s BULLET. The escrow may take: %s.", Bullets(bal), approved)
	if bal.Sign() == 0 {
		s += fmt.Sprintf(" <a href=\"%s\">Buy BULLET</a>.", c.Links.Buy())
	}
	return s
}
