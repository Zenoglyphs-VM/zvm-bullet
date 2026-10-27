package game

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Zenoglyphs-VM/zvm-bullet/internal/store"
)

// Recovery fixtures use a real temporary SQLite database and an in-memory
// escrow. The injected operator failure occurs before any submission. They do
// not qualify transaction ambiguity, a live operator or actual token refunds.
type recoveryGateChain struct {
	*fakeChain
	queryErr      error
	abortErr      error
	queries       int
	abortAttempts int
}

func (f *recoveryGateChain) InProgress(ctx context.Context, chat int64) (bool, error) {
	f.queries++
	if f.queryErr != nil {
		return false, f.queryErr
	}
	return f.fakeChain.InProgress(ctx, chat)
}

func (f *recoveryGateChain) AbortGame(ctx context.Context, chat int64) (common.Hash, error) {
	f.abortAttempts++
	if f.abortErr != nil {
		return common.Hash{}, f.abortErr
	}
	return f.fakeChain.AbortGame(ctx, chat)
}

type recoveryGateFixture struct {
	m     *Manager
	st    *store.Store
	chain *recoveryGateChain
	out   *fakeOut
	path  string
}

func newRecoveryGate(t *testing.T) *recoveryGateFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recovery.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &recoveryGateFixture{st: st, chain: &recoveryGateChain{fakeChain: newFakeChain()},
		out: &fakeOut{ch: make(chan string, 256)}, path: path}
	f.m = NewManager(context.Background(), Config{Chain: f.chain, Store: st, Out: f.out})
	return f
}

func (f *recoveryGateFixture) game(t *testing.T, chat int64, state string) int64 {
	t.Helper()
	ctx, now := context.Background(), time.Unix(1_700_000_000, 0)
	id, err := f.st.NewGame(ctx, chat, 6, bullets(1), 1, now)
	if err != nil {
		t.Fatal(err)
	}
	switch state {
	case "joining":
	case "starting":
		err = f.st.Starting(ctx, id, 1, strings.Repeat("1", 64), "synthetic reveal")
	case "playing":
		err = f.st.Started(ctx, id, common.HexToHash("0x01").Hex(), nil, now)
	default:
		t.Fatalf("unexpected recovery fixture state %q", state)
	}
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *recoveryGateFixture) state(t *testing.T, id int64, want string) {
	t.Helper()
	g, err := f.st.Game(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if g.State != want {
		t.Errorf("game %d state %q, want retained %q", id, g.State, want)
	}
}

func (f *recoveryGateFixture) refuseStateWrite(t *testing.T, state string) {
	t.Helper()
	if state != "aborted" && state != "cancelled" {
		t.Fatal("unexpected synthetic state-write fault")
	}
	db, err := sql.Open("sqlite", "file:"+f.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(fmt.Sprintf(`CREATE TRIGGER recovery_gate_refusal
BEFORE UPDATE OF state ON games WHEN NEW.state = '%s'
BEGIN SELECT RAISE(FAIL, 'synthetic recovery storage refusal'); END`, state))
	if err != nil {
		t.Fatal(err)
	}
}

func (f *recoveryGateFixture) noSuccessClaim(t *testing.T) {
	t.Helper()
	for {
		select {
		case text := <-f.out.ch:
			lower := strings.ToLower(text)
			if strings.Contains(lower, "every stake refunded") || strings.Contains(lower, "no bets were collected") {
				t.Errorf("unresolved recovery emitted a completion claim: %s", text)
			}
		default:
			return
		}
	}
}

func TestRecoveryGateOperatorUnavailable(t *testing.T) {
	f := newRecoveryGate(t)
	id := f.game(t, -101, "playing")
	f.chain.inGame[-101] = true
	want := errors.New("synthetic operator unavailable before submission")
	f.chain.abortErr = want
	if err := f.m.Recover(); !errors.Is(err, want) {
		t.Errorf("operator failure did not stop recovery: %v", err)
	}
	f.state(t, id, "playing")
	if f.chain.abortAttempts != 1 || len(f.chain.aborts) != 0 {
		t.Errorf("operator failure attempted or completed an unexpected refund: %d, %v", f.chain.abortAttempts, f.chain.aborts)
	}
	f.noSuccessClaim(t)
}

func TestRecoveryGateInactiveStartedGame(t *testing.T) {
	for _, state := range []string{"starting", "playing"} {
		t.Run(state, func(t *testing.T) {
			f := newRecoveryGate(t)
			id := f.game(t, -101, state)
			if err := f.m.Recover(); err == nil {
				t.Error("an inactive current escrow was accepted as proof that no stakes were collected")
			}
			f.state(t, id, state)
			if f.chain.abortAttempts != 0 {
				t.Error("unknown historical outcome caused a new abort submission")
			}
			f.noSuccessClaim(t)
		})
	}
}

func TestRecoveryGateCancellationRecordFailure(t *testing.T) {
	f := newRecoveryGate(t)
	id := f.game(t, -101, "joining")
	f.refuseStateWrite(t, "cancelled")
	if err := f.m.Recover(); err == nil || !strings.Contains(err.Error(), "synthetic recovery storage refusal") {
		t.Errorf("failed durable cancellation did not stop recovery: %v", err)
	}
	f.state(t, id, "joining")
	if f.chain.queries != 0 || f.chain.abortAttempts != 0 {
		t.Error("a joining cancellation unexpectedly touched the escrow")
	}
	f.noSuccessClaim(t)
}

func TestRecoveryGateRefundRecordFailure(t *testing.T) {
	f := newRecoveryGate(t)
	id := f.game(t, -101, "playing")
	f.chain.inGame[-101] = true
	f.refuseStateWrite(t, "aborted")
	err := f.m.Recover()
	if err == nil || !strings.Contains(err.Error(), "synthetic recovery storage refusal") {
		t.Errorf("failed durable refund record did not stop recovery: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), common.HexToHash("0x03").Hex()) {
		t.Errorf("recovery discarded the available refund transaction identity: %v", err)
	}
	f.state(t, id, "playing")
	if f.chain.abortAttempts != 1 || len(f.chain.aborts) != 1 || f.chain.inGame[-101] {
		t.Error("unexpected synthetic refund transition")
	}
	f.noSuccessClaim(t)
}

func TestRecoveryGateQueryFailure(t *testing.T) {
	f := newRecoveryGate(t)
	id := f.game(t, -101, "playing")
	want := errors.New("synthetic escrow query refusal")
	f.chain.queryErr = want
	if err := f.m.Recover(); !errors.Is(err, want) {
		t.Errorf("escrow read failure did not stop recovery: %v", err)
	}
	f.state(t, id, "playing")
	if f.chain.abortAttempts != 0 {
		t.Error("failed escrow read caused an abort submission")
	}
	f.noSuccessClaim(t)
}

func TestRecoveryGateCompletedRecoveryIsStable(t *testing.T) {
	f := newRecoveryGate(t)
	playing := f.game(t, -101, "playing")
	joining := f.game(t, -102, "joining")
	f.chain.inGame[-101] = true
	for attempt := 0; attempt < 2; attempt++ {
		if err := f.m.Recover(); err != nil {
			t.Fatal(err)
		}
		f.state(t, playing, "aborted")
		f.state(t, joining, "cancelled")
		if f.chain.abortAttempts != 1 || len(f.chain.aborts) != 1 || f.chain.queries != 1 {
			t.Fatalf("completed recovery repeated a chain operation: attempts %d, aborts %v, queries %d", f.chain.abortAttempts, f.chain.aborts, f.chain.queries)
		}
	}
	if len(f.chain.newGames) != 0 || len(f.chain.ends) != 0 {
		t.Error("recovery started or ended a new game")
	}
}
