package game

import (
	"fmt"
	"html"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Zenoglyphs-VM/zvm-bullet/internal/zns"
)

// Links builds the URLs every on-chain action is shown with, the way mainnet's
// bot linked each escrow transaction on Etherscan.
type Links struct {
	Explorer string // e.g. http://localhost:47080/explorer
	Swap     string // e.g. http://localhost:47080/glyphswap
	Token    common.Address
	// Names reads primary names for Address; nil prints bare addresses.
	Names *zns.Names
}

func (l Links) Tx(h common.Hash) string {
	return fmt.Sprintf(`<a href="%s/tx/%s">%s…%s</a>`, l.Explorer, h.Hex(), h.Hex()[:8], h.Hex()[62:])
}

// Address links an address to the explorer, reading as its ZNS primary name
// when it has one (verified both ways by zns.Names), and as 0x1234…abcd
// otherwise.
func (l Links) Address(a common.Address) string {
	if name := l.Names.Name(a); name != "" {
		return fmt.Sprintf(`<a href="%s/address/%s">%s</a>`, l.Explorer, a.Hex(), html.EscapeString(name))
	}
	return fmt.Sprintf(`<a href="%s/address/%s">%s…%s</a>`, l.Explorer, a.Hex(), a.Hex()[:6], a.Hex()[38:])
}

// Contract is the token's contract tab, where connectAndApprove is called.
func (l Links) Contract() string {
	return fmt.Sprintf("%s/address/%s?tab=contract", l.Explorer, l.Token.Hex())
}

// Buy is the swap page with BULLET preselected against ZNN.
func (l Links) Buy() string {
	return fmt.Sprintf("%s/?in=znn&out=%s", l.Swap, strings.ToLower(l.Token.Hex()))
}

var unit = big.NewInt(100_000_000) // 10^8: BULLET has 8 decimals

// Bullets formats base units as BULLET, without trailing zeros.
func Bullets(v *big.Int) string {
	if v == nil {
		return "0"
	}
	neg := v.Sign() < 0
	a := new(big.Int).Abs(v)
	q, r := new(big.Int).QuoRem(a, unit, new(big.Int))
	s := q.String()
	if r.Sign() != 0 {
		frac := strings.TrimRight(fmt.Sprintf("%08s", r.String()), "0")
		s += "." + frac
	}
	// Group thousands in the whole part.
	whole, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	s = b.String()
	if frac != "" {
		s += "." + frac
	}
	if neg {
		s = "-" + s
	}
	return s
}

// ParseBullets reads a BULLET amount such as "50", "12.5" or "1,000".
func ParseBullets(s string) (*big.Int, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() <= 0 {
		return nil, fmt.Errorf("%q is not an amount of BULLET", s)
	}
	r.Mul(r, new(big.Rat).SetInt(unit))
	if !r.IsInt() {
		return nil, fmt.Errorf("%q has more than 8 decimals", s)
	}
	return r.Num(), nil
}

// Mention links a name to a Telegram user, which notifies them.
func Mention(id int64, name string) string {
	return fmt.Sprintf(`<a href="tg://user?id=%d">%s</a>`, id, html.EscapeString(name))
}

func esc(s string) string { return html.EscapeString(s) }
