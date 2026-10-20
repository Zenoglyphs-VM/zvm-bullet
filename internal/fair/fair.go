// Package fair is the commit and the reveal that make the bullet's chamber
// checkable, as the original game's docs describe them:
//
//	alpha   = UNIX time + [each player's bet]           the VRF's input
//	beta    = VRF(alpha)                                 64 bytes
//	chamber = beta mod revolverSize                      beta read big-endian
//	commit  = SHA-256(decimal(chamber) || salt)          sent with newGame
//
// and at the end, endGame carries beta, salt, the VRF public key, the proof
// and alpha, labelled as mainnet labelled them. With those, anyone checks
// that beta is the VRF's output for alpha under the published key, that the
// chamber follows from beta, and that it hashes to the commitment the game
// started with, so the chamber was fixed before the first pull.
//
// The encoding is pinned here because the docs leave it open: the chamber is
// its decimal digits in ASCII ("0" to "11"), the salt is 16 random bytes
// written as 32 lowercase hex characters, and the hash is over the ASCII of
// the two concatenated. alpha is "<seconds>.<microseconds>,[b1, b2, ...]",
// bets in base units in join order, the shape mainnet's reveals have.
//
// What this does not prove, in this design or mainnet's: the bot chooses the
// moment in alpha and holds the VRF key, so it could try timestamps until one
// puts the bullet where it likes before it commits. The commitment stops the
// chamber moving once play starts; it does not stop that. Mainnet had the
// same property.
package fair

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/Zenoglyphs-VM/zvm-bullet/internal/vrf"
)

type Commit struct {
	Size    int
	Alpha   string
	Beta    []byte
	Proof   []byte
	Salt    string // 32 lowercase hex characters
	Chamber int
	Hash    [32]byte
}

// Alpha formats the VRF input.
func Alpha(now time.Time, bets []*big.Int) string {
	parts := make([]string, len(bets))
	for i, b := range bets {
		parts[i] = b.String()
	}
	return fmt.Sprintf("%d.%06d,[%s]", now.Unix(), now.Nanosecond()/1000, strings.Join(parts, ", "))
}

// ChamberOf reads beta as a big-endian integer and reduces it by the size.
func ChamberOf(beta []byte, size int) int {
	return int(new(big.Int).Mod(new(big.Int).SetBytes(beta), big.NewInt(int64(size))).Int64())
}

// Hash is the commitment to a chamber under a salt.
func Hash(chamber int, salt string) [32]byte {
	return sha256.Sum256([]byte(strconv.Itoa(chamber) + salt))
}

// New draws a game's chamber: a fresh salt, the VRF over alpha, and the
// commitment.
func New(key *vrf.PrivateKey, size int, bets []*big.Int, now time.Time) (*Commit, error) {
	if size < 2 {
		return nil, errors.New("a revolver has at least two chambers")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	alpha := Alpha(now, bets)
	proof, beta, err := key.Prove([]byte(alpha))
	if err != nil {
		return nil, err
	}
	c := &Commit{Size: size, Alpha: alpha, Beta: beta, Proof: proof, Salt: hex.EncodeToString(salt)}
	c.Chamber = ChamberOf(beta, size)
	c.Hash = Hash(c.Chamber, c.Salt)
	return c, nil
}

// Reveal is endGame's string array, labelled the way mainnet's were.
func (c *Commit) Reveal(publicKey []byte) []string {
	return []string{
		"beta: 0x" + hex.EncodeToString(c.Beta),
		"salt: " + c.Salt,
		"publickey: 0x" + hex.EncodeToString(publicKey),
		"proof: 0x" + hex.EncodeToString(c.Proof),
		"alpha: " + strconv.Quote(c.Alpha),
	}
}

// Checked is what Verify establishes.
type Checked struct {
	Chamber   int
	Alpha     string
	PublicKey []byte
}

// Verify checks an endGame reveal against the size and commitment its newGame
// carried. It does not decide whose key is trusted: a caller compares
// PublicKey with the operator's published one.
func Verify(size int, commit [32]byte, reveal []string) (*Checked, error) {
	if size < 2 {
		return nil, errors.New("revolver size below two")
	}
	f := map[string]string{}
	for _, s := range reveal {
		k, v, ok := strings.Cut(s, ": ")
		if !ok {
			return nil, fmt.Errorf("unlabelled reveal entry %q", s)
		}
		f[k] = v
	}
	hexField := func(name string) ([]byte, error) {
		v, ok := f[name]
		if !ok {
			return nil, fmt.Errorf("the reveal has no %s", name)
		}
		b, err := hex.DecodeString(strings.TrimPrefix(v, "0x"))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return b, nil
	}
	beta, err := hexField("beta")
	if err != nil {
		return nil, err
	}
	pk, err := hexField("publickey")
	if err != nil {
		return nil, err
	}
	proof, err := hexField("proof")
	if err != nil {
		return nil, err
	}
	salt, ok := f["salt"]
	if !ok {
		return nil, errors.New("the reveal has no salt")
	}
	alphaQ, ok := f["alpha"]
	if !ok {
		return nil, errors.New("the reveal has no alpha")
	}
	alpha, err := strconv.Unquote(alphaQ)
	if err != nil {
		return nil, fmt.Errorf("alpha is not a quoted string: %w", err)
	}

	out, err := vrf.Verify(pk, []byte(alpha), proof)
	if err != nil {
		return nil, fmt.Errorf("the VRF proof does not verify: %w", err)
	}
	if hex.EncodeToString(out) != hex.EncodeToString(beta) {
		return nil, errors.New("beta is not the VRF's output for alpha")
	}
	chamber := ChamberOf(beta, size)
	if Hash(chamber, salt) != commit {
		return nil, fmt.Errorf("chamber %d with this salt does not hash to the commitment", chamber)
	}
	return &Checked{Chamber: chamber, Alpha: alpha, PublicKey: pk}, nil
}
