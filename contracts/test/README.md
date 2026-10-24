# Isolated contract behavior

With Node 22 or later, run `npm ci && npm test` in this directory. The independent
lock leaves the production compiler and web dependency graphs unchanged.

The suite uses the actual production compiler helper: Solidity 0.8.19, Paris,
optimizer 1,000,000. Both complete Go-embedded artifacts must match fresh current
source compilation, including literal source text and metadata. The refreshed
artifacts correct the previously saved CRLF inputs to the repository's LF text;
ABI and executable creation/runtime bytes excluding terminal metadata remain
unchanged. The original `check:mainnet` also runs in CI. It compares recorded
creation inputs, with the existing metadata exclusion, and makes no fresh
Ethereum query.

Normal tests execute the real Bullet token and unchanged escrow. They create a
real in-memory V2 pool, exercise buys/sells and tax collection with independent
reserve arithmetic, and cover roles, launch, approvals, bet caps, proportional
settlement, burns, revenue rounding, concurrent games, aborts and full-state
rollback. Four fixed seeds exercise 128 mixed game operations against independent
balance, supply, liability and decoded receipt models. A small controlled token
forces late collection, payout, revenue and refund failures. No token or escrow
storage is overridden.

`upstream-contracts` contains compact ABI/creation-code fixtures derived without
bytecode changes from the pinned `zvm-node` exports, the exact WZNN source, and
the complete retained standard JSON source inputs for the Uniswap compiler
cohorts. The provenance records their source/archive/file hashes, compiler
versions and settings. Original license notices and the full GPL text are kept.
The four counterparts are not freshly recompiled by this suite.

The VM explicitly uses Osaka for retained WZNN. Bullet's constructor selects a
fixed router address. The fixture first executes the pinned router constructor
with the actual factory/WZNN, then installs exactly that returned runtime at the
fixed address as one explicit synthetic code alias. Constructor immutables and
both getter results are checked. The actual factory creates the Pair, whose
runtime must equal its pinned export. This alias does not establish canonical
deployment history or router-address provenance.

These are unsigned EthereumJS message frames. Canonical transaction validation,
intrinsic gas, fees, actual ZVM execution, production gas limits, RPC, Telegram,
real wallet/fund flows, randomness fairness and the complete Go game engine are
separate acceptance gates. Existing live Go tests remain gated; their skips do
not establish integration. The normal configured escrow parameters are tested;
this suite does not qualify arbitrary administrative/economic configurations.
