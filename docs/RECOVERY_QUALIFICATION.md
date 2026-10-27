# Restart recovery and authority limits

Startup calls `Manager.Recover` before registering commands or polling updates.
An unfinished recovery must return an error so this startup gate remains closed.
The retained game record is evidence to reconcile; a current inactive escrow is
not proof that a previously starting or playing game never collected stakes.

A joining game can be cancelled only after its cancellation is recorded. A
confirmed abort is announced only after its refund transaction is recorded.
If that database write fails, recovery returns the available transaction hash
with the error and preserves the unfinished row. An operator must reconcile
the original transaction and database before restarting; this path does not
manufacture a successful record or submit another refund for an inactive game.

The recovery tests use actual temporary SQLite files and the production manager
and store. SQL triggers refuse selected state writes. An in-memory escrow
supplies the chain interface, including an operator failure explicitly before
submission. Controls cover query refusal, a completed cancellation/refund and a
repeat recovery that submits nothing further. The workflow also runs the
selected recovery tests with the Go race detector.

These cases do not qualify the live bot, a real operator key or token refunds.
They do not provide a durable transaction-intent journal, resolve an uncertain
send, cover every start/end crash window, or prove recovery after hardware loss.
Original transaction identity must be preserved when an outcome is unknown.

The commitment fixes a chamber after publication. The operator chooses the
timestamp supplied to its VRF and can try inputs before publishing; excluding
house wallets does not make that selection unbiased. The escrow owner can
collect approved BULLET from connected wallets. `connectAndApprove` grants the
escrow an unlimited allowance. A separately confirmed finite `approve` limits
the allowance only from that point; the connection transaction itself still
grants unlimited approval. Wallet funding and allowance are separate exposure
bounds, and a later connection can restore unlimited approval. These are user
and operator controls, not an enforced per-game authorization protocol.

The contract comparison checks recorded executable bytecode while excluding
terminal compiler metadata hashes. It does not establish full byte-for-byte
equality, a fresh mainnet observation or an independently accepted deployment.
All crash/reconnect paths, robust transaction reconciliation, operator-loss
procedures and enforced exposure policy remain separate qualification work.
