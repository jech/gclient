package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gorilla/websocket"
	"github.com/jech/gclient"
)

func main() {
	var username, password string
	var insecure bool
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr,
			"Usage: %s group\n", os.Args[0],
		)
		flag.PrintDefaults()
	}
	flag.StringVar(&username, "username", "chat-example",
		"`username` to use for login")
	flag.StringVar(&password, "password", "",
		"`password` to use for login")
	flag.BoolVar(&insecure, "insecure", false,
		"don't check server certificates")
	flag.BoolVar(&gclient.Debug, "debug", false,
		"enable protocol logging")
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(1)
	}

	client := gclient.NewClient()

	if insecure {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		client.SetHTTPClient(&http.Client{
			Transport: t,
		})

		d := *websocket.DefaultDialer
		d.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		client.SetDialer(&d)
	}

	err := client.Connect(context.Background(), flag.Arg(0))
	if err != nil {
		log.Fatalf("Connect: %v", err)
	}

	err = client.Join(
		context.Background(), flag.Arg(0), username, password,
	)
	if err != nil {
		log.Fatalf("Join: %v", err)
	}

	messages := make(chan string)
	go func(messages chan<- string) {
		defer close(messages)
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return
			}
			messages <- scanner.Text()
		}
	}(messages)

	terminate := make(chan os.Signal, 1)
	signal.Notify(terminate, syscall.SIGINT, syscall.SIGTERM)

outer:
	for {
		select {
		case <-terminate:
			break outer
		case m, ok := <-messages:
			if !ok {
				break outer
			}
			client.Chat("", "", m)
		case e := <-client.EventCh:
			switch e := e.(type) {
			case gclient.JoinedEvent:
				if e.Kind == "failed" {
					log.Printf("Join failed: %v", e.Value)
					break outer
				}
			case gclient.UserMessageEvent:
				if e.Kind == "error" || e.Kind == "warning" {
					log.Printf("%v: %v", e.Kind, e.Value)
				}
			case gclient.ChatEvent:
				if e.Kind == "me" {
					fmt.Printf("* %v %v\n",
						e.Username, e.Value,
					)
				} else if e.Username == "" {
					fmt.Printf("%v\n", e.Value)
				} else {
					fmt.Printf("%v: %v\n",
						e.Username, e.Value,
					)
				}
			case error:
				log.Println(e)
				break outer
			}
		}
	}
}
