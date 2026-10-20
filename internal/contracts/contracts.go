// Package contracts holds the two Bullet Game contracts as compiled by
// contracts/scripts/build.mjs: the BULLET token (mainnet's, with the ZVM's router)
// and the TelegramRussianRoulette escrow (mainnet's, unchanged).
package contracts

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

//go:embed artifacts/*.json
var files embed.FS

// Artifact is one build output.
type Artifact struct {
	ContractName      string          `json:"contractName"`
	SourceName        string          `json:"sourceName"`
	CompilerVersion   string          `json:"compilerVersion"`
	ABIJSON           json.RawMessage `json:"abi"`
	Bytecode          hexutil.Bytes   `json:"bytecode"`
	DeployedBytecode  hexutil.Bytes   `json:"deployedBytecode"`
	StandardJSONInput json.RawMessage `json:"standardJsonInput"`

	ABI abi.ABI `json:"-"`
}

func load(name string) *Artifact {
	raw, err := files.ReadFile("artifacts/" + name + ".json")
	if err != nil {
		panic(err)
	}
	var a Artifact
	if err := json.Unmarshal(raw, &a); err != nil {
		panic(fmt.Errorf("%s: %w", name, err))
	}
	if a.ABI, err = abi.JSON(strings.NewReader(string(a.ABIJSON))); err != nil {
		panic(fmt.Errorf("%s abi: %w", name, err))
	}
	return &a
}

var (
	Token  = load("BulletGame")
	Escrow = load("TelegramRussianRoulette")
)

// Mainnet's parameters, read off Ethereum and kept here so the the ZVM deploy
// cannot drift from them by accident.
const (
	// Decimals of BULLET.
	Decimals = 8
	// EscrowMinimumBet, EscrowRevenueBps and EscrowBurnBps are the escrow's
	// constructor arguments on mainnet (0x4d2E…5DBE): a minimum bet of 1,000
	// base units, 9% of the loser's stake to revenue and 1% burned.
	EscrowMinimumBet = 1000
	EscrowRevenueBps = 900
	EscrowBurnBps    = 100
)

// ChainID is the ZVM's EVM chain id, the one the token's constructor accepts.
const ChainID = 7340469

// Router is the Uniswap V2 router the the ZVM token is built against. It is a
// constant in the contract, so it is a constant here; deploy checks that the
// node still reports the same router before sending anything.
var Router = common.HexToAddress("0xA381AE1E4C9573E38ccD95D37646C891D9e4Ca34")

// Pack encodes a call.
func (a *Artifact) Pack(method string, args ...any) []byte {
	data, err := a.ABI.Pack(method, args...)
	if err != nil {
		panic(fmt.Errorf("%s.%s: %w", a.ContractName, method, err))
	}
	return data
}

// DeployData is the creation code followed by the encoded constructor
// arguments.
func (a *Artifact) DeployData(args ...any) ([]byte, error) {
	ctor, err := a.ABI.Pack("", args...)
	if err != nil {
		return nil, err
	}
	return append(append([]byte(nil), a.Bytecode...), ctor...), nil
}
