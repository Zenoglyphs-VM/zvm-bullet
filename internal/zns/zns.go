// Package zns reads ZNS primary names: what an address calls itself on the
// ZVM (zvm-zns), so a message can say alice.znn where it would say
// 0x1234…abcd.
//
// It asks the registry directly, as ENS's UniversalResolver does: the
// address's reverse node names a resolver, whose `name` is the claim, and the
// claim counts only when that name's own resolver points back at the
// address. Anyone can claim any name in a reverse record; only the forward
// check makes it true. This package is duplicated in zvm-rain rather than
// shared, because neither bot depends on zvm-core.
package zns

import (
	"context"
	"strings"
	"sync"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

var (
	selResolver   = crypto.Keccak256([]byte("resolver(bytes32)"))[:4]
	selName       = crypto.Keccak256([]byte("name(bytes32)"))[:4]
	selAddr       = crypto.Keccak256([]byte("addr(bytes32)"))[:4]
	stringType, _ = abi.NewType("string", "", nil)
)

// A name is cached for ten minutes, and "no name" as well: a group chat
// mentions the same few addresses over and over, and a primary name changes
// rarely.
const ttl = 10 * time.Minute

type entry struct {
	name string
	at   time.Time
}

// Names answers "what does this address call itself". A nil *Names answers
// "" for everything, so a bot on a chain without ZNS needs no special case.
type Names struct {
	eth      ethereum.ContractCaller
	registry common.Address
	mu       sync.Mutex
	cache    map[common.Address]entry
}

// New reads names through ZNS's registry (ENSRegistry in zvm-zns's
// deployment record). Every devnet deploys its own ZNS, so the registry is a
// setting, and New returns nil when it is the zero address: no names.
func New(eth ethereum.ContractCaller, registry common.Address) *Names {
	if registry == (common.Address{}) {
		return nil
	}
	return &Names{eth: eth, registry: registry, cache: map[common.Address]entry{}}
}

// Name is a's verified primary name, or "". It never takes longer than two
// seconds: a message is not held up for a name.
func (n *Names) Name(a common.Address) string {
	if n == nil {
		return ""
	}
	n.mu.Lock()
	if e, ok := n.cache[a]; ok && time.Since(e.at) < ttl {
		n.mu.Unlock()
		return e.name
	}
	n.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	name, err := n.lookup(ctx, a)
	if err != nil {
		// Unanswered is not "no name": don't cache it, ask again next time.
		return ""
	}
	n.mu.Lock()
	n.cache[a] = entry{name: name, at: time.Now()}
	n.mu.Unlock()
	return name
}

func (n *Names) lookup(ctx context.Context, a common.Address) (string, error) {
	reverse := Namehash(strings.ToLower(a.Hex()[2:]) + ".addr.reverse")
	resolver, err := n.address(ctx, n.registry, selResolver, reverse)
	if err != nil || resolver == (common.Address{}) {
		return "", err
	}
	out, err := n.call(ctx, resolver, selName, reverse)
	if err != nil || len(out) == 0 {
		return "", err
	}
	vals, err := abi.Arguments{{Type: stringType}}.Unpack(out)
	if err != nil {
		return "", nil
	}
	name, _ := vals[0].(string)
	if name == "" || !strings.HasSuffix(name, ".znn") {
		return "", nil
	}
	forward := Namehash(name)
	fr, err := n.address(ctx, n.registry, selResolver, forward)
	if err != nil || fr == (common.Address{}) {
		return "", err
	}
	back, err := n.address(ctx, fr, selAddr, forward)
	if err != nil {
		return "", err
	}
	if back != a {
		return "", nil
	}
	return name, nil
}

func (n *Names) call(ctx context.Context, to common.Address, sel []byte, node common.Hash) ([]byte, error) {
	data := append(append([]byte{}, sel...), node.Bytes()...)
	return n.eth.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
}

func (n *Names) address(ctx context.Context, to common.Address, sel []byte, node common.Hash) (common.Address, error) {
	out, err := n.call(ctx, to, sel, node)
	if err != nil || len(out) < 32 {
		return common.Address{}, err
	}
	return common.BytesToAddress(out[12:32]), nil
}

// Namehash is ENS's (EIP-137). Names here are already in normal form: ZNS
// accepts only a-z, 0-9 and "-".
func Namehash(name string) common.Hash {
	node := common.Hash{}
	if name == "" {
		return node
	}
	labels := strings.Split(name, ".")
	for i := len(labels) - 1; i >= 0; i-- {
		node = crypto.Keccak256Hash(node.Bytes(), crypto.Keccak256([]byte(labels[i])))
	}
	return node
}
