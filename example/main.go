package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/artyom/twistbot"
)

func main() {
	addr, token := "localhost:8080", ""
	flag.StringVar(&addr, "addr", addr, "address to listen")
	flag.StringVar(&token, "token", os.Getenv("TOKEN"), "token to verify incoming messages (TOKEN env)")
	flag.Parse()
	if err := run(addr, token); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(addr, token string) error {
	handler := &twistbot.Handler{
		Token: token,
		Usage: "I'm just a bot, I can only greet you back, if you say 'Hello'",
	}
	handler.AddRule(greeter)
	return http.ListenAndServe(addr, handler)
}

func greeter(ctx context.Context, w http.ResponseWriter, msg twistbot.Message) error {
	if !strings.HasPrefix(msg.Text, "Hello") {
		return twistbot.SkipRule
	}
	fmt.Fprintf(w, "Hello to you as well, %s!\n", msg.UserName)
	return nil
}
