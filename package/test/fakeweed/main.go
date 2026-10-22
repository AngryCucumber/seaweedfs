// Command fakeweed stands in for bin/weed in package/test/scripts_test.sh.
// It writes its pid, working directory, WEED_CONFIG_DIR and arguments to
// $FAKE_WEED_RECORD and then behaves as $FAKE_WEED_MODE says:
//
//	""             run until SIGTERM
//	"exit"         fail at once
//	"ignore-term"  ignore SIGTERM, run until killed
package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	cwd, _ := os.Getwd()
	record := fmt.Sprintf("pid=%d\ncwd=%s\nWEED_CONFIG_DIR=%s\n", os.Getpid(), cwd, os.Getenv("WEED_CONFIG_DIR"))
	for _, arg := range os.Args[1:] {
		record += "arg=" + arg + "\n"
	}
	if path := os.Getenv("FAKE_WEED_RECORD"); path != "" {
		if err := os.WriteFile(path, []byte(record), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	fmt.Println("fakeweed started:", strings.Join(os.Args[1:], " "))
	switch os.Getenv("FAKE_WEED_MODE") {
	case "exit":
		fmt.Fprintln(os.Stderr, "fakeweed: simulated startup failure")
		os.Exit(3)
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		for {
			time.Sleep(time.Hour)
		}
	default:
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		<-term
		fmt.Println("fakeweed: SIGTERM")
	}
}
