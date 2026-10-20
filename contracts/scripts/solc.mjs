// One compile, with exactly the settings the mainnet contracts were verified
// with (mainnet/mainnet.json): solc 0.8.19, optimizer on at 1,000,000 runs,
// evmVersion paris, ipfs metadata hash. The metadata hash covers the source
// unit's name and text, so a source compiled under another file name, or with
// one byte changed, gives different bytecode, which is what makes the mainnet
// comparison meaningful.
import {createRequire} from 'node:module'
import {readFileSync} from 'node:fs'

const require = createRequire(import.meta.url)
const solc = require('solc')

export const SETTINGS = {
  evmVersion: 'paris',
  libraries: {},
  metadata: {bytecodeHash: 'ipfs'},
  optimizer: {enabled: true, runs: 1000000},
  remappings: [],
  outputSelection: {'*': {'*': ['abi', 'evm.bytecode.object', 'evm.deployedBytecode.object', 'evm.deployedBytecode.immutableReferences', 'metadata']}},
}

export function version() {
  return solc.version()
}

// compile builds one single-file source unit and returns the named contract's
// output together with the standard JSON input that produced it, which is what
// the node's Etherscan-compatible verifier takes.
export function compile(unitName, path, contractName) {
  const input = {language: 'Solidity', sources: {[unitName]: {content: readFileSync(path, 'utf8')}}, settings: SETTINGS}
  const out = JSON.parse(solc.compile(JSON.stringify(input)))
  const errors = (out.errors || []).filter((e) => e.severity === 'error')
  if (errors.length) throw new Error(errors.map((e) => e.formattedMessage).join('\n'))
  const c = out.contracts?.[unitName]?.[contractName]
  if (!c) throw new Error(`${contractName} not found in ${unitName}`)
  return {input, output: c}
}

// stripMetadata drops the CBOR metadata that solc appends to runtime and
// creation code: its last two bytes are its own length.
export function stripMetadata(hex) {
  const h = hex.startsWith('0x') ? hex.slice(2) : hex
  const len = parseInt(h.slice(-4), 16)
  return h.slice(0, h.length - (len + 2) * 2)
}
