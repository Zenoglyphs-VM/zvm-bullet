// Package keys is the operator's secrets file: the bot's key, the two
// wallets mainnet kept apart from it, and the VRF seed.
//
// Unlike the rest of zvm's devnet keys, none of these may be derived from
// anything public. The bot owns the escrow, and the escrow's owner can call
// newGame with any player list: with a public key, anyone could stake a
// connected player's approved BULLET into a game against themselves and name
// that player the loser. The VRF seed is secret for the reason the bullet is.
package keys

import (
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Zenoglyphs-VM/zvm-bullet/internal/vrf"
)

// File is the JSON on disk. Mainnet's roles, by the token's own names:
// the bot is the deployer, the escrow's owner and myWallet (the dev wallet,
// "same as deployer, in order for funds to be used as gas"); the marketing
// wallet receives 10% of supply at launch; the revenue wallet receives 1% of
// every taxed trade and 9% of every loser's stake.
type File struct {
	Bot       string `json:"bot"`
	Marketing string `json:"marketing"`
	Revenue   string `json:"revenue"`
	VRFSeed   string `json:"vrfSeed"`
}

type Keys struct {
	Bot, Marketing, Revenue *ecdsa.PrivateKey
	VRF                     *vrf.PrivateKey
}

func (k *Keys) BotAddress() common.Address { return crypto.PubkeyToAddress(k.Bot.PublicKey) }
func (k *Keys) MarketingAddress() common.Address {
	return crypto.PubkeyToAddress(k.Marketing.PublicKey)
}
func (k *Keys) RevenueAddress() common.Address { return crypto.PubkeyToAddress(k.Revenue.PublicKey) }

// Generate writes a new secrets file and refuses to replace one: losing the
// bot key loses the escrow's owner, and the token's ownership is renounced at
// launch, so there is no second chance to point it at another escrow.
func Generate(path string) (*Keys, error) {
	var f File
	for _, dst := range []*string{&f.Bot, &f.Marketing, &f.Revenue} {
		k, err := crypto.GenerateKey()
		if err != nil {
			return nil, err
		}
		*dst = hex.EncodeToString(crypto.FromECDSA(k))
	}
	seed := make([]byte, vrf.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	f.VRFSeed = hex.EncodeToString(seed)
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := out.Write(append(raw, '\n')); err != nil {
		out.Close()
		return nil, err
	}
	if err := out.Close(); err != nil {
		return nil, err
	}
	return Load(path)
}

// Load reads a secrets file.
func Load(path string) (*Keys, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var k Keys
	for _, x := range []struct {
		name string
		hex  string
		dst  **ecdsa.PrivateKey
	}{{"bot", f.Bot, &k.Bot}, {"marketing", f.Marketing, &k.Marketing}, {"revenue", f.Revenue, &k.Revenue}} {
		if x.hex == "" {
			return nil, fmt.Errorf("%s: no %s key", path, x.name)
		}
		key, err := crypto.HexToECDSA(strings.TrimPrefix(x.hex, "0x"))
		if err != nil {
			return nil, fmt.Errorf("%s: %s key: %w", path, x.name, err)
		}
		*x.dst = key
	}
	seed, err := hex.DecodeString(strings.TrimPrefix(f.VRFSeed, "0x"))
	if err != nil {
		return nil, fmt.Errorf("%s: vrfSeed: %w", path, err)
	}
	if k.VRF, err = vrf.NewKey(seed); err != nil {
		return nil, fmt.Errorf("%s: vrfSeed: %w", path, err)
	}
	if k.BotAddress() == k.MarketingAddress() || k.BotAddress() == k.RevenueAddress() || k.MarketingAddress() == k.RevenueAddress() {
		return nil, errors.New("the bot, marketing and revenue keys must be three different keys")
	}
	return &k, nil
}

// DevEVMKey is zvm's deterministic devnet key, keccak256("zvm-dev-evm-<i>"),
// the same derivation as zvm's devnet.EVMKey. Index 900 is the devnet
// deployer, which holds the ZNN that provisions everything here.
func DevEVMKey(i uint32) *ecdsa.PrivateKey {
	key, err := crypto.ToECDSA(crypto.Keccak256([]byte(fmt.Sprintf("zvm-dev-evm-%d", i))))
	if err != nil {
		panic(err)
	}
	return key
}
