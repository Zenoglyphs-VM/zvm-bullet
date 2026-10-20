// zvm-bullet is Bullet Game on the ZVM: Russian Roulette in Telegram, betting
// BULLET through the contracts mainnet ran, on the ZVM.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

const usage = `zvm-bullet: Bullet Game on the ZVM

  keys    -out FILE                 generate the operator's secrets file
  deploy  -keys FILE -record FILE   launch the token and escrow, as mainnet did
  check   -record FILE              read a deployment back and check it
  bot     -keys FILE -record FILE   run the Telegram bot
  verify  -record FILE TX           check an endGame's reveal against its newGame
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	var err error
	args := os.Args[2:]
	switch os.Args[1] {
	case "keys":
		err = cmdKeys(args)
	case "deploy":
		err = cmdDeploy(ctx, args)
	case "check":
		err = cmdCheck(ctx, args)
	case "bot":
		err = cmdBot(ctx, args)
	case "verify":
		err = cmdVerify(ctx, args)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "zvm-bullet:", err)
		os.Exit(1)
	}
}
