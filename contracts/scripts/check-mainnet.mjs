// Proves mainnet/ holds the source that is actually deployed on Ethereum, not
// merely a source someone once verified: compile each file with the recorded
// settings and compare against the creation bytecode Ethereum ran, which
// mainnet/mainnet.json holds together with the constructor arguments.
//
// Creation code rather than runtime code, because the runtime code of both
// contracts has immutables (decimals, the chain id, the domain separator, the
// escrow's parameters) written into it by the constructor, and the creation
// code has not.
import {readFileSync} from 'node:fs'
import {compile, stripMetadata, version} from './solc.mjs'

const mainnet = JSON.parse(readFileSync(new URL('../mainnet/mainnet.json', import.meta.url), 'utf8'))
let failed = 0
console.log(`solc ${version()}`)

for (const [name, unit] of [['BulletGame', 'BulletGame.sol'], ['TelegramRussianRoulette', 'TelegramRussianRoulette.sol']]) {
  const m = mainnet[name]
  const {output} = compile(unit, new URL(`../mainnet/${unit}`, import.meta.url), name)
  const built = output.evm.bytecode.object.toLowerCase()
  const args = (m.constructorArgs || '').toLowerCase()
  const onchain = m.creationBytecode.toLowerCase().replace(/^0x/, '')
  // Blockscout's creation bytecode is the whole creation input: code, then the
  // ABI-encoded constructor arguments.
  const expected = args ? onchain.slice(0, onchain.length - args.length) : onchain
  if (args && !onchain.endsWith(args)) {
    console.log(`FAIL ${name}: the creation input does not end with the recorded constructor arguments`)
    failed++
    continue
  }
  if (built === expected) {
    console.log(`ok   ${name} at ${m.address}: creation code identical, ${built.length / 2} bytes, metadata hash included`)
  } else if (stripMetadata(built) === stripMetadata(expected)) {
    // What Sourcify calls a partial match, and the best anyone can do here:
    // Sourcify's and Blockscout's own records of both contracts are partial
    // too. Every executable byte is identical; the CBOR tail differs because it
    // hashes the author's exact file, comments and whitespace included, and
    // that file was never published.
    console.log(`ok   ${name} at ${m.address}: executable code identical, ${stripMetadata(built).length / 2} bytes; metadata hash differs (a partial match, as on Sourcify)`)
  } else {
    console.log(`FAIL ${name}: creation code differs (${built.length / 2} built, ${expected.length / 2} on chain)`)
    failed++
  }
}
process.exit(failed ? 1 : 0)
